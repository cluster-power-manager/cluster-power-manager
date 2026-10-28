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

package transform

import (
	"testing"
	"time"

	"github.com/cluster-power-manager/cluster-power-manager/controllers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// largePod builds a pod populated with both the fields the PowerPod path reads and
// a representative set of fields it does not, so the transform can be checked
// to keep the former and drop the latter.
func largePod() *corev1.Pod {
	now := metav1.NewTime(time.Now())
	reqs := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"),
			corev1.ResourceName(controllers.ResourcePrefix + "performance"): resource.MustParse("2"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"),
			corev1.ResourceName(controllers.ResourcePrefix + "performance"): resource.MustParse("2"),
		},
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "my-pod",
			Namespace:         "my-ns",
			UID:               "uid-123",
			ResourceVersion:   "4242",
			DeletionTimestamp: &now,
			// Dropped fields:
			Labels:      map[string]string{"app": "foo"},
			Annotations: map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "huge-blob"},
			ManagedFields: []metav1.ManagedFieldsEntry{
				{Manager: "kubelet", Operation: metav1.ManagedFieldsOperationUpdate},
			},
			Finalizers: []string{"example.com/finalizer"},
		},
		Spec: corev1.PodSpec{
			NodeName:           "node-a",
			ServiceAccountName: "dropped-sa",
			Tolerations:        []corev1.Toleration{{Key: "dropped"}},
			InitContainers: []corev1.Container{
				{Name: "init-0", Image: "dropped", Resources: reqs, Env: []corev1.EnvVar{{Name: "DROP", Value: "x"}}},
			},
			Containers: []corev1.Container{
				{
					Name:         "main",
					Image:        "dropped-image",
					Command:      []string{"dropped"},
					Args:         []string{"dropped"},
					Env:          []corev1.EnvVar{{Name: "DROP", Value: "x"}},
					VolumeMounts: []corev1.VolumeMount{{Name: "dropped"}},
					Resources:    reqs,
				},
			},
		},
		Status: corev1.PodStatus{
			Phase:    corev1.PodRunning,
			QOSClass: corev1.PodQOSGuaranteed,
			PodIP:    "10.0.0.1", // dropped
			InitContainerStatuses: []corev1.ContainerStatus{
				{Name: "init-0", ContainerID: "containerd://init0", Image: "dropped", RestartCount: 3},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "main", ContainerID: "containerd://main", Image: "dropped", RestartCount: 7},
			},
		},
	}
}

func TestTransformPodForCache(t *testing.T) {
	out, err := TransformPodForCache(largePod())
	require.NoError(t, err)
	transformed, ok := out.(*corev1.Pod)
	require.True(t, ok)

	// --- Preserved metadata ---
	assert.Equal(t, "my-pod", transformed.Name)
	assert.Equal(t, "my-ns", transformed.Namespace)
	assert.Equal(t, "uid-123", string(transformed.UID))
	assert.Equal(t, "4242", transformed.ResourceVersion)
	assert.NotNil(t, transformed.DeletionTimestamp)

	// --- Dropped metadata ---
	assert.Nil(t, transformed.Labels)
	assert.Nil(t, transformed.Annotations)
	assert.Nil(t, transformed.ManagedFields)
	assert.Nil(t, transformed.Finalizers)

	// --- Preserved spec ---
	assert.Equal(t, "node-a", transformed.Spec.NodeName)
	require.Len(t, transformed.Spec.Containers, 1)
	require.Len(t, transformed.Spec.InitContainers, 1)
	assert.Equal(t, "main", transformed.Spec.Containers[0].Name)
	assert.Equal(t, "init-0", transformed.Spec.InitContainers[0].Name)
	// Resources are read whole: requests, limits and claims.
	assert.Equal(t, largePod().Spec.Containers[0].Resources, transformed.Spec.Containers[0].Resources)

	// --- Dropped spec ---
	assert.Empty(t, transformed.Spec.ServiceAccountName)
	assert.Nil(t, transformed.Spec.Tolerations)
	assert.Empty(t, transformed.Spec.Containers[0].Image)
	assert.Nil(t, transformed.Spec.Containers[0].Env)
	assert.Nil(t, transformed.Spec.Containers[0].VolumeMounts)

	// --- Preserved status ---
	assert.Equal(t, corev1.PodRunning, transformed.Status.Phase)
	assert.Equal(t, corev1.PodQOSGuaranteed, transformed.Status.QOSClass)
	require.Len(t, transformed.Status.ContainerStatuses, 1)
	require.Len(t, transformed.Status.InitContainerStatuses, 1)
	assert.Equal(t, "main", transformed.Status.ContainerStatuses[0].Name)
	assert.Equal(t, "containerd://main", transformed.Status.ContainerStatuses[0].ContainerID)
	assert.Equal(t, "init-0", transformed.Status.InitContainerStatuses[0].Name)
	assert.Equal(t, "containerd://init0", transformed.Status.InitContainerStatuses[0].ContainerID)

	// --- Dropped status ---
	assert.Empty(t, transformed.Status.PodIP)
	assert.Zero(t, transformed.Status.ContainerStatuses[0].RestartCount)
	assert.Empty(t, transformed.Status.ContainerStatuses[0].Image)
}

func TestTransformPodForCache_PreservesPowerPredicateAndID(t *testing.T) {
	out, err := TransformPodForCache(largePod())
	require.NoError(t, err)
	transformed := out.(*corev1.Pod)

	// The transformed pod must still satisfy the readers that decide power-relevance.
	t.Setenv("NODE_NAME", "node-a")
	assert.True(t, controllers.PowerReleventPodPredicate(transformed), "transformed power pod should still match predicate")
	_, hasPowerRequest := transformed.Spec.Containers[0].Resources.Requests[corev1.ResourceName(controllers.ResourcePrefix+"performance")]
	assert.True(t, hasPowerRequest, "transformed container should retain its power request")
	assert.Equal(t, "containerd://main", transformed.Status.ContainerStatuses[0].ContainerID)
}

func TestTransformPodForCache_Idempotent(t *testing.T) {
	once, err := TransformPodForCache(largePod())
	require.NoError(t, err)
	twice, err := TransformPodForCache(once)
	require.NoError(t, err)
	assert.Equal(t, once, twice, "transforming an already-transformed pod must be a no-op")
}

func TestTransformPodForCache_NonPodPassthrough(t *testing.T) {
	in := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}
	out, err := TransformPodForCache(in)
	require.NoError(t, err)
	assert.Same(t, in, out, "a non-pod object must be returned unchanged")
}
