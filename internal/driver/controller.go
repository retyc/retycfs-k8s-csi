package driver

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

	"github.com/retyc/retycfs-k8s-csi/internal/identity"
	"github.com/retyc/retycfs-k8s-csi/internal/retycclient"
)

// ControllerServer implements csi.ControllerServer by execing the retyc CLI. 1 CSI volume = 1
// Retyc dataroom, titled after the CSI-generated volume name (req.GetName(), always unique), and
// identified by the dataroom ID (the volume handle).
type ControllerServer struct {
	csi.UnimplementedControllerServer
	// Retyc runs the CLI with the driver's environment and no identity: each request gets a copy
	// that runs as its identity (see client).
	Retyc *retycclient.Client
	// IdentityDir holds the driver's default identity (its Secret, mounted), read at each request
	// that carries no CSI secrets.
	IdentityDir string
	// Kube reads the claim behind a CreateVolume request (its annotations) and the namespace of a
	// tenant identity. In-cluster client in production, k8s.io/client-go/kubernetes/fake in tests.
	Kube kubernetes.Interface
	// Events records on the claims what their owners must know (initial members); nil records
	// nothing.
	Events record.EventRecorder

	// members tracks the detached additions of initial members (see addInitialMembers), so that
	// tests can wait for them.
	members sync.WaitGroup
}

// client returns the retyc client of a request, running as the tenant's identity when the request
// carries secrets, the driver's default one otherwise (see resolveIdentity). namespace is the
// claim's namespace, which a tenant identity must have been enrolled for; "" skips that check, for
// the requests that do not name the claim (delete, expand, modify: the volume was created by then).
func (s *ControllerServer) client(
	ctx context.Context, secrets map[string]string, namespace string,
) (*retycclient.Client, error) {
	creds, err := resolveIdentity(secrets, s.IdentityDir, nil)
	if err != nil {
		return nil, err
	}
	if namespace != "" {
		if err := checkIdentityNamespace(ctx, s.Kube, creds, namespace); err != nil {
			return nil, err
		}
	}

	return s.Retyc.WithIdentity(creds), nil
}

func (s *ControllerServer) ControllerGetCapabilities(
	context.Context, *csi.ControllerGetCapabilitiesRequest,
) (*csi.ControllerGetCapabilitiesResponse, error) {
	capability := func(t csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
		return &csi.ControllerServiceCapability{
			Type: &csi.ControllerServiceCapability_Rpc{
				Rpc: &csi.ControllerServiceCapability_RPC{Type: t},
			},
		}
	}

	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			capability(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
			capability(csi.ControllerServiceCapability_RPC_EXPAND_VOLUME),
			capability(csi.ControllerServiceCapability_RPC_MODIFY_VOLUME),
		},
	}, nil
}

// isSupportedCapability accepts any filesystem-mount access mode (single- or multi-node); block
// volumes are rejected — a FUSE mount is a filesystem, not a raw device.
func isSupportedCapability(cap *csi.VolumeCapability) bool {
	if cap.GetBlock() != nil {
		return false
	}
	switch cap.GetAccessMode().GetMode() {
	case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:
		return true
	default:
		return false
	}
}

// Parameters. The csi.storage.k8s.io/ prefix is reserved for the sidecars: secret references
// (stripped before the call) and the claim metadata added by csi-provisioner
// --extra-create-metadata (or csi-resizer --extra-modify-metadata).
const (
	kubernetesParameterPrefix = "csi.storage.k8s.io/"
	pvcNameParameter          = kubernetesParameterPrefix + "pvc/name"
	pvcNamespaceParameter     = kubernetesParameterPrefix + "pvc/namespace"
	// versioningParameter is the VolumeAttributesClass parameter applied by ControllerModifyVolume
	// (and at creation, from CreateVolumeRequest.mutable_parameters).
	versioningParameter = "versioning"
)

// Volume context keys, persisted on the PV (spec.csi.volumeAttributes) and handed to
// NodeStageVolume, which reads the claim's annotations from them.
const (
	volumeContextPVCName   = "pvcName"
	volumeContextNamespace = "pvcNamespace"
)

// checkParameters rejects StorageClass parameters the driver does not know. The driver has none
// of its own: only the sidecars' csi.storage.k8s.io/* keys are accepted.
func checkParameters(params map[string]string) error {
	for key := range params {
		if !strings.HasPrefix(key, kubernetesParameterPrefix) {
			return status.Errorf(codes.InvalidArgument, "unknown parameter %q", key)
		}
	}

	return nil
}

// parseMutableParameters reads the VolumeAttributesClass parameters. versioning is nil when the
// class does not set it. Unknown keys are refused, except the sidecars' csi.storage.k8s.io/* ones.
func parseMutableParameters(params map[string]string) (versioning *bool, err error) {
	for key, value := range params {
		switch {
		case key == versioningParameter:
			v, err := strconv.ParseBool(value)
			if err != nil {
				return nil, status.Errorf(codes.InvalidArgument,
					"mutable parameter %s = %q: want true or false", key, value)
			}
			versioning = &v
		case strings.HasPrefix(key, kubernetesParameterPrefix):
		default:
			return nil, status.Errorf(codes.InvalidArgument, "unknown mutable parameter %q", key)
		}
	}

	return versioning, nil
}

// MinDataroomCapacity is the smallest storage reservation the Retyc API accepts for a dataroom
// (20 GiB: below it, 422 "must be at least 21474836480 bytes"). A smaller request is rounded up to
// it, as a block storage rounds up to its allocation unit, and the volume reports the real size.
const MinDataroomCapacity int64 = 20 << 30

// reservedBytes validates a capacity range and returns the reservation to make: its required bytes
// rounded up to MinDataroomCapacity, or 0 (no reservation, the owner's plan bounds the dataroom)
// when the range is unset. A limit below the floor cannot be met: OutOfRange.
func reservedBytes(r *csi.CapacityRange) (int64, error) {
	required, limit := r.GetRequiredBytes(), r.GetLimitBytes()
	if required < 0 || limit < 0 {
		return 0, status.Error(codes.InvalidArgument, "capacity range must not be negative")
	}
	if limit > 0 && required > limit {
		return 0, status.Errorf(codes.OutOfRange,
			"required_bytes %d is above limit_bytes %d", required, limit)
	}
	if required == 0 && limit == 0 {
		return 0, nil
	}
	if limit > 0 && limit < MinDataroomCapacity {
		return 0, status.Errorf(codes.OutOfRange,
			"limit_bytes %d is below the smallest dataroom reservation, %d bytes (20 GiB)", limit, MinDataroomCapacity)
	}

	return max(required, MinDataroomCapacity), nil
}

func (s *ControllerServer) CreateVolume(
	ctx context.Context, req *csi.CreateVolumeRequest,
) (*csi.CreateVolumeResponse, error) {
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}
	if len(req.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	for _, cap := range req.GetVolumeCapabilities() {
		if !isSupportedCapability(cap) {
			return nil, status.Error(codes.InvalidArgument, "only filesystem-mount volume capabilities are supported")
		}
	}
	if err := checkParameters(req.GetParameters()); err != nil {
		return nil, err
	}
	versioning, err := parseMutableParameters(req.GetMutableParameters())
	if err != nil {
		return nil, err
	}
	capacity, err := reservedBytes(req.GetCapacityRange())
	if err != nil {
		return nil, err
	}

	// Everything that can be refused is checked before anything is created on the Retyc side:
	// the claim and its owner annotation first.
	pvcName, pvcNamespace := req.GetParameters()[pvcNameParameter], req.GetParameters()[pvcNamespaceParameter]
	pvc, members, err := s.checkClaim(ctx, pvcNamespace, pvcName)
	if err != nil {
		return nil, err
	}

	if err := s.checkTenantSecrets(ctx, pvc, req.GetSecrets()); err != nil {
		return nil, err
	}
	retyc, err := s.client(ctx, req.GetSecrets(), pvcNamespace)
	if err != nil {
		return nil, err
	}

	// Idempotency (CSI requires CreateVolume to be safely retriable): if a dataroom titled `name`
	// already exists, reuse it instead of creating a duplicate. This runs before the quota check
	// so that a retry after a lost response still succeeds when that dataroom used the last slot.
	// Best-effort: there is no unique constraint on the backend, a racing double-create is still
	// possible.
	existing, err := retyc.ListDatarooms(ctx)
	if err != nil {
		return nil, apiError(err, "listing datarooms")
	}
	for _, dr := range existing.Items {
		if dr.Title == name {
			// CSI: the same name with an incompatible capacity is AlreadyExists. A dataroom
			// without reservation (nil) is bounded by the owner's plan: nothing to compare, the
			// requested size is reported as is.
			reserved := capacity
			if dr.StorageCapacity != nil {
				reserved = *dr.StorageCapacity
				limit := req.GetCapacityRange().GetLimitBytes()
				if reserved < capacity || (limit > 0 && reserved > limit) {
					return nil, status.Errorf(codes.AlreadyExists,
						"dataroom %q for volume %q reserves %d bytes, incompatible with the requested range (required %d, limit %d)",
						dr.ID, name, reserved, capacity, limit)
				}
			}
			klog.Infof("CreateVolume: reusing existing dataroom %q for volume %q", dr.ID, name)
			if len(members) > 0 {
				// Members are added by the call that creates the dataroom only (see addInitialMembers),
				// possibly still running: nothing to report here, it reports its own failures.
				s.event(pvc, corev1.EventTypeNormal, reasonInitialMembers, "Dataroom %s of volume %s already exists: "+
					"its initial members (%s) are managed by the attempt that created it", dr.ID, name, AnnotationInitialMembers)
			}

			return createVolumeResponse(dr.ID, reserved, pvcName, pvcNamespace), nil
		}
	}

	quota, err := retyc.Quota(ctx)
	if err != nil {
		return nil, apiError(err, "checking quota")
	}
	if quota.IsUploadReadOnly {
		return nil, status.Error(codes.ResourceExhausted, "retyc account storage quota exceeded (read-only)")
	}
	if !quota.HasDataroomQuota() {
		return nil, status.Errorf(codes.ResourceExhausted, "retyc account dataroom limit reached (%d/%d)",
			quota.CountDataroom, *quota.MaxCountDataroom)
	}

	// The claim's request, rounded up to the API's floor; no request, no reservation.
	var reservation *int64
	if capacity > 0 {
		reservation = &capacity
	}
	result, err := retyc.CreateDataroom(ctx, name, reservation, versioning)
	if err != nil {
		return nil, apiError(err, "creating dataroom for volume %q", name)
	}
	klog.Infof("CreateVolume: created dataroom %q for volume %q (%d bytes)", result.ID, name, capacity)
	s.startInitialMembers(ctx, retyc, pvc, result.ID, members)

	return createVolumeResponse(result.ID, capacity, pvcName, pvcNamespace), nil
}

// providerSecretParam is the StorageClass parameter that gives its volumes provisioner secrets: the
// chart sets it on the tenant classes, whose volumes must never be created with the driver's own
// identity.
const providerSecretParam = kubernetesParameterPrefix + //nolint:gosec // G101: a parameter name
	"provisioner-secret-name"

// checkTenantSecrets refuses, with FailedPrecondition, a request of a tenant class (one with
// providerSecretParam) that carries no credentials: an empty or missing Secret must never fall back
// to the driver's identity. Incomplete credentials are refused by client. A class not found does not
// refuse (nothing to check); an API error is Unavailable.
func (s *ControllerServer) checkTenantSecrets(
	ctx context.Context, pvc *corev1.PersistentVolumeClaim, secrets map[string]string,
) error {
	class := pvc.Spec.StorageClassName
	if class == nil || *class == "" {
		return nil
	}
	creds, err := identity.FromSecrets(secrets)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "CSI secrets: %v", err)
	}
	if creds != nil {
		return nil
	}
	sc, err := s.Kube.StorageV1().StorageClasses().Get(ctx, *class, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		klog.Warningf("CreateVolume: StorageClass %s of claim %s/%s not found, not checked", *class, pvc.Namespace,
			pvc.Name)

		return nil
	}
	if err != nil {
		return status.Errorf(codes.Unavailable, "reading StorageClass %s: %v", *class, err)
	}
	if _, tenant := sc.Parameters[providerSecretParam]; tenant {
		return status.Errorf(codes.FailedPrecondition, "StorageClass %s is a tenant class (%s) but the request "+
			"carries no credentials (empty or missing Secret): the driver's own identity is not used for it",
			*class, providerSecretParam)
	}

	return nil
}

// checkClaim reads the claim a volume is provisioned for, validates its owner annotations and
// returns it with its initial members.
func (s *ControllerServer) checkClaim(
	ctx context.Context, namespace, name string,
) (*corev1.PersistentVolumeClaim, []member, error) {
	if namespace == "" || name == "" {
		return nil, nil, status.Errorf(codes.InvalidArgument,
			"parameters %s and %s are required (csi-provisioner --extra-create-metadata)",
			pvcNameParameter, pvcNamespaceParameter)
	}
	if s.Kube == nil {
		return nil, nil, status.Error(codes.FailedPrecondition, "no Kubernetes client: cannot read the claim")
	}
	pvc, err := s.Kube.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil, status.Errorf(codes.NotFound, "claim %s/%s not found", namespace, name)
	}
	if err != nil {
		return nil, nil, status.Errorf(codes.Unavailable, "reading claim %s/%s: %v", namespace, name, err)
	}
	if _, _, err := claimOwner(pvc.GetAnnotations()); err != nil {
		return nil, nil, status.Errorf(codes.InvalidArgument, "claim %s/%s: %v", namespace, name, err)
	}
	members, err := parseInitialMembers(pvc.GetAnnotations())
	if err != nil {
		return nil, nil, status.Errorf(codes.InvalidArgument, "claim %s/%s: %v", namespace, name, err)
	}

	return pvc, members, nil
}

// Event reasons, on the claim of the volume.
const (
	reasonInitialMembers = "InitialMembers"
	reasonNoAdmin        = "NoHumanAdmin"
)

// initialMembersTimeout bounds the additions of the initial members, each a CLI call with a rekey.
// The controller pod's terminationGracePeriodSeconds covers it (see Wait).
const initialMembersTimeout = 5 * time.Minute

// Wait returns once the additions of initial members still running are done: the process calls it
// before exiting, so that a rollout right after a CreateVolume does not drop them. Each is bounded
// by initialMembersTimeout.
func (s *ControllerServer) Wait() {
	s.members.Wait()
}

// startInitialMembers runs addInitialMembers in the background, so that CreateVolume answers right
// after the creation instead of holding the provisioner (whose timeout would make it retry).
func (s *ControllerServer) startInitialMembers(
	ctx context.Context, retyc *retycclient.Client, pvc *corev1.PersistentVolumeClaim, id string, members []member,
) {
	// Detached from the request, which ends with the response.
	ctx = context.WithoutCancel(ctx)
	s.members.Go(func() { s.addInitialMembers(ctx, retyc, pvc, id, members) })
}

// addInitialMembers adds the claim's initial members to the dataroom just created, once: a failure
// does not fail the volume, whose dataroom exists, and is not retried, but reported on the claim
// (whoever owns the claim adds the members by hand). Only the call that creates the dataroom adds
// them, never a retry that finds it, so that they are never added again in a loop; and they run
// in the background, detached from the request, so that the provisioner giving up on a slow call does not cut them
// short. With no admin added, the machine alone can transfer the dataroom: that is reported too.
func (s *ControllerServer) addInitialMembers(
	ctx context.Context, retyc *retycclient.Client, pvc *corev1.PersistentVolumeClaim, id string, members []member,
) {
	ctx, cancel := context.WithTimeout(ctx, initialMembersTimeout)
	defer cancel()
	var failed []string
	admins := 0
	for _, m := range members {
		if err := retyc.AddMember(ctx, id, m.Email, m.Role); err != nil {
			klog.Warningf("CreateVolume: dataroom %q: adding %s as %s: %v", id, m.Email, m.Role, err)
			failed = append(failed, fmt.Sprintf("%s as %s (%v)", m.Email, m.Role, err))

			continue
		}
		klog.Infof("CreateVolume: dataroom %q: added %s as %s", id, m.Email, m.Role)
		if m.Role == roleAdmin {
			admins++
		}
	}
	if len(failed) > 0 {
		s.event(pvc, corev1.EventTypeWarning, reasonInitialMembers, "Dataroom %s: could not add %s; the driver does "+
			"not retry: add them with `retyc dataroom user add`", id, strings.Join(failed, ", "))
	}
	if admins == 0 {
		s.event(pvc, corev1.EventTypeWarning, reasonNoAdmin, "Dataroom %s has no human admin (annotation %s): it can "+
			"only be transferred while its machine account is active", id, AnnotationInitialMembers)
	}
}

// event records an event on a claim; a no-op without a recorder or a claim.
func (s *ControllerServer) event(pvc *corev1.PersistentVolumeClaim, eventType, reason, format string, args ...any) {
	if s.Events == nil || pvc == nil {
		return
	}
	s.Events.Eventf(pvc, eventType, reason, format, args...)
}

// createVolumeResponse builds the CSI response for a dataroom. The requested capacity is the
// dataroom's reservation. VolumeContext names the claim, whose annotations NodeStageVolume reads.
func createVolumeResponse(id string, capacity int64, pvcName, pvcNamespace string) *csi.CreateVolumeResponse {
	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      id,
			CapacityBytes: capacity,
			VolumeContext: map[string]string{
				volumeContextPVCName:   pvcName,
				volumeContextNamespace: pvcNamespace,
			},
		},
	}
}

func (s *ControllerServer) DeleteVolume(
	ctx context.Context, req *csi.DeleteVolumeRequest,
) (*csi.DeleteVolumeResponse, error) {
	id := req.GetVolumeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	retyc, err := s.client(ctx, req.GetSecrets(), "")
	if err != nil {
		return nil, err
	}

	_, err = retyc.DeleteDataroom(ctx, id)
	if err != nil {
		// DeleteVolume must be idempotent: a dataroom already gone (e.g. a retried call after a
		// prior successful delete) is success, not an error.
		if retycclient.IsNotFound(err) {
			klog.Infof("DeleteVolume: dataroom %q already gone, treating as success", id)

			return &csi.DeleteVolumeResponse{}, nil
		}

		return nil, apiError(err, "deleting dataroom %q", id)
	}
	klog.Infof("DeleteVolume: deleted dataroom %q", id)

	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerExpandVolume enlarges the dataroom's reservation on the owner's plan. Nothing to do
// on the node (NodeExpansionRequired is false): the reservation is enforced by the API at once, and
// a running mount reports it (df, statfs) once its cached dataroom stats expire (the cache TTL,
// 1 min by default).
func (s *ControllerServer) ControllerExpandVolume(
	ctx context.Context, req *csi.ControllerExpandVolumeRequest,
) (*csi.ControllerExpandVolumeResponse, error) {
	id := req.GetVolumeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	if req.GetCapacityRange().GetRequiredBytes() == 0 {
		return nil, status.Error(codes.InvalidArgument, "capacity_range.required_bytes is required")
	}
	capacity, err := reservedBytes(req.GetCapacityRange())
	if err != nil {
		return nil, err
	}

	retyc, err := s.client(ctx, req.GetSecrets(), "")
	if err != nil {
		return nil, err
	}
	// `set` with the current reservation is accepted without effect (a retry after a lost response
	// succeeds), but a smaller one is a shrink the API refuses, and CSI wants a volume already at
	// or above the request to succeed with its real size. So the current reservation is read first,
	// and `set` skipped when it already covers the request: a reservation made larger elsewhere (the
	// web app, the CLI, a reused dataroom) is reported as is rather than refused. A dataroom missing
	// from the listing falls through to `set`, which reports NotFound.
	list, err := retyc.ListDatarooms(ctx)
	if err != nil {
		return nil, apiError(err, "listing datarooms")
	}
	for _, dr := range list.Items {
		if dr.ID == id && dr.StorageCapacity != nil && *dr.StorageCapacity >= capacity {
			klog.Infof("ControllerExpandVolume: dataroom %q already reserves %d bytes", id, *dr.StorageCapacity)

			return &csi.ControllerExpandVolumeResponse{
				CapacityBytes: *dr.StorageCapacity, NodeExpansionRequired: false,
			}, nil
		}
	}
	if err := retyc.UpdateDataroom(ctx, id, &capacity, nil); err != nil {
		return nil, updateError(id, err)
	}
	klog.Infof("ControllerExpandVolume: dataroom %q now reserves %d bytes", id, capacity)

	return &csi.ControllerExpandVolumeResponse{CapacityBytes: capacity, NodeExpansionRequired: false}, nil
}

// ControllerModifyVolume applies a VolumeAttributesClass: the `versioning` parameter. Disabling
// versioning is confirmed (-y) on the caller's behalf: the next completed upload of a file then
// deletes its older versions.
func (s *ControllerServer) ControllerModifyVolume(
	ctx context.Context, req *csi.ControllerModifyVolumeRequest,
) (*csi.ControllerModifyVolumeResponse, error) {
	id := req.GetVolumeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	versioning, err := parseMutableParameters(req.GetMutableParameters())
	if err != nil {
		return nil, err
	}
	if versioning == nil {
		// Only `versioning` is mutable: a class without it (or with only sidecar metadata) asks
		// for nothing the driver manages, and answering OK lets the resizer mark the claim as
		// modified rather than stuck retrying. Unknown keys were already refused above.
		return &csi.ControllerModifyVolumeResponse{}, nil
	}

	retyc, err := s.client(ctx, req.GetSecrets(), "")
	if err != nil {
		return nil, err
	}
	if err := retyc.UpdateDataroom(ctx, id, nil, versioning); err != nil {
		return nil, updateError(id, err)
	}
	klog.Infof("ControllerModifyVolume: dataroom %q versioning=%t", id, *versioning)

	return &csi.ControllerModifyVolumeResponse{}, nil
}

// updateError maps a `dataroom set` failure to a CSI status.
func updateError(id string, err error) error {
	if retycclient.IsNotFound(err) {
		return status.Errorf(codes.NotFound, "dataroom %q not found: %v", id, err)
	}

	return apiError(err, "updating dataroom %q", id)
}

// apiError maps a failure of the CLI to a CSI status, prefixed with what was being done, from the
// error's kind (or its text, with a CLI older than the kinds: see retycclient):
//   - no storage left (plan_exceeded, storage_exceeded): ResourceExhausted;
//   - a reservation that cannot be shrunk or is below the minimum: OutOfRange;
//   - any other validation error (invalid): InvalidArgument;
//   - the machine account refused (exit code 77), its configuration refused (78), this CLI refused
//     as too old (cli_too_old): FailedPrecondition, with what to fix. Not Unauthenticated: in gRPC
//     it says the RPC's caller is not authenticated, and here the sidecar's call is fine, it is the
//     identity in the CSI secrets (or the driver's own) that Retyc refuses;
//   - the API's 403 (forbidden): PermissionDenied;
//   - the API's rate limit (rate_limited): Unavailable, which the sidecars retry with a backoff;
//   - the rest: Internal.
func apiError(err error, format string, args ...any) error {
	code, hint := codes.Internal, ""
	switch {
	case retycclient.IsStorageExhausted(err):
		code = codes.ResourceExhausted
	case retycclient.IsNotEnlarged(err), retycclient.IsCapacityRefused(err):
		code = codes.OutOfRange
	case retycclient.IsValidation(err):
		code = codes.InvalidArgument
	case retycclient.IsMachineRefused(err):
		code, hint = codes.FailedPrecondition,
			" (the machine account is disabled or deleted on the Retyc side: enable it again in Organization > "+
				"Machines, or recreate the RetycIdentity)"
	case retycclient.IsConfig(err):
		code, hint = codes.FailedPrecondition,
			" (the machine account's configuration is refused: its AGE key is not the account's active key, a "+
				"file of it is readable by others, or RETYC_TOKEN/RETYC_KEY_PASSPHRASE are set alongside it)"
	case retycclient.IsCLITooOld(err):
		code, hint = codes.FailedPrecondition,
			" (the Retyc API refuses the retyc CLI of the driver image as too old: upgrade the driver)"
	case retycclient.IsForbidden(err):
		code = codes.PermissionDenied
	case retycclient.IsRateLimited(err):
		code = codes.Unavailable
	}

	return status.Errorf(code, "%s: %v%s", fmt.Sprintf(format, args...), err, hint)
}

func (s *ControllerServer) ValidateVolumeCapabilities(
	_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest,
) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}
	for _, cap := range req.GetVolumeCapabilities() {
		if !isSupportedCapability(cap) {
			return &csi.ValidateVolumeCapabilitiesResponse{
				Message: "only filesystem-mount volume capabilities are supported",
			}, nil
		}
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeContext:      req.GetVolumeContext(),
			VolumeCapabilities: req.GetVolumeCapabilities(),
			Parameters:         req.GetParameters(),
		},
	}, nil
}
