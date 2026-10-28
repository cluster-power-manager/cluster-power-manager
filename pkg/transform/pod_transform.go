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
)

// TransformPodForCache is a cache TransformFunc shared by the manager and
// node-agent Pod informers.
// This transform drops every field the PowerPod path never reads (managedFields,
// labels, annotations, env, volumes, probes, images, most
// of status, ...) so each cached pod is smaller than the full object. How much
// smaller depends on how much the pod carries in the dropped fields.
//
// The preserved fields are exactly those read by the PowerPod controller path
// plus the spec.nodeName field index:
//
//   - metadata.name, metadata.namespace, metadata.uid, metadata.resourceVersion,
//     metadata.deletionTimestamp (resourceVersion is kept for informer/watch bookkeeping)
//   - spec.nodeName
//   - spec.{containers,initContainers}[].name
//   - spec.{containers,initContainers}[].resources (requests, limits and claims are all read)
//   - status.phase, status.qosClass
//   - status.{containerStatuses,initContainerStatuses}[].{name,containerID}
//
// The manager's PowerProfile delete check only reads metadata.name and
// namespace, status.phase, and requested power-resource keys. The
// manager and node-agent share this transform for now; trim it further for the
// node-agent if future memory measurements show a need.
//
// IMPORTANT: if the PowerPod path starts reading a new pod field, it must be
// added here or it will silently read as a zero value. TestTransformPodForCache
// guards the preserved set.
//
// The function is idempotent: transforming an already-transformed pod yields an
// equal pod object.
func TransformPodForCache(obj interface{}) (interface{}, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		// Not a *Pod (e.g. a cache.DeletedFinalStateUnknown tombstone); leave untouched.
		return obj, nil
	}

	return &corev1.Pod{
		TypeMeta: pod.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Name:              pod.Name,
			Namespace:         pod.Namespace,
			UID:               pod.UID,
			ResourceVersion:   pod.ResourceVersion,
			DeletionTimestamp: pod.DeletionTimestamp,
		},
		Spec: corev1.PodSpec{
			NodeName:       pod.Spec.NodeName,
			Containers:     transformContainers(pod.Spec.Containers),
			InitContainers: transformContainers(pod.Spec.InitContainers),
		},
		Status: corev1.PodStatus{
			Phase:                 pod.Status.Phase,
			QOSClass:              pod.Status.QOSClass,
			ContainerStatuses:     transformContainerStatuses(pod.Status.ContainerStatuses),
			InitContainerStatuses: transformContainerStatuses(pod.Status.InitContainerStatuses),
		},
	}, nil
}

// transformContainers keeps only the container name and resource requirements,
// which are all the PowerPod path inspects per container.
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
// getContainerID.
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
