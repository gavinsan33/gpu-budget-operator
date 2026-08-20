package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SingletonConfigName is the only object name GpuBudgetOperatorConfig is
// ever reconciled under - mirroring the convention OpenShift's own
// config.openshift.io singletons use (Infrastructure/Network/etc., all
// named "cluster"). The operator ignores any GpuBudgetOperatorConfig
// created under a different name rather than picking one arbitrarily,
// since silently choosing among several would make "which config is
// actually active" impossible to answer by inspection.
const SingletonConfigName = "cluster"

// GPURate is the USD-per-GPU-hour rate for one GPU family (e.g. "A100"),
// matched against a GpuBudget's observed gpuType by prefix - see
// controllers.GPURates.RateFor.
type GPURate struct {
	// Family is the GPU family name (e.g. "A100", "H100"), matched against
	// gpuType by prefix - case-insensitive.
	// +kubebuilder:validation:Required
	Family string `json:"family"`

	// DollarsPerGPUHour is the USD cost per GPU-hour for this family. A
	// zero or absent rate is treated as "not configured," not "free" - a
	// GpuBudget with spec.dollarsLimit whose usage includes an unpriced
	// type fails loudly instead of silently undercounting cost.
	// +kubebuilder:validation:Required
	DollarsPerGPUHour float64 `json:"dollarsPerGPUHour"`
}

// GpuBudgetOperatorConfigSpec holds the operator-wide settings that used to
// be --prometheus-url/--gpu-rate command-line flags. Moving them into a CR
// the operator watches (rather than reading once at process startup) means
// changing either takes effect on the next reconcile, with no restart, and
// - unlike a flag baked into a Deployment/ClusterServiceVersion at build
// time - can be set at install time by editing a plain Kubernetes object
// instead of rebuilding/repushing an image.
type GpuBudgetOperatorConfigSpec struct {
	// PrometheusURL is the single, cluster-wide Prometheus/Thanos base URL
	// every GpuBudget is evaluated against. There is no per-namespace
	// override, since splitting GPU accounting across multiple Prometheus
	// instances would make budgets impossible to compare or reason about
	// cluster-wide.
	// +kubebuilder:validation:Required
	PrometheusURL string `json:"prometheusURL"`

	// GPURates is the operator-wide $/GPU-hour rate table used to compute
	// spec.dollarsLimit compliance. Add an entry for any GPU family a
	// GpuBudget with spec.dollarsLimit might use - no code change needed.
	// +optional
	GPURates []GPURate `json:"gpuRates,omitempty"`
}

// GpuBudgetOperatorConfigStatus reports whether the operator has picked up
// this configuration successfully.
type GpuBudgetOperatorConfigStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations, e.g. whether
	// PrometheusURL was reachable the last time any GpuBudget reconciled.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=gboc
// +kubebuilder:printcolumn:name="PrometheusURL",type=string,JSONPath=".spec.prometheusURL"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"
type GpuBudgetOperatorConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GpuBudgetOperatorConfigSpec   `json:"spec,omitempty"`
	Status GpuBudgetOperatorConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type GpuBudgetOperatorConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GpuBudgetOperatorConfig `json:"items"`
}
