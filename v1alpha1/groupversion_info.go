// +kubebuilder:object:generate=true
// +groupName=gpubudget.io
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
	GroupVersion  = schema.GroupVersion{Group: "gpubudget.io", Version: "v1alpha1"}
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
	AddToScheme   = SchemeBuilder.AddToScheme
)

func init() {
	SchemeBuilder.Register(&GpuBudget{}, &GpuBudgetList{})
	SchemeBuilder.Register(&GpuBudgetOperatorConfig{}, &GpuBudgetOperatorConfigList{})
}
