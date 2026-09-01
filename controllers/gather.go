package controllers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	batchv1ac "k8s.io/client-go/applyconfigurations/batch/v1"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"

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

	gatherSuffix         = "gather"
	gatherResultSuffix   = "gather-result"
	gatheredSecretSuffix = "gathered"
)

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
			WithBackoffLimit(0).
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
