package gather

import (
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// selectPod returns the alphabetically first ready pod matching the selector.
//
// Alphabetical rather than newest, so repeated gathers keep hitting the same
// pod. A value read off one replica can differ from the next, and a
// credential that changes between reconciles for no visible reason is worse
// than one read from a pod that is not the freshest.
//
// Not ready is not worth waiting for. The Job runs with backoffLimit zero, so
// there is no retry to wait through, and a service whose pods are not up is a
// condition for the Arcanum to report rather than one for the Job to block
// on.
func (r *Resolver) selectPod(
	ctx context.Context,
	namespace string,
	selector *metav1.LabelSelector,
) (string, error) {
	labelSelector, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return "", fmt.Errorf("building pod selector: %w", err)
	}

	pods := &corev1.PodList{}

	err = r.Client.List(ctx, pods,
		client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: labelSelector})
	if err != nil {
		return "", fmt.Errorf("listing pods: %w", err)
	}

	var ready []string

	for _, p := range pods.Items {
		if podIsReady(p) {
			ready = append(ready, p.GetName())
		}
	}

	if len(ready) == 0 {
		return "", fmt.Errorf("no ready pod matches %s", labelSelector)
	}

	slices.Sort(ready)

	return ready[0], nil
}

// podIsReady reports whether the pod carries a true Ready condition. A pod
// with no conditions at all has not been scheduled yet and is not ready.
func podIsReady(p corev1.Pod) bool {
	for _, condition := range p.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}

	return false
}
