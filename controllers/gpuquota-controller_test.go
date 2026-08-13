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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL, GPURates: GPURates{"A100": 30}}
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

// TestReconcile_PricesFullSKUGpuTypeNotJustBareFamilyName reproduces the
// bug found running against a real cluster: the default GPU-hours query's
// gpuType comes from a node's full product label via label_replace(...,
// "NVIDIA-(.+)"), so a real cluster reports gpuType as a full SKU like
// "A100-SXM4-80GB", not the bare family name "A100" every unit test above
// uses. --gpu-rate=A100=<usd> must still price it (RateFor matches by family
// prefix, not exact equality) - this is the one test in the suite using a
// full-SKU gpuType end-to-end through Reconcile, not just RateFor directly.
func TestReconcile_PricesFullSKUGpuTypeNotJustBareFamilyName(t *testing.T) {
	prom := promStub(t, map[string]float64{"A100-SXM4-80GB": 10})
	defer prom.Close()

	scheme := newScheme(t)
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "gavin-test"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			Period:       gpuquotav1alpha1.PeriodMonthly,
			DollarsLimit: float64Ptr(100),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gq).WithStatusSubresource(gq).Build()

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL, GPURates: GPURates{"A100": 30}}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var got gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase == gpuquotav1alpha1.PhaseUnknown {
		t.Fatalf("expected the full-SKU gpuType to be priced via family-prefix matching, got Unknown phase: %+v", got.Status.Conditions)
	}
	if got.Status.DollarsUsed != 300 {
		t.Fatalf("expected DollarsUsed=300 (10h * $30 A100 rate) for gpuType \"A100-SXM4-80GB\", got %v", got.Status.DollarsUsed)
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
	if got.Status.Conditions[0].Reason != "UnpricedGPUType" {
		t.Fatalf("expected UnpricedGPUType reason, got %s", got.Status.Conditions[0].Reason)
	}
}

// TestReconcile_UnpricedGPUTypeTakesNoEnforcementAction confirms a fresh
// (never-enforced) quota that immediately hits an unpriced GPU type doesn't
// touch any workload - markFailed returns before ever reaching the
// enforcement decision at all.
func TestReconcile_UnpricedGPUTypeTakesNoEnforcementAction(t *testing.T) {
	prom := promStub(t, map[string]float64{"H100": 5})
	defer prom.Close()

	scheme := newScheme(t)
	dep := gpuDeployment("gavin-test", "model", 3)
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "gavin-test"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			Period:       gpuquotav1alpha1.PeriodMonthly,
			DollarsLimit: float64Ptr(1000),
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
	if got.Status.Phase != gpuquotav1alpha1.PhaseUnknown {
		t.Fatalf("expected Unknown phase, got %s", got.Status.Phase)
	}
	if len(got.Status.EnforcedResources) != 0 {
		t.Fatalf("expected no EnforcedResources recorded, got %+v", got.Status.EnforcedResources)
	}

	var gotDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDep); err != nil {
		t.Fatal(err)
	}
	if *gotDep.Spec.Replicas != 3 {
		t.Fatalf("expected deployment untouched at 3 replicas, got %d", *gotDep.Spec.Replicas)
	}
}

// TestReconcile_UnpricedGPUTypeAfterFixResolvesToCompliant covers the
// simple recovery path: a never-enforced quota hits an unpriced type
// (Unknown), then a later reconcile with the rate now configured and usage
// under budget resolves cleanly to Compliant.
func TestReconcile_UnpricedGPUTypeAfterFixResolvesToCompliant(t *testing.T) {
	prom := promStub(t, map[string]float64{"H100": 0.1})
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

	// First reconcile: no rate configured yet.
	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var unknown gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &unknown); err != nil {
		t.Fatal(err)
	}
	if unknown.Status.Phase != gpuquotav1alpha1.PhaseUnknown {
		t.Fatalf("expected Unknown phase before the rate is configured, got %s", unknown.Status.Phase)
	}

	// Rate now configured (e.g. the operator restarted with --gpu-rate=H100=<usd>
	// set) and usage is comfortably under the $1000 limit.
	r.GPURates = GPURates{"H100": 1.10}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile after rate configured: %v", err)
	}

	var got gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != gpuquotav1alpha1.PhaseCompliant {
		t.Fatalf("expected Compliant phase once the rate is fixed and usage is under budget, got %s", got.Status.Phase)
	}
}

// TestReconcile_UnpricedGPUTypeWhileEnforcedStaysStickyUntilReset is the
// regression test for the bug the sticky-enforcement fix above closes:
// a namespace that's already Enforced, whose usage later includes a newly
// unpriced GPU type, transiently fails with Phase=Unknown (via markFailed) -
// but that failure must NOT lose track of the already-zeroed workload. Once
// the rate gets configured and usage happens to read as back under budget,
// the quota must still report Enforced (and the workload must still be at
// 0 replicas) rather than silently flipping to Compliant with nothing ever
// having gone through gpuquota.io/reset. Keying stickiness off Phase alone
// (instead of len(EnforcedResources) > 0) would make this test fail, since
// markFailed's Phase=Unknown would erase the "was Enforced" signal.
func TestReconcile_UnpricedGPUTypeWhileEnforcedStaysStickyUntilReset(t *testing.T) {
	prom := promStub(t, map[string]float64{"A100": 50})
	defer prom.Close()

	scheme := newScheme(t)
	dep := gpuDeployment("gavin-test", "model", 3)
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "gavin-test"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			Period:        gpuquotav1alpha1.PeriodMonthly,
			GPUHoursLimit: float64Ptr(10),
			DollarsLimit:  float64Ptr(1000),
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, gq).WithStatusSubresource(gq).Build()

	// First reconcile: over the GPU-hours limit, A100 is priced - enforces
	// normally.
	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL, GPURates: GPURates{"A100": 30}}
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

	// A workload using a brand-new, unpriced GPU type (H100) shows up
	// alongside the existing A100 usage. computeUsage now fails.
	prom.Close()
	mixedProm := promStub(t, map[string]float64{"A100": 50, "H100": 5})
	defer mixedProm.Close()
	r.PrometheusURL = mixedProm.URL
	r.promClient = nil

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile with unpriced H100 usage: %v", err)
	}
	var unknown gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &unknown); err != nil {
		t.Fatal(err)
	}
	if unknown.Status.Phase != gpuquotav1alpha1.PhaseUnknown {
		t.Fatalf("expected Unknown phase while H100 is unpriced, got %s", unknown.Status.Phase)
	}
	if len(unknown.Status.EnforcedResources) != 1 {
		t.Fatalf("expected EnforcedResources to survive the transient Unknown phase untouched, got %+v", unknown.Status.EnforcedResources)
	}

	var gotDepStillZero appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDepStillZero); err != nil {
		t.Fatal(err)
	}
	if *gotDepStillZero.Spec.Replicas != 0 {
		t.Fatalf("expected deployment to remain at 0 replicas during the transient Unknown phase, got %d", *gotDepStillZero.Spec.Replicas)
	}

	// H100 gets priced (e.g. --gpu-rate=H100=1.10 configured), and -
	// critically - usage this time reads as comfortably under both limits.
	mixedProm.Close()
	lowUsageProm := promStub(t, map[string]float64{"A100": 0.1})
	defer lowUsageProm.Close()
	r.PrometheusURL = lowUsageProm.URL
	r.promClient = nil
	r.GPURates["H100"] = 1.10

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile after H100 priced and usage drops: %v", err)
	}

	var final gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &final); err != nil {
		t.Fatal(err)
	}
	if final.Status.Phase != gpuquotav1alpha1.PhaseEnforced {
		t.Fatalf("expected phase to remain Enforced (sticky via EnforcedResources) despite usage now reading under budget - only reset lifts enforcement, got %s", final.Status.Phase)
	}
	if len(final.Status.EnforcedResources) != 1 {
		t.Fatalf("expected EnforcedResources to remain populated, got %+v", final.Status.EnforcedResources)
	}

	var gotDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDep); err != nil {
		t.Fatal(err)
	}
	if *gotDep.Spec.Replicas != 0 {
		t.Fatalf("expected deployment to remain at 0 replicas - it was never restored via gpuquota.io/reset, got %d", *gotDep.Spec.Replicas)
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
	dep.Annotations = map[string]string{"gpuquota.io/original-replicas": "3"}
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
	if gotStandalone.Annotations["gpuquota.io/original-replicas"] != "3" {
		t.Fatalf("expected original replicas annotation to record 3, got %q", gotStandalone.Annotations["gpuquota.io/original-replicas"])
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
	if got.Annotations["gpuquota.io/original-replicas"] != "3" {
		t.Fatalf("expected original replicas annotation to record 3, got %q", got.Annotations["gpuquota.io/original-replicas"])
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

// TestReconcile_PartialEnforcementFailurePersistsSuccessfulEntries reproduces
// the bug where a transient conflict enforcing one GPU workload (the
// StatefulSet, here) caused the Deployment successfully zeroed in the same
// EnforceNamespace call to never be recorded in
// Status.EnforcedResources - Reconcile used to bail out on the first
// enforcement error before ever merging or persisting what had already
// succeeded. Losing that entry means gpuquota.io/reset later finds nothing
// to restore for it: the workload stays scaled to zero forever with no
// error ever surfaced again.
func TestReconcile_PartialEnforcementFailurePersistsSuccessfulEntries(t *testing.T) {
	prom := promStub(t, map[string]float64{"A100": 50})
	defer prom.Close()

	scheme := newScheme(t)
	dep := gpuDeployment("gavin-test", "model", 3)
	sts := gpuStatefulSet("gavin-test", "training", 1)
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "gavin-test"},
		Spec: gpuquotav1alpha1.GpuQuotaSpec{
			Period:        gpuquotav1alpha1.PeriodMonthly,
			GPUHoursLimit: float64Ptr(10),
		},
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, sts, gq).WithStatusSubresource(gq).Build()
	c := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if _, ok := obj.(*appsv1.StatefulSet); ok {
				return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "statefulsets"}, obj.GetName(), nil)
			}
			return cli.Update(ctx, obj, opts...)
		},
	})

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err == nil {
		t.Fatal("expected Reconcile to return the StatefulSet enforcement error, got nil")
	}

	var got gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != gpuquotav1alpha1.PhaseEnforced {
		t.Fatalf("expected Enforced phase despite the partial failure, got %s", got.Status.Phase)
	}
	found := false
	for _, res := range got.Status.EnforcedResources {
		if res.Kind == "Deployment" && res.Name == "model" {
			found = true
		}
		if res.Kind == "StatefulSet" && res.Name == "training" {
			t.Errorf("did not expect StatefulSet/training in EnforcedResources, its update failed: %+v", got.Status.EnforcedResources)
		}
	}
	if !found {
		t.Fatalf("expected Deployment/model in EnforcedResources despite the StatefulSet failing in the same pass, got %+v", got.Status.EnforcedResources)
	}

	var gotDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDep); err != nil {
		t.Fatal(err)
	}
	if *gotDep.Spec.Replicas != 0 {
		t.Fatalf("expected Deployment/model scaled to 0, got %d", *gotDep.Spec.Replicas)
	}
}

// TestReconcile_StatusConflictIsRetriedNotDropped reproduces the other half
// of the same bug: a resourceVersion conflict on the GpuQuota's own status
// update (e.g. from a concurrent spec edit) used to make Reconcile give up
// immediately via a plain Status().Update(), discarding this reconcile's
// freshly computed EnforcedResources/phase entirely - the next reconcile
// would then only recover them if the failing resource kind's capture
// happened to succeed again, which it wouldn't (its gpuquota.io/original-*
// annotation is already set). persistStatus must instead retry against a
// freshly re-fetched copy of the object.
func TestReconcile_StatusConflictIsRetriedNotDropped(t *testing.T) {
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
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, gq).WithStatusSubresource(gq).Build()

	conflictsLeft := 1
	c := interceptor.NewClient(base, interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cli client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if subResourceName == "status" && conflictsLeft > 0 {
				conflictsLeft--
				return apierrors.NewConflict(schema.GroupResource{Group: "gpuquota.io", Resource: "gpuquotas"}, obj.GetName(), nil)
			}
			return cli.Status().Update(ctx, obj, opts...)
		},
	})

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile: %v (persistStatus should have retried the single injected conflict)", err)
	}
	if conflictsLeft != 0 {
		t.Fatalf("expected the injected conflict to have been consumed by a retry, conflictsLeft=%d", conflictsLeft)
	}

	var got gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != gpuquotav1alpha1.PhaseEnforced {
		t.Fatalf("expected Enforced phase to have survived the retried conflict, got %s", got.Status.Phase)
	}
	if len(got.Status.EnforcedResources) != 1 {
		t.Fatalf("expected 1 enforced resource to have survived the retried conflict, got %d", len(got.Status.EnforcedResources))
	}
}

// TestReconcile_ReapplyingHigherLimitAloneDoesNotLiftEnforcement covers the
// first half of a real "reapply the quota, then reset" workflow: raising
// spec.GPUHoursLimit (simulating `oc apply -f quota.yaml` with a bigger
// number) via a plain spec Update, with NO reset annotation, must not lift
// enforcement by itself even though usage is now back under the new limit -
// gpuquota.io/reset is the only thing that ever lifts it (see
// TestReconcile_EnforcementIsNeverAutomaticallyLifted for the equivalent
// usage-drops-instead-of-limit-rises case).
func TestReconcile_ReapplyingHigherLimitAloneDoesNotLiftEnforcement(t *testing.T) {
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

	// Simulate `oc apply` raising the limit well above current usage (50h),
	// with no reset annotation.
	afterEnforce.Spec.GPUHoursLimit = float64Ptr(1000)
	if err := c.Update(context.Background(), &afterEnforce); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile after raising the limit: %v", err)
	}

	var afterReapply gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &afterReapply); err != nil {
		t.Fatal(err)
	}
	if afterReapply.Status.Phase != gpuquotav1alpha1.PhaseEnforced {
		t.Fatalf("expected phase to remain Enforced despite the higher limit - only reset lifts enforcement, got %s", afterReapply.Status.Phase)
	}
	if len(afterReapply.Status.EnforcedResources) != 1 {
		t.Fatalf("expected EnforcedResources to remain populated, got %+v", afterReapply.Status.EnforcedResources)
	}

	var gotDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDep); err != nil {
		t.Fatal(err)
	}
	if *gotDep.Spec.Replicas != 0 {
		t.Fatalf("expected deployment to remain scaled to 0 despite the higher limit, got %d replicas", *gotDep.Spec.Replicas)
	}
}

// TestReconcile_ReapplyRaisingLimitThenResetFullyRestores covers the exact
// workflow reported against a real cluster: enforce, then separately (a)
// reapply the quota with spec.GPUHoursLimit raised above current usage and
// (b) annotate gpuquota.io/reset=true - two independent updates, as
// `oc apply` followed by `oc annotate` would produce, landing before the
// next reconcile picks either up. A single subsequent reconcile must fully
// restore the workload and end up Compliant with EnforcedResources cleared.
func TestReconcile_ReapplyRaisingLimitThenResetFullyRestores(t *testing.T) {
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

	// `oc apply -f quota.yaml` with a raised limit (usage is 50h; new limit
	// is well above it) ...
	enforced.Spec.GPUHoursLimit = float64Ptr(1000)
	if err := c.Update(context.Background(), &enforced); err != nil {
		t.Fatal(err)
	}
	// ... then, separately, `oc annotate gpuquota ... gpuquota.io/reset=true`.
	var beforeReset gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &beforeReset); err != nil {
		t.Fatal(err)
	}
	beforeReset.Annotations = map[string]string{gpuquotav1alpha1.ResetAnnotation: "true"}
	if err := c.Update(context.Background(), &beforeReset); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("reconcile after reapply+reset: %v", err)
	}

	var afterReset gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &afterReset); err != nil {
		t.Fatal(err)
	}
	if _, ok := afterReset.Annotations[gpuquotav1alpha1.ResetAnnotation]; ok {
		t.Fatal("expected reset annotation to be removed after processing")
	}
	if afterReset.Status.Phase != gpuquotav1alpha1.PhaseCompliant {
		t.Fatalf("expected Compliant phase, usage (50h) is now under the raised limit (1000h), got %s", afterReset.Status.Phase)
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
	if _, ok := gotDep.Annotations["gpuquota.io/original-replicas"]; ok {
		t.Fatal("expected the deployment's original-replicas annotation removed after restore")
	}
}

// TestReconcile_ResetSurvivesConflictClearingTheAnnotation reproduces a
// third failure mode observed against a real cluster: RestoreNamespace
// itself succeeds (the workload is genuinely restored), but the subsequent
// plain Update that clears gpuquota.io/reset off the GpuQuota conflicts
// (e.g. a concurrent edit bumped its resourceVersion in between). Reconcile
// returns that error without persisting anything this pass - by design,
// since handleManualReset returns before ever reaching persistStatus - so
// the reset annotation is still "true" and status still shows Enforced.
// The workload must already be restored regardless (RestoreNamespace ran
// to completion before the conflict), and controller-runtime's normal
// error-requeue (simulated here as a second Reconcile call) must finish
// the job cleanly, with RestoreNamespace safely no-op'ing on the
// already-restored resource the second time around.
func TestReconcile_ResetSurvivesConflictClearingTheAnnotation(t *testing.T) {
	prom := promStub(t, map[string]float64{"A100": 1})
	defer prom.Close()

	scheme := newScheme(t)
	dep := gpuDeployment("gavin-test", "model", 3)
	dep.Spec.Replicas = int32Ptr(0)
	dep.Annotations = map[string]string{"gpuquota.io/original-replicas": "3"}
	gq := &gpuquotav1alpha1.GpuQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name: "quota", Namespace: "gavin-test",
			Annotations: map[string]string{gpuquotav1alpha1.ResetAnnotation: "true"},
		},
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
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, gq).WithStatusSubresource(gq).Build()

	conflictsLeft := 1
	c := interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if q, ok := obj.(*gpuquotav1alpha1.GpuQuota); ok && q.Annotations[gpuquotav1alpha1.ResetAnnotation] != "true" && conflictsLeft > 0 {
				// This is specifically handleManualReset's own
				// annotation-clearing Update (the reset annotation is
				// already gone from the object being written) - not the
				// test's own setup writes above.
				conflictsLeft--
				return apierrors.NewConflict(schema.GroupResource{Group: "gpuquota.io", Resource: "gpuquotas"}, obj.GetName(), nil)
			}
			return cli.Update(ctx, obj, opts...)
		},
	})

	r := &GpuQuotaReconciler{Client: c, Scheme: scheme, PrometheusURL: prom.URL}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err == nil {
		t.Fatal("expected the first reconcile to surface the injected conflict clearing the reset annotation")
	}

	// The workload must already be restored - RestoreNamespace completed
	// successfully before the conflict on the GpuQuota's own annotation.
	var gotDepMidway appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDepMidway); err != nil {
		t.Fatal(err)
	}
	if *gotDepMidway.Spec.Replicas != 3 {
		t.Fatalf("expected deployment already restored to 3 replicas even though the reset overall failed, got %d", *gotDepMidway.Spec.Replicas)
	}

	var midway gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &midway); err != nil {
		t.Fatal(err)
	}
	if midway.Annotations[gpuquotav1alpha1.ResetAnnotation] != "true" {
		t.Fatal("expected the reset annotation to remain present after the failed clear, ready for a retry")
	}
	if midway.Status.Phase != gpuquotav1alpha1.PhaseEnforced {
		t.Fatalf("expected status to remain unchanged (Enforced) since Reconcile returned before persistStatus, got %s", midway.Status.Phase)
	}

	// Simulate controller-runtime's error-requeue with a second Reconcile call.
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gq)}); err != nil {
		t.Fatalf("expected the retried reconcile to succeed once the conflict clears, got: %v", err)
	}

	var final gpuquotav1alpha1.GpuQuota
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(gq), &final); err != nil {
		t.Fatal(err)
	}
	if _, ok := final.Annotations[gpuquotav1alpha1.ResetAnnotation]; ok {
		t.Fatal("expected reset annotation removed after the retry succeeds")
	}
	if final.Status.Phase != gpuquotav1alpha1.PhaseCompliant {
		t.Fatalf("expected Compliant phase after the retry, got %s", final.Status.Phase)
	}
	if len(final.Status.EnforcedResources) != 0 {
		t.Fatalf("expected EnforcedResources cleared after the retry, got %+v", final.Status.EnforcedResources)
	}

	var gotDep appsv1.Deployment
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(dep), &gotDep); err != nil {
		t.Fatal(err)
	}
	if *gotDep.Spec.Replicas != 3 {
		t.Fatalf("expected deployment still at 3 replicas after the retry (already restored, must stay a no-op), got %d", *gotDep.Spec.Replicas)
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
