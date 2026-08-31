package gather

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
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

func resultKey() types.NamespacedName {
	return types.NamespacedName{Name: "sample-gather-result", Namespace: testNamespace}
}

func TestWriteResult_SuccessRecordsOK(t *testing.T) {
	c := newResolver().Client

	err := WriteResult(context.Background(), c, testNamespace, resultKey().Name, nil, nil)

	require.NoError(t, err)

	got := &corev1.ConfigMap{}
	require.NoError(t, c.Get(context.Background(), resultKey(), got))
	assert.Equal(t, ResultStatusOK, got.Data[ResultStatusKey])
}

// This message is the only thing the controller has to report a failure with,
// since it never reads the Job's log.
func TestWriteResult_FailureRecordsTheMessage(t *testing.T) {
	c := newResolver().Client

	err := WriteResult(context.Background(), c, testNamespace, resultKey().Name, nil,
		errors.New(`key "USERNAME": getting Secret "postgres-poc-app": not found`))

	require.NoError(t, err)

	got := &corev1.ConfigMap{}
	require.NoError(t, c.Get(context.Background(), resultKey(), got))
	assert.Equal(t, ResultStatusError, got.Data[ResultStatusKey])
	assert.Contains(t, got.Data[ResultMessageKey], "postgres-poc-app")
}

// A refreshed gather reuses the ConfigMap name, so a stale error left next to
// a fresh success would have the controller reporting a failure that is over.
func TestWriteResult_OverwritesAnEarlierResult(t *testing.T) {
	c := newResolver().Client
	ctx := context.Background()

	require.NoError(t, WriteResult(ctx, c, testNamespace, resultKey().Name, nil, errors.New("boom")))
	require.NoError(t, WriteResult(ctx, c, testNamespace, resultKey().Name, nil, nil))

	got := &corev1.ConfigMap{}
	require.NoError(t, c.Get(ctx, resultKey(), got))
	assert.Equal(t, ResultStatusOK, got.Data[ResultStatusKey])
	assert.Empty(t, got.Data[ResultMessageKey])
}

// Deleting the Arcanum has to take the result ConfigMap with it. The Job is
// the only process that ever writes this object, so it is the only one that
// can put the reference on.
func TestWriteResult_CarriesTheOwnerReference(t *testing.T) {
	c := newResolver().Client
	owner := metav1ac.OwnerReference().
		WithAPIVersion("arcana.helmetica.io/v1").
		WithKind("Arcanum").
		WithName("sample").
		WithUID(types.UID("d6f7a1b2-0000-4000-8000-000000000001")).
		WithController(true)

	err := WriteResult(context.Background(), c, testNamespace, resultKey().Name, owner, nil)

	require.NoError(t, err)

	got := &corev1.ConfigMap{}
	require.NoError(t, c.Get(context.Background(), resultKey(), got))
	require.Len(t, got.OwnerReferences, 1)
	assert.Equal(t, "sample", got.OwnerReferences[0].Name)
	assert.True(t, *got.OwnerReferences[0].Controller)

	// Setting it needs update on the owner's finalizers subresource, which
	// the admin ClusterRole does not cover for a custom kind.
	assert.Nil(t, got.OwnerReferences[0].BlockOwnerDeletion,
		"blockOwnerDeletion would have the API server reject the whole write")
}
