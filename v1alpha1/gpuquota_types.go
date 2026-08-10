package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GpuQuotaSpec defines the GPU budget for the namespace the GpuQuota lives in.
type GpuQuotaSpec struct {
	// GPULimit is the maximum number of GPUs that may be concurrently active
	// (i.e. reporting non-zero utilization) across the namespace.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Minimum=0
	GPULimit int32 `json:"gpuLimit"`

	// PrometheusURL overrides the operator-wide default Prometheus/Thanos
	// endpoint used to evaluate GPU usage for this namespace.
	// +optional
	PrometheusURL string `json:"prometheusURL,omitempty"`

	// Query overrides the default PromQL used to compute current GPU usage.
	// The string "__NAMESPACE__" is substituted with the namespace name.
	// The query must return a single scalar/vector sample representing the
	// number of GPUs currently in use by the namespace.
	// +optional
	Query string `json:"query,omitempty"`

	// CheckInterval is how often usage is re-evaluated while the namespace
	// is within quota.
	// +optional
	// +kubebuilder:default="1m"
	CheckInterval metav1.Duration `json:"checkInterval,omitempty"`

	// GracePeriod is how long usage must remain continuously over GPULimit
	// before the operator enforces the quota. This absorbs short bursts.
	// +optional
	// +kubebuilder:default="2m"
	GracePeriod metav1.Duration `json:"gracePeriod,omitempty"`

	// CooldownPeriod is the minimum time between enforcement actions against
	// the same namespace, to avoid repeatedly thrashing workloads.
	// +optional
	// +kubebuilder:default="5m"
	CooldownPeriod metav1.Duration `json:"cooldownPeriod,omitempty"`

	// AutoRestore controls whether workloads that were scaled down/suspended
	// by this controller are automatically restored once usage falls back
	// under GPULimit.
	// +optional
	// +kubebuilder:default=true
	AutoRestore bool `json:"autoRestore,omitempty"`
}

// GpuQuotaPhase summarizes the current compliance state of the namespace.
type GpuQuotaPhase string

const (
	PhaseCompliant GpuQuotaPhase = "Compliant"
	PhaseViolating GpuQuotaPhase = "Violating"
	PhaseEnforced  GpuQuotaPhase = "Enforced"
	PhaseUnknown   GpuQuotaPhase = "Unknown"
)

// EnforcementAction records a single action taken against a workload.
type EnforcedResource struct {
	// APIVersion of the affected resource.
	APIVersion string `json:"apiVersion"`
	// Kind of the affected resource (Deployment, InferenceService, JobSet).
	Kind string `json:"kind"`
	// Name of the affected resource.
	Name string `json:"name"`
	// Action taken: "ScaledToZero", "Suspended", or "Deleted".
	Action string `json:"action"`
	// EnforcedAt is when the action was taken.
	EnforcedAt metav1.Time `json:"enforcedAt"`
}

// GpuQuotaStatus reports the observed GPU usage and any enforcement taken.
type GpuQuotaStatus struct {
	// CurrentUsage is the most recently observed number of active GPUs.
	// +optional
	CurrentUsage int32 `json:"currentUsage,omitempty"`

	// Phase is the current compliance state.
	// +optional
	Phase GpuQuotaPhase `json:"phase,omitempty"`

	// FirstViolationTime is when usage was first observed over GPULimit
	// during the current violation streak. Cleared once compliant again.
	// +optional
	FirstViolationTime *metav1.Time `json:"firstViolationTime,omitempty"`

	// LastCheckedTime is when usage was last queried from Prometheus.
	// +optional
	LastCheckedTime *metav1.Time `json:"lastCheckedTime,omitempty"`

	// LastEnforcementTime is when enforcement last ran against this namespace.
	// +optional
	LastEnforcementTime *metav1.Time `json:"lastEnforcementTime,omitempty"`

	// EnforcedResources lists workloads currently scaled down/suspended by
	// this controller and awaiting restoration.
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
// +kubebuilder:printcolumn:name="Limit",type=integer,JSONPath=".spec.gpuLimit"
// +kubebuilder:printcolumn:name="Usage",type=integer,JSONPath=".status.currentUsage"
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
