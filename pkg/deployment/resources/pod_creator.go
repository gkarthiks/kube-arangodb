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
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/arangodb/go-driver/jwt"
	api "github.com/arangodb/kube-arangodb/pkg/apis/deployment/v1alpha"
	"github.com/arangodb/kube-arangodb/pkg/util/constants"
	"github.com/arangodb/kube-arangodb/pkg/util/k8sutil"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type optionPair struct {
	Key   string
	Value string
}

// CompareTo returns -1 if o < other, 0 if o == other, 1 otherwise
func (o optionPair) CompareTo(other optionPair) int {
	rc := strings.Compare(o.Key, other.Key)
	if rc < 0 {
		return -1
	} else if rc > 0 {
		return 1
	}
	return strings.Compare(o.Value, other.Value)
}

// createArangodArgs creates command line arguments for an arangod server in the given group.
func createArangodArgs(apiObject metav1.Object, deplSpec api.DeploymentSpec, group api.ServerGroup,
	agents api.MemberStatusList, id string, autoUpgrade bool) []string {
	options := make([]optionPair, 0, 64)
	svrSpec := deplSpec.GetServerGroupSpec(group)

	// Endpoint
	listenAddr := "[::]"
	/*	if apiObject.Spec.Di.DisableIPv6 {
		listenAddr = "0.0.0.0"
	}*/
	//scheme := NewURLSchemes(bsCfg.SslKeyFile != "").Arangod
	scheme := "tcp"
	if deplSpec.IsSecure() {
		scheme = "ssl"
	}
	options = append(options,
		optionPair{"--server.endpoint", fmt.Sprintf("%s://%s:%d", scheme, listenAddr, k8sutil.ArangoPort)},
	)

	// Authentication
	if deplSpec.IsAuthenticated() {
		// With authentication
		options = append(options,
			optionPair{"--server.authentication", "true"},
			optionPair{"--server.jwt-secret", "$(" + constants.EnvArangodJWTSecret + ")"},
		)
	} else {
		// Without authentication
		options = append(options,
			optionPair{"--server.authentication", "false"},
		)
	}

	// Storage engine
	options = append(options,
		optionPair{"--server.storage-engine", deplSpec.GetStorageEngine().AsArangoArgument()},
	)

	// Logging
	options = append(options,
		optionPair{"--log.level", "INFO"},
	)

	// TLS
	if deplSpec.IsSecure() {
		keyPath := filepath.Join(k8sutil.TLSKeyfileVolumeMountDir, constants.SecretTLSKeyfile)
		options = append(options,
			optionPair{"--ssl.keyfile", keyPath},
			optionPair{"--ssl.ecdh-curve", ""}, // This way arangod accepts curves other than P256 as well.
		)
		/*if bsCfg.SslKeyFile != "" {
			if bsCfg.SslCAFile != "" {
				sslSection.Settings["cafile"] = bsCfg.SslCAFile
			}
			config = append(config, sslSection)
		}*/
	}

	// RocksDB
	if deplSpec.RocksDB.IsEncrypted() {
		keyPath := filepath.Join(k8sutil.RocksDBEncryptionVolumeMountDir, constants.SecretEncryptionKey)
		options = append(options,
			optionPair{"--rocksdb.encryption-keyfile", keyPath},
		)
	}

	options = append(options,
		optionPair{"--database.directory", k8sutil.ArangodVolumeMountDir},
		optionPair{"--log.output", "+"},
	)

	// Auto upgrade?
	if autoUpgrade {
		options = append(options,
			optionPair{"--database.auto-upgrade", "true"},
		)
	}

	/*	if config.ServerThreads != 0 {
		options = append(options,
			optionPair{"--server.threads", strconv.Itoa(config.ServerThreads)})
	}*/
	/*if config.DebugCluster {
		options = append(options,
			optionPair{"--log.level", "startup=trace"})
	}*/
	myTCPURL := scheme + "://" + net.JoinHostPort(k8sutil.CreatePodDNSName(apiObject, group.AsRole(), id), strconv.Itoa(k8sutil.ArangoPort))
	addAgentEndpoints := false
	switch group {
	case api.ServerGroupAgents:
		options = append(options,
			optionPair{"--agency.disaster-recovery-id", id},
			optionPair{"--agency.activate", "true"},
			optionPair{"--agency.my-address", myTCPURL},
			optionPair{"--agency.size", strconv.Itoa(deplSpec.Agents.GetCount())},
			optionPair{"--agency.supervision", "true"},
			optionPair{"--foxx.queues", "false"},
			optionPair{"--server.statistics", "false"},
		)
		for _, p := range agents {
			if p.ID != id {
				dnsName := k8sutil.CreatePodDNSName(apiObject, api.ServerGroupAgents.AsRole(), p.ID)
				options = append(options,
					optionPair{"--agency.endpoint", fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(dnsName, strconv.Itoa(k8sutil.ArangoPort)))},
				)
			}
		}
	case api.ServerGroupDBServers:
		addAgentEndpoints = true
		options = append(options,
			optionPair{"--cluster.my-address", myTCPURL},
			optionPair{"--cluster.my-role", "PRIMARY"},
			optionPair{"--foxx.queues", "false"},
			optionPair{"--server.statistics", "true"},
		)
	case api.ServerGroupCoordinators:
		addAgentEndpoints = true
		options = append(options,
			optionPair{"--cluster.my-address", myTCPURL},
			optionPair{"--cluster.my-role", "COORDINATOR"},
			optionPair{"--foxx.queues", "true"},
			optionPair{"--server.statistics", "true"},
		)
	case api.ServerGroupSingle:
		options = append(options,
			optionPair{"--foxx.queues", "true"},
			optionPair{"--server.statistics", "true"},
		)
		if deplSpec.GetMode() == api.DeploymentModeActiveFailover {
			addAgentEndpoints = true
			options = append(options,
				optionPair{"--replication.automatic-failover", "true"},
				optionPair{"--cluster.my-address", myTCPURL},
				optionPair{"--cluster.my-role", "SINGLE"},
			)
		}
	}
	if addAgentEndpoints {
		for _, p := range agents {
			dnsName := k8sutil.CreatePodDNSName(apiObject, api.ServerGroupAgents.AsRole(), p.ID)
			options = append(options,
				optionPair{"--cluster.agency-endpoint",
					fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(dnsName, strconv.Itoa(k8sutil.ArangoPort)))},
			)
		}
	}

	args := make([]string, 0, len(options)+len(svrSpec.Args))
	sort.Slice(options, func(i, j int) bool {
		return options[i].CompareTo(options[j]) < 0
	})
	for _, o := range options {
		args = append(args, o.Key+"="+o.Value)
	}
	args = append(args, svrSpec.Args...)

	return args
}

// createArangoSyncArgs creates command line arguments for an arangosync server in the given group.
func createArangoSyncArgs(spec api.DeploymentSpec, group api.ServerGroup, groupSpec api.ServerGroupSpec, agents api.MemberStatusList, id string) []string {
	// TODO
	return nil
}

// createLivenessProbe creates configuration for a liveness probe of a server in the given group.
func (r *Resources) createLivenessProbe(spec api.DeploymentSpec, group api.ServerGroup) (*k8sutil.HTTPProbeConfig, error) {
	switch group {
	case api.ServerGroupSingle, api.ServerGroupAgents, api.ServerGroupDBServers:
		authorization := ""
		if spec.IsAuthenticated() {
			secretData, err := r.getJWTSecret(spec)
			if err != nil {
				return nil, maskAny(err)
			}
			authorization, err = jwt.CreateArangodJwtAuthorizationHeader(secretData, "kube-arangodb")
			if err != nil {
				return nil, maskAny(err)
			}
		}
		return &k8sutil.HTTPProbeConfig{
			LocalPath:     "/_api/version",
			Secure:        spec.IsSecure(),
			Authorization: authorization,
		}, nil
	case api.ServerGroupCoordinators:
		return nil, nil
	case api.ServerGroupSyncMasters, api.ServerGroupSyncWorkers:
		authorization := ""
		if spec.Sync.Monitoring.GetTokenSecretName() != "" {
			// Use monitoring token
			token, err := r.getSyncMonitoringToken(spec)
			if err != nil {
				return nil, maskAny(err)
			}
			authorization = "bearer: " + token
			if err != nil {
				return nil, maskAny(err)
			}
		} else if group == api.ServerGroupSyncMasters {
			// Fall back to JWT secret
			secretData, err := r.getSyncJWTSecret(spec)
			if err != nil {
				return nil, maskAny(err)
			}
			authorization, err = jwt.CreateArangodJwtAuthorizationHeader(secretData, "kube-arangodb")
			if err != nil {
				return nil, maskAny(err)
			}
		} else {
			// Don't have a probe
			return nil, nil
		}
		return &k8sutil.HTTPProbeConfig{
			LocalPath:     "/_api/version",
			Secure:        spec.IsSecure(),
			Authorization: authorization,
		}, nil
	default:
		return nil, nil
	}
}

// createReadinessProbe creates configuration for a readiness probe of a server in the given group.
func (r *Resources) createReadinessProbe(spec api.DeploymentSpec, group api.ServerGroup) (*k8sutil.HTTPProbeConfig, error) {
	if group != api.ServerGroupCoordinators {
		return nil, nil
	}
	authorization := ""
	if spec.IsAuthenticated() {
		secretData, err := r.getJWTSecret(spec)
		if err != nil {
			return nil, maskAny(err)
		}
		authorization, err = jwt.CreateArangodJwtAuthorizationHeader(secretData, "kube-arangodb")
		if err != nil {
			return nil, maskAny(err)
		}
	}
	return &k8sutil.HTTPProbeConfig{
		LocalPath:     "/_api/version",
		Secure:        spec.IsSecure(),
		Authorization: authorization,
	}, nil
}

// createPodForMember creates all Pods listed in member status
func (r *Resources) createPodForMember(spec api.DeploymentSpec, group api.ServerGroup,
	groupSpec api.ServerGroupSpec, m api.MemberStatus, memberStatusList *api.MemberStatusList) error {
	kubecli := r.context.GetKubeCli()
	log := r.log
	apiObject := r.context.GetAPIObject()
	ns := r.context.GetNamespace()
	status := r.context.GetStatus()

	// Update pod name
	role := group.AsRole()
	roleAbbr := group.AsRoleAbbreviated()
	podSuffix := createPodSuffix(spec)
	m.PodName = k8sutil.CreatePodName(apiObject.GetName(), roleAbbr, m.ID, podSuffix)
	newPhase := api.MemberPhaseCreated
	// Create pod
	if group.IsArangod() {
		// Find image ID
		info, found := status.Images.GetByImage(spec.GetImage())
		if !found {
			log.Debug().Str("image", spec.GetImage()).Msg("Image ID is not known yet for image")
			return nil
		}
		// Prepare arguments
		autoUpgrade := m.Conditions.IsTrue(api.ConditionTypeAutoUpgrade)
		if autoUpgrade {
			newPhase = api.MemberPhaseUpgrading
		}
		args := createArangodArgs(apiObject, spec, group, status.Members.Agents, m.ID, autoUpgrade)
		env := make(map[string]k8sutil.EnvValue)
		livenessProbe, err := r.createLivenessProbe(spec, group)
		if err != nil {
			return maskAny(err)
		}
		readinessProbe, err := r.createReadinessProbe(spec, group)
		if err != nil {
			return maskAny(err)
		}
		tlsKeyfileSecretName := ""
		if spec.IsSecure() {
			tlsKeyfileSecretName = k8sutil.CreateTLSKeyfileSecretName(apiObject.GetName(), role, m.ID)
			serverNames := []string{
				k8sutil.CreateDatabaseClientServiceDNSName(apiObject),
				k8sutil.CreatePodDNSName(apiObject, role, m.ID),
			}
			owner := apiObject.AsOwner()
			if err := createServerCertificate(log, kubecli.CoreV1(), serverNames, spec.TLS, tlsKeyfileSecretName, ns, &owner); err != nil && !k8sutil.IsAlreadyExists(err) {
				return maskAny(errors.Wrapf(err, "Failed to create TLS keyfile secret"))
			}
		}
		rocksdbEncryptionSecretName := ""
		if spec.RocksDB.IsEncrypted() {
			rocksdbEncryptionSecretName = spec.RocksDB.Encryption.GetKeySecretName()
			if err := k8sutil.ValidateEncryptionKeySecret(kubecli.CoreV1(), rocksdbEncryptionSecretName, ns); err != nil {
				return maskAny(errors.Wrapf(err, "RocksDB encryption key secret validation failed"))
			}
		}
		if spec.IsAuthenticated() {
			env[constants.EnvArangodJWTSecret] = k8sutil.EnvValue{
				SecretName: spec.Authentication.GetJWTSecretName(),
				SecretKey:  constants.SecretKeyJWT,
			}
		}
		engine := spec.GetStorageEngine().AsArangoArgument()
		requireUUID := group == api.ServerGroupDBServers && m.IsInitialized
		if err := k8sutil.CreateArangodPod(kubecli, spec.IsDevelopment(), apiObject, role, m.ID, m.PodName, m.PersistentVolumeClaimName, info.ImageID, spec.GetImagePullPolicy(),
			engine, requireUUID, args, env, livenessProbe, readinessProbe, tlsKeyfileSecretName, rocksdbEncryptionSecretName); err != nil {
			return maskAny(err)
		}
		log.Debug().Str("pod-name", m.PodName).Msg("Created pod")
	} else if group.IsArangosync() {
		// Find image ID
		info, found := status.Images.GetByImage(spec.Sync.GetImage())
		if !found {
			log.Debug().Str("image", spec.Sync.GetImage()).Msg("Image ID is not known yet for image")
			return nil
		}
		// Prepare arguments
		args := createArangoSyncArgs(spec, group, groupSpec, status.Members.Agents, m.ID)
		env := make(map[string]k8sutil.EnvValue)
		livenessProbe, err := r.createLivenessProbe(spec, group)
		if err != nil {
			return maskAny(err)
		}
		affinityWithRole := ""
		if group == api.ServerGroupSyncWorkers {
			affinityWithRole = api.ServerGroupDBServers.AsRole()
		}
		if err := k8sutil.CreateArangoSyncPod(kubecli, spec.IsDevelopment(), apiObject, role, m.ID, m.PodName, info.ImageID, spec.Sync.GetImagePullPolicy(), args, env, livenessProbe, affinityWithRole); err != nil {
			return maskAny(err)
		}
		log.Debug().Str("pod-name", m.PodName).Msg("Created pod")
	}
	// Record new member phase
	m.Phase = newPhase
	m.Conditions.Remove(api.ConditionTypeReady)
	m.Conditions.Remove(api.ConditionTypeTerminated)
	m.Conditions.Remove(api.ConditionTypeAutoUpgrade)
	if err := memberStatusList.Update(m); err != nil {
		return maskAny(err)
	}
	if err := r.context.UpdateStatus(status); err != nil {
		return maskAny(err)
	}
	// Create event
	r.context.CreateEvent(k8sutil.NewPodCreatedEvent(m.PodName, role, apiObject))

	return nil
}

// EnsurePods creates all Pods listed in member status
func (r *Resources) EnsurePods() error {
	iterator := r.context.GetServerGroupIterator()
	status := r.context.GetStatus()
	if err := iterator.ForeachServerGroup(func(group api.ServerGroup, groupSpec api.ServerGroupSpec, status *api.MemberStatusList) error {
		for _, m := range *status {
			if m.Phase != api.MemberPhaseNone {
				continue
			}
			spec := r.context.GetSpec()
			if err := r.createPodForMember(spec, group, groupSpec, m, status); err != nil {
				return maskAny(err)
			}
		}
		return nil
	}, &status); err != nil {
		return maskAny(err)
	}
	return nil
}

func createPodSuffix(spec api.DeploymentSpec) string {
	raw, _ := json.Marshal(spec)
	hash := sha1.Sum(raw)
	return fmt.Sprintf("%0x", hash)[:6]
}
