package enforce

import (
	"context"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	gpubudgetv1alpha1 "github.com/gsanders/gpu-budget-operator/v1alpha1"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func gpuDeployment(namespace, name string, replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       gpuPodSpec(),
			},
		},
	}
}

func gpuStatefulSet(namespace, name string, replicas int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: name,
			Selector:    &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       gpuPodSpec(),
			},
		},
	}
}

func gpuReplicaSet(namespace, name string, replicas int32) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec:       gpuPodSpec(),
			},
		},
	}
}

func gpuPodSpec() corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "example.com/model:latest",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
				},
			},
		}},
	}
}

// TestEnforceNamespace_ContinuesPastOneKindsFailure reproduces the bug where
// a transient conflict enforcing one resource kind (StatefulSets, here)
// prevented every kind processed after it (ReplicaSets, in the real
// EnforceNamespace ordering) from being enforced at all, even though nothing
// was wrong with them - EnforceNamespace used to bail out on the very first
// error. It also asserts the Deployment enforced before the failure isn't
// lost from the returned slice either.
func TestEnforceNamespace_ContinuesPastOneKindsFailure(t *testing.T) {
	scheme := newScheme(t)
	namespace := "team-a"
	dep := gpuDeployment(namespace, "dep", 1)
	sts := gpuStatefulSet(namespace, "sts", 1)
	rs := gpuReplicaSet(namespace, "rs", 1)

	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, sts, rs).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*appsv1.StatefulSet); ok {
				return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "statefulsets"}, obj.GetName(), nil)
			}
			return cli.Update(ctx, obj, opts...)
		},
	})

	e := &Enforcer{Client: c}
	enforced, err := e.EnforceNamespace(context.Background(), namespace)
	if err == nil {
		t.Fatal("expected an error from the failed StatefulSet update, got nil")
	}

	kinds := map[string]bool{}
	for _, res := range enforced {
		kinds[res.Kind+"/"+res.Name] = true
	}
	if !kinds["Deployment/dep"] {
		t.Errorf("expected Deployment/dep in enforced (processed before the failing kind), got %+v", enforced)
	}
	if !kinds["ReplicaSet/rs"] {
		t.Errorf("expected ReplicaSet/rs in enforced (processed after the failing kind) - EnforceNamespace must keep going past one kind's failure, got %+v", enforced)
	}
	if kinds["StatefulSet/sts"] {
		t.Errorf("did not expect StatefulSet/sts in enforced, its update failed: %+v", enforced)
	}

	var gotDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "dep"}, &gotDep); err != nil {
		t.Fatal(err)
	}
	if *gotDep.Spec.Replicas != 0 {
		t.Errorf("expected Deployment/dep scaled to 0, got %d", *gotDep.Spec.Replicas)
	}

	var gotRS appsv1.ReplicaSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "rs"}, &gotRS); err != nil {
		t.Fatal(err)
	}
	if *gotRS.Spec.Replicas != 0 {
		t.Errorf("expected ReplicaSet/rs scaled to 0 despite StatefulSet/sts failing first, got %d", *gotRS.Spec.Replicas)
	}

	var gotSTS appsv1.StatefulSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "sts"}, &gotSTS); err != nil {
		t.Fatal(err)
	}
	if *gotSTS.Spec.Replicas != 1 {
		t.Errorf("expected StatefulSet/sts untouched at 1 replica (its update failed), got %d", *gotSTS.Spec.Replicas)
	}
}

// zeroedDeployment/zeroedStatefulSet/zeroedReplicaSet build a resource in the
// already-enforced state EnforceNamespace would have left it in: 0 replicas,
// carrying AnnotationOriginalReplicas - i.e. exactly what RestoreNamespace is
// meant to reverse.
func zeroedDeployment(namespace, name string, originalReplicas int32) *appsv1.Deployment {
	dep := gpuDeployment(namespace, name, 0)
	dep.Annotations = map[string]string{AnnotationOriginalReplicas: fmt.Sprintf("%d", originalReplicas)}
	return dep
}

func zeroedStatefulSet(namespace, name string, originalReplicas int32) *appsv1.StatefulSet {
	sts := gpuStatefulSet(namespace, name, 0)
	sts.Annotations = map[string]string{AnnotationOriginalReplicas: fmt.Sprintf("%d", originalReplicas)}
	return sts
}

func zeroedReplicaSet(namespace, name string, originalReplicas int32) *appsv1.ReplicaSet {
	rs := gpuReplicaSet(namespace, name, 0)
	rs.Annotations = map[string]string{AnnotationOriginalReplicas: fmt.Sprintf("%d", originalReplicas)}
	return rs
}

func enforcedResource(kind, name string) gpubudgetv1alpha1.EnforcedResource {
	return gpubudgetv1alpha1.EnforcedResource{Kind: kind, Name: name}
}

// TestRestoreNamespace_ContinuesPastOneEntrysFailure reproduces the
// "reapply the quota to raise the limit, then reset" scenario reported
// against a real cluster: restoring a namespace with several previously
// enforced resources, where one resource's restore fails (a persistent
// conflict, here simulated on the StatefulSet). RestoreNamespace used to
// stop at the first failure, permanently head-of-line-blocking every
// resource listed after it - since the caller always retries the exact
// same (still-unrestored) list, in the same order, after any error, so a
// resource stuck failing meant nothing after it in the list ever got
// restored, on any retry. It must instead restore everything it can and
// report the failure(s) alongside that.
func TestRestoreNamespace_ContinuesPastOneEntrysFailure(t *testing.T) {
	scheme := newScheme(t)
	namespace := "team-a"
	dep := zeroedDeployment(namespace, "dep", 1)
	sts := zeroedStatefulSet(namespace, "sts", 1)
	rs := zeroedReplicaSet(namespace, "rs", 1)

	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, sts, rs).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*appsv1.StatefulSet); ok {
				return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "statefulsets"}, obj.GetName(), nil)
			}
			return cli.Update(ctx, obj, opts...)
		},
	})

	e := &Enforcer{Client: c}
	enforced := []gpubudgetv1alpha1.EnforcedResource{
		enforcedResource("Deployment", "dep"),
		enforcedResource("StatefulSet", "sts"),
		enforcedResource("ReplicaSet", "rs"),
	}
	err := e.RestoreNamespace(context.Background(), namespace, enforced)
	if err == nil {
		t.Fatal("expected an error from the failed StatefulSet restore, got nil")
	}

	var gotDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "dep"}, &gotDep); err != nil {
		t.Fatal(err)
	}
	if *gotDep.Spec.Replicas != 1 {
		t.Errorf("expected Deployment/dep restored to 1 replica despite StatefulSet/sts failing, got %d", *gotDep.Spec.Replicas)
	}
	if _, ok := gotDep.Annotations[AnnotationOriginalReplicas]; ok {
		t.Errorf("expected Deployment/dep's original-replicas annotation removed, still present: %+v", gotDep.Annotations)
	}

	var gotRS appsv1.ReplicaSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "rs"}, &gotRS); err != nil {
		t.Fatal(err)
	}
	if *gotRS.Spec.Replicas != 1 {
		t.Errorf("expected ReplicaSet/rs restored to 1 replica (listed after the failing StatefulSet) despite it failing, got %d", *gotRS.Spec.Replicas)
	}

	var gotSTS appsv1.StatefulSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "sts"}, &gotSTS); err != nil {
		t.Fatal(err)
	}
	if *gotSTS.Spec.Replicas != 0 {
		t.Errorf("expected StatefulSet/sts to remain at 0 replicas (its restore failed), got %d", *gotSTS.Spec.Replicas)
	}
	if _, ok := gotSTS.Annotations[AnnotationOriginalReplicas]; !ok {
		t.Error("expected StatefulSet/sts's original-replicas annotation to remain (restore failed), it was removed")
	}
}

// TestRestoreNamespace_RetryAfterPartialFailureFinishesTheRest simulates the
// natural retry that follows TestRestoreNamespace_ContinuesPastOneEntrysFailure:
// the caller (Reconcile, via controller-runtime's error requeue) calls
// RestoreNamespace again with the exact same list. Resources already
// restored on the first pass must be left alone (no annotation to key off
// of - a safe no-op), and the previously-failing one must now succeed once
// its transient conflict clears.
func TestRestoreNamespace_RetryAfterPartialFailureFinishesTheRest(t *testing.T) {
	scheme := newScheme(t)
	namespace := "team-a"
	dep := zeroedDeployment(namespace, "dep", 1)
	sts := zeroedStatefulSet(namespace, "sts", 1)

	failSTS := true
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, sts).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*appsv1.StatefulSet); ok && failSTS {
				return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "statefulsets"}, obj.GetName(), nil)
			}
			return cli.Update(ctx, obj, opts...)
		},
	})

	e := &Enforcer{Client: c}
	enforced := []gpubudgetv1alpha1.EnforcedResource{
		enforcedResource("Deployment", "dep"),
		enforcedResource("StatefulSet", "sts"),
	}

	if err := e.RestoreNamespace(context.Background(), namespace, enforced); err == nil {
		t.Fatal("expected the first RestoreNamespace call to fail on StatefulSet/sts")
	}

	failSTS = false
	if err := e.RestoreNamespace(context.Background(), namespace, enforced); err != nil {
		t.Fatalf("expected the retry to succeed once the conflict clears, got: %v", err)
	}

	var gotDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "dep"}, &gotDep); err != nil {
		t.Fatal(err)
	}
	if *gotDep.Spec.Replicas != 1 {
		t.Errorf("expected Deployment/dep still at 1 replica after the retry (already restored, must be a no-op), got %d", *gotDep.Spec.Replicas)
	}

	var gotSTS appsv1.StatefulSet
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: "sts"}, &gotSTS); err != nil {
		t.Fatal(err)
	}
	if *gotSTS.Spec.Replicas != 1 {
		t.Errorf("expected StatefulSet/sts restored to 1 replica on the retry, got %d", *gotSTS.Spec.Replicas)
	}
	if _, ok := gotSTS.Annotations[AnnotationOriginalReplicas]; ok {
		t.Error("expected StatefulSet/sts's original-replicas annotation removed after the successful retry")
	}
}
