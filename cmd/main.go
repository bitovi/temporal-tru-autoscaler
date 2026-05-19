// main.go is the entrypoint for the Temporal TRU Autoscaler controller.
// It wires together the controller-runtime manager, registers the CRD scheme,
// and starts the reconciliation loop.
package main

import (
	"flag"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. GCP, Azure, OIDC).
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	temporalv1alpha1 "github.com/bitovi/temporal-tru-autoscaler/api/v1alpha1"
	"github.com/bitovi/temporal-tru-autoscaler/internal/controller"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(temporalv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		metricsAddr          string
		enableLeaderElection bool
		probeAddr            string
		reconcileInterval    time.Duration
		controllerNamespace  string
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080",
		"Address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"Address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure only one controller is active at a time.")
	flag.DurationVar(&reconcileInterval, "reconcile-interval", 30*time.Second,
		"How often to poll each TemporalTRUAutoscaler resource for APS metrics.")
	flag.StringVar(&controllerNamespace, "controller-namespace", "",
		"Namespace where the controller is running. Secrets are read from this namespace. "+
			"Defaults to the POD_NAMESPACE environment variable or 'temporal-autoscaler'.")

	opts := zap.Options{
		Development: false,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Resolve the controller namespace.
	if controllerNamespace == "" {
		controllerNamespace = os.Getenv("POD_NAMESPACE")
	}
	if controllerNamespace == "" {
		controllerNamespace = "temporal-autoscaler"
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "temporal-tru-autoscaler.bitovi.com",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.TemporalTRUAutoscalerReconciler{
		Client:              mgr.GetClient(),
		Scheme:              mgr.GetScheme(),
		Recorder:            mgr.GetEventRecorderFor("temporal-tru-autoscaler"),
		ReconcileInterval:   reconcileInterval,
		ControllerNamespace: controllerNamespace,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "TemporalTRUAutoscaler")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager",
		"reconcileInterval", reconcileInterval,
		"controllerNamespace", controllerNamespace,
		"leaderElection", enableLeaderElection,
	)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
