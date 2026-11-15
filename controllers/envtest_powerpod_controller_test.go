//go:build envtest

/*
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
package controllers

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	powerv1alpha1 "github.com/cluster-power-manager/cluster-power-manager/api/v1alpha1"
	"github.com/cluster-power-manager/cluster-power-manager/pkg/podresourcesclient"
	"github.com/cluster-power-manager/cluster-power-manager/pkg/podstate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	podresourcesapi "k8s.io/kubelet/pkg/apis/podresources/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// createPodReconcilerWithEnvTest creates a PowerPodReconciler using the
// cache-backed client from setupEnvTestWithPodCache, so the reconciler reads
// transformed Pods just like it does in production. PowerLibrary and
// PodResourcesClient are mocked since they depend on node-local resources.
func createPodReconcilerWithEnvTest(
	t *testing.T,
	cl client.Client,
	podResources []*podresourcesapi.PodResources,
	sharedPoolCPUs []uint,
) *PowerPodReconciler {
	t.Helper()

	s := scheme.Scheme

	state, err := podstate.NewState()
	require.NoError(t, err)

	// Mock the PowerLibrary to return the expected pools.
	mockHost := new(hostMock)
	mockHost.On("GetExclusivePool", profilePerformance).Return(createMockPoolWithCPUs([]uint{}))
	mockHost.On("GetExclusivePool", profileBalancePerformance).Return(createMockPoolWithCPUs([]uint{}))
	mockHost.On("GetSharedPool").Return(createMockPoolWithCPUs(sharedPoolCPUs))

	// Mock the PodResourcesClient to return the expected pod resources.
	fakeListResponse := &podresourcesapi.ListPodResourcesResponse{
		PodResources: podResources,
	}
	fakePodResClient := &fakePodResourcesClient{listResponse: fakeListResponse}

	// Create the PodResourcesClient.
	podResourcesClient := &podresourcesclient.PodResourcesClient{
		Client:                fakePodResClient,
		CPUControlPlaneClient: fakePodResClient,
	}

	return &PowerPodReconciler{
		Client:             cl,
		Log:                ctrl.Log.WithName("testing"),
		Scheme:             s,
		State:              state,
		PodResourcesClient: *podResourcesClient,
		PowerLibrary:       mockHost,
		NodeName:           os.Getenv(NodeNameEnv),
	}
}

// createPodReconcilePrereqs creates the common prerequisite resources for pod reconciliation
// envtests: Node, PowerNodeState, and the specified PowerProfiles.
func createPodReconcilePrereqs(t *testing.T, cl client.Client, ctx context.Context, nodeName string, profileNames ...string) {
	t.Helper()

	require.NoError(t, cl.Create(ctx, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: nodeName},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU: *resource.NewQuantity(16, resource.DecimalSI),
			},
		},
	}))
	createTestPowerNodeState(t, cl, fmt.Sprintf("%s-power-state", nodeName))
	for _, name := range profileNames {
		require.NoError(t, cl.Create(ctx, &powerv1alpha1.PowerProfile{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: PowerNamespace},
			Spec:       powerv1alpha1.PowerProfileSpec{},
		}))
	}
}

// createPerformancePowerPod creates a running guaranteed Pod that requests the performance
// PowerProfile in its single container.
func createPerformancePowerPod(t *testing.T, cl client.Client, ctx context.Context, nodeName string) *corev1.Pod {
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: PowerNamespace},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name:  "perf-container",
				Image: "perf-pod",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
						corev1.ResourceName(ResourcePrefix + profilePerformance): *resource.NewQuantity(2, resource.DecimalSI),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
						corev1.ResourceName(ResourcePrefix + profilePerformance): *resource.NewQuantity(2, resource.DecimalSI),
					},
				},
			}},
		},
	}
	require.NoError(t, cl.Create(ctx, pod))
	// Update pod status (envtest doesn't set status on create).
	pod.Status = corev1.PodStatus{
		Phase:    corev1.PodRunning,
		QOSClass: corev1.PodQOSGuaranteed,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "perf-container", ContainerID: "cri-o://container-abc",
		}},
	}
	require.NoError(t, cl.Status().Update(ctx, pod))
	return pod
}

// singleExclusiveContainer returns the only container recorded in the
// PowerNodeState exclusive entry.
func singleExclusiveContainer(t *testing.T, cl client.Client, ctx context.Context, pnsKey client.ObjectKey) powerv1alpha1.PowerContainer {
	t.Helper()

	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, pnsKey, pns))
	require.NotNil(t, pns.Status.CPUPools)
	require.Len(t, pns.Status.CPUPools.Exclusive, 1)
	require.Len(t, pns.Status.CPUPools.Exclusive[0].PowerContainers, 1)
	return pns.Status.CPUPools.Exclusive[0].PowerContainers[0]
}

// deleteProfileAndWaitForCache deletes a PowerProfile with the direct client and
// waits until the reconciler's cached client observes the deletion.
func deleteProfileAndWaitForCache(t *testing.T, cl client.Client, r *PowerPodReconciler, name string) {
	t.Helper()

	ctx := context.TODO()
	require.NoError(t, cl.Delete(ctx, &powerv1alpha1.PowerProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: PowerNamespace},
	}))
	require.Eventually(t, func() bool {
		return apierrors.IsNotFound(r.Client.Get(ctx, client.ObjectKey{Name: name, Namespace: PowerNamespace}, &powerv1alpha1.PowerProfile{}))
	}, 10*time.Second, 50*time.Millisecond)
}

// failingProfileGetClient returns err for every PowerProfile Get and delegates
// everything else to the wrapped client.
type failingProfileGetClient struct {
	client.Client
	err error
}

func (c *failingProfileGetClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, isProfile := obj.(*powerv1alpha1.PowerProfile); isProfile {
		return c.err
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func TestSSA_PodReconcile_SinglePod(t *testing.T) {
	cl, cachedCl, cleanup := setupEnvTestWithPodCache(t)
	defer cleanup()

	nodeName := "test-node"
	t.Setenv("NODE_NAME", nodeName)
	ctx := context.TODO()

	createPodReconcilePrereqs(t, cl, ctx, nodeName, profilePerformance)

	// Create a guaranteed pod requesting the performance PowerProfile.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod", Namespace: PowerNamespace,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name:  "perf-container",
				Image: "perf-pod",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
						corev1.ResourceName(ResourcePrefix + profilePerformance): *resource.NewQuantity(2, resource.DecimalSI),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
						corev1.ResourceName(ResourcePrefix + profilePerformance): *resource.NewQuantity(2, resource.DecimalSI),
					},
				},
			}},
		},
		Status: corev1.PodStatus{
			Phase:    corev1.PodRunning,
			QOSClass: corev1.PodQOSGuaranteed,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "perf-container", ContainerID: "cri-o://container-abc",
			}},
		},
	}
	require.NoError(t, cl.Create(ctx, pod))
	// Update pod status (envtest doesn't set status on create).
	pod.Status = corev1.PodStatus{
		Phase:    corev1.PodRunning,
		QOSClass: corev1.PodQOSGuaranteed,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "perf-container", ContainerID: "cri-o://container-abc",
		}},
	}
	require.NoError(t, cl.Status().Update(ctx, pod))

	r := createPodReconcilerWithEnvTest(t, cachedCl,
		[]*podresourcesapi.PodResources{{
			Name: "test-pod", Namespace: PowerNamespace,
			Containers: []*podresourcesapi.ContainerResources{{
				Name: "perf-container", CpuIds: []int64{4, 5},
			}},
		}},
		[]uint{0, 1, 2, 3, 4, 5, 6, 7},
	)

	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "test-pod", Namespace: PowerNamespace},
	})
	assert.NoError(t, err)

	// Verify PowerNodeState has the pod's exclusive entry via real SSA.
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{
		Name: fmt.Sprintf("%s-power-state", nodeName), Namespace: PowerNamespace,
	}, pns))

	require.NotNil(t, pns.Status.CPUPools)
	require.Len(t, pns.Status.CPUPools.Exclusive, 1)
	assert.Equal(t, string(pod.UID), pns.Status.CPUPools.Exclusive[0].PodUID)
	assert.Equal(t, "test-pod", pns.Status.CPUPools.Exclusive[0].Pod)
	require.Len(t, pns.Status.CPUPools.Exclusive[0].PowerContainers, 1)
	pc := pns.Status.CPUPools.Exclusive[0].PowerContainers[0]
	assert.Equal(t, "perf-container", pc.Name)
	assert.Equal(t, profilePerformance, pc.PowerProfile)
	assert.ElementsMatch(t, []uint{4, 5}, pc.CPUIDs)

	// Verify SSA field manager ownership for the exclusive entry.
	expectedManager := fmt.Sprintf("powerpod-controller.%s", pod.UID)
	var found bool
	for _, mf := range pns.ManagedFields {
		if mf.Manager == expectedManager {
			found = true
			break
		}
	}
	assert.True(t, found, "expected field manager %q in ManagedFields", expectedManager)
}

// TestSSA_PodReconcile_ProfileDeletedWhilePodRunning reproduces issue #116: a
// PowerProfile disappears while a Pod is running, and the CPUs assigned to the
// Pod must still return to the shared pool when the Pod ends.
func TestSSA_PodReconcile_ProfileDeletedWhilePodRunning(t *testing.T) {
	// Test setup.
	cl, cachedCl, cleanup := setupEnvTestWithPodCache(t)
	defer cleanup()

	nodeName := "test-node"
	t.Setenv("NODE_NAME", nodeName)
	ctx := context.TODO()

	createPodReconcilePrereqs(t, cl, ctx, nodeName, profilePerformance)

	// Create Power Pod.
	pod := createPerformancePowerPod(t, cl, ctx, nodeName)

	r := createPodReconcilerWithEnvTest(t, cachedCl,
		[]*podresourcesapi.PodResources{{
			Name: "test-pod", Namespace: PowerNamespace,
			Containers: []*podresourcesapi.ContainerResources{{
				Name: "perf-container", CpuIds: []int64{4, 5},
			}},
		}},
		[]uint{0, 1, 2, 3, 4, 5, 6, 7},
	)

	// Create exclusive and shared pools.
	exclusivePool := r.PowerLibrary.(*hostMock).GetExclusivePool(profilePerformance).(*poolMock)
	sharedPool := r.PowerLibrary.(*hostMock).GetSharedPool().(*poolMock)

	req := reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "test-pod", Namespace: PowerNamespace},
	}
	pnsKey := client.ObjectKey{Name: fmt.Sprintf("%s-power-state", nodeName), Namespace: PowerNamespace}

	// Reconcile with the profile present: the CPUs move to the exclusive pool.
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, pnsKey, pns))
	require.NotNil(t, pns.Status.CPUPools)
	require.Len(t, pns.Status.CPUPools.Exclusive, 1)
	require.Len(t, pns.Status.CPUPools.Exclusive[0].PowerContainers, 1)
	pc := pns.Status.CPUPools.Exclusive[0].PowerContainers[0]
	assert.Equal(t, profilePerformance, pc.PowerProfile)
	assert.ElementsMatch(t, []uint{4, 5}, pc.CPUIDs)
	assert.Empty(t, pc.Errors)
	exclusivePool.AssertCalled(t, "MoveCPUIDs", []uint{4, 5})
	sharedPool.AssertNotCalled(t, "MoveCPUIDs", mock.Anything)

	// Delete the profile while the pod is still running. The reconciler reads
	// PowerProfiles through the envtest cache, so wait for it to catch up.
	deleteProfileAndWaitForCache(t, cl, r, profilePerformance)

	// Reconcile again - it should fail on the unavailable profile but keeps the recorded
	// CPU IDs so they can be released when the pod ends.
	unavailableMsg := fmt.Sprintf("power profile '%s' is not available on node %s", profilePerformance, nodeName)
	_, err = r.Reconcile(ctx, req)
	require.ErrorContains(t, err, unavailableMsg)

	pns = &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, pnsKey, pns))
	require.NotNil(t, pns.Status.CPUPools)
	require.Len(t, pns.Status.CPUPools.Exclusive, 1)
	require.Len(t, pns.Status.CPUPools.Exclusive[0].PowerContainers, 1)
	pc = pns.Status.CPUPools.Exclusive[0].PowerContainers[0]
	assert.ElementsMatch(t, []uint{4, 5}, pc.CPUIDs)
	require.NotEmpty(t, pc.Errors)
	assert.Contains(t, strings.Join(pc.Errors, "\n"), unavailableMsg)
	exclusivePool.AssertNumberOfCalls(t, "MoveCPUIDs", 1)
	sharedPool.AssertNotCalled(t, "MoveCPUIDs", mock.Anything)

	// Deleting the pod releases the recorded CPUs even though their profile is
	// gone. Call the deletion handler directly: envtest has no kubelet, so a
	// scheduled pod would never finish terminating.
	logger := ctrl.Log.WithName("testing")
	_, err = r.handlePodDeletion(ctx, pod, nodeName, &logger)
	require.NoError(t, err)

	sharedPool.AssertCalled(t, "MoveCPUIDs", []uint{4, 5})

	pns = &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, pnsKey, pns))
	if pns.Status.CPUPools != nil {
		assert.Empty(t, pns.Status.CPUPools.Exclusive)
	}
}

// TestSSA_PodReconcile_AvailabilityErrorKeepsRecordedCPUs asserts that a transient error
// while checking profile availability aborts the reconcile before any status write:
// the Pod's existing PowerNodeState entry keeps its recorded CPU IDs and no CPUs move.
func TestSSA_PodReconcile_AvailabilityErrorKeepsRecordedCPUs(t *testing.T) {
	// Test setup.
	cl, cachedCl, cleanup := setupEnvTestWithPodCache(t)
	defer cleanup()

	nodeName := "test-node"
	t.Setenv("NODE_NAME", nodeName)
	ctx := context.TODO()

	createPodReconcilePrereqs(t, cl, ctx, nodeName, profilePerformance)
	createPerformancePowerPod(t, cl, ctx, nodeName)

	r := createPodReconcilerWithEnvTest(t, cachedCl,
		[]*podresourcesapi.PodResources{{
			Name: "test-pod", Namespace: PowerNamespace,
			Containers: []*podresourcesapi.ContainerResources{{
				Name: "perf-container", CpuIds: []int64{5, 6},
			}},
		}},
		[]uint{0, 1, 2, 3, 4, 5, 6, 7},
	)
	exclusivePool := r.PowerLibrary.(*hostMock).GetExclusivePool(profilePerformance).(*poolMock)

	req := reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "test-pod", Namespace: PowerNamespace},
	}
	pnsKey := client.ObjectKey{Name: fmt.Sprintf("%s-power-state", nodeName), Namespace: PowerNamespace}

	// Reconcile once with the profile available: the entry records the CPUs it owns.
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	pc := singleExclusiveContainer(t, cl, ctx, pnsKey)
	assert.Equal(t, profilePerformance, pc.PowerProfile)
	assert.ElementsMatch(t, []uint{5, 6}, pc.CPUIDs)
	assert.Empty(t, pc.Errors)
	exclusivePool.AssertNumberOfCalls(t, "MoveCPUIDs", 1)

	// Make the availability check fail as if the API server were briefly unavailable.
	r.Client = &failingProfileGetClient{Client: r.Client, err: fmt.Errorf("transient availability check failure")}

	// Reconcile again, expect error.
	_, err = r.Reconcile(ctx, req)
	require.ErrorContains(t, err, profilePerformance)
	require.ErrorContains(t, err, "transient availability check failure")

	// The status entry is untouched: the container still has its recorded CPU IDs.
	pc = singleExclusiveContainer(t, cl, ctx, pnsKey)
	assert.ElementsMatch(t, []uint{5, 6}, pc.CPUIDs)
	assert.Empty(t, pc.Errors)

	// The reconcile aborted before moving anything into the exclusive pool.
	exclusivePool.AssertNumberOfCalls(t, "MoveCPUIDs", 1)
}

// TestSSA_PodReconcile_CPUReadErrorKeepsRecordedCPUs asserts that an error reading a
// container's CPUs aborts the reconcile before any status write: the Pod's existing
// PowerNodeState entry keeps its recorded CPU IDs and no CPUs move. This holds for both
// an available and an unavailable profile entry, since a dropped or emptied entry would
// leak CPUs when the Pod is deleted.
func TestSSA_PodReconcile_CPUReadErrorKeepsRecordedCPUs(t *testing.T) {
	testCases := []struct {
		name            string
		profileProvided bool
	}{
		{name: "available profile", profileProvided: true},
		{name: "unavailable profile", profileProvided: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Test setup.
			cl, cachedCl, cleanup := setupEnvTestWithPodCache(t)
			defer cleanup()

			nodeName := "test-node"
			t.Setenv("NODE_NAME", nodeName)
			ctx := context.TODO()

			createPodReconcilePrereqs(t, cl, ctx, nodeName, profilePerformance)
			createPerformancePowerPod(t, cl, ctx, nodeName)

			r := createPodReconcilerWithEnvTest(t, cachedCl,
				[]*podresourcesapi.PodResources{{
					Name: "test-pod", Namespace: PowerNamespace,
					Containers: []*podresourcesapi.ContainerResources{{
						Name: "perf-container", CpuIds: []int64{5, 6},
					}},
				}},
				[]uint{0, 1, 2, 3, 4, 5, 6, 7},
			)
			exclusivePool := r.PowerLibrary.(*hostMock).GetExclusivePool(profilePerformance).(*poolMock)

			req := reconcile.Request{
				NamespacedName: client.ObjectKey{Name: "test-pod", Namespace: PowerNamespace},
			}
			pnsKey := client.ObjectKey{Name: fmt.Sprintf("%s-power-state", nodeName), Namespace: PowerNamespace}

			// Reconcile once with the profile available: the entry records the CPUs it owns.
			_, err := r.Reconcile(ctx, req)
			require.NoError(t, err)

			pc := singleExclusiveContainer(t, cl, ctx, pnsKey)
			assert.ElementsMatch(t, []uint{5, 6}, pc.CPUIDs)
			exclusivePool.AssertNumberOfCalls(t, "MoveCPUIDs", 1)

			if !tc.profileProvided {
				deleteProfileAndWaitForCache(t, cl, r, profilePerformance)
			}

			// No podresources entry for the container, so reading its CPUs fails.
			r.PodResourcesClient = *createFakePodResourcesListerClient(nil)

			_, err = r.Reconcile(ctx, req)
			require.ErrorContains(t, err, "reading CPUs of container")

			// The status entry is untouched: the container still has its recorded CPU IDs.
			pc = singleExclusiveContainer(t, cl, ctx, pnsKey)
			assert.ElementsMatch(t, []uint{5, 6}, pc.CPUIDs)
			assert.Empty(t, pc.Errors)

			// The reconcile aborted before moving anything into the exclusive pool.
			exclusivePool.AssertNumberOfCalls(t, "MoveCPUIDs", 1)
		})
	}
}

// TestSSA_PodReconcile_RegularInitContainerRecordedAsError asserts that a PowerProfile
// requested by a regular init container is recorded as a permanent error instead of
// aborting the reconcile: the kubelet PodResources API never reports CPUs for such
// containers, so the pod's sidecar and app containers must still be processed.
func TestSSA_PodReconcile_RegularInitContainerRecordedAsError(t *testing.T) {
	// Test setup.
	cl, cachedCl, cleanup := setupEnvTestWithPodCache(t)
	defer cleanup()

	nodeName := "test-node"
	t.Setenv("NODE_NAME", nodeName)
	ctx := context.TODO()

	createPodReconcilePrereqs(t, cl, ctx, nodeName, profilePerformance, profileBalancePerformance)

	// Build and create Power Pod.
	restartPolicyAlways := corev1.ContainerRestartPolicyAlways
	// Memory requests must equal limits, otherwise the API server computes a
	// non-Guaranteed QoS class at create and status.qosClass is immutable.
	containerWithProfile := func(name, profile string, cpus int64) corev1.Container {
		return corev1.Container{
			Name:  name,
			Image: "test-image",
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:                            *resource.NewQuantity(cpus, resource.DecimalSI),
					corev1.ResourceMemory:                         *resource.NewQuantity(128, resource.DecimalSI),
					corev1.ResourceName(ResourcePrefix + profile): *resource.NewQuantity(cpus, resource.DecimalSI),
				},
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:                            *resource.NewQuantity(cpus, resource.DecimalSI),
					corev1.ResourceMemory:                         *resource.NewQuantity(128, resource.DecimalSI),
					corev1.ResourceName(ResourcePrefix + profile): *resource.NewQuantity(cpus, resource.DecimalSI),
				},
			},
		}
	}

	sidecar := containerWithProfile("sidecar", profilePerformance, 1)
	sidecar.RestartPolicy = &restartPolicyAlways
	initContainer := containerWithProfile("init", profileBalancePerformance, 1)
	appContainer := containerWithProfile("app", profilePerformance, 2)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: PowerNamespace},
		Spec: corev1.PodSpec{
			NodeName:       nodeName,
			InitContainers: []corev1.Container{sidecar, initContainer},
			Containers:     []corev1.Container{appContainer},
		},
	}
	require.NoError(t, cl.Create(ctx, pod))
	// Update pod status (envtest doesn't set status on create).
	pod.Status = corev1.PodStatus{
		Phase:    corev1.PodRunning,
		QOSClass: corev1.PodQOSGuaranteed,
		InitContainerStatuses: []corev1.ContainerStatus{
			{Name: "sidecar", ContainerID: "cri-o://sidecar"},
			{Name: "init", ContainerID: "cri-o://init"},
		},
		ContainerStatuses: []corev1.ContainerStatus{
			{Name: "app", ContainerID: "cri-o://app"},
		},
	}
	require.NoError(t, cl.Status().Update(ctx, pod))

	// The kubelet reports CPUs for the sidecar and the app container only.
	r := createPodReconcilerWithEnvTest(t, cachedCl,
		[]*podresourcesapi.PodResources{{
			Name: "test-pod", Namespace: PowerNamespace,
			Containers: []*podresourcesapi.ContainerResources{
				{Name: "sidecar", CpuIds: []int64{5}},
				{Name: "app", CpuIds: []int64{6, 7}},
			},
		}},
		[]uint{0, 1, 2, 3, 4, 5, 6, 7},
	)
	exclusivePool := r.PowerLibrary.(*hostMock).GetExclusivePool(profilePerformance).(*poolMock)

	// The regular init container must not fail the reconcile.
	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "test-pod", Namespace: PowerNamespace},
	})
	require.NoError(t, err)

	pnsKey := client.ObjectKey{Name: fmt.Sprintf("%s-power-state", nodeName), Namespace: PowerNamespace}
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, pnsKey, pns))
	require.NotNil(t, pns.Status.CPUPools)
	require.Len(t, pns.Status.CPUPools.Exclusive, 1)
	assert.Equal(t, string(pod.UID), pns.Status.CPUPools.Exclusive[0].PodUID)
	recordedContainers := pns.Status.CPUPools.Exclusive[0].PowerContainers
	require.Len(t, recordedContainers, 3)

	byName := make(map[string]powerv1alpha1.PowerContainer, len(recordedContainers))
	for _, c := range recordedContainers {
		byName[c.Name] = c
	}

	// The sidecar and the app container keep their CPUs and have no errors.
	assert.ElementsMatch(t, []uint{5}, byName["sidecar"].CPUIDs)
	assert.Equal(t, profilePerformance, byName["sidecar"].PowerProfile)
	assert.Empty(t, byName["sidecar"].Errors)
	assert.ElementsMatch(t, []uint{6, 7}, byName["app"].CPUIDs)
	assert.Equal(t, profilePerformance, byName["app"].PowerProfile)
	assert.Empty(t, byName["app"].Errors)

	// The regular init container is recorded with its profile, no CPUs and a
	// permanent error.
	require.Contains(t, byName, "init")
	assert.Empty(t, byName["init"].CPUIDs)
	assert.Equal(t, profileBalancePerformance, byName["init"].PowerProfile)
	require.Len(t, byName["init"].Errors, 1)
	assert.Contains(t, byName["init"].Errors[0], "cannot be applied to init container")

	// The sidecar and app CPUs still move into the exclusive pool.
	exclusivePool.AssertCalled(t, "MoveCPUIDs", []uint{5})
	exclusivePool.AssertCalled(t, "MoveCPUIDs", []uint{6, 7})

	// handlePodDeletion releases the recorded sidecar and app CPUs back to the shared pool.
	logger := ctrl.Log.WithName("testing")
	_, err = r.handlePodDeletion(ctx, pod, nodeName, &logger)
	require.NoError(t, err)

	sharedPool := r.PowerLibrary.(*hostMock).GetSharedPool().(*poolMock)
	sharedPool.AssertCalled(t, "MoveCPUIDs", []uint{5})
	sharedPool.AssertCalled(t, "MoveCPUIDs", []uint{6, 7})

	pns = &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, pnsKey, pns))
	if pns.Status.CPUPools != nil {
		assert.Empty(t, pns.Status.CPUPools.Exclusive)
	}
}

func TestSSA_PodReconcile_TwoPodsMerge(t *testing.T) {
	cl, cachedCl, cleanup := setupEnvTestWithPodCache(t)
	defer cleanup()

	nodeName := "test-node"
	t.Setenv("NODE_NAME", nodeName)
	ctx := context.TODO()

	createPodReconcilePrereqs(t, cl, ctx, nodeName, profilePerformance, profileBalancePerformance)

	// --- Create pod-1 with two containers requesting different profiles. ---
	pod1Obj := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-1", Namespace: PowerNamespace},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{
				{
					Name: "c1", Image: "perf-pod",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
							corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
							corev1.ResourceName(ResourcePrefix + profilePerformance): *resource.NewQuantity(2, resource.DecimalSI),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
							corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
							corev1.ResourceName(ResourcePrefix + profilePerformance): *resource.NewQuantity(2, resource.DecimalSI),
						},
					},
				},
				{
					Name: "c2", Image: "perf-pod",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
							corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
							corev1.ResourceName(ResourcePrefix + profileBalancePerformance): *resource.NewQuantity(2, resource.DecimalSI),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
							corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
							corev1.ResourceName(ResourcePrefix + profileBalancePerformance): *resource.NewQuantity(2, resource.DecimalSI),
						},
					},
				},
			},
		},
	}
	require.NoError(t, cl.Create(ctx, pod1Obj))
	pod1Obj.Status = corev1.PodStatus{
		Phase:    corev1.PodRunning,
		QOSClass: corev1.PodQOSGuaranteed,
		ContainerStatuses: []corev1.ContainerStatus{
			{Name: "c1", ContainerID: "cri-o://pod-1-c1"},
			{Name: "c2", ContainerID: "cri-o://pod-1-c2"},
		},
	}
	require.NoError(t, cl.Status().Update(ctx, pod1Obj))
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "pod-1", Namespace: PowerNamespace}, pod1Obj))

	// --- Create pod-2 with a single container. ---
	pod2Obj := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod-2", Namespace: PowerNamespace},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name: "c3", Image: "perf-pod",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
						corev1.ResourceName(ResourcePrefix + profilePerformance): *resource.NewQuantity(2, resource.DecimalSI),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(2, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
						corev1.ResourceName(ResourcePrefix + profilePerformance): *resource.NewQuantity(2, resource.DecimalSI),
					},
				},
			}},
		},
	}
	require.NoError(t, cl.Create(ctx, pod2Obj))
	pod2Obj.Status = corev1.PodStatus{
		Phase:    corev1.PodRunning,
		QOSClass: corev1.PodQOSGuaranteed,
		ContainerStatuses: []corev1.ContainerStatus{
			{Name: "c3", ContainerID: "cri-o://pod-2-c3"},
		},
	}
	require.NoError(t, cl.Status().Update(ctx, pod2Obj))
	require.NoError(t, cl.Get(ctx, client.ObjectKey{Name: "pod-2", Namespace: PowerNamespace}, pod2Obj))

	// Get the UIDs of the pods.
	uid1 := string(pod1Obj.UID)
	uid2 := string(pod2Obj.UID)

	// Create a single reconciler that knows about both pods' resources.
	r := createPodReconcilerWithEnvTest(t, cachedCl,
		[]*podresourcesapi.PodResources{
			{
				Name: "pod-1", Namespace: PowerNamespace,
				Containers: []*podresourcesapi.ContainerResources{
					{Name: "c1", CpuIds: []int64{1, 2}},
					{Name: "c2", CpuIds: []int64{5, 6}},
				},
			},
			{
				Name: "pod-2", Namespace: PowerNamespace,
				Containers: []*podresourcesapi.ContainerResources{
					{Name: "c3", CpuIds: []int64{3, 4}},
				},
			},
		},
		[]uint{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
	)

	// Reconcile both pods with the same reconciler.
	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "pod-1", Namespace: PowerNamespace},
	})
	require.NoError(t, err)
	_, err = r.Reconcile(ctx, reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "pod-2", Namespace: PowerNamespace},
	})
	require.NoError(t, err)

	// Verify both pods coexist in PowerNodeState via real SSA merge.
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{
		Name: fmt.Sprintf("%s-power-state", nodeName), Namespace: PowerNamespace,
	}, pns))

	require.NotNil(t, pns.Status.CPUPools)
	require.Len(t, pns.Status.CPUPools.Exclusive, 2, "both pods should be present via correct SSA merge")

	// Collect entries by podUID for stable assertions.
	podEntries := map[string]powerv1alpha1.ExclusiveCPUPoolStatus{}
	for _, entry := range pns.Status.CPUPools.Exclusive {
		podEntries[entry.PodUID] = entry
	}

	// Check pod 1: two containers with different profiles.
	pod1 := podEntries[uid1]
	assert.Equal(t, "pod-1", pod1.Pod)
	require.Len(t, pod1.PowerContainers, 2)
	containersByName := map[string]powerv1alpha1.PowerContainer{}
	for _, pc := range pod1.PowerContainers {
		containersByName[pc.Name] = pc
	}
	assert.Equal(t, profilePerformance, containersByName["c1"].PowerProfile)
	assert.ElementsMatch(t, []uint{1, 2}, containersByName["c1"].CPUIDs)
	assert.Equal(t, profileBalancePerformance, containersByName["c2"].PowerProfile)
	assert.ElementsMatch(t, []uint{5, 6}, containersByName["c2"].CPUIDs)

	// Check pod 2: single container.
	pod2 := podEntries[uid2]
	assert.Equal(t, "pod-2", pod2.Pod)
	require.Len(t, pod2.PowerContainers, 1)
	assert.Equal(t, profilePerformance, pod2.PowerContainers[0].PowerProfile)
	assert.ElementsMatch(t, []uint{3, 4}, pod2.PowerContainers[0].CPUIDs)

	// Verify SSA field manager ownership for both pods.
	managers := map[string]bool{}
	for _, mf := range pns.ManagedFields {
		managers[mf.Manager] = true
	}
	assert.True(t, managers[fmt.Sprintf("powerpod-controller.%s", uid1)], "expected field manager for pod-1")
	assert.True(t, managers[fmt.Sprintf("powerpod-controller.%s", uid2)], "expected field manager for pod-2")
}

// TestSSA_PodReconcile_NoDuplicateContainers verifies that reconciling the same pod
// multiple times does not create duplicate container entries in PowerNodeState.
func TestSSA_PodReconcile_NoDuplicateContainers(t *testing.T) {
	cl, cachedCl, cleanup := setupEnvTestWithPodCache(t)
	defer cleanup()

	nodeName := "test-node"
	t.Setenv("NODE_NAME", nodeName)
	ctx := context.TODO()

	createPodReconcilePrereqs(t, cl, ctx, nodeName, profilePerformance)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod", Namespace: PowerNamespace,
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name:  "perf-container",
				Image: "perf-pod",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(3, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
						corev1.ResourceName(ResourcePrefix + profilePerformance): *resource.NewQuantity(3, resource.DecimalSI),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    *resource.NewQuantity(3, resource.DecimalSI),
						corev1.ResourceMemory: *resource.NewQuantity(128, resource.DecimalSI),
						corev1.ResourceName(ResourcePrefix + profilePerformance): *resource.NewQuantity(3, resource.DecimalSI),
					},
				},
			}},
		},
	}
	require.NoError(t, cl.Create(ctx, pod))
	pod.Status = corev1.PodStatus{
		Phase:    corev1.PodRunning,
		QOSClass: corev1.PodQOSGuaranteed,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "perf-container", ContainerID: "cri-o://container-abc",
		}},
	}
	require.NoError(t, cl.Status().Update(ctx, pod))

	r := createPodReconcilerWithEnvTest(t, cachedCl,
		[]*podresourcesapi.PodResources{{
			Name: "test-pod", Namespace: PowerNamespace,
			Containers: []*podresourcesapi.ContainerResources{{
				Name: "perf-container", CpuIds: []int64{1, 5, 8},
			}},
		}},
		[]uint{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
	)

	req := reconcile.Request{
		NamespacedName: client.ObjectKey{Name: "test-pod", Namespace: PowerNamespace},
	}

	// Reconcile the same pod twice.
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)

	// Verify PowerNodeState has exactly one entry with one container — no duplicates.
	pns := &powerv1alpha1.PowerNodeState{}
	require.NoError(t, cl.Get(ctx, client.ObjectKey{
		Name: fmt.Sprintf("%s-power-state", nodeName), Namespace: PowerNamespace,
	}, pns))

	require.NotNil(t, pns.Status.CPUPools)
	require.Len(t, pns.Status.CPUPools.Exclusive, 1, "should have exactly one pod entry")
	assert.Equal(t, string(pod.UID), pns.Status.CPUPools.Exclusive[0].PodUID)
	require.Len(t, pns.Status.CPUPools.Exclusive[0].PowerContainers, 1, "should have exactly one container — no duplicates")
	pc := pns.Status.CPUPools.Exclusive[0].PowerContainers[0]
	assert.Equal(t, "perf-container", pc.Name)
	assert.Equal(t, profilePerformance, pc.PowerProfile)
	assert.ElementsMatch(t, []uint{1, 5, 8}, pc.CPUIDs)
}

func TestSSA_ExclusivePoolFieldManagerOwnership(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	nodeName := "test-node"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	createTestPowerNodeState(t, cl, pnsName)

	// Create a minimal reconciler with the envtest client.
	r := &PowerPodReconciler{Client: cl}
	logger := ctrl.Log.WithName("testing")

	// Add pod 1.
	err := r.addPowerNodeStatusExclusiveEntry(ctx, nodeName, "uid-1", "pod-1",
		[]powerv1alpha1.PowerContainer{
			{Name: "c1", ID: "cid-1", PowerProfile: profilePerformance, CPUIDs: []uint{1, 2}},
		}, &logger)
	require.NoError(t, err)

	// Add pod 2.
	err = r.addPowerNodeStatusExclusiveEntry(ctx, nodeName, "uid-2", "pod-2",
		[]powerv1alpha1.PowerContainer{
			{Name: "c2", ID: "cid-2", PowerProfile: profileBalancePower, CPUIDs: []uint{3, 4}},
		}, &logger)
	require.NoError(t, err)

	// Both pods should coexist.
	pns := &powerv1alpha1.PowerNodeState{}
	err = cl.Get(ctx, client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns)
	require.NoError(t, err)

	require.NotNil(t, pns.Status.CPUPools)
	assert.Len(t, pns.Status.CPUPools.Exclusive, 2, "both pod entries should be present")
}

func TestSSA_ExclusivePoolPodRemovalPreservesOtherPods(t *testing.T) {
	cl, cleanup := setupEnvTest(t)
	defer cleanup()

	ctx := context.TODO()
	nodeName := "test-node"
	pnsName := fmt.Sprintf("%s-power-state", nodeName)
	createTestPowerNodeState(t, cl, pnsName)

	r := &PowerPodReconciler{Client: cl}
	logger := ctrl.Log.WithName("testing")

	// Add two pods.
	err := r.addPowerNodeStatusExclusiveEntry(ctx, nodeName, "uid-1", "pod-1",
		[]powerv1alpha1.PowerContainer{
			{Name: "c1", ID: "cid-1", PowerProfile: profilePerformance, CPUIDs: []uint{1, 2}},
		}, &logger)
	require.NoError(t, err)

	err = r.addPowerNodeStatusExclusiveEntry(ctx, nodeName, "uid-2", "pod-2",
		[]powerv1alpha1.PowerContainer{
			{Name: "c2", ID: "cid-2", PowerProfile: profilePerformance, CPUIDs: []uint{3, 4}},
		}, &logger)
	require.NoError(t, err)

	// Remove pod 1.
	err = r.removePowerNodeStatusExclusiveEntry(ctx, nodeName, "uid-1", &logger)
	require.NoError(t, err)

	// Only pod 2 should remain.
	pns := &powerv1alpha1.PowerNodeState{}
	err = cl.Get(ctx, client.ObjectKey{Name: pnsName, Namespace: PowerNamespace}, pns)
	require.NoError(t, err)

	require.NotNil(t, pns.Status.CPUPools)
	assert.Len(t, pns.Status.CPUPools.Exclusive, 1, "only pod-2 should remain")
	assert.Equal(t, "uid-2", pns.Status.CPUPools.Exclusive[0].PodUID)
}
