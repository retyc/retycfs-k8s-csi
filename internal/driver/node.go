package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	mountutils "k8s.io/mount-utils"

	"github.com/retyc/retycfs-k8s-csi/internal/mountpod"
)

// Mounter is what the node plugin needs from the host's mount table (mount.Mounter).
type Mounter interface {
	BindMount(source, target string, readonly bool) error
	Unpublish(target string) error
	State(target string) (mounted, corrupted bool, err error)
	Unmount(target string) error
	Detach(target string) error
	Rebind(source, target string, readonly bool) error
}

// Event reasons, on the claim of the volume.
const (
	reasonWritesLost    = "WritesLost"
	reasonMountFailed   = "MountFailed"
	reasonMountGaveUp   = "MountGaveUp"
	reasonMountOOM      = "MountOOMKilled"
	reasonConfigChanged = "MountConfigChanged"
)

// NodeServer implements csi.NodeServer. NodeStageVolume starts a retycfs mount pod per volume,
// which mounts it directly on the staging path; NodePublishVolume bind-mounts the staging path into
// each pod's target path, the standard CSI pattern that lets several pods on the same node share
// one mount. The FUSE mount is thus the only mount of the volume on the host besides the target
// paths: kubelet's check that the staging mount has no other reference before NodeUnstageVolume
// passes. The mount pods are the only record of the staged volumes (see Recover).
type NodeServer struct {
	csi.UnimplementedNodeServer
	// NodeID is the Kubernetes name of the node: the mount pods run there.
	NodeID  string
	Mounter Mounter
	Pods    *mountpod.Manager
	// Kube reads the claims (their annotations, and their UID for the events).
	Kube kubernetes.Interface
	// Events records on the claims what the volume's users must know; nil records nothing.
	Events record.EventRecorder
	// IdentityDir holds the driver's own identity (its Secret, mounted), read at each stage without
	// node-stage secrets.
	IdentityDir string
	// Cache is the listing cache of every mount pod (node-wide).
	Cache mountpod.Cache
	// MountInfo reads the mount table; nil reads /proc/self/mountinfo.
	MountInfo func() ([]mountutils.MountInfo, error)

	// Metrics are updated by every Recover round; nil keeps none.
	Metrics *NodeMetrics

	locks  keyLock  // by volume ID, as mountpod.Manager requires
	busy   sync.Map // volume ID → time.Time: a stage or unstage is under way, see markBusy
	gaveUp sync.Map // volume ID → time.Time: MountGaveUp was reported, then last seen; see stageGaveUp
	// pod UID → struct{}: the exit of a mount pod already reported while kubelet retried the stage,
	// not to report it again when the pod is released; see reportExitEarly.
	exitReported sync.Map
	// driverDirUnreadable is set once the error on kubelet's driver directory is logged (see
	// driverDirReadable).
	driverDirUnreadable atomic.Bool
}

func (s *NodeServer) NodeGetCapabilities(
	context.Context, *csi.NodeGetCapabilitiesRequest,
) (*csi.NodeGetCapabilitiesResponse, error) {
	capability := func(t csi.NodeServiceCapability_RPC_Type) *csi.NodeServiceCapability {
		return &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{Type: t},
			},
		}
	}

	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			capability(csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME),
			capability(csi.NodeServiceCapability_RPC_GET_VOLUME_STATS),
			capability(csi.NodeServiceCapability_RPC_VOLUME_CONDITION),
			// Without it, kubelet maps ReadWriteOncePod onto the older SINGLE_NODE_WRITER.
			capability(csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER),
		},
	}, nil
}

func (s *NodeServer) NodeGetInfo(context.Context, *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{NodeId: s.NodeID}, nil
}

// namespaceName is a Kubernetes namespace name (RFC 1123 label).
var namespaceName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// claimNamespace returns the claim namespace recorded in a volume context, or "" when it is absent
// or not a namespace name: a static PV's attributes are free text.
func claimNamespace(volumeContext map[string]string) string {
	ns := volumeContext[volumeContextNamespace]
	if ns != "" && !namespaceName.MatchString(ns) {
		klog.Warningf("ignoring volume_context[%q] = %q: not a namespace name", volumeContextNamespace, ns)

		return ""
	}

	return ns
}

// busyFor is how long a stage or unstage that returned Aborted keeps its volume out of the
// recovery's orphan sweep: kubelet retries within about two minutes.
const busyFor = 10 * time.Minute

// markBusy records that kubelet is staging or unstaging a volume: between two of its retries the
// mount pod may not be mounted on the staging path yet, or be draining, and must be left to the
// stage or unstage. The mark stays only while the call returns Aborted, that is while kubelet
// retries: any other outcome clears it (see clearBusy).
func (s *NodeServer) markBusy(volumeID string) { s.busy.Store(volumeID, time.Now()) }

// clearBusy drops the mark of markBusy once a stage or unstage returns anything but Aborted.
func (s *NodeServer) clearBusy(volumeID string, err error) {
	if status.Code(err) != codes.Aborted {
		s.busy.Delete(volumeID)
	}
}

func (s *NodeServer) isBusy(volumeID string) bool {
	v, ok := s.busy.Load(volumeID)

	return ok && time.Since(v.(time.Time)) < busyFor
}

func (s *NodeServer) NodeStageVolume(
	ctx context.Context, req *csi.NodeStageVolumeRequest,
) (_ *csi.NodeStageVolumeResponse, err error) {
	volumeID, staging := req.GetVolumeId(), req.GetStagingTargetPath()
	if volumeID == "" || staging == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID and staging path are required")
	}
	if req.GetVolumeCapability() == nil || !isSupportedCapability(req.GetVolumeCapability()) {
		return nil, status.Error(codes.InvalidArgument, "only filesystem volumes are supported")
	}
	defer s.locks.Lock(volumeID)()
	s.markBusy(volumeID)
	defer func() { s.clearBusy(volumeID, err) }()

	spec, claim, err := s.stageSpec(ctx, req)
	if err != nil {
		return nil, err
	}
	if _, err := s.ensureMounted(ctx, spec, claim); err != nil {
		return nil, err
	}
	if err := s.checkStaged(volumeID, staging); err != nil {
		return nil, err
	}
	s.gaveUp.Delete(volumeID)
	klog.Infof("NodeStageVolume: volume %s staged at %s", volumeID, staging)

	return &csi.NodeStageVolumeResponse{}, nil
}

// stageSpec builds the mount pod spec of a stage request: settings from the claim's annotations
// over the volume context (a static PV's volumeAttributes), credentials from the node-stage
// secrets (a tenant's) or the driver's default identity, never for a volume of a tenant class (see
// checkDefaultIdentity), and a tenant identity only in the namespace it was enrolled for (see
// checkIdentityNamespace).
func (s *NodeServer) stageSpec(
	ctx context.Context, req *csi.NodeStageVolumeRequest,
) (*mountpod.Spec, *corev1.PersistentVolumeClaim, error) {
	vc := req.GetVolumeContext()
	settings := make(map[string]string, len(vc))
	for k, v := range vc {
		settings[k] = v
	}
	name, namespace := vc[volumeContextPVCName], claimNamespace(vc)
	var claim *corev1.PersistentVolumeClaim
	if name != "" && namespace != "" {
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
		for k, v := range pvc.Annotations {
			settings[k] = v
		}
		claim = pvc
	}
	ms, err := parseMountSettings(settings)
	if err != nil {
		return nil, nil, status.Errorf(codes.InvalidArgument, "volume %s (claim %s/%s): %v", req.GetVolumeId(),
			namespace, name, err)
	}
	creds, err := resolveIdentity(req.GetSecrets(), s.IdentityDir, func() error {
		return s.checkDefaultIdentity(ctx, req.GetVolumeId(), claim)
	})
	if err != nil {
		return nil, nil, err
	}
	// A tenant identity is checked against the claim's namespace, and refused without a claim.
	claimNS := ""
	if claim != nil {
		claimNS = claim.Namespace
	}
	if err := checkIdentityNamespace(ctx, s.Kube, creds, claimNS); err != nil {
		return nil, nil, err
	}

	return &mountpod.Spec{
		VolumeID: req.GetVolumeId(), NodeName: s.NodeID, PVCName: name, PVCNamespace: namespace,
		StagingPath: req.GetStagingTargetPath(), Credentials: *creds, Cache: s.Cache,
		UID: ms.UID, GID: ms.GID, Options: ms.Options, Resources: ms.Resources,
	}, claim, nil
}

// ensureMounted makes sure the volume's mount pod runs spec and has mounted it on spec's staging
// path, and returns Aborted until then: kubelet retries, and a first mount (image pull, login)
// can outlast one call. A pod running another config is recreated if the volume is not
// published on the node, kept otherwise. A pod mounted on another staging path is recreated, unless
// that path is still mounted (FailedPrecondition). The caller holds the volume's lock.
//
// The FUSE mount is not on the staging path yet, so every error that a retry may cure is
// Unavailable or Aborted: on a final code, kubelet removes the staging path, vol_data.json and
// their directory, which a mount pod already started mounts from the host (its hostPath would point
// to a removed directory). Only the errors no retry cures are final: an invalid spec, the give-up.
//
// Once the pods of a volume gave up (credentials refused), the stage fails with FailedPrecondition:
// kubelet marks the volume as not staged (no NodeUnstageVolume follows), removes vol_data.json,
// reports FailedMount on the consumer pod and calls NodeStageVolume again with its exponential
// backoff (up to about 2 minutes) for as long as the pod waits for the volume. The exited pod is
// released at the next sweep (see deleteOrphan), its finalizer and Secret with it, but the Manager
// keeps the give-up, so each retry fails at once, without a new pod nor a new login, and reports
// MountGaveUp only once, until the credentials change (another config hash), a give-up on a 77
// expires (mountpod.GiveUpRetryAfter, not extended by the retries: one new try, the machine may
// have been enabled again), the exited pod is deleted by hand before the sweep (`kubectl delete
// pod`, see mountpod's startOver), no stage found it given up for orphanGrace (see pruneBackoffs) or the
// node plugin restarts.
func (s *NodeServer) ensureMounted(
	ctx context.Context, spec *mountpod.Spec, claim *corev1.PersistentVolumeClaim,
) (mountpod.State, error) {
	var st mountpod.State
	for range maxEnsureRounds {
		var err error
		st, err = s.Pods.Ensure(ctx, spec)
		switch {
		case errors.Is(err, mountpod.ErrGaveUp):
			if st.ExitMessage == "" {
				st.ExitMessage = st.Backoff.LastExitMessage // the pod is gone (see deleteOrphan)
			}
			s.stageGaveUp(claim, spec, st, err)

			return st, status.Errorf(codes.FailedPrecondition, "%v; last output: %s", err, st.ExitMessage)
		case errors.Is(err, mountpod.ErrInvalidSpec):
			return st, status.Errorf(codes.InvalidArgument, "volume %s: %v", spec.VolumeID, err)
		case err != nil: // the Kubernetes API, or Prepare
			return st, status.Errorf(codes.Unavailable, "mount pod of volume %s: %v", spec.VolumeID, err)
		}
		s.gaveUp.Delete(spec.VolumeID) // no longer given up (deleted by hand): a new give-up is reported
		if st.Phase == mountpod.PhaseTerminating {
			// Being deleted (a former recreate, an eviction): the stage finishes the deletion and
			// starts the next pod at once, without the restart backoff of the recovery.
			if err := s.recreate(ctx, spec); err != nil {
				return st, err
			}

			continue
		}
		starting := st.Phase == mountpod.PhasePending || st.Phase == mountpod.PhaseRunning
		if starting && st.StagingPath != spec.StagingPath {
			if err := s.restage(ctx, spec, st); err != nil {
				return st, err
			}

			continue
		}
		if st.ConfigChanged && starting {
			if binds, err := s.publishedAt(spec.VolumeID); err != nil {
				return st, status.Errorf(codes.Unavailable, "reading the mount table: %v", err)
			} else if len(binds) > 0 {
				s.event(claim, corev1.EventTypeWarning, reasonConfigChanged,
					"The settings of volume %s changed (credentials, annotations or image), but it is still in use on "+
						"node %s: its mount pod %s keeps the former ones until the volume is remounted there",
					spec.VolumeID, spec.NodeName, st.Name)
			} else if err := s.recreate(ctx, spec); err != nil {
				return st, err
			} else {
				continue
			}
		}

		return st, s.checkReady(ctx, st)
	}

	return st, status.Errorf(codes.Aborted, "mount pod %s of volume %s is being replaced", st.Name, spec.VolumeID)
}

// maxEnsureRounds bounds the loop of ensureMounted: each round releases or recreates a pod.
const maxEnsureRounds = 4

// stageGaveUp records that a stage found the volume's pods given up, and reports it on the claim
// the first time only: kubelet retries the stage every two minutes or so. The time recorded, each
// retry's, keeps the give-up from the sweep of the volumes gone (see pruneBackoffs).
func (s *NodeServer) stageGaveUp(claim *corev1.PersistentVolumeClaim, spec *mountpod.Spec, st mountpod.State,
	err error,
) {
	if _, reported := s.gaveUp.Swap(spec.VolumeID, s.Pods.Clock()); reported {
		klog.V(2).Infof("NodeStageVolume: volume %s: %v", spec.VolumeID, err)

		return
	}
	s.event(claim, corev1.EventTypeWarning, reasonMountGaveUp,
		"Volume %s cannot be mounted on node %s: %v. Last output: %s", spec.VolumeID, spec.NodeName, err,
		st.ExitMessage)
}

// restage handles a mount pod that mounts the volume on another staging path than the request's.
// kubelet's staging path is the SHA-256 of the volume handle, one per volume and node: such a pod is
// one left by a former layout (kubelet used to stage by PV name). While that path is still
// mounted the stage is refused, a final error that leaves the mount alone (kubelet only removes the
// request's path, which no pod mounts); otherwise the pod is recreated on the new path.
func (s *NodeServer) restage(ctx context.Context, spec *mountpod.Spec, st mountpod.State) error {
	infos, err := s.mountInfo()
	if err != nil {
		return status.Errorf(codes.Unavailable, "reading the mount table: %v", err)
	}
	if _, mounted := topMount(infos, st.StagingPath); mounted {
		return status.Errorf(codes.FailedPrecondition, "volume %s is still mounted on node %s at %s, a staging "+
			"path of a former kubelet layout, by mount pod %s: drain the pods using it on this node, or delete "+
			"that mount pod, before staging it at %s", spec.VolumeID, spec.NodeName, st.StagingPath, st.Name,
			spec.StagingPath)
	}
	klog.Infof("NodeStageVolume: mount pod %s of volume %s mounts %q, not %s: recreating it", st.Name,
		spec.VolumeID, st.StagingPath, spec.StagingPath)

	return s.recreate(ctx, spec)
}

// recreate stops a mount pod (a former config, a former staging path, or one being deleted) for
// ensureMounted to start a new one: nil once it is gone, Aborted while it drains. The mount process
// unmounts the staging path itself once drained; Prepare unmounts what a killed one left.
func (s *NodeServer) recreate(ctx context.Context, spec *mountpod.Spec) error {
	st, err := s.Pods.Delete(ctx, spec.NodeName, spec.VolumeID)
	if err != nil {
		return status.Errorf(codes.Unavailable, "%v", err)
	}
	if st.Phase == mountpod.PhaseTerminating {
		return status.Errorf(codes.Aborted, "volume %s: waiting for mount pod %s to drain before starting a new one",
			spec.VolumeID, st.Name)
	}
	s.reportExit(ctx, st)
	klog.Infof("NodeStageVolume: starting a new mount pod for volume %s", spec.VolumeID)

	return nil
}

// checkReady returns nil once the pod's /readyz answers, Aborted with the reason otherwise.
func (s *NodeServer) checkReady(ctx context.Context, st mountpod.State) error {
	switch st.Phase {
	case mountpod.PhaseRunning:
		if err := s.Pods.Ready(ctx, st.Pod); err != nil {
			return status.Errorf(codes.Aborted, "volume %s is not mounted yet: %v", st.VolumeID, err)
		}

		return nil
	case mountpod.PhasePending:
		if st.WaitingReason != "" {
			return status.Errorf(codes.Aborted, "mount pod %s is starting: %s: %s", st.Name, st.WaitingReason,
				st.WaitingMessage)
		}

		return status.Errorf(codes.Aborted, "mount pod %s is starting", st.Name)
	case mountpod.PhaseExited, mountpod.PhaseAbsent:
		s.reportExitEarly(ctx, st)
		next := "now"
		if !st.Backoff.NextRestart.IsZero() {
			next = "at " + st.Backoff.NextRestart.Format(time.RFC3339)
		}

		return status.Errorf(codes.Aborted, "mount pod %s exited (code %s, %s %s), %d failures in a row: restarting %s",
			st.Name, exitCode(st.ExitCode, st.Backoff.LastExitCode), st.ExitReason, st.ExitMessage,
			st.Backoff.ConsecutiveFailures, next)
	default:
		return status.Errorf(codes.Aborted, "mount pod %s is %s", st.Name, st.Phase)
	}
}

func exitCode(code *int32, last int32) string {
	if code != nil {
		return fmt.Sprint(*code)
	}

	return fmt.Sprint(last)
}

// checkStaged checks that the node plugin sees the mount pod's FUSE mount on the staging path,
// alive. The mount table unreadable is Unavailable; a mount pod ready but whose mount is missing or
// dead here is Internal: the FUSE mount is on the staging path for kubelet, which leaves it alone.
func (s *NodeServer) checkStaged(volumeID, staging string) error {
	infos, err := s.mountInfo()
	if err != nil {
		return status.Errorf(codes.Unavailable, "staging volume %s: reading the mount table: %v", volumeID, err)
	}
	top, ok := topMount(infos, staging)
	if !ok || top.Source != mountSource(volumeID) {
		return status.Errorf(codes.Internal, "staging volume %s: the mount pod is ready but %s is not mounted in "+
			"the node plugin: check that the node plugin mounts %s with mountPropagation Bidirectional", volumeID,
			staging, path.Join(s.Pods.Config().KubeletDir, "plugins"))
	}
	if _, corrupted, err := s.Mounter.State(staging); err != nil || corrupted {
		return status.Errorf(codes.Internal, "staging volume %s: the mount on %s does not answer (corrupted: %t): %v",
			volumeID, staging, corrupted, err)
	}

	return nil
}

// NodeUnstageVolume stops the mount pod first and unmounts the staging path last: the staging mount
// is the FUSE mount itself, and unmounting it from the host would cut the process off before it
// drained. After SIGTERM the process sends the pending writes, then unmounts the staging path and
// exits; Aborted is returned meanwhile (up to the drain timeout). Once the pod is gone, whatever is
// left on the staging path (a dead mount, from a process killed at the end of its grace period) is
// unmounted, so the path is unmounted when kubelet gets OK.
func (s *NodeServer) NodeUnstageVolume(
	ctx context.Context, req *csi.NodeUnstageVolumeRequest,
) (_ *csi.NodeUnstageVolumeResponse, err error) {
	volumeID, staging := req.GetVolumeId(), req.GetStagingTargetPath()
	if volumeID == "" || staging == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID and staging path are required")
	}
	defer s.locks.Lock(volumeID)()
	s.markBusy(volumeID)
	defer func() { s.clearBusy(volumeID, err) }()

	// kubelet unpublishes every target path first. A bind left behind keeps the mount process from
	// unmounting: it is killed at the end of its grace period, and the dead mount unmounted below.
	if binds, err := s.publishedAt(volumeID); err == nil && len(binds) > 0 {
		klog.Warningf("NodeUnstageVolume: volume %s is still bound at %v: its mount pod stops once they are gone, "+
			"or at the end of its grace period", volumeID, binds)
	}
	st, err := s.Pods.Delete(ctx, s.NodeID, volumeID)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "stopping the mount pod of volume %s: %v", volumeID, err)
	}
	if st.Phase == mountpod.PhaseTerminating {
		return nil, status.Errorf(codes.Aborted, "mount pod %s is draining: sending the pending writes, for up to %s",
			st.Name, s.Pods.Config().DrainTimeout)
	}
	s.reportExit(ctx, st)
	// An explicit unstage ends a give-up: the next stage, if any, tries anew.
	s.gaveUp.Delete(volumeID)
	s.Pods.Forget(volumeID)
	if err := s.Mounter.Unpublish(staging); err != nil {
		return nil, status.Errorf(codes.Internal, "unmounting %s: %v", staging, err)
	}
	klog.Infof("NodeUnstageVolume: volume %s unstaged from %s", volumeID, staging)

	return &csi.NodeUnstageVolumeResponse{}, nil
}

// reportExit tells how a released mount pod ended: an Event on the claim when writes were lost
// (WritesLost: exit 75, or a loss the output reports, see mountpod.LostWrites), or when it failed
// with code 1 (MountFailed), or when it was killed without being asked to stop, an OOM kill most
// likely (MountOOMKilled). 77 and 78
// are left to the backoff (MountGaveUp).
func (s *NodeServer) reportExit(ctx context.Context, st mountpod.State) {
	if st.Phase != mountpod.PhaseExited || st.ExitCode == nil {
		return
	}
	if st.Pod != nil {
		if _, done := s.exitReported.LoadAndDelete(st.Pod.UID); done {
			return
		}
	}
	s.emitExit(ctx, st)
}

// emitExit logs the exit of a mount pod and records the Event it calls for on the claim.
func (s *NodeServer) emitExit(ctx context.Context, st mountpod.State) {
	code := *st.ExitCode
	switch {
	case code == 0:
		klog.Infof("mount pod %s of volume %s exited cleanly", st.Name, st.VolumeID)
	case mountpod.LostWrites(st):
		klog.Errorf("mount pod %s of volume %s exited with code %d: %s: %s", st.Name, st.VolumeID, code,
			mountpod.ExitLabel(st), st.ExitMessage)
		s.event(s.claimRef(ctx, st.PVCNamespace, st.PVCName), corev1.EventTypeWarning, reasonWritesLost,
			"Writes lost on volume %s, node %s: files being written could not be sent to Retyc before the mount "+
				"pod stopped (code %d). Its output: %s", st.VolumeID, st.NodeName, code, st.ExitMessage)
	case code == mountpod.ExitFailed:
		klog.Errorf("mount pod %s of volume %s failed with code 1 (%s): %s", st.Name, st.VolumeID,
			mountpod.ExitLabel(st), st.ExitMessage)
		what := "failed before mounting it (nothing was written)"
		if st.WasReady {
			what = "failed after mounting it (it reported no lost writes)"
		}
		s.event(s.claimRef(ctx, st.PVCNamespace, st.PVCName), corev1.EventTypeWarning, reasonMountFailed,
			"The mount pod of volume %s on node %s %s. Its output: %s", st.VolumeID, st.NodeName, what, st.ExitMessage)
	case killedUnasked(st):
		limit := memoryLimit(st.Pod)
		how := "was killed at its memory limit (" + limit + ")"
		if st.ExitReason != "OOMKilled" {
			// containerd does not always tell an OOM kill: a kill nobody asked for is most likely one.
			how = "was killed (SIGKILL) while serving, most likely at its memory limit (" + limit + ")"
		}
		klog.Errorf("mount pod %s of volume %s %s", st.Name, st.VolumeID, how)
		s.event(s.claimRef(ctx, st.PVCNamespace, st.PVCName), corev1.EventTypeWarning, reasonMountOOM,
			"The mount pod of volume %s on node %s %s: files being written then may not have reached Retyc. "+
				"Every file or folder walked holds memory until the kernel forgets it; raise the "+
				"csi.retycfs.com/mount-memory-limit annotation of the claim", st.VolumeID, st.NodeName, how)
	default:
		klog.Warningf("mount pod %s of volume %s exited with code %d (%s): %s", st.Name, st.VolumeID, code,
			mountpod.ExitLabel(st), st.ExitMessage)
	}
}

// killedUnasked says whether a mount pod was killed (137) while no deletion asked it to stop: an OOM
// kill, which containerd reports as OOMKilled only some of the time, or a kill -9. A 137 of a pod
// being deleted is the end of the grace period of a drain. The exit is seen at once, before the
// node plugin deletes the pod to recreate it (reportExitEarly).
func killedUnasked(st mountpod.State) bool {
	if st.ExitReason == "OOMKilled" {
		return true
	}

	return st.ExitCode != nil && *st.ExitCode == 137 && st.Pod != nil && st.Pod.DeletionTimestamp == nil
}

// memoryLimit returns the memory limit of a mount pod's container, "no limit" without one.
func memoryLimit(pod *corev1.Pod) string {
	if pod != nil {
		for _, c := range pod.Spec.Containers {
			if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
				return q.String()
			}
		}
	}

	return "no limit"
}

// reportExitEarly reports the exit of a mount pod that a stage finds dead, once per pod: the pod
// is only released at its restart, and the claim would otherwise get no Event while kubelet
// retries (MountFailed for a failed start). reportExit skips it at the release.
func (s *NodeServer) reportExitEarly(ctx context.Context, st mountpod.State) {
	// A pod deleted by hand is still Terminating (its finalizer holds it) once its container exited.
	if (st.Phase != mountpod.PhaseExited && st.Phase != mountpod.PhaseTerminating) || st.ExitCode == nil ||
		st.Pod == nil {
		return
	}
	if _, done := s.exitReported.LoadOrStore(st.Pod.UID, struct{}{}); done {
		return
	}
	s.emitExit(ctx, st)
}

// claimRef returns the claim to record an event on, nil without one. The claim is read for its
// UID, which `kubectl describe` matches events on.
func (s *NodeServer) claimRef(ctx context.Context, namespace, name string) runtime.Object {
	if namespace == "" || name == "" {
		return nil
	}
	if s.Kube != nil {
		pvc, err := s.Kube.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return pvc
		}
	}

	return &corev1.ObjectReference{Kind: "PersistentVolumeClaim", APIVersion: "v1", Namespace: namespace, Name: name}
}

func (s *NodeServer) event(obj runtime.Object, eventType, reason, format string, args ...any) {
	if s.Events == nil || obj == nil {
		return
	}
	if pvc, ok := obj.(*corev1.PersistentVolumeClaim); ok && pvc == nil {
		return
	}
	s.Events.Eventf(obj, eventType, reason, format, args...)
}

func (s *NodeServer) NodePublishVolume(
	ctx context.Context, req *csi.NodePublishVolumeRequest,
) (*csi.NodePublishVolumeResponse, error) {
	source := req.GetStagingTargetPath()
	target := req.GetTargetPath()
	if source == "" || target == "" || req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID, staging and target paths are required")
	}
	// kubelet does not stage a volume again for a new pod while another pod on the node still
	// uses it: a dead staging mount has to be repaired here, or the pod never starts.
	if err := s.repairStaging(ctx, req.GetVolumeId(), source); err != nil {
		return nil, status.Error(codes.Unavailable, err.Error())
	}

	klog.Infof("NodePublishVolume: bind-mounting %s at %s (readonly=%t)", source, target, req.GetReadonly())
	if err := s.Mounter.BindMount(source, target, req.GetReadonly()); err != nil {
		return nil, status.Errorf(codes.Internal, "publishing volume: %v", err)
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (s *NodeServer) NodeUnpublishVolume(
	_ context.Context, req *csi.NodeUnpublishVolumeRequest,
) (*csi.NodeUnpublishVolumeResponse, error) {
	target := req.GetTargetPath()
	if target == "" {
		return nil, status.Error(codes.InvalidArgument, "target path is required")
	}

	if err := s.Mounter.Unpublish(target); err != nil {
		return nil, status.Errorf(codes.Internal, "unpublishing %s: %v", target, err)
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (s *NodeServer) NodeGetVolumeStats(
	ctx context.Context, req *csi.NodeGetVolumeStatsRequest,
) (*csi.NodeGetVolumeStatsResponse, error) {
	volumeID, path := req.GetVolumeId(), req.GetVolumePath()
	if volumeID == "" || path == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID and volume path are required")
	}
	mounted, corrupted, err := s.Mounter.State(path)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "checking %s: %v", path, err)
	}
	if !mounted {
		return nil, status.Errorf(codes.NotFound, "volume %s is not mounted at %s", volumeID, path)
	}
	condition := s.volumeCondition(ctx, volumeID)
	if corrupted {
		return &csi.NodeGetVolumeStatsResponse{VolumeCondition: &csi.VolumeCondition{
			Abnormal: true, Message: fmt.Sprintf("the mount at %s is dead (transport endpoint is not connected): "+
				"the node plugin remounts it; %s", path, condition.GetMessage()),
		}}, nil
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, status.Errorf(codes.NotFound, "%s: %v", path, err)
		}

		return &csi.NodeGetVolumeStatsResponse{VolumeCondition: &csi.VolumeCondition{
			Abnormal: true, Message: fmt.Sprintf("statfs %s: %v", path, err),
		}}, nil
	}
	bsize := uint64(fs.Bsize) //nolint:gosec // G115: a block size is positive

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Unit:      csi.VolumeUsage_BYTES,
				Total:     toInt64(fs.Blocks * bsize),
				Available: toInt64(fs.Bavail * bsize),
				Used:      toInt64((fs.Blocks - fs.Bfree) * bsize),
			},
			{
				Unit: csi.VolumeUsage_INODES, Total: toInt64(fs.Files), Available: toInt64(fs.Ffree),
				Used: toInt64(fs.Files - fs.Ffree),
			},
		},
		VolumeCondition: condition,
	}, nil
}

func toInt64(v uint64) int64 {
	return int64(min(v, 1<<63-1)) //nolint:gosec // G115: bounded above
}

// volumeCondition is abnormal while the volume's mount pod is not running and ready, nil when the
// API cannot tell (a transient error is no reason to flag the volume).
func (s *NodeServer) volumeCondition(ctx context.Context, volumeID string) *csi.VolumeCondition {
	st, err := s.Pods.Get(ctx, s.NodeID, volumeID)
	if err != nil {
		klog.V(2).Infof("NodeGetVolumeStats: %v", err)

		return nil
	}
	if st.Phase != mountpod.PhaseRunning {
		return &csi.VolumeCondition{Abnormal: true, Message: fmt.Sprintf("mount pod %s is %s %s", st.Name, st.Phase,
			st.WaitingReason)}
	}
	if err := s.Pods.Ready(ctx, st.Pod); err != nil {
		return &csi.VolumeCondition{Abnormal: true, Message: err.Error()}
	}

	return &csi.VolumeCondition{Message: fmt.Sprintf("mount pod %s is ready", st.Name)}
}
