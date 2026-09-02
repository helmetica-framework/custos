package gather

import (
	"context"
	"errors"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
)

type fakeExecutor struct {
	out          string
	err          error
	gotPod       string
	gotContainer string
	gotCmd       []string
}

func (f *fakeExecutor) Exec(_ context.Context, _, pod, container string, command []string) (string, error) {
	f.gotPod = pod
	f.gotContainer = container
	f.gotCmd = command
	return f.out, f.err
}

func pod(name string, ready bool, labels map[string]string, containers ...string) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}

	if len(containers) == 0 {
		containers = []string{"app"}
	}

	spec := corev1.PodSpec{}
	for _, c := range containers {
		spec.Containers = append(spec.Containers, corev1.Container{Name: c, Image: "busybox"})
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels},
		Spec:       spec,
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

// A zero PodWait checks once and gives up, which is what the unit tests and a
// hand-run gather want. The waiting behaviour is opt in, below.
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

// The API server only defaults the container when the pod has exactly one of
// them. Anything with a sidecar gets "a container name must be specified",
// naming the choices, so the resolver has to pick rather than leave it empty.
func TestResolve_ExecDefaultsToTheFirstContainer(t *testing.T) {
	r := newResolver(pod("postgres-0", true, map[string]string{"app": "postgres"}, "postgres", "exporter"))
	exec := &fakeExecutor{out: "poc"}
	r.Executor = exec

	plan := execPlan()
	plan.Entries[0].Container = ""

	_, err := r.Resolve(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, "postgres", exec.gotContainer)
}

func TestResolve_ExecUsesTheNamedContainer(t *testing.T) {
	r := newResolver(pod("postgres-0", true, map[string]string{"app": "postgres"}, "postgres", "exporter"))
	exec := &fakeExecutor{out: "poc"}
	r.Executor = exec

	plan := execPlan()
	plan.Entries[0].Container = "exporter"

	_, err := r.Resolve(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, "exporter", exec.gotContainer)
}

// The pod not being up yet is the common case, and failing on the spot turns
// it into a wasted Job attempt. Waiting inside the run costs a few idle
// seconds and no second pod.
func TestResolve_ExecWaitsForAPodToBecomeReady(t *testing.T) {
	r := newResolver(pod("postgres-0", false, map[string]string{"app": "postgres"}))
	r.Executor = &fakeExecutor{out: "poc"}
	r.PodWait = 5 * time.Second
	r.PodPoll = 10 * time.Millisecond

	// Buffered and checked below, because a failed fixture would otherwise
	// look exactly like a wait that never noticed.
	flipped := make(chan error, 1)

	go func() {
		time.Sleep(50 * time.Millisecond)

		ctx := context.Background()
		key := types.NamespacedName{Name: "postgres-0", Namespace: testNamespace}

		current := &corev1.Pod{}
		if err := r.Client.Get(ctx, key, current); err != nil {
			flipped <- err

			return
		}

		// Status().Update, not Update. The fake client lists v1/Pod in
		// inTreeResourcesWithStatus, so a plain Update drops the change and
		// the pod stays not ready forever.
		current.Status.Conditions[0].Status = corev1.ConditionTrue
		flipped <- r.Client.Status().Update(ctx, current)
	}()

	got, err := r.Resolve(context.Background(), execPlan())

	require.NoError(t, <-flipped, "the fixture has to actually become ready")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"DATABASE": "poc"}, got)
}

// The wait is bounded. A service that never comes up has to fail the run so
// the Job's own retry, and then the controller's, get their turn.
func TestResolve_ExecGivesUpAfterTheWait(t *testing.T) {
	r := newResolver(pod("postgres-0", false, map[string]string{"app": "postgres"}))
	r.Executor = &fakeExecutor{out: "poc"}
	r.PodWait = 100 * time.Millisecond
	r.PodPoll = 10 * time.Millisecond

	start := time.Now()

	_, err := r.Resolve(context.Background(), execPlan())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no ready pod")
	assert.GreaterOrEqual(t, time.Since(start), r.PodWait, "it has to actually wait before giving up")
}

// A workload that puts a sidecar first can say which container it means, the
// same way it tells kubectl exec.
func TestResolve_ExecHonoursTheDefaultContainerAnnotation(t *testing.T) {
	target := pod("postgres-0", true, map[string]string{"app": "postgres"}, "exporter", "postgres")
	target.Annotations = map[string]string{defaultContainerAnnotation: "postgres"}
	r := newResolver(target)
	exec := &fakeExecutor{out: "poc"}
	r.Executor = exec

	plan := execPlan()
	plan.Entries[0].Container = ""

	_, err := r.Resolve(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, "postgres", exec.gotContainer)
}

// The Arcanum is more specific than the pod's own hint, so it wins.
func TestResolve_ExecPrefersTheNamedContainerOverTheAnnotation(t *testing.T) {
	target := pod("postgres-0", true, map[string]string{"app": "postgres"}, "postgres", "exporter")
	target.Annotations = map[string]string{defaultContainerAnnotation: "postgres"}
	r := newResolver(target)
	exec := &fakeExecutor{out: "poc"}
	r.Executor = exec

	plan := execPlan()
	plan.Entries[0].Container = "exporter"

	_, err := r.Resolve(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, "exporter", exec.gotContainer)
}

// An annotation left behind by a container that no longer exists would
// otherwise fail the exec with an API error naming something nobody wrote in
// the Arcanum.
func TestResolve_ExecIgnoresADefaultContainerThatIsNotThere(t *testing.T) {
	target := pod("postgres-0", true, map[string]string{"app": "postgres"}, "postgres", "exporter")
	target.Annotations = map[string]string{defaultContainerAnnotation: "long-gone"}
	r := newResolver(target)
	exec := &fakeExecutor{out: "poc"}
	r.Executor = exec

	plan := execPlan()
	plan.Entries[0].Container = ""

	_, err := r.Resolve(context.Background(), plan)

	require.NoError(t, err)
	assert.Equal(t, "postgres", exec.gotContainer)
}

// The Arcanum is more specific than the flag the Job was started with, so an
// entry that names its own wait gets it.
func TestResolve_ExecEntryWaitOverridesTheResolverDefault(t *testing.T) {
	r := newResolver(pod("postgres-0", false, map[string]string{"app": "postgres"}))
	r.Executor = &fakeExecutor{out: "poc"}
	r.PodWait = time.Hour
	r.PodPoll = 10 * time.Millisecond

	plan := execPlan()
	plan.Entries[0].PodWait = &metav1.Duration{Duration: 100 * time.Millisecond}

	start := time.Now()

	_, err := r.Resolve(context.Background(), plan)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no ready pod")
	assert.Less(t, time.Since(start), time.Minute,
		"the entry asked for 100ms, so the resolver's hour must not apply")
}
