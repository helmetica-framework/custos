package gather

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type fakeExecutor struct {
	out    string
	err    error
	gotPod string
	gotCmd []string
}

func (f *fakeExecutor) Exec(_ context.Context, _, pod, _ string, command []string) (string, error) {
	f.gotPod = pod
	f.gotCmd = command
	return f.out, f.err
}

func pod(name string, ready bool, labels map[string]string) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}

func execPlan() Plan {
	return Plan{Namespace: testNamespace, Entries: []Entry{{
		Key:  "DATABASE",
		Kind: EntryExec,
		PodSelector: &metav1.LabelSelector{
			MatchLabels: map[string]string{"app": "postgres"},
		},
		Container: "postgres",
		Command:   []string{"psql", "-tAc", "select current_database()"},
	}}}
}

func TestResolve_ExecReturnsStdout(t *testing.T) {
	r := newResolver(pod("postgres-0", true, map[string]string{"app": "postgres"}))
	exec := &fakeExecutor{out: "poc\n"}
	r.Executor = exec

	got, err := r.Resolve(context.Background(), execPlan())

	require.NoError(t, err)
	assert.Equal(t, map[string]string{"DATABASE": "poc"},
		got, "the trailing newline a shell adds is not part of the value")
	assert.Equal(t, "postgres-0", exec.gotPod)
	assert.Equal(t, []string{"psql", "-tAc", "select current_database()"}, exec.gotCmd)
}

// Repeated gathers have to hit the same pod, or a value read from one replica
// could differ from the next run for no visible reason.
func TestResolve_ExecPicksTheFirstReadyPodByName(t *testing.T) {
	r := newResolver(
		pod("postgres-2", true, map[string]string{"app": "postgres"}),
		pod("postgres-0", true, map[string]string{"app": "postgres"}),
		pod("postgres-1", true, map[string]string{"app": "postgres"}),
	)
	exec := &fakeExecutor{out: "poc"}
	r.Executor = exec

	_, err := r.Resolve(context.Background(), execPlan())

	require.NoError(t, err)
	assert.Equal(t, "postgres-0", exec.gotPod)
}

func TestResolve_ExecSkipsPodsThatAreNotReady(t *testing.T) {
	r := newResolver(
		pod("postgres-0", false, map[string]string{"app": "postgres"}),
		pod("postgres-1", true, map[string]string{"app": "postgres"}),
	)
	exec := &fakeExecutor{out: "poc"}
	r.Executor = exec

	_, err := r.Resolve(context.Background(), execPlan())

	require.NoError(t, err)
	assert.Equal(t, "postgres-1", exec.gotPod)
}

// backoffLimit is 0, so there is no retry to wait through. A service whose
// pods are not up is a condition for the Arcanum to report, not for the Job
// to block on.
func TestResolve_ExecWithNoReadyPodFails(t *testing.T) {
	r := newResolver(pod("postgres-0", false, map[string]string{"app": "postgres"}))
	r.Executor = &fakeExecutor{out: "poc"}

	_, err := r.Resolve(context.Background(), execPlan())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE")
	assert.Contains(t, err.Error(), "no ready pod")
}

func TestResolve_ExecFailureNamesTheKey(t *testing.T) {
	r := newResolver(pod("postgres-0", true, map[string]string{"app": "postgres"}))
	r.Executor = &fakeExecutor{err: errors.New("command terminated with exit code 1")}

	_, err := r.Resolve(context.Background(), execPlan())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE")
}
