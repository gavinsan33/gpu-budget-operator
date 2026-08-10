// Package enforce scales down (and, when unscalable, deletes) GPU-consuming
// workloads in a namespace that has exceeded its GpuQuota, and restores them
// once usage falls back within budget.
package enforce

import (
	"context"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gpuquotav1alpha1 "github.com/gsanders/gpu-quota-operator/v1alpha1"
)

const (
	// GPUResourceName is the extended resource requested by GPU workloads.
	GPUResourceName = corev1.ResourceName("nvidia.com/gpu")

	// AnnotationOriginalReplicas remembers a Deployment's replica count
	// before it was scaled to zero, so it can be restored later.
	AnnotationOriginalReplicas = "gpuquota.example.com/original-replicas"

	// AnnotationOriginalReplicaSpec remembers an InferenceService component's
	// original min/max replicas as JSON before it was zeroed out.
	AnnotationOriginalReplicaSpec = "gpuquota.example.com/original-replica-spec"

	// AnnotationOriginalSuspend remembers a JobSet's original suspend value.
	AnnotationOriginalSuspend = "gpuquota.example.com/original-suspend"

	ActionScaledToZero = "ScaledToZero"
	ActionSuspended    = "Suspended"
	ActionDeleted      = "Deleted"
)

var (
	jobSetGVK     = schema.GroupVersionKind{Group: "jobset.x-k8s.io", Version: "v1alpha2", Kind: "JobSet"}
	jobSetListGVK = schema.GroupVersionKind{Group: "jobset.x-k8s.io", Version: "v1alpha2", Kind: "JobSetList"}

	inferenceServiceGVK     = schema.GroupVersionKind{Group: "serving.kserve.io", Version: "v1beta1", Kind: "InferenceService"}
	inferenceServiceListGVK = schema.GroupVersionKind{Group: "serving.kserve.io", Version: "v1beta1", Kind: "InferenceServiceList"}

	// inferenceServiceComponents are the top-level spec fields KServe uses
	// for its predictor/transformer/explainer components, each of which
	// supports minReplicas/maxReplicas.
	inferenceServiceComponents = []string{"predictor", "transformer", "explainer"}
)

// componentReplicaSpec captures the original min/max replicas for one
// InferenceService component so it can be restored later. A nil pointer
// means the field was unset in the original spec.
type componentReplicaSpec struct {
	MinReplicas *int64 `json:"minReplicas"`
	MaxReplicas *int64 `json:"maxReplicas"`
}

// Enforcer scales down or suspends GPU-consuming workloads in a namespace.
type Enforcer struct {
	Client client.Client
}

// EnforceNamespace scales every GPU-requesting Deployment to zero replicas,
// suspends every GPU-requesting JobSet, zeroes the replica bounds of every
// GPU-requesting InferenceService, and deletes standalone GPU Pods (ones with
// no controlling owner, since a bare Pod can't be scaled or suspended) in the
// namespace. It returns the set of resources it acted on.
func (e *Enforcer) EnforceNamespace(ctx context.Context, namespace string) ([]gpuquotav1alpha1.EnforcedResource, error) {
	var enforced []gpuquotav1alpha1.EnforcedResource

	deployments, err := e.enforceDeployments(ctx, namespace)
	if err != nil {
		return enforced, err
	}
	enforced = append(enforced, deployments...)

	jobSets, err := e.enforceJobSets(ctx, namespace)
	if err != nil {
		return enforced, err
	}
	enforced = append(enforced, jobSets...)

	inferenceServices, err := e.enforceInferenceServices(ctx, namespace)
	if err != nil {
		return enforced, err
	}
	enforced = append(enforced, inferenceServices...)

	pods, err := e.enforcePods(ctx, namespace)
	if err != nil {
		return enforced, err
	}
	enforced = append(enforced, pods...)

	return enforced, nil
}

// RestoreNamespace reverses enforcement for every resource previously
// recorded in enforcedResources, restoring original replica counts / suspend
// state. Resources that no longer exist are skipped. Deleted Pods have no
// restore action - deletion isn't reversible, so a "Pod"/Deleted entry is
// left in status purely as a historical record of what enforcement did.
func (e *Enforcer) RestoreNamespace(ctx context.Context, namespace string, enforcedResources []gpuquotav1alpha1.EnforcedResource) error {
	for _, res := range enforcedResources {
		var err error
		switch res.Kind {
		case "Deployment":
			err = e.restoreDeployment(ctx, namespace, res.Name)
		case "JobSet":
			err = e.restoreJobSet(ctx, namespace, res.Name)
		case "InferenceService":
			err = e.restoreInferenceService(ctx, namespace, res.Name)
		case "Pod":
			continue
		default:
			continue
		}
		if err != nil {
			return fmt.Errorf("restoring %s/%s: %w", res.Kind, res.Name, err)
		}
	}
	return nil
}

func (e *Enforcer) enforceDeployments(ctx context.Context, namespace string) ([]gpuquotav1alpha1.EnforcedResource, error) {
	var list appsv1.DeploymentList
	if err := e.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing deployments in %s: %w", namespace, err)
	}

	var enforced []gpuquotav1alpha1.EnforcedResource
	for i := range list.Items {
		dep := &list.Items[i]
		if dep.Spec.Replicas != nil && *dep.Spec.Replicas == 0 {
			continue
		}
		if !podTemplateRequestsGPU(dep.Spec.Template.Spec) {
			continue
		}

		original := int32(1)
		if dep.Spec.Replicas != nil {
			original = *dep.Spec.Replicas
		}
		if dep.Annotations == nil {
			dep.Annotations = map[string]string{}
		}
		dep.Annotations[AnnotationOriginalReplicas] = fmt.Sprintf("%d", original)
		zero := int32(0)
		dep.Spec.Replicas = &zero

		if err := e.Client.Update(ctx, dep); err != nil {
			return enforced, fmt.Errorf("scaling deployment %s/%s to zero: %w", namespace, dep.Name, err)
		}
		enforced = append(enforced, gpuquotav1alpha1.EnforcedResource{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
			Name:       dep.Name,
			Action:     ActionScaledToZero,
			EnforcedAt: metav1.Now(),
		})
	}
	return enforced, nil
}

func (e *Enforcer) restoreDeployment(ctx context.Context, namespace, name string) error {
	var dep appsv1.Deployment
	if err := e.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &dep); err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	raw, ok := dep.Annotations[AnnotationOriginalReplicas]
	if !ok {
		return nil
	}
	var original int32
	if _, err := fmt.Sscanf(raw, "%d", &original); err != nil {
		return fmt.Errorf("parsing original replica annotation %q: %w", raw, err)
	}
	dep.Spec.Replicas = &original
	delete(dep.Annotations, AnnotationOriginalReplicas)
	return e.Client.Update(ctx, &dep)
}

func (e *Enforcer) enforceJobSets(ctx context.Context, namespace string) ([]gpuquotav1alpha1.EnforcedResource, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(jobSetListGVK)
	if err := e.Client.List(ctx, list, client.InNamespace(namespace)); err != nil {
		if isNoKindMatch(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing jobsets in %s: %w", namespace, err)
	}

	var enforced []gpuquotav1alpha1.EnforcedResource
	for i := range list.Items {
		obj := &list.Items[i]
		suspended, found, _ := unstructured.NestedBool(obj.Object, "spec", "suspend")
		if found && suspended {
			continue
		}
		if !unstructuredRequestsGPU(obj.Object) {
			continue
		}

		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[AnnotationOriginalSuspend] = fmt.Sprintf("%t", found && suspended)
		obj.SetAnnotations(annotations)
		if err := unstructured.SetNestedField(obj.Object, true, "spec", "suspend"); err != nil {
			return enforced, fmt.Errorf("setting suspend on jobset %s/%s: %w", namespace, obj.GetName(), err)
		}

		if err := e.Client.Update(ctx, obj); err != nil {
			return enforced, fmt.Errorf("suspending jobset %s/%s: %w", namespace, obj.GetName(), err)
		}
		enforced = append(enforced, gpuquotav1alpha1.EnforcedResource{
			APIVersion: jobSetGVK.GroupVersion().String(),
			Kind:       jobSetGVK.Kind,
			Name:       obj.GetName(),
			Action:     ActionSuspended,
			EnforcedAt: metav1.Now(),
		})
	}
	return enforced, nil
}

func (e *Enforcer) restoreJobSet(ctx context.Context, namespace, name string) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(jobSetGVK)
	if err := e.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	annotations := obj.GetAnnotations()
	raw, ok := annotations[AnnotationOriginalSuspend]
	if !ok {
		return nil
	}
	original := raw == "true"
	if err := unstructured.SetNestedField(obj.Object, original, "spec", "suspend"); err != nil {
		return fmt.Errorf("restoring suspend on jobset %s/%s: %w", namespace, name, err)
	}
	delete(annotations, AnnotationOriginalSuspend)
	obj.SetAnnotations(annotations)
	return e.Client.Update(ctx, obj)
}

func (e *Enforcer) enforceInferenceServices(ctx context.Context, namespace string) ([]gpuquotav1alpha1.EnforcedResource, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(inferenceServiceListGVK)
	if err := e.Client.List(ctx, list, client.InNamespace(namespace)); err != nil {
		if isNoKindMatch(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing inferenceservices in %s: %w", namespace, err)
	}

	var enforced []gpuquotav1alpha1.EnforcedResource
	for i := range list.Items {
		obj := &list.Items[i]
		if !unstructuredRequestsGPU(obj.Object) {
			continue
		}

		original := map[string]componentReplicaSpec{}
		changed := false
		for _, component := range inferenceServiceComponents {
			spec, found, _ := unstructured.NestedMap(obj.Object, "spec", component)
			if !found {
				continue
			}
			if isZeroed(spec) {
				continue
			}
			original[component] = readReplicaSpec(spec)
			if err := unstructured.SetNestedField(obj.Object, int64(0), "spec", component, "minReplicas"); err != nil {
				return enforced, err
			}
			if err := unstructured.SetNestedField(obj.Object, int64(0), "spec", component, "maxReplicas"); err != nil {
				return enforced, err
			}
			changed = true
		}
		if !changed {
			continue
		}

		originalJSON, err := json.Marshal(original)
		if err != nil {
			return enforced, fmt.Errorf("marshaling original replica spec: %w", err)
		}
		annotations := obj.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[AnnotationOriginalReplicaSpec] = string(originalJSON)
		obj.SetAnnotations(annotations)

		if err := e.Client.Update(ctx, obj); err != nil {
			return enforced, fmt.Errorf("zeroing inferenceservice %s/%s: %w", namespace, obj.GetName(), err)
		}
		enforced = append(enforced, gpuquotav1alpha1.EnforcedResource{
			APIVersion: inferenceServiceGVK.GroupVersion().String(),
			Kind:       inferenceServiceGVK.Kind,
			Name:       obj.GetName(),
			Action:     ActionScaledToZero,
			EnforcedAt: metav1.Now(),
		})
	}
	return enforced, nil
}

func (e *Enforcer) restoreInferenceService(ctx context.Context, namespace, name string) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(inferenceServiceGVK)
	if err := e.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj); err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	annotations := obj.GetAnnotations()
	raw, ok := annotations[AnnotationOriginalReplicaSpec]
	if !ok {
		return nil
	}
	var original map[string]componentReplicaSpec
	if err := json.Unmarshal([]byte(raw), &original); err != nil {
		return fmt.Errorf("unmarshaling original replica spec for %s/%s: %w", namespace, name, err)
	}
	for component, spec := range original {
		if spec.MinReplicas != nil {
			if err := unstructured.SetNestedField(obj.Object, *spec.MinReplicas, "spec", component, "minReplicas"); err != nil {
				return err
			}
		} else {
			unstructured.RemoveNestedField(obj.Object, "spec", component, "minReplicas")
		}
		if spec.MaxReplicas != nil {
			if err := unstructured.SetNestedField(obj.Object, *spec.MaxReplicas, "spec", component, "maxReplicas"); err != nil {
				return err
			}
		} else {
			unstructured.RemoveNestedField(obj.Object, "spec", component, "maxReplicas")
		}
	}
	delete(annotations, AnnotationOriginalReplicaSpec)
	obj.SetAnnotations(annotations)
	return e.Client.Update(ctx, obj)
}

// enforcePods deletes standalone GPU Pods in the namespace: Pods with no
// controller owner reference at all (e.g. created directly via `kubectl run`
// or a bare manifest, not by a Deployment/JobSet/InferenceService/anything
// else). Pods owned by something are left alone here - either their owner is
// one of the kinds already handled above (in which case scaling/suspending
// the owner is the correct action, and the owner's controller will delete or
// recreate the Pod on its own schedule), or it's an owner kind this operator
// doesn't manage, in which case deleting the Pod out from under its
// controller would just cause an immediate, pointless recreation. Deletion
// is the only "scale to zero" primitive available for a bare Pod, and it is
// NOT reversible - restoring a deleted Pod is not attempted.
func (e *Enforcer) enforcePods(ctx context.Context, namespace string) ([]gpuquotav1alpha1.EnforcedResource, error) {
	var list corev1.PodList
	if err := e.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing pods in %s: %w", namespace, err)
	}

	var enforced []gpuquotav1alpha1.EnforcedResource
	for i := range list.Items {
		pod := &list.Items[i]
		if len(pod.OwnerReferences) > 0 {
			continue
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		if !podTemplateRequestsGPU(pod.Spec) {
			continue
		}

		if err := e.Client.Delete(ctx, pod); err != nil {
			if isNotFound(err) {
				continue
			}
			return enforced, fmt.Errorf("deleting pod %s/%s: %w", namespace, pod.Name, err)
		}
		enforced = append(enforced, gpuquotav1alpha1.EnforcedResource{
			APIVersion: "v1",
			Kind:       "Pod",
			Name:       pod.Name,
			Action:     ActionDeleted,
			EnforcedAt: metav1.Now(),
		})
	}
	return enforced, nil
}

func readReplicaSpec(spec map[string]interface{}) componentReplicaSpec {
	var out componentReplicaSpec
	if v, found, _ := unstructured.NestedInt64(spec, "minReplicas"); found {
		out.MinReplicas = &v
	}
	if v, found, _ := unstructured.NestedInt64(spec, "maxReplicas"); found {
		out.MaxReplicas = &v
	}
	return out
}

func isZeroed(spec map[string]interface{}) bool {
	minR, minFound, _ := unstructured.NestedInt64(spec, "minReplicas")
	maxR, maxFound, _ := unstructured.NestedInt64(spec, "maxReplicas")
	return minFound && maxFound && minR == 0 && maxR == 0
}

// podTemplateRequestsGPU reports whether any container in the pod spec
// requests or limits the GPU extended resource.
func podTemplateRequestsGPU(spec corev1.PodSpec) bool {
	containers := append([]corev1.Container{}, spec.Containers...)
	containers = append(containers, spec.InitContainers...)
	for _, c := range containers {
		if quantityPositive(c.Resources.Requests[GPUResourceName]) || quantityPositive(c.Resources.Limits[GPUResourceName]) {
			return true
		}
	}
	return false
}

func quantityPositive(q resource.Quantity) bool {
	return !q.IsZero()
}

// unstructuredRequestsGPU walks an arbitrary unstructured object looking for
// a "nvidia.com/gpu" resource key with a positive value anywhere in its
// tree. This is deliberately shape-agnostic since JobSet and InferenceService
// nest containers/resources differently across API versions.
func unstructuredRequestsGPU(obj map[string]interface{}) bool {
	return scanForGPURequest(obj)
}

func scanForGPURequest(node interface{}) bool {
	switch v := node.(type) {
	case map[string]interface{}:
		if raw, ok := v[string(GPUResourceName)]; ok {
			if s, ok := raw.(string); ok {
				if q, err := resource.ParseQuantity(s); err == nil && quantityPositive(q) {
					return true
				}
			}
		}
		for _, child := range v {
			if scanForGPURequest(child) {
				return true
			}
		}
	case []interface{}:
		for _, child := range v {
			if scanForGPURequest(child) {
				return true
			}
		}
	}
	return false
}

func isNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}

// isNoKindMatch reports whether the CRD backing an optional watched type
// (JobSet, InferenceService) isn't installed in the cluster, so listing it
// should be treated as "nothing to enforce" rather than a reconcile error.
func isNoKindMatch(err error) bool {
	return meta.IsNoMatchError(err) || apierrors.IsNotFound(err)
}
