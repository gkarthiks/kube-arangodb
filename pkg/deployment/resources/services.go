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

package resources

import (
	"time"

	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/arangodb/kube-arangodb/pkg/util/k8sutil"
)

// EnsureServices creates all services needed to service the deployment
func (r *Resources) EnsureServices() error {
	log := r.log
	kubecli := r.context.GetKubeCli()
	apiObject := r.context.GetAPIObject()
	ns := apiObject.GetNamespace()
	owner := apiObject.AsOwner()
	spec := r.context.GetSpec()

	// Headless service
	svcName, newlyCreated, err := k8sutil.CreateHeadlessService(kubecli, apiObject, owner)
	if err != nil {
		log.Debug().Err(err).Msg("Failed to create headless service")
		return maskAny(err)
	}
	if newlyCreated {
		log.Debug().Str("service", svcName).Msg("Created headless service")
	}

	// Internal database client service
	single := spec.GetMode().HasSingleServers()
	svcName, newlyCreated, err = k8sutil.CreateDatabaseClientService(kubecli, apiObject, single, owner)
	if err != nil {
		log.Debug().Err(err).Msg("Failed to create database client service")
		return maskAny(err)
	}
	if newlyCreated {
		log.Debug().Str("service", svcName).Msg("Created database client service")
	}
	status := r.context.GetStatus()
	if status.ServiceName != svcName {
		status.ServiceName = svcName
		if err := r.context.UpdateStatus(status); err != nil {
			return maskAny(err)
		}
	}

	// Database external access service
	createExternalAccessService := false
	deleteExternalAccessService := false
	eaServiceType := spec.ExternalAccess.GetType().AsServiceType() // Note: Type auto defaults to ServiceTypeLoadBalancer
	eaServiceName := k8sutil.CreateDatabaseExternalAccessServiceName(apiObject.GetName())
	svcCli := kubecli.CoreV1().Services(ns)
	if existing, err := svcCli.Get(eaServiceName, metav1.GetOptions{}); err == nil {
		// External access service exists
		loadBalancerIP := spec.ExternalAccess.GetLoadBalancerIP()
		nodePort := spec.ExternalAccess.GetNodePort()
		if spec.ExternalAccess.GetType().IsNone() {
			// Should not be there, remove it
			deleteExternalAccessService = true
		} else if spec.ExternalAccess.GetType().IsAuto() {
			// Inspect existing service.
			if existing.Spec.Type == v1.ServiceTypeLoadBalancer {
				// See if LoadBalancer has been configured & the service is "old enough"
				oldEnoughTimestamp := time.Now().Add(-1 * time.Minute) // How long does the load-balancer provisioner have to act.
				if len(existing.Status.LoadBalancer.Ingress) == 0 && existing.GetObjectMeta().GetCreationTimestamp().Time.Before(oldEnoughTimestamp) {
					log.Info().Str("service", eaServiceName).Msg("LoadBalancerIP of database external access service is not set, switching to NodePort")
					createExternalAccessService = true
					eaServiceType = v1.ServiceTypeNodePort
					deleteExternalAccessService = true // Remove the LoadBalancer ex service, then add the NodePort one
				} else if existing.Spec.Type == v1.ServiceTypeLoadBalancer && (loadBalancerIP != "" && existing.Spec.LoadBalancerIP != loadBalancerIP) {
					deleteExternalAccessService = true // LoadBalancerIP is wrong, remove the current and replace with proper one
					createExternalAccessService = true
				} else if existing.Spec.Type == v1.ServiceTypeNodePort && len(existing.Spec.Ports) == 1 && (nodePort != 0 && existing.Spec.Ports[0].NodePort != int32(nodePort)) {
					deleteExternalAccessService = true // NodePort is wrong, remove the current and replace with proper one
					createExternalAccessService = true
				}
			}
		} else if spec.ExternalAccess.GetType().IsLoadBalancer() {
			if existing.Spec.Type != v1.ServiceTypeLoadBalancer || (loadBalancerIP != "" && existing.Spec.LoadBalancerIP != loadBalancerIP) {
				deleteExternalAccessService = true // Remove the current and replace with proper one
				createExternalAccessService = true
			}
		} else if spec.ExternalAccess.GetType().IsNodePort() {
			if existing.Spec.Type != v1.ServiceTypeNodePort || len(existing.Spec.Ports) != 1 || (nodePort != 0 && existing.Spec.Ports[0].NodePort != int32(nodePort)) {
				deleteExternalAccessService = true // Remove the current and replace with proper one
				createExternalAccessService = true
			}
		}
	} else if k8sutil.IsNotFound(err) {
		// External access service does not exist
		if !spec.ExternalAccess.GetType().IsNone() {
			createExternalAccessService = true
		}
	}
	if deleteExternalAccessService {
		log.Info().Str("service", eaServiceName).Msg("Removing obsolete database external access service")
		if err := svcCli.Delete(eaServiceName, &metav1.DeleteOptions{}); err != nil {
			log.Debug().Err(err).Msg("Failed to remove database external access service")
			return maskAny(err)
		}
	}
	if createExternalAccessService {
		// Let's create or update the database external access service
		nodePort := spec.ExternalAccess.GetNodePort()
		loadBalancerIP := spec.ExternalAccess.GetLoadBalancerIP()
		_, newlyCreated, err := k8sutil.CreateDatabaseExternalAccessService(kubecli, apiObject, single, eaServiceType, nodePort, loadBalancerIP, apiObject.AsOwner())
		if err != nil {
			log.Debug().Err(err).Msg("Failed to create database external access service")
			return maskAny(err)
		}
		if newlyCreated {
			log.Debug().Str("service", eaServiceName).Msg("Created database external access service")
		}
	}

	if spec.Sync.IsEnabled() {
		// Internal sync master service
		svcName, newlyCreated, err := k8sutil.CreateSyncMasterClientService(kubecli, apiObject, owner)
		if err != nil {
			log.Debug().Err(err).Msg("Failed to create syncmaster client service")
			return maskAny(err)
		}
		if newlyCreated {
			log.Debug().Str("service", svcName).Msg("Created syncmasters service")
		}
		status := r.context.GetStatus()
		if status.SyncServiceName != svcName {
			status.SyncServiceName = svcName
			if err := r.context.UpdateStatus(status); err != nil {
				return maskAny(err)
			}
		}
	}
	return nil
}
