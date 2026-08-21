package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ServiceCAConfigMapName is the ConfigMap the operator creates (if
	// missing) and mounts for OpenShift's service-ca operator to inject the
	// cluster's serving CA bundle into - see metrics/prometheus.go's
	// serviceCACertFile. OLM's install strategy can only create
	// Deployments/(Cluster)Roles/(Cluster)RoleBindings/ServiceAccounts, not
	// arbitrary ConfigMaps, so a human used to have to `oc apply` this
	// after subscribing; the operator now provisions it itself instead.
	ServiceCAConfigMapName = "gpu-budget-operator-service-ca"

	// MonitoringClusterRoleBindingName is the ClusterRoleBinding the
	// operator creates (if missing), binding its own ServiceAccount to
	// OpenShift's built-in cluster-monitoring-view ClusterRole. OLM's
	// clusterPermissions can only grant rules a CSV defines itself, not
	// bind to a pre-existing external ClusterRole by name, so this is
	// self-provisioned for the same reason as the ConfigMap above.
	MonitoringClusterRoleBindingName = "gpu-budget-operator-monitoring-view"

	// ControllerManagerServiceAccountName is the operator's own
	// ServiceAccount name - fixed by convention across every manifest in
	// this repo (manager/deploy/deployment.yaml, the OLM CSV, etc.), not
	// something discoverable at runtime via the downward API.
	ControllerManagerServiceAccountName = "gpu-budget-operator-controller-manager"

	monitoringClusterRoleName = "cluster-monitoring-view"
)

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=create
// +kubebuilder:rbac:groups="",resources=configmaps,resourceNames=gpu-budget-operator-service-ca,verbs=get;update
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=create
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,resourceNames=gpu-budget-operator-monitoring-view,verbs=get
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,resourceNames=cluster-monitoring-view,verbs=bind

// EnsurePrerequisites creates, if missing, the two objects this operator
// needs that OLM's install strategy has no mechanism to create itself (see
// the constants above). Both creates are idempotent - AlreadyExists is not
// an error - so this is safe to call on every startup, not just a fresh
// install, and namespace/serviceAccountName are the only inputs needed.
//
// Errors here are returned, not swallowed, but main.go treats them as
// non-fatal: the reconcile loop already fails loudly per-GpuBudget
// (PrometheusClientError/markFailed) whenever either prerequisite is
// genuinely missing or broken, so a failed self-provision attempt leaves
// the operator no worse off than before this existed - it just means a
// human still has to create whichever one failed.
func EnsurePrerequisites(ctx context.Context, cl client.Client, namespace, serviceAccountName string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceCAConfigMapName,
			Namespace: namespace,
			Annotations: map[string]string{
				"service.beta.openshift.io/inject-cabundle": "true",
			},
		},
	}
	if err := cl.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating ConfigMap %s/%s: %w", namespace, ServiceCAConfigMapName, err)
	}

	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: MonitoringClusterRoleBindingName},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     monitoringClusterRoleName,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      serviceAccountName,
			Namespace: namespace,
		}},
	}
	if err := cl.Create(ctx, crb); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating ClusterRoleBinding %s: %w", MonitoringClusterRoleBindingName, err)
	}
	return nil
}
