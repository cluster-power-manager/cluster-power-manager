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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

// TransformPodForManager is the typed manager-side Pod cache transform. The
// PowerProfile delete check only reads metadata.name, metadata.namespace,
// status.phase and container resource requests, so this keeps exactly those
// fields plus the metadata needed for informer bookkeeping.
func TransformPodForManager(pod *corev1.Pod) *corev1.Pod {
	transformed := transformPodBase(pod)
	transformed.Spec.Containers = transformContainerResources(pod.Spec.Containers)
	transformed.Spec.InitContainers = transformContainerResources(pod.Spec.InitContainers)
	return transformed
}

// TransformPodForManagerCache adapts TransformPodForManager to the cache's
// TransformFunc.
var TransformPodForManagerCache = podCacheTransform(TransformPodForManager)

// TransformPodForAgent is the typed node-agent Pod cache transform. It extends
// transformPodBase with the fields read by the PowerPod controller path:
//
//   - metadata.uid
//   - spec.nodeName
//   - spec.{containers,initContainers}[].name
//   - spec.{containers,initContainers}[].resources (requests, limits and claims)
//   - status.qosClass
//   - status.{containerStatuses,initContainerStatuses}[].{name,containerID}
//
// IMPORTANT: if the PowerPod path starts reading a new pod field, it must be
// added here or it will silently read as a zero value. TestTransformPodForAgent guards
// the preserved set.
//
// Both transforms are idempotent: transforming an already-transformed pod
// yields an equal pod object.
func TransformPodForAgent(pod *corev1.Pod) *corev1.Pod {
	transformed := transformPodBase(pod)
	transformed.ObjectMeta.UID = pod.UID
	transformed.Spec.NodeName = pod.Spec.NodeName
	transformed.Spec.Containers = transformContainers(pod.Spec.Containers)
	transformed.Spec.InitContainers = transformContainers(pod.Spec.InitContainers)
	transformed.Status.QOSClass = pod.Status.QOSClass
	transformed.Status.ContainerStatuses = transformContainerStatuses(pod.Status.ContainerStatuses)
	transformed.Status.InitContainerStatuses = transformContainerStatuses(pod.Status.InitContainerStatuses)
	return transformed
}

// TransformPodForAgentCache adapts TransformPodForAgent to the cache's
// TransformFunc.
var TransformPodForAgentCache = podCacheTransform(TransformPodForAgent)

// podCacheTransform adapts a typed Pod transform to cache.TransformFunc.
// Non-Pod objects are returned unchanged so cache tombstones pass through.
func podCacheTransform(transformPod func(*corev1.Pod) *corev1.Pod) cache.TransformFunc {
	return func(obj interface{}) (interface{}, error) {
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			// Not a *Pod (e.g. a cache.DeletedFinalStateUnknown tombstone); leave untouched.
			return obj, nil
		}
		return transformPod(pod), nil
	}
}

// transformPodBase keeps the fields shared by every Pod cache transform,
// dropping managedFields, labels, annotations, env, volumes, probes, images and
// most of status. Finalizers are kept because they are tiny and terminating
// objects are expected to retain them.
func transformPodBase(pod *corev1.Pod) *corev1.Pod {
	return &corev1.Pod{
		TypeMeta: pod.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Name:              pod.Name,
			Namespace:         pod.Namespace,
			ResourceVersion:   pod.ResourceVersion,
			DeletionTimestamp: pod.DeletionTimestamp,
			Finalizers:        append([]string(nil), pod.Finalizers...),
		},
		Status: corev1.PodStatus{
			Phase: pod.Status.Phase,
		},
	}
}

// transformContainerResources keeps only the resource requirements, which are
// all the PowerProfile delete check reads from a container. Empty input becomes
// nil so repeated transforms stay equal.
func transformContainerResources(in []corev1.Container) []corev1.Container {
	if len(in) == 0 {
		return nil
	}
	out := make([]corev1.Container, len(in))
	for i := range in {
		out[i] = corev1.Container{Resources: in[i].Resources}
	}
	return out
}

// transformContainers keeps only the container name and resource requirements,
// which are all the PowerPod path inspects per container. Empty input becomes
// nil so repeated transforms stay equal.
func transformContainers(in []corev1.Container) []corev1.Container {
	if len(in) == 0 {
		return nil
	}
	out := make([]corev1.Container, len(in))
	for i := range in {
		out[i] = corev1.Container{
			Name:      in[i].Name,
			Resources: in[i].Resources,
		}
	}
	return out
}

// transformContainerStatuses keeps only the name->containerID mapping used by
// getContainerID. Empty input becomes nil so repeated transforms stay equal.
func transformContainerStatuses(in []corev1.ContainerStatus) []corev1.ContainerStatus {
	if len(in) == 0 {
		return nil
	}
	out := make([]corev1.ContainerStatus, len(in))
	for i := range in {
		out[i] = corev1.ContainerStatus{
			Name:        in[i].Name,
			ContainerID: in[i].ContainerID,
		}
	}
	return out
}
