/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	updatev1beta1 "github.com/microsoft/cluster-api-inplace-updater/api/v1beta1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
	runtimev1 "sigs.k8s.io/cluster-api/exp/runtime/api/v1alpha1"
	"sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/clusterctl"
	"sigs.k8s.io/cluster-api/util"
)

// clusterInPlaceUpgradeSpecInput is the input for clusterInPlaceUpgradeSpec.
type clusterInPlaceUpgradeSpecInput struct {
	E2EConfig             *clusterctl.E2EConfig
	ClusterctlConfigPath  string
	BootstrapClusterProxy framework.ClusterProxy
	ArtifactFolder        string
	SkipCleanup           bool

	// InfrastructureProviders specifies the infrastructure to use for clusterctl
	// operations (Example: get cluster templates).
	// Note: In most cases this need not be specified. It only needs to be specified when
	// multiple infrastructure providers (ex: CAPD + in-memory) are installed on the cluster as clusterctl will not be
	// able to identify the default.
	InfrastructureProvider *string

	// ControlPlaneMachineCount is used in `config cluster` to configure the count of the control plane machines used in the test.
	// Default is 1.
	ControlPlaneMachineCount *int64

	// WorkerMachineCount is used in `config cluster` to configure the count of the worker machines used in the test.
	// NOTE: If the WORKER_MACHINE_COUNT var is used multiple times in the cluster template, the absolute count of
	// worker machines is a multiple of WorkerMachineCount.
	// Default is 2.
	WorkerMachineCount *int64

	// Flavor to use when creating the cluster for testing, "upgrades" is used if not specified.
	Flavor *string
}

// clusterInPlaceUpgradeSpec implements a spec that upgrades a cluster and runs the Kubernetes conformance suite.
// Upgrading a cluster refers to upgrading the control-plane and worker nodes (managed by MD and machine pools).
// NOTE: This test only works with a KubeadmControlPlane.
// NOTE: This test works with Clusters with and without ClusterClass.
// When using ClusterClass the ClusterClass must have the variables "etcdImageTag" and "coreDNSImageTag" of type string.
// Those variables should have corresponding patches which set the etcd and CoreDNS tags in KCP.
func clusterInPlaceUpgradeSpec(ctx context.Context, inputGetter func() clusterInPlaceUpgradeSpecInput) {
	const (
		specName = "k8s-inplace-upgrade"
	)

	var (
		input         clusterInPlaceUpgradeSpecInput
		namespace     *corev1.Namespace
		cancelWatches context.CancelFunc

		controlPlaneMachineCount int64
		workerMachineCount       int64

		clusterResources *clusterctl.ApplyClusterTemplateAndWaitResult
		clusterName      string
	)

	BeforeEach(func() {
		Expect(ctx).NotTo(BeNil(), "ctx is required for %s spec", specName)
		input = inputGetter()
		Expect(input.E2EConfig).ToNot(BeNil(), "Invalid argument. input.E2EConfig can't be nil when calling %s spec", specName)
		Expect(input.ClusterctlConfigPath).To(BeAnExistingFile(), "Invalid argument. input.ClusterctlConfigPath must be an existing file when calling %s spec", specName)
		Expect(input.BootstrapClusterProxy).ToNot(BeNil(), "Invalid argument. input.BootstrapClusterProxy can't be nil when calling %s spec", specName)
		Expect(os.MkdirAll(input.ArtifactFolder, 0750)).To(Succeed(), "Invalid argument. input.ArtifactFolder can't be created for %s spec", specName)

		Expect(input.E2EConfig.Variables).To(HaveKey(KubernetesVersionUpgradeFrom))
		Expect(input.E2EConfig.Variables).To(HaveKey(KubernetesVersionUpgradeTo))

		if input.ControlPlaneMachineCount == nil {
			controlPlaneMachineCount = 1
		} else {
			controlPlaneMachineCount = *input.ControlPlaneMachineCount
		}

		if input.WorkerMachineCount == nil {
			workerMachineCount = 2
		} else {
			workerMachineCount = *input.WorkerMachineCount
		}

		// Set up a Namespace where to host objects for this spec and create a watcher for the Namespace events.
		namespace, cancelWatches = setupSpecNamespace(ctx, specName, input.BootstrapClusterProxy, input.ArtifactFolder, nil)
		clusterName = fmt.Sprintf("%s-%s", specName, util.RandomString(6))
		clusterResources = new(clusterctl.ApplyClusterTemplateAndWaitResult)
	})

	It("Should create, upgrade and delete a workload cluster", func() {
		// NOTE: cluster-inplace-updater extension is already deployed in the management cluster. If for any reason in future we want
		// to make this test more self-contained this test should be modified in order to create an additional
		// management cluster; also the E2E test configuration should be modified introducing something like
		// optional:true allowing to define which providers should not be installed by default in
		// a management cluster.

		By("Deploy Test Extension ExtensionConfig")

		Expect(input.BootstrapClusterProxy.GetClient().Create(ctx,
			extensionConfig(specName, namespace.Name))).
			To(Succeed(), "Failed to create the extension config")

		By("Creating a workload cluster; creation waits for BeforeClusterCreateHook to gate the operation")

		// clusterRef := types.NamespacedName{
		// 	Name:      clusterName,
		// 	Namespace: namespace.Name,
		// }

		infrastructureProvider := clusterctl.DefaultInfrastructureProvider
		if input.InfrastructureProvider != nil {
			infrastructureProvider = *input.InfrastructureProvider
		}

		clusterctl.ApplyClusterTemplateAndWait(ctx, clusterctl.ApplyClusterTemplateAndWaitInput{
			ClusterProxy: input.BootstrapClusterProxy,
			ConfigCluster: clusterctl.ConfigClusterInput{
				LogFolder:                filepath.Join(input.ArtifactFolder, "clusters", input.BootstrapClusterProxy.GetName()),
				ClusterctlConfigPath:     input.ClusterctlConfigPath,
				KubeconfigPath:           input.BootstrapClusterProxy.GetKubeconfigPath(),
				InfrastructureProvider:   infrastructureProvider,
				Flavor:                   pointer.StringDeref(input.Flavor, "upgrades"),
				Namespace:                namespace.Name,
				ClusterName:              clusterName,
				KubernetesVersion:        input.E2EConfig.GetVariable(KubernetesVersionUpgradeFrom),
				ControlPlaneMachineCount: pointer.Int64(controlPlaneMachineCount),
				WorkerMachineCount:       pointer.Int64(workerMachineCount),
			},
			WaitForClusterIntervals:      input.E2EConfig.GetIntervals(specName, "wait-cluster"),
			WaitForControlPlaneIntervals: input.E2EConfig.GetIntervals(specName, "wait-control-plane"),
			WaitForMachineDeployments:    input.E2EConfig.GetIntervals(specName, "wait-worker-nodes"),
			WaitForMachinePools:          input.E2EConfig.GetIntervals(specName, "wait-machine-pool-nodes"),
		}, clusterResources)

		// // Upgrade the Cluster topology to run through an inplace upgrade process
		// By("Upgrading the Cluster topology; creation waits for BeforeClusterUpgradeHook and AfterControlPlaneUpgradeHook to gate the operation")
		// framework.UpgradeClusterTopologyAndWaitForUpgrade(ctx, framework.UpgradeClusterTopologyAndWaitForUpgradeInput{
		// 	ClusterProxy:                   input.BootstrapClusterProxy,
		// 	Cluster:                        clusterResources.Cluster,
		// 	ControlPlane:                   clusterResources.ControlPlane,
		// 	MachineDeployments:             clusterResources.MachineDeployments,
		// 	MachinePools:                   clusterResources.MachinePools,
		// 	KubernetesUpgradeVersion:       input.E2EConfig.GetVariable(KubernetesVersionUpgradeTo),
		// 	WaitForMachinesToBeUpgraded:    input.E2EConfig.GetIntervals(specName, "wait-machine-upgrade"),
		// 	WaitForMachinePoolToBeUpgraded: input.E2EConfig.GetIntervals(specName, "wait-machine-pool-upgrade"),
		// 	WaitForKubeProxyUpgrade:        input.E2EConfig.GetIntervals(specName, "wait-machine-upgrade"),
		// 	WaitForDNSUpgrade:              input.E2EConfig.GetIntervals(specName, "wait-machine-upgrade"),
		// 	WaitForEtcdUpgrade:             input.E2EConfig.GetIntervals(specName, "wait-machine-upgrade"),
		// 	PreWaitForControlPlaneToBeUpgraded: func() {
		// 		// TODO:
		// 		// 1. Eventually UpgradeTask is created, and with correct spec
		// 		// 2. Watch UpgradeTask status, and if progress get reflected to CAPI objs
		// 	},
		// })

		task := &updatev1beta1.UpdateTask{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace.Name,
				Name:      clusterName,
			},
			Spec: updatev1beta1.UpdateTaskSpec{
				//
			},
		}

		By("Waiting until nodes are ready")
		workloadProxy := input.BootstrapClusterProxy.GetWorkloadCluster(ctx, namespace.Name, clusterResources.Cluster.Name)
		workloadClient := workloadProxy.GetClient()
		framework.WaitForNodesReady(ctx, framework.WaitForNodesReadyInput{
			Lister:            workloadClient,
			KubernetesVersion: input.E2EConfig.GetVariable(KubernetesVersionUpgradeTo),
			Count:             int(clusterResources.ExpectedTotalNodes()),
			WaitForNodesReady: input.E2EConfig.GetIntervals(specName, "wait-nodes-ready"),
		})

		By("PASSED!")
	})

	AfterEach(func() {
		// Delete the extensionConfig first to ensure the BeforeDeleteCluster hook doesn't block deletion.
		Eventually(func() error {
			return input.BootstrapClusterProxy.GetClient().Delete(ctx, extensionConfig(specName, namespace.Name))
		}, 10*time.Second, 1*time.Second).Should(Succeed(), "delete extensionConfig failed")

		// Dumps all the resources in the spec Namespace, then cleanups the cluster object and the spec Namespace itself.
		dumpSpecResourcesAndCleanup(ctx, specName, input.BootstrapClusterProxy, input.ArtifactFolder, namespace, cancelWatches, clusterResources.Cluster, input.E2EConfig.GetIntervals, input.SkipCleanup)
	})
}

// extensionConfig generates an ExtensionConfig.
// We make sure this cluster-wide object does not conflict with others by using a random generated
// name and a NamespaceSelector selecting on the namespace of the current test.
// Thus, this object is "namespaced" to the current test even though it's a cluster-wide object.
func extensionConfig(name, namespace string) *runtimev1.ExtensionConfig {
	return &runtimev1.ExtensionConfig{
		ObjectMeta: metav1.ObjectMeta{
			// Note: We have to use a constant name here as we have to be able to reference it in the ClusterClass
			// when configuring external patches.
			Name: name,
			Annotations: map[string]string{
				// Note: this assumes the test extension get deployed in the default namespace defined in its own runtime-extensions-components.yaml
				runtimev1.InjectCAFromSecretAnnotation: "capi-inplace-updater-system/capi-inplace-updater-webhook-service-cert",
			},
		},
		Spec: runtimev1.ExtensionConfigSpec{
			ClientConfig: runtimev1.ClientConfig{
				Service: &runtimev1.ServiceReference{
					Name: "capi-inplace-updater-webhook-service",
					// Note: this assumes the test extension get deployed in the default namespace defined in its own runtime-extensions-components.yaml
					Namespace: "capi-inplace-updater-system",
				},
			},
			NamespaceSelector: &metav1.LabelSelector{
				// Note: we are limiting the test extension to be used by the namespace where the test is run.
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "kubernetes.io/metadata.name",
						Operator: metav1.LabelSelectorOpIn,
						Values:   []string{namespace},
					},
				},
			},
			Settings: map[string]string{
				// Add settings if needed
			},
		},
	}
}
