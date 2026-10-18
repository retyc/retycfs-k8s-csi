package enroller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"
)

// EnsureClusterIdentity creates the RetycIdentity of the driver's namespace, the cluster-wide
// identity, when DefaultOrganizationID is set and it does not exist: Helm cannot install a CRD and
// an object of it in one release, so the enroller makes it. It never changes an existing one. One
// deleted by hand is created again and enrolls anew, with new keys and a new request to approve:
// its Secret is replaced even while PVs use it (the default PVs name none, blocking would only
// leave them without identity), with a Warning event ClusterIdentityReplaced, since the existing
// datarooms belong to the previous machine and an administrator must add the new one to them.
// This is wanted in cluster mode, where the driver always asks for its default identity; to stop
// it, unset --default-organization-id. It reports whether it created one.
func (e *Enroller) EnsureClusterIdentity(ctx context.Context) (bool, error) {
	if e.DefaultOrganizationID == "" {
		return false, nil
	}
	res := e.resource(e.Namespace)
	_, err := res.Get(ctx, ObjectName, metav1.GetOptions{})
	if err == nil {
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("reading RetycIdentity %s/%s: %w", e.Namespace, ObjectName, err)
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": GVR.GroupVersion().String(),
		"kind":       Kind,
		"metadata": map[string]any{
			"name": ObjectName, "namespace": e.Namespace,
			"labels": map[string]any{labelManagedBy: managedBy},
		},
		// Explicit, so that the organization shows and stays (the field is immutable).
		"spec": map[string]any{"organizationID": e.DefaultOrganizationID},
	}}
	_, err = res.Create(ctx, obj, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("creating RetycIdentity %s/%s: %w", e.Namespace, ObjectName, err)
	}
	klog.Infof("enroller: RetycIdentity %s/%s created for the cluster-wide identity, organization %s",
		e.Namespace, ObjectName, e.DefaultOrganizationID)

	return true, nil
}

// ensureClusterIdentity is EnsureClusterIdentity with the error logged.
func (e *Enroller) ensureClusterIdentity(ctx context.Context) {
	if _, err := e.EnsureClusterIdentity(ctx); err != nil {
		klog.Errorf("enroller: %v", err)
	}
}
