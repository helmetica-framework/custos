package cmd

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"github.com/spf13/cobra"
	"go.uber.org/multierr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	//+kubebuilder:scaffold:imports
	arcanav1 "github.com/helmetica-framework/custos/api/v1"
	"github.com/helmetica-framework/custos/controllers"
	"github.com/helmetica-framework/custos/gather"
)

var (
	metricsAddr          string
	enableLeaderElection bool
	probeAddr            string
	gatherImage          string
	zapOpts              = zap.Options{
		Development: true,
	}
)

func init() {
	RootCmd.AddCommand(controllerCmd)

	zapFlagSet := flag.NewFlagSet("zap", flag.ExitOnError)
	zapOpts.BindFlags(zapFlagSet)
	controllerCmd.Flags().AddGoFlagSet(zapFlagSet)

	controllerCmd.Flags().StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	controllerCmd.Flags().StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	controllerCmd.Flags().StringVar(&gatherImage, "gather-image", "",
		"Image the gather Jobs run. It is the same binary as the controller, invoked as 'custos gather'.")
	controllerCmd.Flags().BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")

	defaultNamespace := "default"
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		defaultNamespace = ns
	}
	controllerCmd.Flags().String("controller-namespace", defaultNamespace, "The namespace the controller runs in.")

	controllerCmd.Flags().Bool("metrics-secure", true, "If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	controllerCmd.Flags().String("metrics-cert-path", "", "The directory that contains the metrics server certificate.")
	controllerCmd.Flags().String("metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	controllerCmd.Flags().String("metrics-cert-key", "tls.key", "The name of the metrics server key file.")
}

var controllerCmd = &cobra.Command{
	Use:   "controller",
	Short: "Starts the controller manager",
	Long:  "Starts the controller manager",
	RunE:  runController,
}

func newScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	utilruntime.Must(arcanav1.AddToScheme(scheme))
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme
	return scheme
}

func runController(cmd *cobra.Command, _ []string) error {
	controllerNamespace, cnerr := cmd.Flags().GetString("controller-namespace")

	secureMetrics, smerr := cmd.Flags().GetBool("metrics-secure")
	metricsCertPath, mcperr := cmd.Flags().GetString("metrics-cert-path")
	metricsCertName, mcnerr := cmd.Flags().GetString("metrics-cert-name")
	metricsCertKey, mckerr := cmd.Flags().GetString("metrics-cert-key")

	if err := multierr.Combine(cnerr, mcperr, mcnerr, mckerr, smerr); err != nil {
		return fmt.Errorf("failed to get flags: %w", err)
	}

	// Checked here rather than left to the Job, because a Job with no image
	// only fails the first time somebody needs a credential gathered, which
	// can be long after the deployment looked healthy.
	if gatherImage == "" {
		return fmt.Errorf("--gather-image is required")
	}

	cmd.Println(
		"Starting the controller manager",
		"controller-namespace", controllerNamespace,
	)

	scheme := newScheme()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&zapOpts)))

	var metricsCertWatcher *certwatcher.CertWatcher

	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       []func(*tls.Config){},
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if len(metricsCertPath) > 0 {
		cmd.Println("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(metricsCertPath, metricsCertName),
			filepath.Join(metricsCertPath, metricsCertKey),
		)
		if err != nil {
			cmd.Println("failed to initialize metrics certificate watcher", "error", err)
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	arcanumLabelExists, err := labels.NewRequirement(gather.ArcanumNameLabel, selection.Exists, nil)
	if err != nil {
		return fmt.Errorf("building the secret cache selector: %w", err)
	}

	restConf := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(restConf, ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "custos.arcana.helmetica.io",

		LeaderElectionReleaseOnCancel: true,
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				// Caching every Secret in the cluster is a lot of memory for
				// nothing. Custos only ever watches Secrets it stamped.
				&corev1.Secret{}: {
					Label: labels.SelectorFromSet(labels.Set{}).Add(*arcanumLabelExists),
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("unable to start manager: %w", err)
	}

	lifetimeCtx := cmd.Context()

	am := controllers.ArcanumManager{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Recorder:    mgr.GetEventRecorder("arcanum-controller"),
		Log:         mgr.GetLogger().WithName("arcanum-controller"),
		APIReader:   mgr.GetAPIReader(),
		GatherImage: gatherImage,
	}

	if err := am.SetupWithManager("arcanum", mgr); err != nil {
		return fmt.Errorf("unable to create Arcanum controller: %w", err)
	}

	//+kubebuilder:scaffold:builder

	if metricsCertWatcher != nil {
		cmd.Println("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			cmd.Println("unable to add metrics certificate watcher to manager", err)
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up health check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up ready check: %w", err)
	}

	cmd.Println("Starting the controller manager")
	if err := mgr.Start(lifetimeCtx); err != nil {
		return fmt.Errorf("problem running manager: %w", err)
	}

	return nil
}
