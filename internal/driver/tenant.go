package driver

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/retyc/retycfs-k8s-csi/internal/identity"
)

// DefaultIdentityDir is where the driver's own Secret is mounted (an optional volume): the default
// identity, read at each use, so that it can be filled (the enrollment approved) or replaced
// without a restart.
const DefaultIdentityDir = "/var/run/retycfs-csi/identity"

// resolveIdentity returns the identity of a request: the tenant's from its CSI secrets, otherwise
// the driver's default one, read from dir, after beforeDefault (nil: none) agrees to its use. The
// identity must be complete: one awaiting approval is Unavailable (the sidecars and kubelet retry,
// and the approval may come any time), a missing or incomplete one FailedPrecondition.
func resolveIdentity(secrets map[string]string, dir string, beforeDefault func() error) (*identity.Credentials, error) {
	creds, err := identity.FromSecrets(secrets)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "CSI secrets: %v", err)
	}
	source := "the CSI secrets"
	if creds == nil {
		if beforeDefault != nil {
			if err := beforeDefault(); err != nil {
				return nil, err
			}
		}
		if creds, err = identity.FromDir(dir); err != nil {
			return nil, status.Errorf(codes.Unavailable, "reading the driver's identity in %s: %v", dir, err)
		}
		if creds == nil {
			return nil, status.Errorf(codes.FailedPrecondition, "no credentials: the request carried no secrets and "+
				"the driver has no default identity in %s", dir)
		}
		source = "the driver's identity in " + dir
	}
	if err := creds.Complete(); err != nil {
		code := codes.FailedPrecondition
		if errors.Is(err, identity.ErrPending) {
			code = codes.Unavailable
		}

		return nil, status.Errorf(code, "%s: %v", source, err)
	}

	return creds, nil
}

// checkIdentityNamespace refuses, with FailedPrecondition, a tenant identity (k8s.json) used for a
// volume of another namespace than the one it was enrolled for, told by the namespace's UID: a
// tenant's Secret copied to another namespace, or kept across the deletion of its own. namespace is
// the volume's claim namespace, "" when unknown, which a tenant identity cannot be checked against.
func checkIdentityNamespace(
	ctx context.Context, kube kubernetes.Interface, creds *identity.Credentials, namespace string,
) error {
	info, err := creds.K8s()
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "identity: %v", err)
	}
	if info == nil || info.Mode != identity.ModeTenant {
		return nil
	}
	if namespace == "" {
		return status.Errorf(codes.FailedPrecondition, "identity enrolled for namespace %q, but the volume names no "+
			"claim namespace to check it against", info.Namespace)
	}
	if kube == nil {
		return status.Error(codes.FailedPrecondition, "no Kubernetes client: cannot check the identity's namespace")
	}
	ns, err := kube.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return status.Errorf(codes.FailedPrecondition, "namespace %s not found", namespace)
	}
	if err != nil {
		return status.Errorf(codes.Unavailable, "reading namespace %s: %v", namespace, err)
	}
	if err := creds.CheckNamespace(namespace, string(ns.UID)); err != nil {
		return status.Errorf(codes.FailedPrecondition, "%v", err)
	}

	return nil
}

// nodeStageSecretParam is the StorageClass parameter that gives its volumes node-stage secrets:
// the chart sets it on the tenant classes, whose volumes must never be mounted with the driver's
// own identity.
const nodeStageSecretParam = "csi.storage.k8s.io/node-stage-secret-name" //nolint:gosec // G101: a parameter name

// checkDefaultIdentity refuses, with FailedPrecondition, to mount with the driver's own identity a
// volume whose PV or StorageClass expects node-stage secrets: a static PV of a tenant class written
// without nodeStageSecretRef, which kubelet stages without secrets. A dynamically provisioned PV of
// such a class always has the reference.
//
// NodeStageVolume carries neither the PV name nor its class, so the PV is found from the claim's
// spec.volumeName when the volume context names a claim, else by listing the PVs of the driver
// for the volume handle; every PV of the handle is checked, so a global PV written on the same
// dataroom cannot let a tenant one through. No PV at all refuses: kubelet only stages a volume
// that has one. A class not found does not refuse: a class deleted since gives nothing to check.
// An API error is Unavailable: kubelet retries.
func (s *NodeServer) checkDefaultIdentity(
	ctx context.Context, volumeID string, claim *corev1.PersistentVolumeClaim,
) error {
	if s.Kube == nil {
		return status.Error(codes.FailedPrecondition,
			"no Kubernetes client: cannot check that the volume's StorageClass takes the driver's identity")
	}
	pvs, err := s.findPVs(ctx, volumeID, claim)
	if err != nil {
		return status.Errorf(codes.Unavailable, "volume %s: finding its PV: %v", volumeID, err)
	}
	if len(pvs) == 0 {
		return status.Errorf(codes.FailedPrecondition, "volume %s: no PV of driver %s has this handle: "+
			"cannot check that its StorageClass takes the driver's identity", volumeID, DriverName)
	}
	for _, pv := range pvs {
		if err := s.checkPVIdentity(ctx, volumeID, pv); err != nil {
			return err
		}
	}

	return nil
}

// checkPVIdentity refuses pv when it, or its StorageClass, expects node-stage secrets.
func (s *NodeServer) checkPVIdentity(ctx context.Context, volumeID string, pv *corev1.PersistentVolume) error {
	if pv.Spec.CSI.NodeStageSecretRef != nil {
		return status.Errorf(codes.FailedPrecondition, "volume %s: PV %s names node-stage secret %s/%s, but kubelet "+
			"sent no credentials: the driver's own identity is not used in its place", volumeID, pv.Name,
			pv.Spec.CSI.NodeStageSecretRef.Namespace, pv.Spec.CSI.NodeStageSecretRef.Name)
	}
	class := pv.Spec.StorageClassName
	if class == "" {
		return nil
	}
	sc, err := s.Kube.StorageV1().StorageClasses().Get(ctx, class, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		klog.Warningf("NodeStageVolume: volume %s: StorageClass %s of PV %s not found, not checked", volumeID, class,
			pv.Name)

		return nil
	}
	if err != nil {
		return status.Errorf(codes.Unavailable, "volume %s: reading StorageClass %s: %v", volumeID, class, err)
	}
	if _, tenant := sc.Parameters[nodeStageSecretParam]; tenant {
		return status.Errorf(codes.FailedPrecondition, "volume %s: PV %s has no nodeStageSecretRef, but its "+
			"StorageClass %s is a tenant class (%s): the driver's own identity is not used for it. Add "+
			"spec.csi.nodeStageSecretRef to the PV, naming the tenant's identity Secret "+
			"(retycfs-identity-<claim namespace>) in the driver's namespace", volumeID, pv.Name, class,
			nodeStageSecretParam)
	}

	return nil
}

// findPVs returns the PVs of the driver with the volume handle: the claim's own PV first, then
// every other one. More than one is a misconfiguration, but each must pass the check.
func (s *NodeServer) findPVs(
	ctx context.Context, volumeID string, claim *corev1.PersistentVolumeClaim,
) ([]*corev1.PersistentVolume, error) {
	ours := func(pv *corev1.PersistentVolume) bool {
		return pv.Spec.CSI != nil && pv.Spec.CSI.Driver == DriverName && pv.Spec.CSI.VolumeHandle == volumeID
	}
	list, err := s.Kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var found []*corev1.PersistentVolume
	for i := range list.Items {
		pv := &list.Items[i]
		if !ours(pv) {
			continue
		}
		if claim != nil && pv.Name == claim.Spec.VolumeName {
			found = append([]*corev1.PersistentVolume{pv}, found...)
		} else {
			found = append(found, pv)
		}
	}

	return found, nil
}
