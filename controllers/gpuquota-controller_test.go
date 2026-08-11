package controllers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpuquotav1alpha1 "github.com/gsanders/gpu-quota-operator/v1alpha1"
)

// promStub serves a fixed GPU-hours-by-type response from a fake Prometheus
// /api/v1/query endpoint. hoursByType maps gpuType -> cumulative GPU-hours.
func promStub(t *testing.T, hoursByType map[string]float64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		samples := make([]string, 0, len(hoursByType))
		for gpuType, hours := range hoursByType {
			samples = append(samples, fmt.Sprintf(`{"metric":{"gpuType":%q},"value":[1700000000,"%v"]}`, gpuType, hours))
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[%s]}}`, joinJSON(samples))
	}))
}

func joinJSON(samples []string) string {
	out := ""
	for i, s := range samples {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
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
	if err := batchv1.AddToScheme(scheme); err != nil {
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

func gpuReplicaSet(namespace, name string, replicas int32, owned bool) *appsv1.ReplicaSet {
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.ReplicaSetSpec{
			Replicas: &replicas,
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
	if owned {
		rs.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1",
			Kind:       "Deployment",
			Name:       "some-deployment",
			UID:        "some-uid",
		}}
	}
	return rs
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

func gpuJob(namespace, name string, owned bool) *batchv1.Job {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "main",
						Image: "example.com/train:latest",
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
	if owned {
		job.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "batch/v1",
			Kind:       "CronJob",
			Name:       "some-cronjob",
			UID:        "some-uid",
		}}
	}
	return job
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

func float64Ptr(v float64) *float64 { return &v }

func TestReconcile_UnderBudgetDoesNotEnforce(t *testing.T) {
	prom := promStub(t, map[string]float64{"A100": 2})
	defer prom.Close()

	scheme := newScheme(t)
	dep := gpuDeployment("gavin-test", "model", 3)
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "gavin-test"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			Period:        gpuquotav1alpha1.PeriodMonthly,
			GPUHoursLimit: float64Ptr(10),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, gq).WithStatusSubresource(gq).Build()

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL}
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
	if got.Status.GPUHoursUsed != 2 {
		t.Fatalf("expected GPUHoursUsed=2, got %v", got.Status.GPUHoursUsed)
	}
	if got.Status.CurrentPeriodStart == nil {
		t.Fatal("expected CurrentPeriodStart to be set")
	}

	var gotDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDep); err != nil {
		t.Fatal(err)
	}
	if *gotDep.Spec.Replicas != 3 {
		t.Fatalf("expected deployment untouched at 3 replicas, got %d", *gotDep.Spec.Replicas)
	}
}

func TestReconcile_OverGPUHoursLimitEnforcesImmediately(t *testing.T) {
	prom := promStub(t, map[string]float64{"A100": 50})
	defer prom.Close()

	scheme := newScheme(t)
	dep := gpuDeployment("gavin-test", "model", 3)
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "gavin-test"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			Period:        gpuquotav1alpha1.PeriodMonthly,
			GPUHoursLimit: float64Ptr(10),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, gq).WithStatusSubresource(gq).Build()

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != gpuquotav1alpha1.PhaseEnforced {
		t.Fatalf("expected Enforced phase on the very first over-budget reconcile (no grace period), got %s", got.Status.Phase)
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
}

func TestReconcile_DollarsLimitExceededEvenWhenGPUHoursLimitIsNot(t *testing.T) {
	// "whichever comes first": GPUHoursLimit is generous (100h), but at
	// $30/hr for 10 A100-hours that's already $300, over a $100 budget.
	prom := promStub(t, map[string]float64{"A100": 10})
	defer prom.Close()

	scheme := newScheme(t)
	dep := gpuDeployment("gavin-test", "model", 3)
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "gavin-test"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			Period:        gpuquotav1alpha1.PeriodMonthly,
			GPUHoursLimit: float64Ptr(100),
			DollarsLimit:  float64Ptr(100),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, gq).WithStatusSubresource(gq).Build()

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL, GPURates: GPURates{A100: 30}}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.DollarsUsed != 300 {
		t.Fatalf("expected DollarsUsed=300 (10h * $30), got %v", got.Status.DollarsUsed)
	}
	if got.Status.Phase != gpuquotav1alpha1.PhaseEnforced {
		t.Fatalf("expected Enforced phase since dollars exceeded budget despite GPU-hours being under, got %s", got.Status.Phase)
	}
}

func TestReconcile_UnpricedGPUTypeWithDollarsLimitFailsLoudly(t *testing.T) {
	prom := promStub(t, map[string]float64{"H100": 5})
	defer prom.Close()

	scheme := newScheme(t)
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "gavin-test"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			Period:       gpuquotav1alpha1.PeriodMonthly,
			DollarsLimit: float64Ptr(1000),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gq).WithStatusSubresource(gq).Build()

	// GPURates zero-value: H100 has no configured rate.
	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != gpuquotav1alpha1.PhaseUnknown {
		t.Fatalf("expected Unknown phase for an unpriced GPU type, got %s", got.Status.Phase)
	}
}

func TestReconcile_EnforcementIsNeverAutomaticallyLifted(t *testing.T) {
	prom := promStub(t, map[string]float64{"A100": 50})
	defer prom.Close()

	scheme := newScheme(t)
	dep := gpuDeployment("gavin-test", "model", 3)
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "gavin-test"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			Period:        gpuquotav1alpha1.PeriodMonthly,
			GPUHoursLimit: float64Ptr(10),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, gq).WithStatusSubresource(gq).Build()

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var afterEnforce gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &afterEnforce); err != nil {
		t.Fatal(err)
	}
	if afterEnforce.Status.Phase != gpuquotav1alpha1.PhaseEnforced {
		t.Fatalf("expected Enforced phase, got %s", afterEnforce.Status.Phase)
	}

	// Manually restore the deployment to simulate an admin fixing things
	// without touching the GpuQuota - the operator should re-enforce it,
	// since only the reset annotation lifts enforcement.
	var gotDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDep); err != nil {
		t.Fatal(err)
	}
	three := int32(3)
	gotDep.Spec.Replicas = &three
	if err := c.Update(context.Background(), &gotDep); err != nil {
		t.Fatal(err)
	}

	// Usage query now reports well under budget - despite that, phase must
	// stay Enforced and the deployment must be re-scaled to zero, since
	// nothing has lifted enforcement.
	prom.Close()
	lowUsageProm := promStub(t, map[string]float64{"A100": 0.1})
	defer lowUsageProm.Close()
	r.PrometheusURL = lowUsageProm.URL
	r.promClient = nil // force a new client for the new address

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile after usage drop: %v", err)
	}

	var stillEnforced gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &stillEnforced); err != nil {
		t.Fatal(err)
	}
	if stillEnforced.Status.Phase != gpuquotav1alpha1.PhaseEnforced {
		t.Fatalf("expected phase to remain Enforced despite usage dropping under budget, got %s", stillEnforced.Status.Phase)
	}

	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDep); err != nil {
		t.Fatal(err)
	}
	if *gotDep.Spec.Replicas != 0 {
		t.Fatalf("expected deployment re-scaled to 0 after manual restore attempt, got %d replicas", *gotDep.Spec.Replicas)
	}
}

func TestReconcile_ResetAnnotationRestoresAndClearsEnforcement(t *testing.T) {
	prom := promStub(t, map[string]float64{"A100": 50})
	defer prom.Close()

	scheme := newScheme(t)
	dep := gpuDeployment("gavin-test", "model", 3)
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "gavin-test"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			Period:        gpuquotav1alpha1.PeriodMonthly,
			GPUHoursLimit: float64Ptr(10),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, gq).WithStatusSubresource(gq).Build()

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var enforced gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &enforced); err != nil {
		t.Fatal(err)
	}
	if enforced.Status.Phase != gpuquotav1alpha1.PhaseEnforced {
		t.Fatalf("expected Enforced phase, got %s", enforced.Status.Phase)
	}

	// Admin sets the reset annotation. Usage is still over budget (the stub
	// still reports 50h > 10h limit), so this proves reset restores and
	// clears state even without a period boundary or usage recovering -
	// and that it doesn't prevent immediate re-enforcement if still over
	// budget after the restore.
	enforced.Annotations = map[string]string{gpuquotav1alpha1.ResetAnnotation: "true"}
	if err := c.Update(context.Background(), &enforced); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile after reset: %v", err)
	}

	var afterReset gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &afterReset); err != nil {
		t.Fatal(err)
	}
	if _, ok := afterReset.Annotations[gpuquotav1alpha1.ResetAnnotation]; ok {
		t.Fatal("expected reset annotation to be removed after processing")
	}
	// Still over budget, so it re-enforces in the same pass rather than
	// leaving the namespace uncapped.
	if afterReset.Status.Phase != gpuquotav1alpha1.PhaseEnforced {
		t.Fatalf("expected re-enforcement since usage is still over budget, got %s", afterReset.Status.Phase)
	}

	var gotDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDep); err != nil {
		t.Fatal(err)
	}
	if *gotDep.Spec.Replicas != 0 {
		t.Fatalf("expected deployment scaled back to 0 after re-enforcement, got %d replicas", *gotDep.Spec.Replicas)
	}
}

func TestReconcile_ResetAnnotationRestoresFullyWhenBackUnderBudget(t *testing.T) {
	prom := promStub(t, map[string]float64{"A100": 1})
	defer prom.Close()

	scheme := newScheme(t)
	dep := gpuDeployment("gavin-test", "model", 3)
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "gavin-test"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			Period:        gpuquotav1alpha1.PeriodMonthly,
			GPUHoursLimit: float64Ptr(10),
		},
		Status: gpuquotav1alpha1.GpuQuotaStatus{
			Phase: gpuquotav1alpha1.PhaseEnforced,
			EnforcedResources: []gpuquotav1alpha1.EnforcedResource{
				{APIVersion: "apps/v1", Kind: "Deployment", Name: "model", Action: "ScaledToZero", EnforcedAt: metav1.Now()},
			},
		},
	}
	dep.Spec.Replicas = int32Ptr(0)
	dep.Annotations = map[string]string{"gpuquota.example.com/original-replicas": "3"}
	gq.Annotations = map[string]string{gpuquotav1alpha1.ResetAnnotation: "true"}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, gq).WithStatusSubresource(gq).Build()

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var afterReset gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &afterReset); err != nil {
		t.Fatal(err)
	}
	if afterReset.Status.Phase != gpuquotav1alpha1.PhaseCompliant {
		t.Fatalf("expected Compliant phase, usage is now under budget, got %s", afterReset.Status.Phase)
	}
	if len(afterReset.Status.EnforcedResources) != 0 {
		t.Fatalf("expected EnforcedResources cleared, got %+v", afterReset.Status.EnforcedResources)
	}

	var gotDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDep); err != nil {
		t.Fatal(err)
	}
	if *gotDep.Spec.Replicas != 3 {
		t.Fatalf("expected deployment restored to 3 replicas, got %d", *gotDep.Spec.Replicas)
	}
}

func TestReconcile_DeletesBareGPUPodButLeavesOwnedPodAlone(t *testing.T) {
	prom := promStub(t, map[string]float64{"A100": 50})
	defer prom.Close()

	scheme := newScheme(t)
	barePod := gpuPod("gavin-test", "bare-gpu-pod", false)
	ownedPod := gpuPod("gavin-test", "owned-gpu-pod", true)
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "gavin-test"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			Period:        gpuquotav1alpha1.PeriodMonthly,
			GPUHoursLimit: float64Ptr(10),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(barePod, ownedPod, gq).WithStatusSubresource(gq).Build()

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace("gavin-test")); err != nil {
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

func TestReconcile_ScalesStandaloneReplicaSetButLeavesDeploymentOwnedOneAlone(t *testing.T) {
	prom := promStub(t, map[string]float64{"A100": 50})
	defer prom.Close()

	scheme := newScheme(t)
	standaloneRS := gpuReplicaSet("gavin-test", "standalone-rs", 3, false)
	ownedRS := gpuReplicaSet("gavin-test", "owned-rs", 3, true)
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "gavin-test"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			Period:        gpuquotav1alpha1.PeriodMonthly,
			GPUHoursLimit: float64Ptr(10),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(standaloneRS, ownedRS, gq).WithStatusSubresource(gq).Build()

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var gotStandalone appsv1.ReplicaSet
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(standaloneRS), &gotStandalone); err != nil {
		t.Fatal(err)
	}
	if *gotStandalone.Spec.Replicas != 0 {
		t.Fatalf("expected standalone replicaset scaled to 0, got %d replicas", *gotStandalone.Spec.Replicas)
	}
	if gotStandalone.Annotations["gpuquota.example.com/original-replicas"] != "3" {
		t.Fatalf("expected original replicas annotation to record 3, got %q", gotStandalone.Annotations["gpuquota.example.com/original-replicas"])
	}

	var gotOwned appsv1.ReplicaSet
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ownedRS), &gotOwned); err != nil {
		t.Fatal(err)
	}
	if *gotOwned.Spec.Replicas != 3 {
		t.Fatalf("expected deployment-owned replicaset untouched, got %d replicas", *gotOwned.Spec.Replicas)
	}
}

func TestReconcile_ScalesStatefulSetToZero(t *testing.T) {
	prom := promStub(t, map[string]float64{"A100": 50})
	defer prom.Close()

	scheme := newScheme(t)
	sts := gpuStatefulSet("gavin-test", "training", 3)
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "gavin-test"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			Period:        gpuquotav1alpha1.PeriodMonthly,
			GPUHoursLimit: float64Ptr(10),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts, gq).WithStatusSubresource(gq).Build()

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got appsv1.StatefulSet
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &got); err != nil {
		t.Fatal(err)
	}
	if *got.Spec.Replicas != 0 {
		t.Fatalf("expected statefulset scaled to 0, got %d replicas", *got.Spec.Replicas)
	}
	if got.Annotations["gpuquota.example.com/original-replicas"] != "3" {
		t.Fatalf("expected original replicas annotation to record 3, got %q", got.Annotations["gpuquota.example.com/original-replicas"])
	}
}

func TestReconcile_SuspendsStandaloneJobButLeavesOwnedJobAlone(t *testing.T) {
	prom := promStub(t, map[string]float64{"A100": 50})
	defer prom.Close()

	scheme := newScheme(t)
	standaloneJob := gpuJob("gavin-test", "standalone-job", false)
	ownedJob := gpuJob("gavin-test", "owned-job", true)
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "gavin-test"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			Period:        gpuquotav1alpha1.PeriodMonthly,
			GPUHoursLimit: float64Ptr(10),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(standaloneJob, ownedJob, gq).WithStatusSubresource(gq).Build()

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var gotStandalone batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(standaloneJob), &gotStandalone); err != nil {
		t.Fatal(err)
	}
	if gotStandalone.Spec.Suspend == nil || !*gotStandalone.Spec.Suspend {
		t.Fatalf("expected standalone job suspended, got %+v", gotStandalone.Spec.Suspend)
	}

	var gotOwned batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(ownedJob), &gotOwned); err != nil {
		t.Fatal(err)
	}
	if gotOwned.Spec.Suspend != nil && *gotOwned.Spec.Suspend {
		t.Fatalf("expected cronjob-owned job untouched, got suspend=%v", *gotOwned.Spec.Suspend)
	}
}

func TestReconcile_NonGPUDeploymentIsNeverTouched(t *testing.T) {
	prom := promStub(t, map[string]float64{"A100": 50})
	defer prom.Close()

	scheme := newScheme(t)
	dep := gpuDeployment("gavin-test", "plain", 3)
	dep.Spec.Template.Spec.Containers[0].Resources.Requests = nil // strip the GPU request
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "gavin-test"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			Period:        gpuquotav1alpha1.PeriodMonthly,
			GPUHoursLimit: float64Ptr(10),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, gq).WithStatusSubresource(gq).Build()

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL}
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

func TestPeriodStart(t *testing.T) {
	// 2026-08-11 is a Tuesday.
	now := time.Date(2026, time.August, 11, 15, 30, 0, 0, time.UTC)

	cases := []struct {
		period gpuquotav1alpha1.BudgetPeriod
		want   time.Time
	}{
		{gpuquotav1alpha1.PeriodDaily, time.Date(2026, time.August, 11, 0, 0, 0, 0, time.UTC)},
		{gpuquotav1alpha1.PeriodWeekly, time.Date(2026, time.August, 10, 0, 0, 0, 0, time.UTC)}, // Monday
		{gpuquotav1alpha1.PeriodMonthly, time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		got := periodStart(c.period, now)
		if !got.Equal(c.want) {
			t.Errorf("periodStart(%s, %s) = %s, want %s", c.period, now, got, c.want)
		}
	}
}

func int32Ptr(v int32) *int32 { return &v }
