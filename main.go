package main

import (
	"flag"
	"os"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/gsanders/gpu-quota-operator/controllers"
	gpuquotav1alpha1 "github.com/gsanders/gpu-quota-operator/v1alpha1"
)

var (
	scheme   = clientgoscheme.Scheme
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(gpuquotav1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var prometheusURL string
	var gpuRateA100 float64
	var gpuRateH100 float64
	var gpuRateV100 float64
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&prometheusURL, "prometheus-url", "",
		"The single, cluster-wide Prometheus/Thanos base URL every GpuQuota is evaluated against.")
	flag.Float64Var(&gpuRateA100, "gpu-rate-a100", 0,
		"USD cost per GPU-hour for NVIDIA A100. Required if any GpuQuota sets spec.dollarsLimit and uses A100s.")
	flag.Float64Var(&gpuRateH100, "gpu-rate-h100", 0,
		"USD cost per GPU-hour for NVIDIA H100. Required if any GpuQuota sets spec.dollarsLimit and uses H100s.")
	flag.Float64Var(&gpuRateV100, "gpu-rate-v100", 0,
		"USD cost per GPU-hour for NVIDIA V100. Required if any GpuQuota sets spec.dollarsLimit and uses V100s.")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:           scheme,
		Metrics:          metricsserver.Options{BindAddress: metricsAddr},
		LeaderElection:   enableLeaderElection,
		LeaderElectionID: "gpu-quota-operator.gpuquota.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controllers.GpuQuotaReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		PrometheusURL: prometheusURL,
		GPURates: controllers.GPURates{
			A100: gpuRateA100,
			H100: gpuRateH100,
			V100: gpuRateV100,
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "GpuQuota")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
