package main

import (
	"flag"
	"os"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/gsanders/gpu-budget-operator/controllers"
	gpubudgetv1alpha1 "github.com/gsanders/gpu-budget-operator/v1alpha1"
)

var (
	scheme   = clientgoscheme.Scheme
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(gpubudgetv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:           scheme,
		Metrics:          metricsserver.Options{BindAddress: metricsAddr},
		LeaderElection:   enableLeaderElection,
		LeaderElectionID: "gpu-budget-operator.gpubudget.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	// The single, cluster-wide Prometheus URL and per-GPU-family $/GPU-hour
	// rates used to be --prometheus-url/--gpu-rate flags, read once at
	// startup. They're now read from the singleton GpuBudgetOperatorConfig
	// (named gpubudgetv1alpha1.SingletonConfigName) on every reconcile
	// instead, so changing either takes effect immediately with no restart -
	// see samples/gpubudgetoperatorconfig.yaml.
	if err := (&controllers.GpuBudgetReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GpuBudget")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
