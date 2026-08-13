package enforce

import (
	"context"
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
