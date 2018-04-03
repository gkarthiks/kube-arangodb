//
// DISCLAIMER
//
// Copyright 2018 ArangoDB GmbH, Cologne, Germany
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Copyright holder is ArangoDB GmbH, Cologne, Germany
//
// Author Ewout Prangsma
//

package deployment

import (
	"fmt"
	"reflect"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	api "github.com/arangodb/kube-arangodb/pkg/apis/deployment/v1alpha"
	"github.com/arangodb/kube-arangodb/pkg/deployment/chaos"
	"github.com/arangodb/kube-arangodb/pkg/deployment/reconcile"
	"github.com/arangodb/kube-arangodb/pkg/deployment/resilience"
	"github.com/arangodb/kube-arangodb/pkg/deployment/resources"
	"github.com/arangodb/kube-arangodb/pkg/generated/clientset/versioned"
	"github.com/arangodb/kube-arangodb/pkg/util/k8sutil"
	"github.com/arangodb/kube-arangodb/pkg/util/retry"
	"github.com/arangodb/kube-arangodb/pkg/util/trigger"
)

// Config holds configuration settings for a Deployment
type Config struct {
	ServiceAccount string
	AllowChaos     bool
}

// Dependencies holds dependent services for a Deployment
type Dependencies struct {
	Log           zerolog.Logger
	KubeCli       kubernetes.Interface
	DatabaseCRCli versioned.Interface
}

// deploymentEventType strongly typed type of event
type deploymentEventType string

const (
	eventArangoDeploymentUpdated deploymentEventType = "ArangoDeploymentUpdated"
)

// deploymentEvent holds an event passed from the controller to the deployment.
type deploymentEvent struct {
	Type       deploymentEventType
	Deployment *api.ArangoDeployment
}

const (
	deploymentEventQueueSize = 256
	minInspectionInterval    = time.Second // Ensure we inspect the generated resources no less than with this interval
	maxInspectionInterval    = time.Minute // Ensure we inspect the generated resources no less than with this interval
)

// Deployment is the in process state of an ArangoDeployment.
type Deployment struct {
	apiObject *api.ArangoDeployment // API object
	status    api.DeploymentStatus  // Internal status of the CR
	config    Config
	deps      Dependencies

	eventCh chan *deploymentEvent
	stopCh  chan struct{}

	eventsCli corev1.EventInterface

	inspectTrigger            trigger.Trigger
	updateDeploymentTrigger   trigger.Trigger
	clientCache               *clientCache
	recentInspectionErrors    int
	clusterScalingIntegration *clusterScalingIntegration
	reconciler                *reconcile.Reconciler
	resilience                *resilience.Resilience
	resources                 *resources.Resources
	chaosMonkey               *chaos.Monkey
}

// New creates a new Deployment from the given API object.
func New(config Config, deps Dependencies, apiObject *api.ArangoDeployment) (*Deployment, error) {
	if err := apiObject.Spec.Validate(); err != nil {
		return nil, maskAny(err)
	}
	d := &Deployment{
		apiObject:   apiObject,
		status:      *(apiObject.Status.DeepCopy()),
		config:      config,
		deps:        deps,
		eventCh:     make(chan *deploymentEvent, deploymentEventQueueSize),
		stopCh:      make(chan struct{}),
		eventsCli:   deps.KubeCli.Core().Events(apiObject.GetNamespace()),
		clientCache: newClientCache(deps.KubeCli, apiObject),
	}
	d.reconciler = reconcile.NewReconciler(deps.Log, d)
	d.resilience = resilience.NewResilience(deps.Log, d)
	d.resources = resources.NewResources(deps.Log, d)
	if d.status.AcceptedSpec == nil {
		// We've validated the spec, so let's use it from now.
		d.status.AcceptedSpec = apiObject.Spec.DeepCopy()
	}

	go d.run()
	go d.listenForPodEvents(d.stopCh)
	go d.listenForPVCEvents(d.stopCh)
	go d.listenForSecretEvents(d.stopCh)
	go d.listenForServiceEvents(d.stopCh)
	if apiObject.Spec.GetMode() == api.DeploymentModeCluster {
		ci := newClusterScalingIntegration(d)
		d.clusterScalingIntegration = ci
		go ci.ListenForClusterEvents(d.stopCh)
	}
	if config.AllowChaos {
		d.chaosMonkey = chaos.NewMonkey(deps.Log, d)
		d.chaosMonkey.Run(d.stopCh)
	}

	return d, nil
}

// Update the deployment.
// This sends an update event in the deployment event queue.
func (d *Deployment) Update(apiObject *api.ArangoDeployment) {
	d.send(&deploymentEvent{
		Type:       eventArangoDeploymentUpdated,
		Deployment: apiObject,
	})
}

// Delete the deployment.
// Called when the deployment was deleted by the user.
func (d *Deployment) Delete() {
	d.deps.Log.Info().Msg("deployment is deleted by user")
	close(d.stopCh)
}

// send given event into the deployment event queue.
func (d *Deployment) send(ev *deploymentEvent) {
	select {
	case d.eventCh <- ev:
		l, ecap := len(d.eventCh), cap(d.eventCh)
		if l > int(float64(ecap)*0.8) {
			d.deps.Log.Warn().
				Int("used", l).
				Int("capacity", ecap).
				Msg("event queue buffer is almost full")
		}
	case <-d.stopCh:
	}
}

// run is the core the core worker.
// It processes the event queue and polls the state of generated
// resource on a regular basis.
func (d *Deployment) run() {
	log := d.deps.Log

	if d.status.Phase == api.DeploymentPhaseNone {
		// Create secrets
		if err := d.resources.EnsureSecrets(); err != nil {
			d.CreateEvent(k8sutil.NewErrorEvent("Failed to create secrets", err, d.GetAPIObject()))
		}

		// Create services
		if err := d.resources.EnsureServices(); err != nil {
			d.CreateEvent(k8sutil.NewErrorEvent("Failed to create services", err, d.GetAPIObject()))
		}

		// Create members
		if err := d.createInitialMembers(d.apiObject); err != nil {
			d.CreateEvent(k8sutil.NewErrorEvent("Failed to create initial members", err, d.GetAPIObject()))
		}

		// Create PVCs
		if err := d.resources.EnsurePVCs(); err != nil {
			d.CreateEvent(k8sutil.NewErrorEvent("Failed to create persistent volume claims", err, d.GetAPIObject()))
		}

		// Create pods
		if err := d.resources.EnsurePods(); err != nil {
			d.CreateEvent(k8sutil.NewErrorEvent("Failed to create pods", err, d.GetAPIObject()))
		}

		d.status.Phase = api.DeploymentPhaseRunning
		if err := d.updateCRStatus(); err != nil {
			log.Warn().Err(err).Msg("update initial CR status failed")
		}
		log.Info().Msg("start running...")
	}

	inspectionInterval := maxInspectionInterval
	for {
		select {
		case <-d.stopCh:
			// We're being stopped.
			return

		case event := <-d.eventCh:
			// Got event from event queue
			switch event.Type {
			case eventArangoDeploymentUpdated:
				d.updateDeploymentTrigger.Trigger()
			default:
				panic("unknown event type" + event.Type)
			}

		case <-d.inspectTrigger.Done():
			inspectionInterval = d.inspectDeployment(inspectionInterval)

		case <-d.updateDeploymentTrigger.Done():
			if err := d.handleArangoDeploymentUpdatedEvent(); err != nil {
				d.CreateEvent(k8sutil.NewErrorEvent("Failed to handle deployment update", err, d.GetAPIObject()))
			}

		case <-time.After(inspectionInterval):
			// Trigger inspection
			d.inspectTrigger.Trigger()
			// Backoff with next interval
			inspectionInterval = time.Duration(float64(inspectionInterval) * 1.5)
			if inspectionInterval > maxInspectionInterval {
				inspectionInterval = maxInspectionInterval
			}
		}
	}
}

// handleArangoDeploymentUpdatedEvent is called when the deployment is updated by the user.
func (d *Deployment) handleArangoDeploymentUpdatedEvent() error {
	log := d.deps.Log.With().Str("deployment", d.apiObject.GetName()).Logger()

	// Get the most recent version of the deployment from the API server
	current, err := d.deps.DatabaseCRCli.DatabaseV1alpha().ArangoDeployments(d.apiObject.GetNamespace()).Get(d.apiObject.GetName(), metav1.GetOptions{})
	if err != nil {
		log.Debug().Err(err).Msg("Failed to get current version of deployment from API server")
		if k8sutil.IsNotFound(err) {
			return nil
		}
		return maskAny(err)
	}

	specBefore := d.apiObject.Spec
	if d.status.AcceptedSpec != nil {
		specBefore = *d.status.AcceptedSpec
	}
	newAPIObject := current.DeepCopy()
	newAPIObject.Spec.SetDefaultsFrom(specBefore)
	newAPIObject.Status = d.status
	resetFields := specBefore.ResetImmutableFields(&newAPIObject.Spec)
	if len(resetFields) > 0 {
		log.Debug().Strs("fields", resetFields).Msg("Found modified immutable fields")
	}
	if err := newAPIObject.Spec.Validate(); err != nil {
		d.CreateEvent(k8sutil.NewErrorEvent("Validation failed", err, d.apiObject))
		// Try to reset object
		if err := d.updateCRSpec(d.apiObject.Spec); err != nil {
			log.Error().Err(err).Msg("Restore original spec failed")
			d.CreateEvent(k8sutil.NewErrorEvent("Restore original failed", err, d.apiObject))
		}
		return nil
	}
	if len(resetFields) > 0 {
		for _, fieldName := range resetFields {
			log.Debug().Str("field", fieldName).Msg("Reset immutable field")
			d.CreateEvent(k8sutil.NewImmutableFieldEvent(fieldName, d.apiObject))
		}
	}

	// Save updated spec
	if err := d.updateCRSpec(newAPIObject.Spec); err != nil {
		return maskAny(fmt.Errorf("failed to update ArangoDeployment spec: %v", err))
	}
	// Save updated accepted spec
	d.status.AcceptedSpec = newAPIObject.Spec.DeepCopy()
	if err := d.updateCRStatus(); err != nil {
		return maskAny(fmt.Errorf("failed to update ArangoDeployment status: %v", err))
	}

	// Notify cluster of desired server count
	if ci := d.clusterScalingIntegration; ci != nil {
		ci.SendUpdateToCluster(d.apiObject.Spec)
	}

	// Trigger inspect
	d.inspectTrigger.Trigger()

	return nil
}

// CreateEvent creates a given event.
// On error, the error is logged.
func (d *Deployment) CreateEvent(evt *v1.Event) {
	_, err := d.eventsCli.Create(evt)
	if err != nil {
		d.deps.Log.Error().Err(err).Interface("event", *evt).Msg("Failed to record event")
	}
}

// Update the status of the API object from the internal status
func (d *Deployment) updateCRStatus(force ...bool) error {
	// TODO Remove force....
	if len(force) == 0 || !force[0] {
		if reflect.DeepEqual(d.apiObject.Status, d.status) {
			// Nothing has changed
			return nil
		}
	}

	// Send update to API server
	update := d.apiObject.DeepCopy()
	attempt := 0
	for {
		attempt++
		update.Status = d.status
		ns := d.apiObject.GetNamespace()
		newAPIObject, err := d.deps.DatabaseCRCli.DatabaseV1alpha().ArangoDeployments(ns).Update(update)
		if err == nil {
			// Update internal object
			d.apiObject = newAPIObject
			return nil
		}
		if attempt < 10 && k8sutil.IsConflict(err) {
			// API object may have been changed already,
			// Reload api object and try again
			var current *api.ArangoDeployment
			current, err = d.deps.DatabaseCRCli.DatabaseV1alpha().ArangoDeployments(ns).Get(update.GetName(), metav1.GetOptions{})
			if err == nil {
				update = current.DeepCopy()
				continue
			}
		}
		if err != nil {
			d.deps.Log.Debug().Err(err).Msg("failed to patch ArangoDeployment status")
			return maskAny(fmt.Errorf("failed to patch ArangoDeployment status: %v", err))
		}
	}
}

// Update the spec part of the API object (d.apiObject)
// to the given object, while preserving the status.
// On success, d.apiObject is updated.
func (d *Deployment) updateCRSpec(newSpec api.DeploymentSpec) error {
	// Send update to API server
	update := d.apiObject.DeepCopy()
	attempt := 0
	for {
		attempt++
		update.Spec = newSpec
		update.Status = d.status
		ns := d.apiObject.GetNamespace()
		newAPIObject, err := d.deps.DatabaseCRCli.DatabaseV1alpha().ArangoDeployments(ns).Update(update)
		if err == nil {
			// Update internal object
			d.apiObject = newAPIObject
			return nil
		}
		if attempt < 10 && k8sutil.IsConflict(err) {
			// API object may have been changed already,
			// Reload api object and try again
			var current *api.ArangoDeployment
			current, err = d.deps.DatabaseCRCli.DatabaseV1alpha().ArangoDeployments(ns).Get(update.GetName(), metav1.GetOptions{})
			if err == nil {
				update = current.DeepCopy()
				continue
			}
		}
		if err != nil {
			d.deps.Log.Debug().Err(err).Msg("failed to patch ArangoDeployment spec")
			return maskAny(fmt.Errorf("failed to patch ArangoDeployment spec: %v", err))
		}
	}
}

// failOnError reports the given error and sets the deployment status to failed.
// Since there is no recovery from a failed deployment, use with care!
func (d *Deployment) failOnError(err error, msg string) {
	log.Error().Err(err).Msg(msg)
	d.status.Reason = err.Error()
	d.reportFailedStatus()
}

// reportFailedStatus sets the status of the deployment to Failed and keeps trying to forward
// that to the API server.
func (d *Deployment) reportFailedStatus() {
	log := d.deps.Log
	log.Info().Msg("deployment failed. Reporting failed reason...")

	op := func() error {
		d.status.Phase = api.DeploymentPhaseFailed
		err := d.updateCRStatus()
		if err == nil || k8sutil.IsNotFound(err) {
			// Status has been updated
			return nil
		}

		if !k8sutil.IsConflict(err) {
			log.Warn().Err(err).Msg("retry report status: fail to update")
			return maskAny(err)
		}

		depl, err := d.deps.DatabaseCRCli.DatabaseV1alpha().ArangoDeployments(d.apiObject.Namespace).Get(d.apiObject.Name, metav1.GetOptions{})
		if err != nil {
			// Update (PUT) will return conflict even if object is deleted since we have UID set in object.
			// Because it will check UID first and return something like:
			// "Precondition failed: UID in precondition: 0xc42712c0f0, UID in object meta: ".
			if k8sutil.IsNotFound(err) {
				return nil
			}
			log.Warn().Err(err).Msg("retry report status: fail to get latest version")
			return maskAny(err)
		}
		d.apiObject = depl
		return maskAny(fmt.Errorf("retry needed"))
	}

	retry.Retry(op, time.Hour*24*365)
}

// isOwnerOf returns true if the given object belong to this deployment.
func (d *Deployment) isOwnerOf(obj metav1.Object) bool {
	ownerRefs := obj.GetOwnerReferences()
	if len(ownerRefs) < 1 {
		return false
	}
	return ownerRefs[0].UID == d.apiObject.UID
}
