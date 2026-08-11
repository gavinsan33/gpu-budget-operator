package controllers

import (
	"context"
	"fmt"
	"sync"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/gsanders/gpu-quota-operator/enforce"
	"github.com/gsanders/gpu-quota-operator/metrics"
	gpuquotav1alpha1 "github.com/gsanders/gpu-quota-operator/v1alpha1"
)

const readyCondition = "Ready"

// GpuQuotaReconciler watches GpuQuota custom resources, evaluates current
// GPU usage for the namespace they live in against Prometheus, and enforces
// the configured limit by scaling down or suspending GPU-consuming
// workloads.
type GpuQuotaReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// PrometheusURL is the single, cluster-wide Prometheus/Thanos endpoint
	// every GpuQuota is evaluated against. Set once via the operator's
	// --prometheus-url flag - there is no per-namespace override, since
	// splitting GPU accounting across multiple Prometheus instances would
	// make quotas impossible to compare or reason about cluster-wide.
	PrometheusURL string

	// Enforcer performs the actual scale-down/suspend/restore operations.
	// Defaults to &enforce.Enforcer{Client: r.Client} on first use if nil.
	Enforcer *enforce.Enforcer

	promClientMu sync.Mutex
	promClient   *metrics.Client
}

// +kubebuilder:rbac:groups=gpuquota.example.com,resources=gpuquotas,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gpuquota.example.com,resources=gpuquotas/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=gpuquota.example.com,resources=gpuquotas/finalizers,verbs=update
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

	promClient, err := r.prometheusClient()
	if err != nil {
		return r.markFailed(ctx, &gq, "PrometheusClientError", err)
	}

	query := metrics.BuildQuery(gq.Spec.Query, gq.Namespace)
	usage, err := promClient.ActiveGPUCount(ctx, query)
	if err != nil {
		return r.markFailed(ctx, &gq, "MetricsQueryFailed", err)
	}

	now := metav1.Now()
	gq.Status.CurrentUsage = usage
	gq.Status.LastCheckedTime = &now
	gq.Status.ObservedGeneration = gq.Generation

	var result ctrl.Result
	if usage <= gq.Spec.GPULimit {
		result, err = r.reconcileCompliant(ctx, &gq, now)
	} else {
		result, err = r.reconcileViolating(ctx, &gq, now)
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	apimeta.SetStatusCondition(&gq.Status.Conditions, metav1.Condition{
		Type:    readyCondition,
		Status:  metav1.ConditionTrue,
		Reason:  "MetricsEvaluated",
		Message: fmt.Sprintf("usage=%d limit=%d phase=%s", usage, gq.Spec.GPULimit, gq.Status.Phase),
	})
	if err := r.Status().Update(ctx, &gq); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status for %s/%s: %w", gq.Namespace, gq.Name, err)
	}

	logger.V(1).Info("reconciled GpuQuota", "namespace", gq.Namespace, "usage", usage, "limit", gq.Spec.GPULimit, "phase", gq.Status.Phase)
	return result, nil
}

// reconcileCompliant handles the case where usage is within budget: it
// clears any violation streak and, if AutoRestore is set, restores workloads
// previously scaled down by a prior enforcement.
func (r *GpuQuotaReconciler) reconcileCompliant(ctx context.Context, gq *gpuquotav1alpha1.GpuQuota, now metav1.Time) (ctrl.Result, error) {
	gq.Status.FirstViolationTime = nil

	if gq.Status.Phase == gpuquotav1alpha1.PhaseEnforced && gq.Spec.AutoRestore && len(gq.Status.EnforcedResources) > 0 {
		if err := r.enforcer().RestoreNamespace(ctx, gq.Namespace, gq.Status.EnforcedResources); err != nil {
			return ctrl.Result{}, fmt.Errorf("restoring namespace %s: %w", gq.Namespace, err)
		}
		gq.Status.EnforcedResources = nil
	}
	gq.Status.Phase = gpuquotav1alpha1.PhaseCompliant

	return ctrl.Result{RequeueAfter: durationOrDefault(gq.Spec.CheckInterval.Duration, time.Minute)}, nil
}

// reconcileViolating handles the case where usage exceeds the configured
// limit: it tracks how long the violation has persisted, waits out the
// grace period, then enforces (subject to the cooldown between successive
// enforcement actions on the same namespace).
func (r *GpuQuotaReconciler) reconcileViolating(ctx context.Context, gq *gpuquotav1alpha1.GpuQuota, now metav1.Time) (ctrl.Result, error) {
	gracePeriod := durationOrDefault(gq.Spec.GracePeriod.Duration, 2*time.Minute)
	cooldownPeriod := durationOrDefault(gq.Spec.CooldownPeriod.Duration, 5*time.Minute)
	checkInterval := durationOrDefault(gq.Spec.CheckInterval.Duration, time.Minute)

	if gq.Status.FirstViolationTime == nil {
		gq.Status.FirstViolationTime = &now
		gq.Status.Phase = gpuquotav1alpha1.PhaseViolating
		return ctrl.Result{RequeueAfter: minDuration(gracePeriod, checkInterval)}, nil
	}

	violatingFor := now.Sub(gq.Status.FirstViolationTime.Time)
	if violatingFor < gracePeriod {
		gq.Status.Phase = gpuquotav1alpha1.PhaseViolating
		return ctrl.Result{RequeueAfter: gracePeriod - violatingFor}, nil
	}

	if gq.Status.LastEnforcementTime != nil {
		sinceLast := now.Sub(gq.Status.LastEnforcementTime.Time)
		if sinceLast < cooldownPeriod {
			gq.Status.Phase = gpuquotav1alpha1.PhaseEnforced
			return ctrl.Result{RequeueAfter: cooldownPeriod - sinceLast}, nil
		}
	}

	enforced, err := r.enforcer().EnforceNamespace(ctx, gq.Namespace)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("enforcing quota in namespace %s: %w", gq.Namespace, err)
	}
	gq.Status.EnforcedResources = mergeEnforced(gq.Status.EnforcedResources, enforced)
	gq.Status.LastEnforcementTime = &now
	gq.Status.Phase = gpuquotav1alpha1.PhaseEnforced

	return ctrl.Result{RequeueAfter: checkInterval}, nil
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

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// SetupWithManager wires the reconciler into the manager.
func (r *GpuQuotaReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gpuquotav1alpha1.GpuQuota{}).
		Complete(r)
}
