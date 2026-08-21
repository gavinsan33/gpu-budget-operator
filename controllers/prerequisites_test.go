package controllers

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEnsurePrerequisites_CreatesConfigMapAndClusterRoleBinding(t *testing.T) {
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	if err := EnsurePrerequisites(context.Background(), c, "gavin-test", "some-sa"); err != nil {
		t.Fatalf("EnsurePrerequisites: %v", err)
	}

	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "gavin-test", Name: ServiceCAConfigMapName}, &cm); err != nil {
		t.Fatalf("expected ConfigMap to be created: %v", err)
	}
	if cm.Annotations["service.beta.openshift.io/inject-cabundle"] != "true" {
		t.Fatalf("expected inject-cabundle annotation, got %+v", cm.Annotations)
	}

	var crb rbacv1.ClusterRoleBinding
	if err := c.Get(context.Background(), client.ObjectKey{Name: MonitoringClusterRoleBindingName}, &crb); err != nil {
		t.Fatalf("expected ClusterRoleBinding to be created: %v", err)
	}
	if crb.RoleRef.Name != "cluster-monitoring-view" {
		t.Fatalf("expected roleRef to cluster-monitoring-view, got %+v", crb.RoleRef)
	}
	if len(crb.Subjects) != 1 || crb.Subjects[0].Name != "some-sa" || crb.Subjects[0].Namespace != "gavin-test" {
		t.Fatalf("expected subject some-sa in gavin-test, got %+v", crb.Subjects)
	}
}

// TestEnsurePrerequisites_IdempotentOnAlreadyExists covers repeated calls
// (e.g. every operator restart, not just a fresh install) - AlreadyExists
// must not be treated as an error.
func TestEnsurePrerequisites_IdempotentOnAlreadyExists(t *testing.T) {
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	if err := EnsurePrerequisites(context.Background(), c, "gavin-test", "some-sa"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := EnsurePrerequisites(context.Background(), c, "gavin-test", "some-sa"); err != nil {
		t.Fatalf("second call should be a no-op, got: %v", err)
	}
}
