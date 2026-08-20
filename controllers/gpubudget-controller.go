package controllers

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/gsanders/gpu-budget-operator/enforce"
	"github.com/gsanders/gpu-budget-operator/metrics"
	gpubudgetv1alpha1 "github.com/gsanders/gpu-budget-operator/v1alpha1"
)

const readyCondition = "Ready"

// GpuBudgetReconciler watches GpuBudget custom resources, evaluates cumulative
// GPU-hours/dollars consumed by the namespace they live in during the
// current billing period against Prometheus, and enforces the configured
// budget by scaling down or suspending GPU-consuming workloads. Enforcement
// is never lifted automatically - see gpubudgetv1alpha1.ResetAnnotation.
type GpuBudgetReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Enforcer performs the actual scale-down/suspend/restore operations.
	// Defaults to &enforce.Enforcer{Client: r.Client} on first use if nil.
	Enforcer *enforce.Enforcer

	promClientMu  sync.Mutex
	promClient    *metrics.Client
	promClientURL string
}

// +kubebuilder:rbac:groups=gpubudget.io,resources=gpubudgets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gpubudget.io,resources=gpubudgets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gpubudget.io,resources=gpubudgets/finalizers,verbs=update
// +kubebuilder:rbac:groups=gpubudget.io,resources=gpubudgetoperatorconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=jobset.x-k8s.io,resources=jobsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=serving.kserve.io,resources=inferenceservices,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
//
//nolint:lll
func (r *GpuBudgetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var gb gpubudgetv1alpha1.GpuBudget
	if err := r.Get(ctx, req.NamespacedName, &gb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if gb.Annotations[gpubudgetv1alpha1.ResetAnnotation] == "true" {
		if err := r.handleManualReset(ctx, &gb); err != nil {
			return ctrl.Result{}, err
		}
	}

	var cfg gpubudgetv1alpha1.GpuBudgetOperatorConfig
	if err := r.Get(ctx, client.ObjectKey{Name: gpubudgetv1alpha1.SingletonConfigName}, &cfg); err != nil {
		return r.markFailed(ctx, &gb, "OperatorConfigMissing", fmt.Errorf(
			"fetching GpuBudgetOperatorConfig %q: %w (create one cluster-wide - see samples/gpubudgetoperatorconfig.yaml)",
			gpubudgetv1alpha1.SingletonConfigName, err))
	}

	promClient, err := r.prometheusClient(cfg.Spec.PrometheusURL)
	if err != nil {
		return r.markFailed(ctx, &gb, "PrometheusClientError", err)
	}

	now := time.Now().UTC()
	start := periodStart(gb.Spec.Period, now)

	query := metrics.BuildGPUHoursQuery(gb.Spec.Query, gb.Namespace, now.Sub(start))
	hoursByType, err := promClient.GPUHoursByType(ctx, query)
	if err != nil {
		return r.markFailed(ctx, &gb, "MetricsQueryFailed", err)
	}

	rates := ratesFromSpec(cfg.Spec.GPURates)
	totalHours, totalDollars, usageByType, err := r.computeUsage(hoursByType, gb.Spec.DollarsLimit != nil, gb.Namespace, rates)
	if err != nil {
		return r.markFailed(ctx, &gb, "UnpricedGPUType", err)
	}

	nowMeta := metav1.NewTime(now)
	startMeta := metav1.NewTime(start)
	gb.Status.CurrentPeriodStart = &startMeta
	// Enforcement below still compares the unrounded totalHours/totalDollars
	// against the spec limits - rounding here is purely so the recorded and
	// displayed status (kubectl get's GPUHOURSUSED/DOLLARSUSED columns,
	// status.gpuHoursByType) doesn't carry meaningless float noise like
	// 0.08333333333333333.
	gb.Status.GPUHoursUsed = roundToHundredths(totalHours)
	gb.Status.GPUHoursByType = roundUsageByType(usageByType)
	gb.Status.DollarsUsed = roundToHundredths(totalDollars)
	gb.Status.LastCheckedTime = &nowMeta
	gb.Status.ObservedGeneration = gb.Generation

	overBudget := (gb.Spec.GPUHoursLimit != nil && totalHours > *gb.Spec.GPUHoursLimit) ||
		(gb.Spec.DollarsLimit != nil && totalDollars > *gb.Spec.DollarsLimit)

	// A non-empty EnforcedResources, not Phase == PhaseEnforced, is the
	// ground truth for "is anything currently enforced" - Phase can be
	// perturbed to Unknown by an unrelated transient failure (markFailed,
	// e.g. a metrics query error or an unpriced GPU type) on a reconcile
	// that never reaches this point at all, without EnforcedResources ever
	// changing. Keying stickiness off Phase would let a namespace fall
	// straight through to Compliant - clearing nothing, restoring
	// nothing - the moment that transient failure clears and usage
	// happens to read as under budget, silently abandoning already-zeroed
	// workloads with no reset ever having run.
	stickyEnforced := len(gb.Status.EnforcedResources) > 0

	var enforceErr error
	if overBudget || stickyEnforced {
		var enforced []gpubudgetv1alpha1.EnforcedResource
		enforced, enforceErr = r.enforcer().EnforceNamespace(ctx, gb.Namespace)
		// Merge whatever EnforceNamespace acted on BEFORE checking enforceErr:
		// it keeps enforcing every remaining resource kind even after one
		// kind's Update call fails, so enforced can be non-empty even when
		// enforceErr != nil. A resource missing from EnforcedResources is
		// permanently invisible to gpubudget.io/reset (RestoreNamespace only
		// restores what's listed here) - dropping this on enforceErr would
		// leave that resource scaled to zero forever with no error ever
		// surfaced again, since the next reconcile sees its
		// gpubudget.io/original-* annotation already present and won't
		// re-report it either.
		gb.Status.EnforcedResources = mergeEnforced(gb.Status.EnforcedResources, enforced)
		if len(enforced) > 0 {
			gb.Status.LastEnforcementTime = &nowMeta
		}
		gb.Status.Phase = gpubudgetv1alpha1.PhaseEnforced
	} else {
		gb.Status.Phase = gpubudgetv1alpha1.PhaseCompliant
	}

	if enforceErr != nil {
		apimeta.SetStatusCondition(&gb.Status.Conditions, metav1.Condition{
			Type:    readyCondition,
			Status:  metav1.ConditionFalse,
			Reason:  "EnforcementFailed",
			Message: enforceErr.Error(),
		})
	} else {
		apimeta.SetStatusCondition(&gb.Status.Conditions, metav1.Condition{
			Type:    readyCondition,
			Status:  metav1.ConditionTrue,
			Reason:  "UsageEvaluated",
			Message: fmt.Sprintf("gpuHours=%.2f dollars=%.2f phase=%s", totalHours, totalDollars, gb.Status.Phase),
		})
	}
	if err := r.persistStatus(ctx, &gb); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status for %s/%s: %w", gb.Namespace, gb.Name, err)
	}
	if enforceErr != nil {
		return ctrl.Result{}, fmt.Errorf("enforcing budget in namespace %s: %w", gb.Namespace, enforceErr)
	}

	logger.V(1).Info("reconciled GpuBudget", "namespace", gb.Namespace, "gpuHours", totalHours, "dollars", totalDollars, "phase", gb.Status.Phase)
	return ctrl.Result{RequeueAfter: durationOrDefault(gb.Spec.CheckInterval.Duration, 15*time.Minute)}, nil
}

// persistStatus writes gb's in-memory Status onto the current version of the
// object in the API, retrying on resourceVersion conflicts by re-fetching
// the latest copy and reapplying the same desired status (computed entirely
// from this reconcile's own Prometheus/enforcement results, not from
// whatever gb's stale fields were, so it's always safe to replay). A plain
// Status().Update with no retry would silently discard this reconcile's
// work on any conflict - including, worst case, a freshly-computed
// EnforcedResources entry for a resource that was just successfully scaled
// to zero on the cluster a moment earlier, permanently orphaning it (see
// EnforceNamespace's doc comment).
func (r *GpuBudgetReconciler) persistStatus(ctx context.Context, gb *gpubudgetv1alpha1.GpuBudget) error {
	desired := gb.Status.DeepCopy()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest gpubudgetv1alpha1.GpuBudget
		if err := r.Get(ctx, client.ObjectKeyFromObject(gb), &latest); err != nil {
			return err
		}
		latest.Status = *desired
		if err := r.Status().Update(ctx, &latest); err != nil {
			return err
		}
		gb.ResourceVersion = latest.ResourceVersion
		return nil
	})
}

// computeUsage totals GPU-hours across all types and, if needsDollars,
// prices each type via r.GPURates - returning an error naming the first
// unpriced GPU type found, so a misconfigured dollarsLimit fails loudly
// rather than silently undercounting cost.
func (r *GpuBudgetReconciler) computeUsage(hoursByType map[string]float64, needsDollars bool, namespace string, rates GPURates) (totalHours, totalDollars float64, usage []gpubudgetv1alpha1.GPUTypeUsage, err error) {
	usage = make([]gpubudgetv1alpha1.GPUTypeUsage, 0, len(hoursByType))
	for gpuType, hours := range hoursByType {
		totalHours += hours
		usage = append(usage, gpubudgetv1alpha1.GPUTypeUsage{GPUType: gpuType, GPUHours: hours})
		if needsDollars {
			rate, ok := rates.RateFor(gpuType)
			if !ok {
				return 0, 0, nil, fmt.Errorf("namespace %s used unpriced GPU type %q: add it to GpuBudgetOperatorConfig %q's spec.gpuRates", namespace, gpuType, gpubudgetv1alpha1.SingletonConfigName)
			}
			totalDollars += hours * rate
		}
	}
	sort.Slice(usage, func(i, j int) bool { return usage[i].GPUType < usage[j].GPUType })
	return totalHours, totalDollars, usage, nil
}

// roundToHundredths rounds v to 2 decimal places, for recording/display
// purposes only - callers that need to compare against a spec limit should
// use the unrounded value.
func roundToHundredths(v float64) float64 {
	return math.Round(v*100) / 100
}

// roundUsageByType returns a copy of usage with each entry's GPUHours
// rounded via roundToHundredths.
func roundUsageByType(usage []gpubudgetv1alpha1.GPUTypeUsage) []gpubudgetv1alpha1.GPUTypeUsage {
	rounded := make([]gpubudgetv1alpha1.GPUTypeUsage, len(usage))
	for i, u := range usage {
		rounded[i] = gpubudgetv1alpha1.GPUTypeUsage{GPUType: u.GPUType, GPUHours: roundToHundredths(u.GPUHours)}
	}
	return rounded
}

// handleManualReset restores any workloads previously enforced against gb's
// namespace, clears enforcement status, and removes the reset annotation -
// the only way enforcement is ever lifted (see gpubudgetv1alpha1.ResetAnnotation).
func (r *GpuBudgetReconciler) handleManualReset(ctx context.Context, gb *gpubudgetv1alpha1.GpuBudget) error {
	if err := r.enforcer().RestoreNamespace(ctx, gb.Namespace, gb.Status.EnforcedResources); err != nil {
		return fmt.Errorf("restoring namespace %s during manual reset: %w", gb.Namespace, err)
	}
	delete(gb.Annotations, gpubudgetv1alpha1.ResetAnnotation)
	if err := r.Update(ctx, gb); err != nil {
		return fmt.Errorf("clearing reset annotation for %s/%s: %w", gb.Namespace, gb.Name, err)
	}
	// A regular Update() never touches the status subresource, but its
	// response still repopulates gb in place with whatever the server has
	// stored for .status - the pre-reset enforcement state. Set it fresh
	// again here so the rest of Reconcile (and the eventual
	// r.Status().Update() at the end of it) sees post-reset state, not
	// this now-stale copy.
	gb.Status.EnforcedResources = nil
	gb.Status.Phase = gpubudgetv1alpha1.PhaseCompliant
	gb.Status.LastEnforcementTime = nil
	return nil
}

func (r *GpuBudgetReconciler) markFailed(ctx context.Context, gb *gpubudgetv1alpha1.GpuBudget, reason string, cause error) (ctrl.Result, error) {
	gb.Status.Phase = gpubudgetv1alpha1.PhaseUnknown
	apimeta.SetStatusCondition(&gb.Status.Conditions, metav1.Condition{
		Type:    readyCondition,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: cause.Error(),
	})
	if err := r.Status().Update(ctx, gb); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status after %s: %w", reason, err)
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *GpuBudgetReconciler) enforcer() *enforce.Enforcer {
	if r.Enforcer == nil {
		r.Enforcer = &enforce.Enforcer{Client: r.Client}
	}
	return r.Enforcer
}

// prometheusClient returns a cached client for url, rebuilding it if url has
// changed since the last call - GpuBudgetOperatorConfig.spec.prometheusURL
// can be edited at any time, unlike the --prometheus-url flag it replaced,
// which was only ever read once at process startup.
func (r *GpuBudgetReconciler) prometheusClient(url string) (*metrics.Client, error) {
	if url == "" {
		return nil, fmt.Errorf("GpuBudgetOperatorConfig %q has no spec.prometheusURL set", gpubudgetv1alpha1.SingletonConfigName)
	}

	r.promClientMu.Lock()
	defer r.promClientMu.Unlock()
	if r.promClient != nil && r.promClientURL == url {
		return r.promClient, nil
	}
	c, err := metrics.NewClient(metrics.Config{Address: url})
	if err != nil {
		return nil, err
	}
	r.promClient = c
	r.promClientURL = url
	return c, nil
}

// mergeEnforced adds newly enforced resources to the existing set, keyed by
// kind+name, so repeated enforcement passes don't duplicate entries.
func mergeEnforced(existing, additions []gpubudgetv1alpha1.EnforcedResource) []gpubudgetv1alpha1.EnforcedResource {
	seen := make(map[string]bool, len(existing))
	for _, e := range existing {
		seen[e.Kind+"/"+e.Name] = true
	}
	for _, a := range additions {
		key := a.Kind + "/" + a.Name
		if !seen[key] {
			existing = append(existing, a)
			seen[key] = true
		}
	}
	return existing
}

func durationOrDefault(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}

// SetupWithManager wires the reconciler into the manager. Also watches
// GpuBudgetOperatorConfig so that editing the singleton config (a new
// PrometheusURL, an added/changed GPU rate) re-reconciles every GpuBudget
// promptly, rather than waiting up to each one's own spec.checkInterval to
// notice - since one shared config affects all of them at once.
func (r *GpuBudgetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gpubudgetv1alpha1.GpuBudget{}).
		Watches(
			&gpubudgetv1alpha1.GpuBudgetOperatorConfig{},
			handler.EnqueueRequestsFromMapFunc(r.enqueueAllGpuBudgets),
		).
		Complete(r)
}

// enqueueAllGpuBudgets requeues every GpuBudget in the cluster - used only
// as a reaction to a GpuBudgetOperatorConfig change, which is cluster-wide
// by definition (see SetupWithManager).
func (r *GpuBudgetReconciler) enqueueAllGpuBudgets(ctx context.Context, _ client.Object) []reconcile.Request {
	var list gpubudgetv1alpha1.GpuBudgetList
	if err := r.List(ctx, &list); err != nil {
		log.FromContext(ctx).Error(err, "listing GpuBudgets to requeue after GpuBudgetOperatorConfig change")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for _, gb := range list.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&gb)})
	}
	return requests
}
