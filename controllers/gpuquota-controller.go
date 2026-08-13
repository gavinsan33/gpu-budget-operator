package controllers

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gsanders/gpu-quota-operator/enforce"
	"github.com/gsanders/gpu-quota-operator/metrics"
	gpuquotav1alpha1 "github.com/gsanders/gpu-quota-operator/v1alpha1"
)

const readyCondition = "Ready"

// GpuQuotaReconciler watches GpuQuota custom resources, evaluates cumulative
// GPU-hours/dollars consumed by the namespace they live in during the
// current billing period against Prometheus, and enforces the configured
// budget by scaling down or suspending GPU-consuming workloads. Enforcement
// is never lifted automatically - see gpuquotav1alpha1.ResetAnnotation.
type GpuQuotaReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// PrometheusURL is the single, cluster-wide Prometheus/Thanos endpoint
	// every GpuQuota is evaluated against. Set once via the operator's
	// --prometheus-url flag - there is no per-namespace override, since
	// splitting GPU accounting across multiple Prometheus instances would
	// make quotas impossible to compare or reason about cluster-wide.
	PrometheusURL string

	// GPURates is the operator-wide $/GPU-hour rate table used to compute
	// spec.dollarsLimit compliance.
	GPURates GPURates

	// Enforcer performs the actual scale-down/suspend/restore operations.
	// Defaults to &enforce.Enforcer{Client: r.Client} on first use if nil.
	Enforcer *enforce.Enforcer

	promClientMu sync.Mutex
	promClient   *metrics.Client
}

// +kubebuilder:rbac:groups=gpuquota.io,resources=gpuquotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gpuquota.io,resources=gpuquotas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gpuquota.io,resources=gpuquotas/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=replicasets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=jobset.x-k8s.io,resources=jobsets,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=serving.kserve.io,resources=inferenceservices,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
//
//nolint:lll
func (r *GpuQuotaReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var gq gpuquotav1alpha1.GpuQuota
	if err := r.Get(ctx, req.NamespacedName, &gq); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if gq.Annotations[gpuquotav1alpha1.ResetAnnotation] == "true" {
		if err := r.handleManualReset(ctx, &gq); err != nil {
			return ctrl.Result{}, err
		}
	}

	promClient, err := r.prometheusClient()
	if err != nil {
		return r.markFailed(ctx, &gq, "PrometheusClientError", err)
	}

	now := time.Now().UTC()
	start := periodStart(gq.Spec.Period, now)

	query := metrics.BuildGPUHoursQuery(gq.Spec.Query, gq.Namespace, now.Sub(start))
	hoursByType, err := promClient.GPUHoursByType(ctx, query)
	if err != nil {
		return r.markFailed(ctx, &gq, "MetricsQueryFailed", err)
	}

	totalHours, totalDollars, usageByType, err := r.computeUsage(hoursByType, gq.Spec.DollarsLimit != nil, gq.Namespace)
	if err != nil {
		return r.markFailed(ctx, &gq, "UnpricedGPUType", err)
	}

	nowMeta := metav1.NewTime(now)
	startMeta := metav1.NewTime(start)
	gq.Status.CurrentPeriodStart = &startMeta
	// Enforcement below still compares the unrounded totalHours/totalDollars
	// against the spec limits - rounding here is purely so the recorded and
	// displayed status (kubectl get's GPUHOURSUSED/DOLLARSUSED columns,
	// status.gpuHoursByType) doesn't carry meaningless float noise like
	// 0.08333333333333333.
	gq.Status.GPUHoursUsed = roundToHundredths(totalHours)
	gq.Status.GPUHoursByType = roundUsageByType(usageByType)
	gq.Status.DollarsUsed = roundToHundredths(totalDollars)
	gq.Status.LastCheckedTime = &nowMeta
	gq.Status.ObservedGeneration = gq.Generation

	overBudget := (gq.Spec.GPUHoursLimit != nil && totalHours > *gq.Spec.GPUHoursLimit) ||
		(gq.Spec.DollarsLimit != nil && totalDollars > *gq.Spec.DollarsLimit)

	var enforceErr error
	if overBudget || gq.Status.Phase == gpuquotav1alpha1.PhaseEnforced {
		var enforced []gpuquotav1alpha1.EnforcedResource
		enforced, enforceErr = r.enforcer().EnforceNamespace(ctx, gq.Namespace)
		// Merge whatever EnforceNamespace acted on BEFORE checking enforceErr:
		// it keeps enforcing every remaining resource kind even after one
		// kind's Update call fails, so enforced can be non-empty even when
		// enforceErr != nil. A resource missing from EnforcedResources is
		// permanently invisible to gpuquota.io/reset (RestoreNamespace only
		// restores what's listed here) - dropping this on enforceErr would
		// leave that resource scaled to zero forever with no error ever
		// surfaced again, since the next reconcile sees its
		// gpuquota.io/original-* annotation already present and won't
		// re-report it either.
		gq.Status.EnforcedResources = mergeEnforced(gq.Status.EnforcedResources, enforced)
		if len(enforced) > 0 {
			gq.Status.LastEnforcementTime = &nowMeta
		}
		gq.Status.Phase = gpuquotav1alpha1.PhaseEnforced
	} else {
		gq.Status.Phase = gpuquotav1alpha1.PhaseCompliant
	}

	if enforceErr != nil {
		apimeta.SetStatusCondition(&gq.Status.Conditions, metav1.Condition{
			Type:    readyCondition,
			Status:  metav1.ConditionFalse,
			Reason:  "EnforcementFailed",
			Message: enforceErr.Error(),
		})
	} else {
		apimeta.SetStatusCondition(&gq.Status.Conditions, metav1.Condition{
			Type:    readyCondition,
			Status:  metav1.ConditionTrue,
			Reason:  "UsageEvaluated",
			Message: fmt.Sprintf("gpuHours=%.2f dollars=%.2f phase=%s", totalHours, totalDollars, gq.Status.Phase),
		})
	}
	if err := r.persistStatus(ctx, &gq); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status for %s/%s: %w", gq.Namespace, gq.Name, err)
	}
	if enforceErr != nil {
		return ctrl.Result{}, fmt.Errorf("enforcing quota in namespace %s: %w", gq.Namespace, enforceErr)
	}

	logger.V(1).Info("reconciled GpuQuota", "namespace", gq.Namespace, "gpuHours", totalHours, "dollars", totalDollars, "phase", gq.Status.Phase)
	return ctrl.Result{RequeueAfter: durationOrDefault(gq.Spec.CheckInterval.Duration, 15*time.Minute)}, nil
}

// persistStatus writes gq's in-memory Status onto the current version of the
// object in the API, retrying on resourceVersion conflicts by re-fetching
// the latest copy and reapplying the same desired status (computed entirely
// from this reconcile's own Prometheus/enforcement results, not from
// whatever gq's stale fields were, so it's always safe to replay). A plain
// Status().Update with no retry would silently discard this reconcile's
// work on any conflict - including, worst case, a freshly-computed
// EnforcedResources entry for a resource that was just successfully scaled
// to zero on the cluster a moment earlier, permanently orphaning it (see
// EnforceNamespace's doc comment).
func (r *GpuQuotaReconciler) persistStatus(ctx context.Context, gq *gpuquotav1alpha1.GpuQuota) error {
	desired := gq.Status.DeepCopy()
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var latest gpuquotav1alpha1.GpuQuota
		if err := r.Get(ctx, client.ObjectKeyFromObject(gq), &latest); err != nil {
			return err
		}
		latest.Status = *desired
		if err := r.Status().Update(ctx, &latest); err != nil {
			return err
		}
		gq.ResourceVersion = latest.ResourceVersion
		return nil
	})
}

// computeUsage totals GPU-hours across all types and, if needsDollars,
// prices each type via r.GPURates - returning an error naming the first
// unpriced GPU type found, so a misconfigured dollarsLimit fails loudly
// rather than silently undercounting cost.
func (r *GpuQuotaReconciler) computeUsage(hoursByType map[string]float64, needsDollars bool, namespace string) (totalHours, totalDollars float64, usage []gpuquotav1alpha1.GPUTypeUsage, err error) {
	usage = make([]gpuquotav1alpha1.GPUTypeUsage, 0, len(hoursByType))
	for gpuType, hours := range hoursByType {
		totalHours += hours
		usage = append(usage, gpuquotav1alpha1.GPUTypeUsage{GPUType: gpuType, GPUHours: hours})
		if needsDollars {
			rate, ok := r.GPURates.RateFor(gpuType)
			if !ok {
				return 0, 0, nil, fmt.Errorf("namespace %s used unpriced GPU type %q: set the operator's --gpu-rate-%s flag", namespace, gpuType, strings.ToLower(gpuType))
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
func roundUsageByType(usage []gpuquotav1alpha1.GPUTypeUsage) []gpuquotav1alpha1.GPUTypeUsage {
	rounded := make([]gpuquotav1alpha1.GPUTypeUsage, len(usage))
	for i, u := range usage {
		rounded[i] = gpuquotav1alpha1.GPUTypeUsage{GPUType: u.GPUType, GPUHours: roundToHundredths(u.GPUHours)}
	}
	return rounded
}

// handleManualReset restores any workloads previously enforced against gq's
// namespace, clears enforcement status, and removes the reset annotation -
// the only way enforcement is ever lifted (see gpuquotav1alpha1.ResetAnnotation).
func (r *GpuQuotaReconciler) handleManualReset(ctx context.Context, gq *gpuquotav1alpha1.GpuQuota) error {
	if err := r.enforcer().RestoreNamespace(ctx, gq.Namespace, gq.Status.EnforcedResources); err != nil {
		return fmt.Errorf("restoring namespace %s during manual reset: %w", gq.Namespace, err)
	}
	delete(gq.Annotations, gpuquotav1alpha1.ResetAnnotation)
	if err := r.Update(ctx, gq); err != nil {
		return fmt.Errorf("clearing reset annotation for %s/%s: %w", gq.Namespace, gq.Name, err)
	}
	// A regular Update() never touches the status subresource, but its
	// response still repopulates gq in place with whatever the server has
	// stored for .status - the pre-reset enforcement state. Set it fresh
	// again here so the rest of Reconcile (and the eventual
	// r.Status().Update() at the end of it) sees post-reset state, not
	// this now-stale copy.
	gq.Status.EnforcedResources = nil
	gq.Status.Phase = gpuquotav1alpha1.PhaseCompliant
	gq.Status.LastEnforcementTime = nil
	return nil
}

func (r *GpuQuotaReconciler) markFailed(ctx context.Context, gq *gpuquotav1alpha1.GpuQuota, reason string, cause error) (ctrl.Result, error) {
	gq.Status.Phase = gpuquotav1alpha1.PhaseUnknown
	apimeta.SetStatusCondition(&gq.Status.Conditions, metav1.Condition{
		Type:    readyCondition,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: cause.Error(),
	})
	if err := r.Status().Update(ctx, gq); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status after %s: %w", reason, err)
	}
	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

func (r *GpuQuotaReconciler) enforcer() *enforce.Enforcer {
	if r.Enforcer == nil {
		r.Enforcer = &enforce.Enforcer{Client: r.Client}
	}
	return r.Enforcer
}

func (r *GpuQuotaReconciler) prometheusClient() (*metrics.Client, error) {
	if r.PrometheusURL == "" {
		return nil, fmt.Errorf("no prometheus URL configured: set the operator's --prometheus-url flag")
	}

	r.promClientMu.Lock()
	defer r.promClientMu.Unlock()
	if r.promClient != nil {
		return r.promClient, nil
	}
	c, err := metrics.NewClient(metrics.Config{Address: r.PrometheusURL})
	if err != nil {
		return nil, err
	}
	r.promClient = c
	return c, nil
}

// mergeEnforced adds newly enforced resources to the existing set, keyed by
// kind+name, so repeated enforcement passes don't duplicate entries.
func mergeEnforced(existing, additions []gpuquotav1alpha1.EnforcedResource) []gpuquotav1alpha1.EnforcedResource {
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

// SetupWithManager wires the reconciler into the manager.
func (r *GpuQuotaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gpuquotav1alpha1.GpuQuota{}).
		Complete(r)
}
