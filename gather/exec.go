package gather

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/streaming/pkg/httpstream"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// defaultContainerAnnotation is the convention kubectl exec follows when
	// no container is named. Honouring it lets a workload that puts a sidecar
	// first say so once, rather than in every Arcanum that execs into it.
	defaultContainerAnnotation = "kubectl.kubernetes.io/default-container"

	// defaultPodPoll is how often selectPod looks while it waits. Resolver's
	// own field exists so tests can run in milliseconds; nothing sets it in
	// production.
	defaultPodPoll = 2 * time.Second
)

// The keys and values of the result ConfigMap. The controller reads these to
// report a gather without needing pods/log.
const (
	ResultStatusKey   = "status"
	ResultMessageKey  = "message"
	ResultStatusOK    = "ok"
	ResultStatusError = "error"
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
// execContainer is the container to run the command in: the one the entry
// asked for, then the one the pod nominates, then its first.
//
// The API server only defaults this itself when the pod has exactly one
// container. For anything with a sidecar it refuses and lists the choices, so
// leaving the field empty is not the same as asking for the first.
func execContainer(pod *corev1.Pod, requested string) string {
	if requested != "" {
		return requested
	}

	nominated := pod.GetAnnotations()[defaultContainerAnnotation]
	for _, c := range pod.Spec.Containers {
		if c.Name == nominated {
			return nominated
		}
	}

	return pod.Spec.Containers[0].Name
}

func (r *Resolver) selectPod(
	ctx context.Context,
	namespace string,
	selector *metav1.LabelSelector,
	wait time.Duration,
) (*corev1.Pod, error) {
	labelSelector, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("building pod selector: %w", err)
	}

	poll := r.PodPoll
	if poll <= 0 {
		poll = defaultPodPoll
	}

	deadline := time.Now().Add(wait)
	waited := false

	for {
		pod, err := r.readyPod(ctx, namespace, labelSelector)
		if err != nil || pod != nil {
			if waited && pod != nil {
				slog.Info("a pod became ready", "pod", pod.GetName(), "selector", labelSelector.String())
			}

			return pod, err
		}

		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("no ready pod matches %s", labelSelector)
		}

		if !waited {
			slog.Info("waiting for a ready pod", "selector", labelSelector.String(), "wait", wait)

			waited = true
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("no ready pod matches %s: %w", labelSelector, ctx.Err())
		case <-time.After(poll):
		}
	}
}

// readyPod is the alphabetically first ready pod matching the selector, or nil
// when nothing matches right now. Alphabetical rather than newest, so repeated
// gathers hit the same pod.
func (r *Resolver) readyPod(
	ctx context.Context,
	namespace string,
	selector labels.Selector,
) (*corev1.Pod, error) {
	pods := &corev1.PodList{}

	err := r.Client.List(ctx, pods,
		client.InNamespace(namespace),
		client.MatchingLabelsSelector{Selector: selector})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}

	var ready []corev1.Pod

	for _, p := range pods.Items {
		if podIsReady(p) {
			ready = append(ready, p)
		}
	}

	if len(ready) == 0 {
		return nil, nil
	}

	slices.SortFunc(ready, func(a, b corev1.Pod) int {
		return strings.Compare(a.GetName(), b.GetName())
	})

	return &ready[0], nil
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

// PodExecutor runs commands through the pods/exec subresource, with the
// ServiceAccount token the Job carries.
type PodExecutor struct {
	Config *rest.Config
}

// Exec runs command in a pod and returns its stdout. An empty container is
// passed through, since that is already what the API means by the pod's first
// container.
func (e *PodExecutor) Exec(
	ctx context.Context,
	namespace, pod, container string,
	command []string,
) (string, error) {
	exec, err := e.getExec(namespace, pod, container, command)
	if err != nil {
		return "", err
	}

	return e.execute(ctx, exec)
}

// getExec builds the streaming executor for one command.
//
// SPDY is there for API servers too old to speak WebSocket. The two want
// different verbs, GET for the WebSocket and POST for SPDY, which is the
// thing that silently breaks if the pair is ever rebuilt from one URL.
func (e *PodExecutor) getExec(namespace, pod, container string, command []string) (remotecommand.Executor, error) {
	c, err := kubernetes.NewForConfig(e.Config)
	if err != nil {
		return nil, fmt.Errorf("can't create k8s for exec: %w", err)
	}

	req := c.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command:   command,
			Container: container,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	wsExec, err := remotecommand.NewWebSocketExecutor(e.Config, "GET", req.URL().String())
	if err != nil {
		return nil, fmt.Errorf("creating websocket executor: %w", err)
	}

	spdyExec, err := remotecommand.NewSPDYExecutor(e.Config, "POST", req.URL())
	if err != nil {
		return nil, fmt.Errorf("creating spdy executor: %w", err)
	}

	exec, err := remotecommand.NewFallbackExecutor(wsExec, spdyExec, func(err error) bool {
		if httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err) {
			slog.Warn("cannot upgrade to websocket, falling back to SPDY streaming")

			return true
		}

		return false
	})
	if err != nil {
		return nil, fmt.Errorf("creating fallback executor: %w", err)
	}

	return exec, nil
}

// execute streams the command and returns its stdout. A failure carries
// stderr and never stdout, because stdout is the credential and this error
// ends up on the Arcanum's status.
func (e *PodExecutor) execute(ctx context.Context, exec remotecommand.Executor) (string, error) {
	var stdout, stderr bytes.Buffer

	err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  nil,
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})
	if err != nil {
		if stderr.Len() > 0 {
			return "", fmt.Errorf("%w: %s", err, stderr.String())
		}

		return "", err
	}

	return stdout.String(), nil
}

// WriteResult records the outcome of a gather in a ConfigMap, so the
// controller can report it without reading the Job's log and without the
// pods/log permission that would need. A nil runErr means success.
//
// The message is whatever Resolve returned, and this ConfigMap is readable by
// anything with list access in the namespace, which is why no error on that
// path is allowed to carry a resolved value.
//
// Both keys are written every time. A refresh reuses the name, and a stale
// message left beside a fresh success would have the controller reporting a
// failure that is over.
func WriteResult(
	ctx context.Context,
	c client.Client,
	namespace, name string,
	owner *metav1ac.OwnerReferenceApplyConfiguration,
	runErr error,
) error {
	status, message := ResultStatusOK, ""
	if runErr != nil {
		status, message = ResultStatusError, runErr.Error()
	}

	cm := corev1ac.ConfigMap(name, namespace).
		WithData(map[string]string{
			ResultStatusKey:  status,
			ResultMessageKey: message,
		})

	if owner != nil {
		cm.WithOwnerReferences(owner)
	}

	if err := c.Apply(ctx, cm, FieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("applying result ConfigMap %q: %w", name, err)
	}

	return nil
}
