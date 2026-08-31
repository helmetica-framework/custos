package gather

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/streaming/pkg/httpstream"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
