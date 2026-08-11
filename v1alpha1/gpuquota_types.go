package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResetAnnotation, when set to "true" on a GpuQuota, tells the operator to
// restore any workloads it enforced and clear enforcement state. This is the
// only way enforcement is ever lifted - it is never automatic, not when
// usage would otherwise be back under budget and not when a new period
// starts, since cumulative usage never decreases within a period and a new
// period alone doesn't mean the underlying cost problem was addressed. The
// operator removes the annotation itself once processed.
const ResetAnnotation = "gpuquota.io/reset"

// BudgetPeriod is a calendar-aligned billing cycle a GpuQuota's budget
// resets on. These are fixed calendar boundaries, not rolling windows - e.g.
// Monthly always resets on the 1st of the month, never "30 days before now."
type BudgetPeriod string

const (
	PeriodDaily   BudgetPeriod = "Daily"
	PeriodWeekly  BudgetPeriod = "Weekly"
	PeriodMonthly BudgetPeriod = "Monthly"
)

// GpuQuotaSpec defines a cumulative GPU budget for the namespace the
// GpuQuota lives in, over a recurring calendar period.
// +kubebuilder:validation:XValidation:rule="has(self.gpuHoursLimit) || has(self.dollarsLimit)",message="at least one of gpuHoursLimit or dollarsLimit must be set"
type GpuQuotaSpec struct {
	// Period is the calendar-aligned billing cycle the budget resets on.
	// Daily starts at 00:00 UTC; Weekly starts Monday 00:00 UTC; Monthly
	// starts on the 1st at 00:00 UTC.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Enum=Daily;Weekly;Monthly
	Period BudgetPeriod `json:"period"`

	// GPUHoursLimit is the max cumulative GPU-hours, summed across all GPU
	// types, allowed within the current period. At least one of
	// GPUHoursLimit/DollarsLimit must be set; if both are set, whichever is
	// exceeded first triggers enforcement.
	// +optional
	GPUHoursLimit *float64 `json:"gpuHoursLimit,omitempty"`

	// DollarsLimit is the max cumulative cost, in USD, allowed within the
	// current period, computed from GPU-hours-by-type (see Query) and the
	// operator's configured per-GPU-type hourly rates
	// (--gpu-rate-a100/h100/v100 flags). At least one of
	// GPUHoursLimit/DollarsLimit must be set; if both are set, whichever is
	// exceeded first triggers enforcement.
	// +optional
	DollarsLimit *float64 `json:"dollarsLimit,omitempty"`

	// Query overrides the default PromQL used to compute cumulative
	// GPU-hours consumed since the current period started, broken out by
	// GPU type. Must return one sample per GPU type, each labeled "gpuType"
	// with a value matching one of the operator's configured rate keys
	// (a100, h100, v100 - case-insensitive). The literal strings
	// "__NAMESPACE__" and "__RANGE__" are substituted with the target
	// namespace and a PromQL range duration covering "period start to now"
	// respectively. Override this to change how GPU-hours are attributed -
	// e.g. switching from reservation-based accounting (the default, which
	// matches typical GPU-cluster billing: you're charged for what you
	// reserved, not what you used) to DCGM utilization-based accounting -
	// without any code or CRD changes.
	// +optional
	Query string `json:"query,omitempty"`

	// CheckInterval is how often cumulative usage is re-evaluated. Defaults
	// to 15m to match typical GPU-cluster billing granularity - checking
	// more often than billing itself updates doesn't surface anything new.
	// +optional
	// +kubebuilder:default="15m"
	CheckInterval metav1.Duration `json:"checkInterval,omitempty"`
}

// GpuQuotaPhase summarizes the current enforcement state of the namespace.
type GpuQuotaPhase string

const (
	PhaseCompliant GpuQuotaPhase = "Compliant"
	PhaseEnforced  GpuQuotaPhase = "Enforced"
	PhaseUnknown   GpuQuotaPhase = "Unknown"
)

// EnforcedResource records a single action taken against a workload.
type EnforcedResource struct {
	// APIVersion of the affected resource.
	APIVersion string `json:"apiVersion"`
	// Kind of the affected resource.
	Kind string `json:"kind"`
	// Name of the affected resource.
	Name string `json:"name"`
	// Action taken: "ScaledToZero", "Suspended", or "Deleted".
	Action string `json:"action"`
	// EnforcedAt is when the action was taken.
	EnforcedAt metav1.Time `json:"enforcedAt"`
}

// GPUTypeUsage records cumulative GPU-hours consumed by one GPU type in the
// current period.
type GPUTypeUsage struct {
	GPUType  string  `json:"gpuType"`
	GPUHours float64 `json:"gpuHours"`
}

// GpuQuotaStatus reports cumulative usage for the current period and any
// enforcement taken.
type GpuQuotaStatus struct {
	// CurrentPeriodStart is when the current billing period began.
	// +optional
	CurrentPeriodStart *metav1.Time `json:"currentPeriodStart,omitempty"`

	// GPUHoursUsed is cumulative GPU-hours consumed so far in the current
	// period, summed across all GPU types.
	// +optional
	GPUHoursUsed float64 `json:"gpuHoursUsed,omitempty"`

	// GPUHoursByType breaks GPUHoursUsed down per GPU type.
	// +optional
	GPUHoursByType []GPUTypeUsage `json:"gpuHoursByType,omitempty"`

	// DollarsUsed is cumulative cost, in USD, so far in the current period.
	// +optional
	DollarsUsed float64 `json:"dollarsUsed,omitempty"`

	// Phase is the current enforcement state. Enforcement is never lifted
	// automatically - see ResetAnnotation.
	// +optional
	Phase GpuQuotaPhase `json:"phase,omitempty"`

	// LastCheckedTime is when usage was last queried from Prometheus.
	// +optional
	LastCheckedTime *metav1.Time `json:"lastCheckedTime,omitempty"`

	// LastEnforcementTime is when enforcement most recently acted against
	// this namespace.
	// +optional
	LastEnforcementTime *metav1.Time `json:"lastEnforcementTime,omitempty"`

	// EnforcedResources lists workloads currently scaled down/suspended by
	// this controller, awaiting a manual restore.
	// +optional
	EnforcedResources []EnforcedResource `json:"enforcedResources,omitempty"`

	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=gq
// +kubebuilder:printcolumn:name="Period",type=string,JSONPath=".spec.period"
// +kubebuilder:printcolumn:name="GPUHoursUsed",type=string,JSONPath=".status.gpuHoursUsed"
// +kubebuilder:printcolumn:name="DollarsUsed",type=string,JSONPath=".status.dollarsUsed"
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=".status.phase"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type GpuQuota struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GpuQuotaSpec   `json:"spec,omitempty"`
	Status GpuQuotaStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type GpuQuotaList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GpuQuota `json:"items"`
}
