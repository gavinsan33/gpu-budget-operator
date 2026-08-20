// Package enforce scales down (and, when unscalable, deletes) GPU-consuming
// workloads in a namespace that has exceeded its GpuBudget, and restores them
// once usage falls back within budget.
package enforce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gpubudgetv1alpha1 "github.com/gsanders/gpu-budget-operator/v1alpha1"
)

const (
	// GPUResourceName is the extended resource requested by GPU workloads.
	GPUResourceName = corev1.ResourceName("nvidia.com/gpu")

	// AnnotationOriginalReplicas remembers a Deployment's, StatefulSet's, or
	// standalone ReplicaSet's replica count before it was scaled to zero, so
	// it can be restored later.
	AnnotationOriginalReplicas = "gpubudget.io/original-replicas"

	// AnnotationOriginalReplicaSpec remembers an InferenceService component's
	// original min/max replicas as JSON before it was zeroed out.
	AnnotationOriginalReplicaSpec = "gpubudget.io/original-replica-spec"

	// AnnotationOriginalSuspend remembers a JobSet's or standalone Job's
	// original suspend value.
	AnnotationOriginalSuspend = "gpubudget.io/original-suspend"

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

// EnforceNamespace scales every GPU-requesting Deployment, StatefulSet, and
// standalone ReplicaSet (not owned by a Deployment) to zero replicas,
// suspends every GPU-requesting JobSet and standalone Job (not owned by a
// JobSet or CronJob), zeroes the replica bounds of every GPU-requesting
// InferenceService, and deletes standalone GPU Pods (ones with no
// controlling owner, since a bare Pod can't be scaled or suspended) in the
// namespace. It returns the set of resources it acted on.
//
// It always attempts every resource kind, even after an earlier kind fails
// (returning a combined error via errors.Join if any did) - and it always
// returns every resource it successfully acted on so far, alongside that
// error, rather than only the resources from kinds that succeeded before the
// first failure. The caller MUST persist the returned slice regardless of
// whether an error also came back: a resource kind failing here (e.g. a
// transient resourceVersion conflict, common since some workload controllers
// - KServe's, notably - write back to the same object's status concurrently)
// used to make this function bail out immediately, discarding the fact that
// earlier kinds in the same call had already been successfully scaled to
// zero and annotated. The caller had no record that they were now enforced,
// so a later gpubudget.io/reset found nothing to restore for them - the
// workload just stayed at zero forever with no error ever surfaced.
func (e *Enforcer) EnforceNamespace(ctx context.Context, namespace string) ([]gpubudgetv1alpha1.EnforcedResource, error) {
	var enforced []gpubudgetv1alpha1.EnforcedResource
	var errs []error

	for _, step := range []func(context.Context, string) ([]gpubudgetv1alpha1.EnforcedResource, error){
		e.enforceDeployments,
		e.enforceStatefulSets,
		e.enforceReplicaSets,
		e.enforceJobSets,
		e.enforceJobs,
		e.enforceInferenceServices,
		e.enforcePods,
	} {
		res, err := step(ctx, namespace)
		enforced = append(enforced, res...)
		if err != nil {
			errs = append(errs, err)
		}
	}

	return enforced, errors.Join(errs...)
}

// RestoreNamespace reverses enforcement for every resource previously
// recorded in enforcedResources, restoring original replica counts / suspend
// state. Resources that no longer exist are skipped. Deleted Pods have no
// restore action - deletion isn't reversible, so a "Pod"/Deleted entry is
// left in status purely as a historical record of what enforcement did.
//
// It always attempts every entry, even after an earlier one fails, returning
// a combined error via errors.Join if any did - mirroring EnforceNamespace.
// Stopping at the first failure (the original behavior) meant one entry
// stuck failing (a persistent conflict, a resource an admission webhook now
// rejects the restore of, etc.) permanently head-of-line-blocked every entry
// after it in the same list, on every single retry, since the caller always
// retries the same (unrestored) list in the same order after any error.
// Restoring the rest first and surfacing one combined failure gets everything
// restorable actually restored, rather than nothing past the first snag.
func (e *Enforcer) RestoreNamespace(ctx context.Context, namespace string, enforcedResources []gpubudgetv1alpha1.EnforcedResource) error {
	var errs []error
	for _, res := range enforcedResources {
		var err error
		switch res.Kind {
		case "Deployment":
			err = e.restoreDeployment(ctx, namespace, res.Name)
		case "ReplicaSet":
			err = e.restoreReplicaSet(ctx, namespace, res.Name)
		case "StatefulSet":
			err = e.restoreStatefulSet(ctx, namespace, res.Name)
		case "JobSet":
			err = e.restoreJobSet(ctx, namespace, res.Name)
		case "Job":
			err = e.restoreJob(ctx, namespace, res.Name)
		case "InferenceService":
			err = e.restoreInferenceService(ctx, namespace, res.Name)
		case "Pod":
			continue
		default:
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("restoring %s/%s: %w", res.Kind, res.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (e *Enforcer) enforceDeployments(ctx context.Context, namespace string) ([]gpubudgetv1alpha1.EnforcedResource, error) {
	var list appsv1.DeploymentList
	if err := e.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing deployments in %s: %w", namespace, err)
	}

	var enforced []gpubudgetv1alpha1.EnforcedResource
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
		enforced = append(enforced, gpubudgetv1alpha1.EnforcedResource{
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

// enforceReplicaSets scales standalone GPU-requesting ReplicaSets (i.e. ones
// with no owner reference - not the child ReplicaSet of a Deployment) to
// zero replicas. ReplicaSets owned by a Deployment are skipped: the
// Deployment's own enforcement is the correct action there, and scaling the
// child ReplicaSet directly would just be overwritten by the Deployment
// controller reconciling it back to the Deployment's desired replica count.
// enforceStatefulSets scales GPU-requesting StatefulSets to zero replicas,
// mirroring enforceDeployments. Unlike ReplicaSet there's no vanilla
// higher-level controller that owns a StatefulSet, so (unlike ReplicaSets)
// no owner-reference check is needed here.
func (e *Enforcer) enforceStatefulSets(ctx context.Context, namespace string) ([]gpubudgetv1alpha1.EnforcedResource, error) {
	var list appsv1.StatefulSetList
	if err := e.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing statefulsets in %s: %w", namespace, err)
	}

	var enforced []gpubudgetv1alpha1.EnforcedResource
	for i := range list.Items {
		sts := &list.Items[i]
		if sts.Spec.Replicas != nil && *sts.Spec.Replicas == 0 {
			continue
		}
		if !podTemplateRequestsGPU(sts.Spec.Template.Spec) {
			continue
		}

		original := int32(1)
		if sts.Spec.Replicas != nil {
			original = *sts.Spec.Replicas
		}
		if sts.Annotations == nil {
			sts.Annotations = map[string]string{}
		}
		sts.Annotations[AnnotationOriginalReplicas] = fmt.Sprintf("%d", original)
		zero := int32(0)
		sts.Spec.Replicas = &zero

		if err := e.Client.Update(ctx, sts); err != nil {
			return enforced, fmt.Errorf("scaling statefulset %s/%s to zero: %w", namespace, sts.Name, err)
		}
		enforced = append(enforced, gpubudgetv1alpha1.EnforcedResource{
			APIVersion: "apps/v1",
			Kind:       "StatefulSet",
			Name:       sts.Name,
			Action:     ActionScaledToZero,
			EnforcedAt: metav1.Now(),
		})
	}
	return enforced, nil
}

func (e *Enforcer) restoreStatefulSet(ctx context.Context, namespace, name string) error {
	var sts appsv1.StatefulSet
	if err := e.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sts); err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	raw, ok := sts.Annotations[AnnotationOriginalReplicas]
	if !ok {
		return nil
	}
	var original int32
	if _, err := fmt.Sscanf(raw, "%d", &original); err != nil {
		return fmt.Errorf("parsing original replica annotation %q: %w", raw, err)
	}
	sts.Spec.Replicas = &original
	delete(sts.Annotations, AnnotationOriginalReplicas)
	return e.Client.Update(ctx, &sts)
}

func (e *Enforcer) enforceReplicaSets(ctx context.Context, namespace string) ([]gpubudgetv1alpha1.EnforcedResource, error) {
	var list appsv1.ReplicaSetList
	if err := e.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing replicasets in %s: %w", namespace, err)
	}

	var enforced []gpubudgetv1alpha1.EnforcedResource
	for i := range list.Items {
		rs := &list.Items[i]
		if len(rs.OwnerReferences) > 0 {
			continue
		}
		if rs.Spec.Replicas != nil && *rs.Spec.Replicas == 0 {
			continue
		}
		if !podTemplateRequestsGPU(rs.Spec.Template.Spec) {
			continue
		}

		original := int32(1)
		if rs.Spec.Replicas != nil {
			original = *rs.Spec.Replicas
		}
		if rs.Annotations == nil {
			rs.Annotations = map[string]string{}
		}
		rs.Annotations[AnnotationOriginalReplicas] = fmt.Sprintf("%d", original)
		zero := int32(0)
		rs.Spec.Replicas = &zero

		if err := e.Client.Update(ctx, rs); err != nil {
			return enforced, fmt.Errorf("scaling replicaset %s/%s to zero: %w", namespace, rs.Name, err)
		}
		enforced = append(enforced, gpubudgetv1alpha1.EnforcedResource{
			APIVersion: "apps/v1",
			Kind:       "ReplicaSet",
			Name:       rs.Name,
			Action:     ActionScaledToZero,
			EnforcedAt: metav1.Now(),
		})
	}
	return enforced, nil
}

func (e *Enforcer) restoreReplicaSet(ctx context.Context, namespace, name string) error {
	var rs appsv1.ReplicaSet
	if err := e.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &rs); err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	raw, ok := rs.Annotations[AnnotationOriginalReplicas]
	if !ok {
		return nil
	}
	var original int32
	if _, err := fmt.Sscanf(raw, "%d", &original); err != nil {
		return fmt.Errorf("parsing original replica annotation %q: %w", raw, err)
	}
	rs.Spec.Replicas = &original
	delete(rs.Annotations, AnnotationOriginalReplicas)
	return e.Client.Update(ctx, &rs)
}

func (e *Enforcer) enforceJobSets(ctx context.Context, namespace string) ([]gpubudgetv1alpha1.EnforcedResource, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(jobSetListGVK)
	if err := e.Client.List(ctx, list, client.InNamespace(namespace)); err != nil {
		if isNoKindMatch(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing jobsets in %s: %w", namespace, err)
	}

	var enforced []gpubudgetv1alpha1.EnforcedResource
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
		enforced = append(enforced, gpubudgetv1alpha1.EnforcedResource{
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

// enforceJobs suspends standalone GPU-requesting Jobs (i.e. ones with no
// owner reference - not a Job created by a JobSet or a CronJob) via the
// native batch/v1 spec.suspend field. Owned Jobs are skipped: a JobSet's
// child Job is already covered by suspending the JobSet, and a CronJob's
// child Job is a single run that should complete or be handled by
// suspending the CronJob (a future enhancement) rather than fought here.
func (e *Enforcer) enforceJobs(ctx context.Context, namespace string) ([]gpubudgetv1alpha1.EnforcedResource, error) {
	var list batchv1.JobList
	if err := e.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing jobs in %s: %w", namespace, err)
	}

	var enforced []gpubudgetv1alpha1.EnforcedResource
	for i := range list.Items {
		job := &list.Items[i]
		if len(job.OwnerReferences) > 0 {
			continue
		}
		if job.Spec.Suspend != nil && *job.Spec.Suspend {
			continue
		}
		if !podTemplateRequestsGPU(job.Spec.Template.Spec) {
			continue
		}

		if job.Annotations == nil {
			job.Annotations = map[string]string{}
		}
		job.Annotations[AnnotationOriginalSuspend] = "false"
		suspend := true
		job.Spec.Suspend = &suspend

		if err := e.Client.Update(ctx, job); err != nil {
			return enforced, fmt.Errorf("suspending job %s/%s: %w", namespace, job.Name, err)
		}
		enforced = append(enforced, gpubudgetv1alpha1.EnforcedResource{
			APIVersion: "batch/v1",
			Kind:       "Job",
			Name:       job.Name,
			Action:     ActionSuspended,
			EnforcedAt: metav1.Now(),
		})
	}
	return enforced, nil
}

func (e *Enforcer) restoreJob(ctx context.Context, namespace, name string) error {
	var job batchv1.Job
	if err := e.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &job); err != nil {
		if isNotFound(err) {
			return nil
		}
		return err
	}
	raw, ok := job.Annotations[AnnotationOriginalSuspend]
	if !ok {
		return nil
	}
	original := raw == "true"
	job.Spec.Suspend = &original
	delete(job.Annotations, AnnotationOriginalSuspend)
	return e.Client.Update(ctx, &job)
}

func (e *Enforcer) enforceInferenceServices(ctx context.Context, namespace string) ([]gpubudgetv1alpha1.EnforcedResource, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(inferenceServiceListGVK)
	if err := e.Client.List(ctx, list, client.InNamespace(namespace)); err != nil {
		if isNoKindMatch(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("listing inferenceservices in %s: %w", namespace, err)
	}

	var enforced []gpubudgetv1alpha1.EnforcedResource
	for i := range list.Items {
		obj := &list.Items[i]
		if !unstructuredRequestsGPU(obj.Object) {
			continue
		}

		// Idempotency is keyed off whether a component's original was
		// already captured in the annotation, NOT off whether the live
		// spec currently reads as zeroed - KServe's own controller/webhook
		// strips maxReplicas back out of the spec once it's 0 (treating 0
		// as "unset" rather than "pinned to zero"), so re-deriving
		// "already enforced" from the live spec would see minReplicas=0,
		// maxReplicas=absent, wrongly conclude this component was never
		// enforced, and re-capture that already-zeroed state as if it were
		// the original - permanently losing the real original before
		// gpubudget.io/reset ever gets a chance to use it.
		annotations := obj.GetAnnotations()
		original := map[string]componentReplicaSpec{}
		if raw, ok := annotations[AnnotationOriginalReplicaSpec]; ok {
			if err := json.Unmarshal([]byte(raw), &original); err != nil {
				return enforced, fmt.Errorf("unmarshaling existing original replica spec for %s/%s: %w", namespace, obj.GetName(), err)
			}
		}

		// newlyCaptured tracks whether any component's original was
		// captured for the first time this pass (drives whether an
		// EnforcedResource gets recorded); needsUpdate tracks whether the
		// object needs writing back at all, which also covers the
		// re-affirm-only case where every component's original was already
		// captured on an earlier pass but the live spec drifted since.
		newlyCaptured := false
		needsUpdate := false
		for _, component := range inferenceServiceComponents {
			if _, alreadyCaptured := original[component]; alreadyCaptured {
				// Original already recorded - just re-affirm zero in case
				// something reset it, without touching the recorded value.
				if err := unstructured.SetNestedField(obj.Object, int64(0), "spec", component, "minReplicas"); err != nil {
					return enforced, err
				}
				if err := unstructured.SetNestedField(obj.Object, int64(0), "spec", component, "maxReplicas"); err != nil {
					return enforced, err
				}
				needsUpdate = true
				continue
			}
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
			newlyCaptured = true
			needsUpdate = true
		}
		if !needsUpdate {
			continue
		}

		originalJSON, err := json.Marshal(original)
		if err != nil {
			return enforced, fmt.Errorf("marshaling original replica spec: %w", err)
		}
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[AnnotationOriginalReplicaSpec] = string(originalJSON)
		obj.SetAnnotations(annotations)

		if err := e.Client.Update(ctx, obj); err != nil {
			return enforced, fmt.Errorf("zeroing inferenceservice %s/%s: %w", namespace, obj.GetName(), err)
		}
		if newlyCaptured {
			enforced = append(enforced, gpubudgetv1alpha1.EnforcedResource{
				APIVersion: inferenceServiceGVK.GroupVersion().String(),
				Kind:       inferenceServiceGVK.Kind,
				Name:       obj.GetName(),
				Action:     ActionScaledToZero,
				EnforcedAt: metav1.Now(),
			})
		}
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
// controller owner reference at all (e.g. created directly via `oc run`
// or a bare manifest, not by a Deployment/JobSet/InferenceService/anything
// else). Pods owned by something are left alone here - either their owner is
// one of the kinds already handled above (in which case scaling/suspending
// the owner is the correct action, and the owner's controller will delete or
// recreate the Pod on its own schedule), or it's an owner kind this operator
// doesn't manage, in which case deleting the Pod out from under its
// controller would just cause an immediate, pointless recreation. Deletion
// is the only "scale to zero" primitive available for a bare Pod, and it is
// NOT reversible - restoring a deleted Pod is not attempted.
func (e *Enforcer) enforcePods(ctx context.Context, namespace string) ([]gpubudgetv1alpha1.EnforcedResource, error) {
	var list corev1.PodList
	if err := e.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing pods in %s: %w", namespace, err)
	}

	var enforced []gpubudgetv1alpha1.EnforcedResource
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
		enforced = append(enforced, gpubudgetv1alpha1.EnforcedResource{
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
