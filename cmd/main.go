/*
Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"os"
	goruntime "runtime"
	"time"

	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	logsv1 "k8s.io/component-base/logs/api/v1"
	_ "k8s.io/component-base/logs/json/register"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	bootstrapv1 "sigs.k8s.io/cluster-api/bootstrap/kubeadm/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/remote"
	controlplanev1 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1beta1"
	runtimecatalog "sigs.k8s.io/cluster-api/exp/runtime/catalog"
	runtimehooksv1 "sigs.k8s.io/cluster-api/exp/runtime/hooks/api/v1alpha1"
	"sigs.k8s.io/cluster-api/exp/runtime/server"
	"sigs.k8s.io/cluster-api/util/flags"
	"sigs.k8s.io/cluster-api/version"

	updatev1beta1 "github.com/microsoft/cluster-api-inplace-updater/api/v1beta1"
	"github.com/microsoft/cluster-api-inplace-updater/internal/controller"
	"github.com/microsoft/cluster-api-inplace-updater/internal/handlers/lifecycle"
	//+kubebuilder:scaffold:imports
)

var (
	// scheme is a Kubernetes runtime scheme containing all the information about API types.
	// NOTE: it is not mandatory to use scheme in custom RuntimeExtension, but working with typed API objects makes code
	// easier to read and less error-prone than using unstructured or working with raw json/yaml.
	scheme = runtime.NewScheme()
	// Creates a logger to be used during the main func using controller runtime utilities
	// NOTE: it is not mandatory to use controller runtime utilities in custom RuntimeExtension, but it is recommended
	// because it makes log from those components similar to log from controllers.
	setupLog       = ctrl.Log.WithName("setup")
	controllerName = "cluster-api-inplace-updater-manager"

	// catalog contains all information about RuntimeHooks.
	catalog = runtimecatalog.New()

	// flags.
	enableLeaderElection        bool
	leaderElectionLeaseDuration time.Duration
	leaderElectionRenewDeadline time.Duration
	leaderElectionRetryPeriod   time.Duration
	profilerAddress             string
	enableContentionProfiling   bool
	syncPeriod                  time.Duration
	restConfigQPS               float32
	restConfigBurst             int
	webhookPort                 int
	webhookCertDir              string
	healthAddr                  string
	tlsOptions                  = flags.TLSOptions{}
	diagnosticsOptions          = flags.DiagnosticsOptions{}
	logOptions                  = logs.NewOptions()
)

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = apiextensionsv1.AddToScheme(scheme)

	_ = clusterv1.AddToScheme(scheme)
	_ = bootstrapv1.AddToScheme(scheme)
	_ = controlplanev1.AddToScheme(scheme)

	utilruntime.Must(updatev1beta1.AddToScheme(scheme))
	//+kubebuilder:scaffold:scheme

	// Register the RuntimeHook types into the catalog.
	_ = runtimehooksv1.AddToCatalog(catalog)
}

// InitFlags initializes the flags.
func InitFlags(fs *pflag.FlagSet) {
	// Initialize logs flags using Kubernetes component-base machinery.
	// NOTE: it is not mandatory to use Kubernetes component-base machinery in custom RuntimeExtension, but it is
	// recommended because it helps in ensuring consistency across different components in the cluster.
	logsv1.AddFlags(logOptions, fs)

	fs.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")

	fs.DurationVar(&leaderElectionLeaseDuration, "leader-elect-lease-duration", 15*time.Second,
		"Interval at which non-leader candidates will wait to force acquire leadership (duration string)")

	fs.DurationVar(&leaderElectionRenewDeadline, "leader-elect-renew-deadline", 10*time.Second,
		"Duration that the leading controller manager will retry refreshing leadership before giving up (duration string)")

	fs.DurationVar(&leaderElectionRetryPeriod, "leader-elect-retry-period", 2*time.Second,
		"Duration the LeaderElector clients should wait between tries of actions (duration string)")

	fs.StringVar(&profilerAddress, "profiler-address", "",
		"Bind address to expose the pprof profiler (e.g. localhost:6060)")

	fs.BoolVar(&enableContentionProfiling, "contention-profiling", false,
		"Enable block profiling")

	fs.DurationVar(&syncPeriod, "sync-period", 10*time.Minute,
		"The minimum interval at which watched resources are reconciled (e.g. 15m)")

	fs.Float32Var(&restConfigQPS, "kube-api-qps", 20,
		"Maximum queries per second from the controller client to the Kubernetes API server. Defaults to 20")

	fs.IntVar(&restConfigBurst, "kube-api-burst", 30,
		"Maximum number of queries that should be allowed in one burst from the controller client to the Kubernetes API server. Default 30")

	fs.IntVar(&webhookPort, "webhook-port", 9443,
		"Webhook Server port")

	fs.StringVar(&webhookCertDir, "webhook-cert-dir", "/tmp/k8s-webhook-server/serving-certs/",
		"Webhook cert dir, only used when webhook-port is specified.")

	fs.StringVar(&healthAddr, "health-addr", ":9440",
		"The address the health endpoint binds to.")

	flags.AddDiagnosticsOptions(fs, &diagnosticsOptions)
	flags.AddTLSOptions(fs, &tlsOptions)

	// NOTE: it is not mandatory to use the same flag names in all RuntimeExtension, but it is recommended when
	// addressing common concerns like profiler-address, webhook-port, webhook-cert-dir etc. because it helps in ensuring
	// consistency across different components in the cluster.
}

// Add RBAC for the authorized diagnostics endpoint.
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create

func main() {
	// Initialize and parse command line flags.
	InitFlags(pflag.CommandLine)
	pflag.CommandLine.SetNormalizeFunc(cliflag.WordSepNormalizeFunc)
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	// Set log level 2 as default.
	if err := pflag.CommandLine.Set("v", "2"); err != nil {
		setupLog.Error(err, "failed to set default log level")
		os.Exit(1)
	}
	pflag.Parse()

	// Validates logs flags using Kubernetes component-base machinery and apply them
	// so klog will automatically use the right logger.
	// NOTE: klog is the log of choice of component-base machinery.
	if err := logsv1.ValidateAndApply(logOptions, nil); err != nil {
		setupLog.Error(err, "unable to start extension")
		os.Exit(1)
	}

	// Add the klog logger in the context.
	// NOTE: it is not mandatory to use contextual logging in custom RuntimeExtension, but it is recommended
	// because it allows to use a log stored in the context across the entire chain of calls (without
	// requiring an addition log parameter in all the functions).
	ctrl.SetLogger(klog.Background())

	restConfig := ctrl.GetConfigOrDie()
	restConfig.QPS = restConfigQPS
	restConfig.Burst = restConfigBurst
	restConfig.UserAgent = remote.DefaultClusterAPIUserAgent(controllerName)

	tlsOptionOverrides, err := flags.GetTLSOptionOverrideFuncs(tlsOptions)
	if err != nil {
		setupLog.Error(err, "unable to add TLS settings to the webhook server")
		os.Exit(1)
	}

	diagnosticsOpts := flags.GetDiagnosticsOptions(diagnosticsOptions)

	if enableContentionProfiling {
		goruntime.SetBlockProfileRate(1)
	}

	// Create an HTTP server for serving Runtime Extensions.
	runtimeExtensionWebhookServer, err := server.New(server.Options{
		Port:    webhookPort,
		CertDir: webhookCertDir,
		TLSOpts: tlsOptionOverrides,
		Catalog: catalog,
	})
	if err != nil {
		setupLog.Error(err, "error creating runtime extension webhook server")
		os.Exit(1)
	}

	ctrlOptions := ctrl.Options{
		Scheme:                     scheme,
		LeaderElection:             enableLeaderElection,
		LeaderElectionID:           "controller-leader-election-inplace-updater-extension",
		LeaseDuration:              &leaderElectionLeaseDuration,
		RenewDeadline:              &leaderElectionRenewDeadline,
		RetryPeriod:                &leaderElectionRetryPeriod,
		LeaderElectionResourceLock: resourcelock.LeasesResourceLock,
		HealthProbeBindAddress:     healthAddr,
		PprofBindAddress:           profilerAddress,
		Metrics:                    diagnosticsOpts,
		Cache: cache.Options{
			SyncPeriod: &syncPeriod,
		},
		Client: client.Options{
			Cache: &client.CacheOptions{
				DisableFor: []client.Object{
					&corev1.ConfigMap{},
					&corev1.Secret{},
				},
			},
		},
		WebhookServer: runtimeExtensionWebhookServer,
	}

	// Start the manager
	mgr, err := ctrl.NewManager(restConfig, ctrlOptions)
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// Set up a context listening for SIGINT.
	ctx := ctrl.SetupSignalHandler()

	// Setup Runtime Extensions.
	setupLifecycleHookHandlers(mgr, runtimeExtensionWebhookServer)

	// Setup checks and reconcilers.
	setupChecks(mgr)
	setupReconcilers(mgr)

	setupLog.Info("Starting manager", "version", version.Get().String())
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// setupLifecycleHookHandlers sets up Lifecycle Hooks.
func setupLifecycleHookHandlers(mgr ctrl.Manager, runtimeExtensionWebhookServer *server.Server) {
	// Create the ExtensionHandlers for the lifecycle hooks
	// NOTE: it is not mandatory to group all the ExtensionHandlers using a struct, what is important
	// is to have HandlerFunc with the signature defined in sigs.k8s.io/cluster-api/exp/runtime/hooks/api/v1alpha1.
	lifecycleExtensionHandlers := lifecycle.NewExtensionHandlers(mgr.GetClient())

	if err := runtimeExtensionWebhookServer.AddExtensionHandler(server.ExtensionHandler{
		Hook:        runtimehooksv1.BeforeClusterCreate,
		Name:        "before-cluster-create",
		HandlerFunc: lifecycleExtensionHandlers.DoBeforeClusterCreate,
	}); err != nil {
		setupLog.Error(err, "error adding handler")
		os.Exit(1)
	}

	if err := runtimeExtensionWebhookServer.AddExtensionHandler(server.ExtensionHandler{
		Hook:        runtimehooksv1.BeforeClusterUpgrade,
		Name:        "before-cluster-upgrade",
		HandlerFunc: lifecycleExtensionHandlers.DoBeforeClusterUpgrade,
	}); err != nil {
		setupLog.Error(err, "error adding handler")
		os.Exit(1)
	}

	if err := runtimeExtensionWebhookServer.AddExtensionHandler(server.ExtensionHandler{
		Hook:        runtimehooksv1.AfterControlPlaneUpgrade,
		Name:        "after-control-plane-upgrade",
		HandlerFunc: lifecycleExtensionHandlers.DoAfterControlPlaneUpgrade,
	}); err != nil {
		setupLog.Error(err, "error adding handler")
		os.Exit(1)
	}

	if err := runtimeExtensionWebhookServer.AddExtensionHandler(server.ExtensionHandler{
		Hook:        runtimehooksv1.AfterClusterUpgrade,
		Name:        "after-cluster-upgrade",
		HandlerFunc: lifecycleExtensionHandlers.DoAfterClusterUpgrade,
	}); err != nil {
		setupLog.Error(err, "error adding handler")
		os.Exit(1)
	}
}

func setupChecks(mgr ctrl.Manager) {
	if err := mgr.AddReadyzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
		setupLog.Error(err, "unable to create ready check")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("webhook", mgr.GetWebhookServer().StartedChecker()); err != nil {
		setupLog.Error(err, "unable to create health check")
		os.Exit(1)
	}
}

func setupReconcilers(mgr ctrl.Manager) {
	if err := (&controller.UpdateTaskReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "UpgradeTask")
		os.Exit(1)
	}
	if err := (&controller.DockerNodeUpdateTaskReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "DockerNodeUpdateTask")
		os.Exit(1)
	}
	//+kubebuilder:scaffold:builder
}
