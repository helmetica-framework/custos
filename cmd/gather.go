package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/types"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	custosv1 "github.com/helmetica-framework/custos/api/v1"
	"github.com/helmetica-framework/custos/gather"
)

var (
	podWait              time.Duration
	planPath             string
	secretName           string
	resultName           string
	credentialConfigName string
	credentialConfigUID  string
)

const (
	defaultPlanPath = "/etc/custos/plan.json"

	// defaultPodWait absorbs the ordinary case, a helm install that started
	// the gather before the service it execs into was up. The gather pod
	// spends a few seconds scheduling before it even looks, so half a minute
	// covers most of it without eating much of the Job's own budget.
	defaultPodWait = 60 * time.Second
)

func init() {
	RootCmd.AddCommand(gatherCmd)

	gatherCmd.Flags().StringVar(&planPath, "plan", defaultPlanPath, "Path to the plan file the controller mounted.")
	gatherCmd.Flags().StringVar(&secretName, "secret", "", "Name of the Secret to write the gathered values to.")
	gatherCmd.Flags().StringVar(&resultName, "result", "", "Name of the ConfigMap to record the outcome in.")
	gatherCmd.Flags().StringVar(&credentialConfigName, "credentialconfig", "", "Name of the CredentialConfig this gather belongs to.")
	gatherCmd.Flags().StringVar(&credentialConfigUID, "credentialconfig-uid", "", "UID of the CredentialConfig this gather belongs to.")
	gatherCmd.Flags().DurationVar(&podWait, "pod-wait", defaultPodWait,
		"How long to wait for an exec's pod to become ready before failing the run.")
}

var gatherCmd = &cobra.Command{
	Use:   "gather",
	Short: "Resolves a gather plan from inside an instance namespace",
	Long: "Resolves a gather plan from inside an instance namespace. " +
		"Run as a Job by the controller, never by hand.",
	RunE: runGather,
}

// runGather resolves the plan and leaves the values in a Secret beside the
// service, for the controller to render and forward.
//
// It runs as instance-admin, which can read every resource in that one
// namespace and exec into its pods. That is why this is a separate process:
// the controller itself holds none of those permissions.
//
// The result ConfigMap is written either way and the resolve error is
// returned afterwards, so the exit code and the ConfigMap never disagree.
func runGather(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	rawPlan, err := os.ReadFile(planPath)
	if err != nil {
		return fmt.Errorf("reading plan: %w", err)
	}

	plan, err := gather.Unmarshal(rawPlan)
	if err != nil {
		return fmt.Errorf("unmarshalling plan: %w", err)
	}

	slog.Info("gather starting",
		"namespace", plan.Namespace,
		"entries", len(plan.Entries),
		"secret", secretName,
		"podWait", podWait)

	restConf := ctrl.GetConfigOrDie()

	c, err := client.New(restConf, client.Options{Scheme: newScheme()})
	if err != nil {
		return fmt.Errorf("creating k8s client: %w", err)
	}

	resolver := gather.Resolver{
		Client:   c,
		Executor: &gather.PodExecutor{Config: restConf},
		PodWait:  podWait,
	}

	// A half resolved gather must never reach the render stage, so a failure
	// here leaves the Secret alone rather than writing what it did resolve.
	values, runErr := resolver.Resolve(ctx, plan)
	if runErr == nil {
		runErr = applyGathered(ctx, c, plan.Namespace, values)
	}

	if err := gather.WriteResult(ctx, c, plan.Namespace, resultName, gatheredLabels(), credentialConfigOwner(), runErr); err != nil {
		return errors.Join(runErr, err)
	}

	if runErr != nil {
		// Logged as well as recorded, because the ConfigMap is the
		// controller's copy and this one is what survives a process that
		// cannot write it.
		slog.Error("gather failed", "error", runErr)

		return runErr
	}

	slog.Info("gather finished")

	return nil
}

// applyGathered writes the resolved values into the Secret the controller
// reads back.
//
// The credential config-name label is what makes that Secret visible at all. The
// manager's cache is restricted to Secrets carrying it, so an unlabelled one
// reads as NotFound however often the controller looks.
func applyGathered(ctx context.Context, c client.Client, namespace string, values map[string]string) error {
	dvalues := make(map[string][]byte, len(values))
	for k, v := range values {
		dvalues[k] = []byte(v)
	}

	secret := corev1ac.Secret(secretName, namespace).
		WithLabels(gatheredLabels()).
		WithData(dvalues)

	if owner := credentialConfigOwner(); owner != nil {
		secret.WithOwnerReferences(owner)
	}

	slog.Info("writing gathered values", "secret", secretName, "keys", len(values))

	if err := c.Apply(ctx, secret, gather.FieldOwner, client.ForceOwnership); err != nil {
		return fmt.Errorf("applying gathered Secret %q: %w", secretName, err)
	}

	return nil
}

// gatheredLabels is what makes the Job's objects visible to the manager's
// cache.
func gatheredLabels() map[string]string {
	return map[string]string{gather.CredentialConfigNameLabel: credentialConfigName}
}

// credentialConfigOwner ties what the Job writes to the CredentialConfig, so deleting the
// CredentialConfig takes the gathered Secret and the result ConfigMap with it.
//
// blockOwnerDeletion is deliberately left off. Setting it needs update on the
// owner's finalizers subresource, which the admin ClusterRole instance-admin
// is bound to does not cover for a custom kind, and the API server would
// reject the whole write rather than just that field.
func credentialConfigOwner() *metav1ac.OwnerReferenceApplyConfiguration {
	if credentialConfigName == "" || credentialConfigUID == "" {
		return nil
	}

	return metav1ac.OwnerReference().
		WithAPIVersion(custosv1.GroupVersion.String()).
		WithKind("CredentialConfig").
		WithName(credentialConfigName).
		WithUID(types.UID(credentialConfigUID)).
		WithController(true)
}
