package enroller

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/retyc/retycfs-k8s-csi/internal/driver"
	"github.com/retyc/retycfs-k8s-csi/internal/identity"
)

// Secrets of the identities, in the driver's namespace.
const (
	// SecretType marks the Secrets the enroller manages: it never touches another one.
	SecretType corev1.SecretType = "csi.retycfs.com/machine-identity" //nolint:gosec // G101: a type name
	// ClusterSecretName is the Secret of the cluster-wide identity; a tenant's is
	// TenantSecretPrefix + its namespace.
	ClusterSecretName  = "retycfs-identity"  //nolint:gosec // G101: a Secret name
	TenantSecretPrefix = "retycfs-identity-" //nolint:gosec // G101: a Secret name

	LabelNamespace    = "csi.retycfs.com/namespace"
	LabelNamespaceUID = "csi.retycfs.com/namespace-uid"
	// LabelIdentityUID is the UID of the RetycIdentity the keys were made for: another one (the
	// RetycIdentity recreated) enrolls with new keys.
	LabelIdentityUID = "csi.retycfs.com/identity-uid"
	// LabelOrphan marks a Secret whose RetycIdentity is gone while PVs still used it: the sweep
	// deletes it once none does.
	LabelOrphan = "csi.retycfs.com/orphan"
	// AnnotationMachineID is the machine of an approved identity, for the logs once it is gone.
	AnnotationMachineID = "csi.retycfs.com/machine-id"
	labelManagedBy      = "app.kubernetes.io/managed-by"
	managedBy           = "retycfs-enroller"
)

// Annotations external-provisioner sets on a PV for the Secret its deletion needs.
//
//nolint:gosec // G101: annotation names
const (
	annotationDeletionSecretName      = "volume.kubernetes.io/provisioner-deletion-secret-name"
	annotationDeletionSecretNamespace = "volume.kubernetes.io/provisioner-deletion-secret-namespace"
)

// SecretName is the Secret of the identity of a namespace.
func (e *Enroller) SecretName(namespace string) string {
	if namespace == e.Namespace {
		return ClusterSecretName
	}

	return TenantSecretPrefix + namespace
}

// managed says whether the enroller made the Secret.
func managed(s *corev1.Secret) bool {
	return s.Type == SecretType && s.Labels[labelManagedBy] == managedBy
}

// secretFor builds the Secret of ri's identity from files (identity.FileNames).
func (e *Enroller) secretFor(ri *RetycIdentity, nsUID string, files map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: e.SecretName(ri.Namespace), Namespace: e.Namespace,
			Labels: map[string]string{
				labelManagedBy:    managedBy,
				LabelNamespace:    ri.Namespace,
				LabelNamespaceUID: nsUID,
				LabelIdentityUID:  string(ri.UID),
			},
		},
		Type: SecretType,
		Data: files,
	}
}

// k8sInfo is the k8s.json of an identity.
func (e *Enroller) k8sInfo(namespace, nsUID, clusterUID, fingerprint string) ([]byte, error) {
	info := identity.K8sInfo{Mode: identity.ModeCluster, ClusterUID: clusterUID, Fingerprint: fingerprint}
	if namespace != e.Namespace {
		info.Mode, info.Namespace, info.NamespaceUID = identity.ModeTenant, namespace, nsUID
	}
	raw, err := json.Marshal(info)
	if err != nil {
		return nil, fmt.Errorf("encoding %s: %w", identity.K8sFile, err)
	}

	return raw, nil
}

// referenced says whether a PV of the driver uses the Secret name (of the driver's namespace):
// through its CSI secret references or the provisioner's deletion secret. The cluster-wide
// identity is also used by every PV that names no node secret (the mounted default identity): its
// Secret is kept while they exist once its RetycIdentity is gone, but never blocks a new one
// (clearSecret).
func (e *Enroller) referenced(ctx context.Context, name string) (bool, error) {
	pvs, err := e.Kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("listing PVs: %w", err)
	}
	for i := range pvs.Items {
		pv := &pvs.Items[i]
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != driver.DriverName {
			continue
		}
		if pv.Annotations[annotationDeletionSecretName] == name &&
			pv.Annotations[annotationDeletionSecretNamespace] == e.Namespace {
			return true, nil
		}
		csi := pv.Spec.CSI
		refs := []*corev1.SecretReference{csi.NodeStageSecretRef, csi.NodePublishSecretRef,
			csi.ControllerPublishSecretRef, csi.ControllerExpandSecretRef, csi.NodeExpandSecretRef}
		for _, ref := range refs {
			if ref != nil && ref.Name == name && ref.Namespace == e.Namespace {
				return true, nil
			}
		}
		if name == ClusterSecretName && csi.NodeStageSecretRef == nil && csi.NodePublishSecretRef == nil {
			return true, nil
		}
	}

	return false, nil
}

// machineRecord is what the enroller reads of machine.json.
type machineRecord struct {
	MachineID string `json:"machine_id"`
	ClientID  string `json:"client_id"`
}

func readMachine(raw []byte) machineRecord {
	var m machineRecord
	_ = json.Unmarshal(raw, &m) // best effort: the status only

	return m
}
