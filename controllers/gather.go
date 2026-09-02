package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	batchv1ac "k8s.io/client-go/applyconfigurations/batch/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	arcanav1 "github.com/helmetica-framework/custos/api/v1"
	"github.com/helmetica-framework/custos/gather"
)

const (
	// gatherServiceAccount is the ServiceAccount chrysopoeia already creates
	// in every instance namespace, bound to the built-in admin ClusterRole.
	// Borrowing it is what keeps custos itself off pods/exec.
	gatherServiceAccount = "instance-admin"

	// coreAPIVersion is what an objectRef with no apiVersion means. Most
	// references are to a Secret or a ConfigMap.
	coreAPIVersion = "v1"

	gatherPlanMountPath  = "/etc/custos"
	gatherPlanFileName   = "plan.json"
	gatherPlanVolumeName = "plan"
	gatherContainerName  = "gather"

	// dnsLabelMax is what a Kubernetes object name that has to be a DNS label
	// gets to spend.
	dnsLabelMax = 63

	// refreshAnnotation re-runs a gather on demand. Its value is hashed into
	// the Job name, so changing it is what makes the Job a different Job.
	refreshAnnotation = "custos.helmetica.io/refresh"

	gatherSuffix         = "gather"
	gatherResultSuffix   = "gather-result"
	gatheredSecretSuffix = "gathered"

	// gatherBackoffLimit is how many times Kubernetes reruns a gather before
	// the Job gives up. Its own schedule is 10s, 20s, 40s, 80s, 160s and 320s,
	// so six attempts buy about ten minutes of patience for a service that is
	// still starting.
	//
	// This is only safe because an exec is required to be idempotent. A command
	// that mints a value per run would mint seven of them here.
	gatherBackoffLimit = 6

	// gatherRetryInterval is how long custos waits after a Job gives up before
	// deleting it, which is what frees the name for the next one.
	gatherRetryInterval = 5 * time.Minute

	// gatherRetryPoll is the short requeue after a delete, in case the watch on
	// the Job does not wake us. It normally does.
	gatherRetryPoll = 5 * time.Second
)

// jobGaveUp reports whether the Job stopped retrying.
func jobGaveUp(job *batchv1.Job) bool {
	_, gaveUp := jobConditionTime(job, batchv1.JobFailed)
	return gaveUp
}

func jobConditionTime(job *batchv1.Job, condition batchv1.JobConditionType) (time.Time, bool) {
	for _, c := range job.Status.Conditions {
		if c.Type == condition && c.Status == corev1.ConditionTrue {
			return c.LastTransitionTime.Time, true
		}
	}

	return time.Time{}, false
}

// gatherSettledAt is when the Job stopped changing, which is what the retry
// interval is measured from: the JobFailed condition's transition time, or the
// completion time for a Job that finished without leaving values.
func gatherSettledAt(job *batchv1.Job) (time.Time, bool) {
	if failedAt, ok := jobConditionTime(job, batchv1.JobFailed); ok {
		return failedAt, true
	}

	if completedAt, ok := jobConditionTime(job, batchv1.JobComplete); ok {
		return completedAt, true
	}

	if job.Status.CompletionTime != nil {
		return job.Status.CompletionTime.Time, true
	}

	return time.Time{}, false
}

// retryGather deletes a Job that has settled without producing values, so the
// next pass creates it again under the same name.
func (r *ArcanumManager) retryGather(ctx context.Context, job *batchv1.Job, want *phase) error {
	if !job.GetDeletionTimestamp().IsZero() {
		want.RequeueAfter = gatherRetryPoll

		return nil
	}

	settledAt, ok := gatherSettledAt(job)
	if !ok {
		want.RequeueAfter = gatherRetryInterval

		return nil
	}

	if wait := time.Until(settledAt.Add(gatherRetryInterval)); wait > 0 {
		want.RequeueAfter = wait

		return nil
	}

	err := r.Delete(ctx, job,
		client.PropagationPolicy(metav1.DeletePropagationBackground),
		client.Preconditions{UID: new(job.GetUID())})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting gather job %s: %w", job.GetName(), err)
	}

	want.RequeueAfter = gatherRetryPoll

	return nil
}

// needsGather reports whether any entry has to be resolved inside the
// namespace. A mapping without one settles in a single reconcile, with no Job
// and no waiting.
//
// True for SourceObjectRef and SourceExec. Everything else custos resolves on
// its own.
func needsGather(mapping map[string]arcanav1.ValueSource) bool {
	for _, v := range mapping {
		switch v.Source {
		case arcanav1.SourceExec, arcanav1.SourceObjectRef:
			return true
		default:
			continue
		}
	}

	return false
}

// buildPlan turns the gatherable entries into a plan, rendering the metadata
// templates so the Job receives only literals.
//
// The plan's Entry.Key is the mapping key, which is the name the value ends
// up under in the target Secret. A ValueSource's own Key is a different
// thing, the entry to read out of the referenced Secret or ConfigMap, and it
// becomes Entry.DataKey.
func buildPlan(mapping map[string]arcanav1.ValueSource, md metadata) (gather.Plan, error) {
	plan := gather.Plan{Namespace: md.InstanceNamespace}

	for _, key := range slices.Sorted(maps.Keys(mapping)) {
		source := mapping[key]

		switch source.Source {
		case arcanav1.SourceObjectRef:
			name, err := renderMetadata(source.Name, md)
			if err != nil {
				return gather.Plan{}, fmt.Errorf("rendering name for %s: %w", key, err)
			}

			apiVersion := source.APIVersion
			if apiVersion == "" {
				apiVersion = coreAPIVersion
			}

			plan.Entries = append(plan.Entries, gather.Entry{
				Key:        key,
				Kind:       gather.EntryObjectRef,
				APIVersion: apiVersion,
				ObjectKind: source.Kind,
				Name:       name,
				DataKey:    source.Key,
				Path:       source.Path,
			})

		case arcanav1.SourceExec:
			var command []string

			for i, arg := range source.Command {
				rendered, err := renderMetadata(arg, md)
				if err != nil {
					return gather.Plan{}, fmt.Errorf("rendering command argument %d for %s: %w", i, key, err)
				}

				command = append(command, rendered)
			}

			plan.Entries = append(plan.Entries, gather.Entry{
				Key:         key,
				Kind:        gather.EntryExec,
				PodSelector: source.PodSelector,
				Container:   source.Container,
				Command:     command,
				PodWait:     source.PodWait,
			})
		default:
			// Every other source stays in the controller. Sending it to the
			// Job would put a value in a ConfigMap that never needed to
			// leave.
		}
	}

	return plan, nil
}

// gatherHash identifies a plan plus the refresh token. It names the Job, and
// status.gatheredHash records the one whose results are in hand.
//
// The token is hashed in rather than appended, because the result goes into a
// name with only 63 characters to spend and a refresh token is a timestamp.
func gatherHash(plan gather.Plan, refreshToken string) string {
	h := sha256.New()

	fmt.Fprintln(h, plan.Hash(), refreshToken)

	return hex.EncodeToString(h.Sum(nil))[:8]
}

// gatherJobName carries the plan hash, so a changed plan is a new Job rather
// than an edit to a spec that cannot be edited.
func gatherJobName(arcanumName, hash string) string {
	return derivedName(arcanumName, gatherSuffix+"-"+hash)
}

// gatherPlanConfigMapName holds the plan the Job reads.
func gatherPlanConfigMapName(arcanumName string) string {
	return derivedName(arcanumName, gatherSuffix)
}

// gatherResultConfigMapName is where the Job reports what it resolved.
func gatherResultConfigMapName(arcanumName string) string {
	return derivedName(arcanumName, gatherResultSuffix)
}

// gatheredSecretName is where the Job leaves the values themselves.
func gatheredSecretName(arcanumName string) string {
	return derivedName(arcanumName, gatheredSecretSuffix)
}

// derivedName joins the Arcanum's name to a suffix saying what the object is
// for, and keeps the result inside the 63 characters a DNS label allows.
//
// An Arcanum name can already be 63 characters, so any suffix can push it
// over and the API server rejects the name rather than shortening it. The
// room comes out of the Arcanum's part, since the suffix is the only thing
// saying which of the four objects this is.
//
// The trailing hash keeps two Arcana that share a long prefix off one name.
// In a single namespace that would be two services overwriting each other's
// credentials in one Secret on every reconcile.
func derivedName(arcanumName, suffix string) string {
	name := arcanumName + "-" + suffix
	if len(name) <= dnsLabelMax {
		return name
	}

	sum := sha256.Sum256([]byte(name))
	tail := fmt.Sprintf("-%x-%s", sum[:4], suffix)

	prefix := strings.TrimRight(arcanumName[:dnsLabelMax-len(tail)], "-")

	return prefix + tail
}

// gatherJob builds the Job that resolves the plan from inside the instance
// namespace. Its spec is immutable once created, which is why the hash is in
// the name: a changed plan is a different Job, not an edit to a running one.
//
// The Job runs as instance-admin, the ServiceAccount chrysopoeia already
// binds to the admin ClusterRole in that one namespace. That is the whole
// point of the two stage design, since it keeps custos itself off pods/exec
// and off read access to arbitrary kinds.
func (r *ArcanumManager) gatherJob(
	arcanum *arcanav1.Arcanum,
	plan gather.Plan,
	hash string,
) *batchv1ac.JobApplyConfiguration {
	name := arcanum.GetName()
	labels := ownershipLabels(arcanum)

	container := corev1ac.Container().
		WithName(gatherContainerName).
		WithImage(r.GatherImage).
		WithArgs(
			"gather",
			"--plan", path.Join(gatherPlanMountPath, gatherPlanFileName),
			"--secret", gatheredSecretName(name),
			"--result", gatherResultConfigMapName(name),
			"--arcanum", name,
			"--arcanum-uid", string(arcanum.GetUID()),
		).
		WithVolumeMounts(corev1ac.VolumeMount().
			WithName(gatherPlanVolumeName).
			WithMountPath(gatherPlanMountPath).
			WithReadOnly(true)).
		WithSecurityContext(corev1ac.SecurityContext().
			WithAllowPrivilegeEscalation(false).
			WithReadOnlyRootFilesystem(true).
			WithRunAsNonRoot(true).
			WithCapabilities(corev1ac.Capabilities().WithDrop(corev1.Capability("ALL"))))

	volume := corev1ac.Volume().
		WithName(gatherPlanVolumeName).
		WithConfigMap(corev1ac.ConfigMapVolumeSource().
			WithName(gatherPlanConfigMapName(name)))

	return batchv1ac.Job(gatherJobName(name, hash), arcanum.GetNamespace()).
		WithLabels(labels).
		WithOwnerReferences(controllerRef(arcanum)).
		WithSpec(batchv1ac.JobSpec().
			WithBackoffLimit(gatherBackoffLimit).
			WithTemplate(corev1ac.PodTemplateSpec().
				WithLabels(labels).
				WithSpec(corev1ac.PodSpec().
					WithServiceAccountName(gatherServiceAccount).
					WithRestartPolicy(corev1.RestartPolicyNever).
					WithSecurityContext(corev1ac.PodSecurityContext().
						WithRunAsNonRoot(true).
						WithSeccompProfile(corev1ac.SeccompProfile().
							WithType(corev1.SeccompProfileTypeRuntimeDefault))).
					WithContainers(container).
					WithVolumes(volume))))
}

// gatherPlanConfigMap holds the plan the Job reads off its volume. The key is
// the file name the container mounts, so the two have to agree.
func gatherPlanConfigMap(arcanum *arcanav1.Arcanum, plan gather.Plan) (*corev1ac.ConfigMapApplyConfiguration, error) {
	raw, err := plan.Marshal()
	if err != nil {
		return nil, fmt.Errorf("marshalling plan: %w", err)
	}

	return corev1ac.ConfigMap(gatherPlanConfigMapName(arcanum.GetName()), arcanum.GetNamespace()).
		WithLabels(ownershipLabels(arcanum)).
		WithOwnerReferences(controllerRef(arcanum)).
		WithData(map[string]string{gatherPlanFileName: string(raw)}), nil
}

// startGather writes the plan and then the Job that reads it. The order
// matters: a Job scheduled before its ConfigMap exists sits in
// ContainerCreating until the kubelet retries.
func (r *ArcanumManager) startGather(ctx context.Context, arcanum *arcanav1.Arcanum, plan gather.Plan, hash string) error {
	cm, err := gatherPlanConfigMap(arcanum, plan)
	if err != nil {
		return err
	}

	if err := r.Apply(ctx, cm, fieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("applying plan ConfigMap: %w", err)
	}

	if err := r.Apply(ctx, r.gatherJob(arcanum, plan, hash), fieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("applying gather Job: %w", err)
	}

	return nil
}

// deleteStaleGatherJobs removes this Arcanum's gather Jobs other than keep.
//
// A Job's spec is immutable, so a changed plan is a new Job under a new name
// rather than an edit. Without this every spec change would leave another one
// behind until the namespace is full of them.
func (r *ArcanumManager) deleteStaleGatherJobs(ctx context.Context, arcanum *arcanav1.Arcanum, keep string) error {
	jobs := &batchv1.JobList{}

	err := r.List(ctx, jobs,
		client.InNamespace(arcanum.GetNamespace()),
		client.MatchingLabels{arcanumNameLabel: arcanum.GetName()})
	if err != nil {
		return fmt.Errorf("listing gather jobs: %w", err)
	}

	for _, job := range jobs.Items {
		if job.GetName() == keep {
			continue
		}

		// Background propagation, or the Job goes and its pods stay.
		err := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground))
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting gather job %s: %w", job.GetName(), err)
		}
	}

	return nil
}

// gatheredValues reads what the Job left behind. The second return says
// whether the Secret was there at all, which is a state and not an error: it
// is how a gather that has not run yet looks.
func (r *ArcanumManager) gatheredValues(ctx context.Context, arcanum *arcanav1.Arcanum) (map[string]string, bool, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{
		Name:      gatheredSecretName(arcanum.GetName()),
		Namespace: arcanum.GetNamespace(),
	}

	err := r.Get(ctx, key, secret)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("getting gathered secret: %w", err)
	}

	values := make(map[string]string, len(secret.Data))
	for k, v := range secret.Data {
		values[k] = string(v)
	}

	return values, true, nil
}

// gatherResultMessage is what the Job reported about itself, or fallback when
// it reported nothing. Reading this rather than the Job's log is what keeps
// custos off pods/log, which would be a cluster-wide grant over output that
// routinely contains secrets.
//
// The fallback belongs to the caller because the two callers are in different
// situations: one has a Job that gave up, the other a Job that claimed success
// and left nothing behind.
func (r *ArcanumManager) gatherResultMessage(ctx context.Context, arcanum *arcanav1.Arcanum, fallback string) string {
	cm := &corev1.ConfigMap{}
	key := client.ObjectKey{
		Name:      gatherResultConfigMapName(arcanum.GetName()),
		Namespace: arcanum.GetNamespace(),
	}

	if err := r.Get(ctx, key, cm); err != nil {
		return fallback
	}

	if message := cm.Data[gather.ResultMessageKey]; message != "" {
		return message
	}

	return fallback
}
