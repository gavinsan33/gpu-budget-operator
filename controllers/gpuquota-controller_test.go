package controllers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuquotav1alpha1 "github.com/gsanders/gpu-quota-operator/v1alpha1"
)

// promStub serves a fixed active-GPU count from a fake Prometheus /api/v1/query endpoint.
func promStub(t *testing.T, gpuCount int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"%d"]}]}}`, gpuCount)
	}))
}

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := gpuquotav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func gpuDeployment(namespace, name string, replicas int32) *appsv1.Deployment {
	one := replicas
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &one,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "main",
						Image: "example.com/model:latest",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
							},
						},
					}},
				},
			},
		},
	}
}

func TestReconcile_CompliantUsageDoesNotEnforce(t *testing.T) {
	prom := promStub(t, 2)
	defer prom.Close()

	scheme := newScheme(t)
	dep := gpuDeployment("team-a", "model", 3)
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "team-a"},
		Spec:       gpuquotav1alpha1.GpuQuotaSpec{GPULimit: 4, PrometheusURL: prom.URL},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, gq).WithStatusSubresource(gq).Build()

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Fatalf("expected positive requeue interval, got %v", res.RequeueAfter)
	}

	var got gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != gpuquotav1alpha1.PhaseCompliant {
		t.Fatalf("expected Compliant phase, got %s", got.Status.Phase)
	}

	var gotDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDep); err != nil {
		t.Fatal(err)
	}
	if *gotDep.Spec.Replicas != 3 {
		t.Fatalf("expected deployment untouched at 3 replicas, got %d", *gotDep.Spec.Replicas)
	}
}

func TestReconcile_ViolationWaitsOutGracePeriodBeforeEnforcing(t *testing.T) {
	prom := promStub(t, 10)
	defer prom.Close()

	scheme := newScheme(t)
	dep := gpuDeployment("team-a", "model", 3)
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "team-a"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			GPULimit:      4,
			PrometheusURL: prom.URL,
			GracePeriod:   metav1.Duration{Duration: time.Hour}, // long enough that a single reconcile won't cross it
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, gq).WithStatusSubresource(gq).Build()

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != gpuquotav1alpha1.PhaseViolating {
		t.Fatalf("expected Violating phase during grace period, got %s", got.Status.Phase)
	}
	if got.Status.FirstViolationTime == nil {
		t.Fatal("expected FirstViolationTime to be set")
	}

	var gotDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDep); err != nil {
		t.Fatal(err)
	}
	if *gotDep.Spec.Replicas != 3 {
		t.Fatalf("expected deployment untouched during grace period, got %d replicas", *gotDep.Spec.Replicas)
	}
}

func TestReconcile_EnforcesAfterGracePeriodThenRestoresOnCompliance(t *testing.T) {
	prom := promStub(t, 10)
	scheme := newScheme(t)
	dep := gpuDeployment("team-a", "model", 3)
	past := metav1.NewTime(time.Now().Add(-time.Hour))
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "team-a"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			GPULimit:      4,
			PrometheusURL: prom.URL,
			GracePeriod:   metav1.Duration{Duration: time.Minute},
			AutoRestore:   true,
		},
		Status: gpuquotav1alpha1.GpuQuotaStatus{
			FirstViolationTime: &past, // simulate the grace period already having elapsed
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, gq).WithStatusSubresource(gq).Build()

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != gpuquotav1alpha1.PhaseEnforced {
		t.Fatalf("expected Enforced phase, got %s", got.Status.Phase)
	}
	if len(got.Status.EnforcedResources) != 1 {
		t.Fatalf("expected 1 enforced resource, got %d", len(got.Status.EnforcedResources))
	}

	var gotDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDep); err != nil {
		t.Fatal(err)
	}
	if *gotDep.Spec.Replicas != 0 {
		t.Fatalf("expected deployment scaled to 0, got %d replicas", *gotDep.Spec.Replicas)
	}
	if gotDep.Annotations["gpuquota.example.com/original-replicas"] != "3" {
		t.Fatalf("expected original replicas annotation to record 3, got %q", gotDep.Annotations["gpuquota.example.com/original-replicas"])
	}

	// Now usage drops back under quota; the enforced deployment should be restored.
	prom.Close()
	prom = promStub(t, 1)
	defer prom.Close()
	got.Spec.PrometheusURL = prom.URL
	if err := c.Update(context.Background(), &got); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile after recovery: %v", err)
	}

	var afterRestore gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &afterRestore); err != nil {
		t.Fatal(err)
	}
	if afterRestore.Status.Phase != gpuquotav1alpha1.PhaseCompliant {
		t.Fatalf("expected Compliant phase after restore, got %s", afterRestore.Status.Phase)
	}
	if len(afterRestore.Status.EnforcedResources) != 0 {
		t.Fatalf("expected enforced resources cleared after restore, got %d", len(afterRestore.Status.EnforcedResources))
	}

	var restoredDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &restoredDep); err != nil {
		t.Fatal(err)
	}
	if *restoredDep.Spec.Replicas != 3 {
		t.Fatalf("expected deployment restored to 3 replicas, got %d", *restoredDep.Spec.Replicas)
	}
	if _, ok := restoredDep.Annotations["gpuquota.example.com/original-replicas"]; ok {
		t.Fatal("expected original-replicas annotation to be removed after restore")
	}
}

func gpuPod(namespace, name string, owned bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "main",
				Image: "example.com/model:latest",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
					},
				},
			}},
		},
	}
	if owned {
		pod.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1",
			Kind:       "ReplicaSet",
			Name:       "some-replicaset",
			UID:        "some-uid",
		}}
	}
	return pod
}

func TestReconcile_DeletesBareGPUPodButLeavesOwnedPodAlone(t *testing.T) {
	prom := promStub(t, 10)
	defer prom.Close()

	scheme := newScheme(t)
	barePod := gpuPod("team-a", "bare-gpu-pod", false)
	ownedPod := gpuPod("team-a", "owned-gpu-pod", true)
	past := metav1.NewTime(time.Now().Add(-time.Hour))
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "team-a"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			GPULimit:      4,
			PrometheusURL: prom.URL,
			GracePeriod:   metav1.Duration{Duration: time.Minute},
		},
		Status: gpuquotav1alpha1.GpuQuotaStatus{FirstViolationTime: &past},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(barePod, ownedPod, gq).WithStatusSubresource(gq).Build()

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace("team-a")); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 || pods.Items[0].Name != "owned-gpu-pod" {
		t.Fatalf("expected only the owned pod to survive, got %+v", pods.Items)
	}

	var got gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &got); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, res := range got.Status.EnforcedResources {
		if res.Kind == "Pod" && res.Name == "bare-gpu-pod" && res.Action == "Deleted" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected EnforcedResources to record bare-gpu-pod deletion, got %+v", got.Status.EnforcedResources)
	}
}

func TestReconcile_NonGPUDeploymentIsNeverTouched(t *testing.T) {
	prom := promStub(t, 10)
	defer prom.Close()

	scheme := newScheme(t)
	dep := gpuDeployment("team-a", "plain", 3)
	dep.Spec.Template.Spec.Containers[0].Resources.Requests = nil // strip the GPU request
	past := metav1.NewTime(time.Now().Add(-time.Hour))
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "team-a"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			GPULimit:      4,
			PrometheusURL: prom.URL,
			GracePeriod:   metav1.Duration{Duration: time.Minute},
		},
		Status: gpuquotav1alpha1.GpuQuotaStatus{FirstViolationTime: &past},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, gq).WithStatusSubresource(gq).Build()

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var gotDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDep); err != nil {
		t.Fatal(err)
	}
	if *gotDep.Spec.Replicas != 3 {
		t.Fatalf("expected non-GPU deployment untouched, got %d replicas", *gotDep.Spec.Replicas)
	}
}
