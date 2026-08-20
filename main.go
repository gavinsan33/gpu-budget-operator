package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

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

// gpuRatesFlag accumulates repeated --gpu-rate=<family>=<usd> flags into a
// controllers.GPURates, implementing flag.Value so a new GPU family (or a
// changed rate for an existing one) is purely a deployment manifest change,
// never a code change.
type gpuRatesFlag struct {
	rates controllers.GPURates
}

func (f *gpuRatesFlag) String() string {
	if f == nil || len(f.rates) == 0 {
		return ""
	}
	parts := make([]string, 0, len(f.rates))
	for family, rate := range f.rates {
		parts = append(parts, fmt.Sprintf("%s=%g", family, rate))
	}
	return strings.Join(parts, ",")
}

func (f *gpuRatesFlag) Set(value string) error {
	family, rateStr, ok := strings.Cut(value, "=")
	if !ok || family == "" {
		return fmt.Errorf("expected <family>=<usd-per-gpu-hour> (e.g. A100=1.70), got %q", value)
	}
	rate, err := strconv.ParseFloat(rateStr, 64)
	if err != nil {
		return fmt.Errorf("parsing rate in %q: %w", value, err)
	}
	if f.rates == nil {
		f.rates = controllers.GPURates{}
	}
	f.rates[strings.ToUpper(family)] = rate
	return nil
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var prometheusURL string
	gpuRates := &gpuRatesFlag{}
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metrics endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&prometheusURL, "prometheus-url", "",
		"The single, cluster-wide Prometheus/Thanos base URL every GpuBudget is evaluated against.")
	flag.Var(gpuRates, "gpu-rate",
		"USD cost per GPU-hour for a GPU family, as <family>=<usd> (e.g. A100=1.70). "+
			"Repeatable - pass once per GPU family any GpuBudget with spec.dollarsLimit might use. "+
			"Matched against gpuType by family prefix, so one A100 rate covers every A100 SKU.")
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

	if err := (&controllers.GpuBudgetReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		PrometheusURL: prometheusURL,
		GPURates:      gpuRates.rates,
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
