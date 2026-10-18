package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	mountutils "k8s.io/mount-utils"

	"github.com/retyc/retycfs-k8s-csi/internal/mountpod"
)

// procMountInfo is read from the node plugin's own mount namespace, which sees kubelet's staging
// and pod directories, with the mount pods' mounts on the staging paths, through its Bidirectional
// mounts of <kubeletDir>/plugins and <kubeletDir>/pods.
const procMountInfo = "/proc/self/mountinfo"

// RecoverInterval is how often Watch reconciles all the mount pods with the mounts, besides the
// repairs the pod watch triggers.
const RecoverInterval = 30 * time.Second

// orphanGrace is how long the give-up of a volume is remembered once kubelet no longer holds it
// staged, from the last stage that found it given up: kubelet retries a failed stage within about two
// minutes, each retry then fails at once without a new pod nor a new login. The exited pod itself
// is released at the next sweep (see deleteOrphan), and its Secret with it.
const orphanGrace = 30 * time.Minute

// volDataFile is the file kubelet writes in the directory of the staging path before calling
// NodeStageVolume, and removes only after a successful NodeUnstageVolume or a stage that failed with
// a final error: its presence says that kubelet holds the volume staged, or is staging it.
const volDataFile = "vol_data.json"

// kubeletStaged reports whether kubelet holds the volume of stagingPath staged (see volDataFile).
// A stat error other than "does not exist" counts as staged: nothing is deleted on a doubt. So does
// a driver directory (<kubeletDir>/plugins/kubernetes.io/csi/<driver>, above the volume's own) that
// the node plugin cannot read: kubelet's files are then out of its sight, and every volume would
// look unstaged (see driverDirReadable).
func (s *NodeServer) kubeletStaged(stagingPath string) bool {
	if stagingPath == "" {
		return false
	}
	if !s.driverDirReadable(path.Dir(path.Dir(stagingPath))) {
		return true
	}
	_, err := os.Stat(path.Join(path.Dir(stagingPath), volDataFile))

	return !errors.Is(err, os.ErrNotExist)
}

// driverDirReadable reports whether the node plugin can list dir, and logs an error the first time
// it cannot (again only once it could meanwhile): kubelet creates the directory of a driver before
// its first stage and never removes it, so missing, it is a hostPath of <kubeletDir>/plugins not
// mounted in the node plugin, or a kubeletDir that is not kubelet's --root-dir.
func (s *NodeServer) driverDirReadable(dir string) bool {
	f, err := os.Open(dir) //nolint:gosec // G304: a directory under the kubelet directory
	if err == nil {
		_, err = f.Readdirnames(1)
		_ = f.Close()
		if errors.Is(err, io.EOF) {
			err = nil
		}
	}
	if err == nil {
		s.driverDirUnreadable.Store(false)

		return true
	}
	if !s.driverDirUnreadable.Swap(true) {
		klog.Errorf("recovery: cannot read kubelet's directory of the driver: %v. Is kubeletDir kubelet's "+
			"--root-dir, and its plugins directory mounted in the node plugin? Until then no mount pod is deleted "+
			"as an orphan", err)
	}

	return false
}

// mountSource is the source of a volume's retycfs mount in the mount table: the FUSE mount and
// every bind mount of it.
func mountSource(volumeID string) string { return "retyc://" + volumeID }

func (s *NodeServer) mountInfo() ([]mountutils.MountInfo, error) {
	if s.MountInfo != nil {
		return s.MountInfo()
	}

	return mountutils.ParseMountInfo(procMountInfo)
}

// Recover reconciles the mount pods of the node with the mounts, the pods being the only record
// of the staged volumes: for each pod whose volume is staged (kubelet's vol_data.json is next to its
// staging path, recorded on the pod, or pod target paths still bind its mount), a dead pod is
// recreated on the staging path (after unmounting the dead mount) and a bind mount of the staging
// path is stacked on every pod target path still bound to a dead mount. Pods mounting the volume
// with mountPropagation HostToContainer see it come back; the others do at their next container
// restart. A pod whose volume is neither staged nor bound is an orphan (an unstage that never
// completed, a stage that failed for good) and is deleted (see deleteOrphan). Run it once before
// serving CSI requests, then from Watch; a node plugin restart cuts no mount. Nothing is deleted or
// created when the API cannot be read.
func (s *NodeServer) Recover(ctx context.Context) {
	states, err := s.Pods.List(ctx, s.NodeID)
	if err != nil {
		klog.Warningf("recovery: %v", err)

		return
	}
	results := make([]string, len(states))
	var wg sync.WaitGroup
	for i, st := range states {
		wg.Go(func() {
			defer s.locks.Lock(st.VolumeID)()
			state, _, err := s.recoverVolume(ctx, st.VolumeID, false)
			if err != nil {
				klog.Errorf("recovery: volume %s: %v", st.VolumeID, err)
			}
			results[i] = state
		})
	}
	wg.Wait()

	outdated := 0
	for _, st := range states {
		if st.CLIVersion != s.Pods.Config().CLIVersion {
			outdated++
		}
	}
	s.Metrics.update(results, outdated)
	s.warnPodless(states)
	s.pruneBackoffs(ctx, states)
}

// pruneBackoffs drops the restart state, a give-up included, of the volumes gone from the node: no
// mount pod, no stage under way, no vol_data.json next to the last staging path and, for a give-up,
// no stage that found it so for orphanGrace. Without it, a volume that gave up would refuse its next
// stage on this node, however late, until the node plugin restarts.
func (s *NodeServer) pruneBackoffs(ctx context.Context, states []mountpod.State) {
	known := map[string]bool{}
	for _, st := range states {
		known[st.VolumeID] = true
	}
	for volumeID, staging := range s.Pods.Volumes() {
		if known[volumeID] {
			continue
		}
		func() {
			defer s.locks.Lock(volumeID)()
			if s.isBusy(volumeID) || s.kubeletStaged(staging) {
				return
			}
			if at, ok := s.gaveUp.Load(volumeID); ok && s.Pods.Clock().Sub(at.(time.Time)) < orphanGrace {
				return // kubelet may still be retrying the stage: each retry fails at once
			}
			if st, err := s.Pods.Get(ctx, s.NodeID, volumeID); err != nil || st.Phase != mountpod.PhaseAbsent {
				return // created since the list
			}
			klog.V(2).Infof("recovery: volume %s is gone from this node: forgetting its restart state", volumeID)
			s.Pods.Forget(volumeID)
			s.gaveUp.Delete(volumeID)
		}()
	}
}

// Metric states of a mount pod (retycfs_csi_mount_pods{state}).
const (
	statePending     = "pending"
	stateRunning     = "running" // running, not ready
	stateReady       = "ready"
	stateTerminating = "terminating"
	stateExited      = "exited"
)

func metricState(phase mountpod.Phase) string {
	switch phase {
	case mountpod.PhasePending:
		return statePending
	case mountpod.PhaseRunning:
		return stateRunning
	case mountpod.PhaseTerminating:
		return stateTerminating
	case mountpod.PhaseExited:
		return stateExited
	default:
		return ""
	}
}

// recoverVolume repairs one volume (the caller holds its lock) and returns the metric state of its
// mount pod, "" when there is none, and when to look at it again (zero: at the next event or
// round). staged forces the volume to be taken as staged (from NodePublishVolume, which kubelet only
// calls on a staged volume).
func (s *NodeServer) recoverVolume(
	ctx context.Context, volumeID string, staged bool,
) (state string, retryAt time.Time, err error) {
	st, err := s.Pods.Get(ctx, s.NodeID, volumeID)
	if err != nil || st.Phase == mountpod.PhaseAbsent {
		return "", time.Time{}, err
	}
	infos, err := s.mountInfo()
	if err != nil {
		return metricState(st.Phase), time.Time{}, err
	}
	if !staged && s.isBusy(volumeID) {
		return metricState(st.Phase), time.Time{}, nil // kubelet's stage or unstage takes care of it
	}
	if !staged && !s.kubeletStaged(st.StagingPath) && len(s.binds(infos, volumeID)) == 0 {
		state, err := s.deleteOrphan(ctx, st)

		return state, time.Time{}, err
	}

	switch st.Phase {
	case mountpod.PhaseRunning:
		if err := s.Pods.Ready(ctx, st.Pod); err != nil {
			klog.V(2).Infof("recovery: volume %s: %v", volumeID, err)

			return stateRunning, time.Time{}, nil // mounting or stopping: its next status change says
		}
		s.gaveUp.Delete(volumeID)

		return stateReady, time.Time{}, s.rebind(infos, volumeID, st.StagingPath)
	case mountpod.PhaseExited, mountpod.PhaseTerminating:
		// Before Ensure releases it: its exit (lost writes, a failed start) is reported once.
		s.reportExitEarly(ctx, st)
		spec, err := s.Pods.SpecOf(ctx, st, s.Cache)
		if err != nil {
			return metricState(st.Phase), time.Time{}, err
		}
		nst, err := s.Pods.Ensure(ctx, spec)
		if errors.Is(err, mountpod.ErrGaveUp) {
			// A give-up on a 77 expires (GiveUpUntil): one more try then; none for a 78.
			return metricState(nst.Phase), nst.Backoff.GiveUpUntil, s.reportGaveUp(ctx, st, nst, err)
		}
		if err == nil {
			s.gaveUp.Delete(volumeID) // no longer given up (deleted by hand): a new give-up is reported
		}
		if nst.Phase != st.Phase {
			klog.Infof("recovery: volume %s is staged and its mount pod %s was %s: now %s", volumeID, st.Name,
				st.Phase, nst.Phase)
		}
		if nst.Phase == mountpod.PhaseExited || nst.Phase == mountpod.PhaseAbsent {
			// Waiting for its restart time, or past it when the restart failed: not at once then.
			retryAt = nst.Backoff.NextRestart
			if soonest := s.Pods.Clock().Add(minRetryDelay); retryAt.Before(soonest) {
				retryAt = soonest
			}
		}

		return metricState(nst.Phase), retryAt, err
	default:
		return metricState(st.Phase), time.Time{}, nil
	}
}

// reportGaveUp reports, with an Event on the claim and an error log, that a staged volume's mount
// pods gave up: once, when it happens, not at every round. A ready pod, a stage or an unstage of
// the volume clears it.
func (s *NodeServer) reportGaveUp(ctx context.Context, st, nst mountpod.State, err error) error {
	if _, reported := s.gaveUp.LoadOrStore(st.VolumeID, s.Pods.Clock()); reported {
		klog.V(2).Infof("recovery: volume %s: %v", st.VolumeID, err)

		return nil
	}
	s.event(s.claimRef(ctx, st.PVCNamespace, st.PVCName), corev1.EventTypeWarning, reasonMountGaveUp,
		"Volume %s is no longer mounted on node %s: %v. Last output: %s", st.VolumeID, s.NodeID, err,
		nst.ExitMessage)

	return err
}

// deleteOrphan stops a mount pod whose volume kubelet no longer holds staged and that is bound
// nowhere on the node, one step per round, whatever its phase: a stage under way would have its
// vol_data.json. An exited pod that gave up goes too, its finalizer and Secret with it: a stage that
// failed with the give-up is final for kubelet, which removed vol_data.json and never unstages the
// volume. The Manager keeps the give-up (see pruneBackoffs), so that kubelet's next retries of the
// stage, if any, fail at once without a new pod.
func (s *NodeServer) deleteOrphan(ctx context.Context, st mountpod.State) (string, error) {
	klog.Infof("recovery: volume %s is no longer staged on this node: deleting its %s mount pod %s", st.VolumeID,
		st.Phase, st.Name)
	dst, err := s.Pods.Delete(ctx, s.NodeID, st.VolumeID)
	if err != nil {
		return metricState(st.Phase), err
	}
	s.reportExit(ctx, dst)
	if dst.Phase == mountpod.PhaseTerminating {
		return stateTerminating, nil
	}

	return "", nil
}

// rebind stacks a bind of the staging path, where a ready mount pod mounted the volume, on every
// pod target path still bound to a dead mount of the volume. A container that mounts the volume
// with HostToContainer holds a copy of the layer on top when it started. Mounting or unmounting
// above that layer propagates into the container; unmounting the copied layer itself does not
// reach it, and a new bind on the bare directory then would not either. So the dead layers stay
// and the new bind is stacked on them. Past maxTargetLayers, the dead layers on top are unmounted
// first (see unstack): the containers started between two replacements of the mount pod copied
// one of them, and lose the volume until they restart.
func (s *NodeServer) rebind(infos []mountutils.MountInfo, volumeID, staging string) error {
	fuse, ok := topMount(infos, staging)
	if !ok || fuse.Source != mountSource(volumeID) {
		return fmt.Errorf("the mount pod is ready but %q is not mounted in the node plugin", staging)
	}
	var errs []error
	for _, t := range deadTargets(infos, volumeID, device(fuse), s.isTarget) {
		if n := layers(infos, t.MountPoint); n >= maxTargetLayers {
			klog.Warningf("recovery: %s holds %d layers: unmounting the dead ones on top first", t.MountPoint, n)
			if err := s.unstack(t.MountPoint); err != nil {
				errs = append(errs, err)

				continue
			}
		}
		klog.Infof("recovery: rebinding %s", t.MountPoint)
		errs = append(errs, s.Mounter.Rebind(staging, t.MountPoint, slices.Contains(t.MountOptions, "ro")))
	}

	return errors.Join(errs...)
}

// maxTargetLayers is the number of layers on a pod target path from which rebind unmounts the dead
// ones on top before stacking a new one: one layer per replacement of the mount pod while the pod
// runs, and Unpublish has to unmount them all.
const maxTargetLayers = 4

// layers counts the mounts stacked on a mount point.
func layers(infos []mountutils.MountInfo, mountPoint string) int {
	n := 0
	for _, info := range infos {
		if info.MountPoint == mountPoint {
			n++
		}
	}

	return n
}

// unstack unmounts the dead layers on top of mnt, as Prepare does for a staging path, down to a live
// one or to the bottom layer: a container started before the first replacement holds a copy of
// that one, which a bind stacked on it still reaches.
func (s *NodeServer) unstack(mnt string) error {
	for range maxMountLayers {
		infos, err := s.mountInfo()
		if err != nil {
			return err
		}
		mounted, corrupted, err := s.Mounter.State(mnt)
		if err != nil || !mounted || !corrupted || layers(infos, mnt) <= 1 {
			return err
		}
		if err := s.Mounter.Unmount(mnt); err != nil {
			klog.Warningf("unmounting a dead layer of %s: %v; detaching it", mnt, err)
			if err := s.Mounter.Detach(mnt); err != nil {
				return err
			}
		}
	}

	return nil
}

// isTarget reports whether a mount point is a pod target path, under <kubeletDir>/pods. Mounts of
// the volume anywhere else are not where it is published: the staging path, or a copy of a mount
// left in a private mount of the node plugin's own (its /csi socket directory, say), which must
// not be taken for a bind still in use.
func (s *NodeServer) isTarget(mountPoint string) bool {
	return strings.HasPrefix(mountPoint, path.Join(s.Pods.Config().KubeletDir, "pods")+"/")
}

// binds returns the pod target paths with a layer of the volume's mount, dead or alive: where the
// volume is still published.
func (s *NodeServer) binds(infos []mountutils.MountInfo, volumeID string) []string {
	var out []string
	for _, info := range infos {
		if info.Source == mountSource(volumeID) && s.isTarget(info.MountPoint) && !slices.Contains(out, info.MountPoint) {
			out = append(out, info.MountPoint)
		}
	}

	return out
}

// publishedAt returns the pod target paths the volume is bound on.
func (s *NodeServer) publishedAt(volumeID string) ([]string, error) {
	infos, err := s.mountInfo()
	if err != nil {
		return nil, err
	}

	return s.binds(infos, volumeID), nil
}

// warnPodless logs the volumes still mounted on the node that have no mount pod: deleted by hand
// with their finalizer removed. Nothing is left to recreate them from.
func (s *NodeServer) warnPodless(states []mountpod.State) {
	infos, err := s.mountInfo()
	if err != nil {
		return
	}
	known := map[string]bool{}
	for _, st := range states {
		known[st.VolumeID] = true
	}
	warned := map[string]bool{}
	for _, info := range infos {
		id, ok := strings.CutPrefix(info.Source, "retyc://")
		if !ok || known[id] || warned[id] || s.isBusy(id) {
			continue
		}
		warned[id] = true
		klog.Warningf("recovery: volume %s is mounted at %s but has no mount pod on this node: reschedule the pods "+
			"using it on this node", id, info.MountPoint)
	}
}

// repairStaging remounts stagingPath if its mount is dead or gone (locked per volume). kubelet
// only publishes a volume it staged, so an unmounted staging path is one a failed recovery left.
func (s *NodeServer) repairStaging(ctx context.Context, volumeID, stagingPath string) error {
	healthy := func() (bool, error) {
		mounted, corrupted, err := s.Mounter.State(stagingPath)

		return mounted && !corrupted, err
	}
	if ok, err := healthy(); err != nil || ok {
		return err
	}
	defer s.locks.Lock(volumeID)()
	if ok, err := healthy(); err != nil || ok {
		return err // repaired by a concurrent call
	}
	st, err := s.Pods.Get(ctx, s.NodeID, volumeID)
	if err != nil {
		return err
	}
	if st.Phase == mountpod.PhaseAbsent {
		return fmt.Errorf("staging mount %s is down and volume %s has no mount pod on this node: reschedule the "+
			"pods using it on this node", stagingPath, volumeID)
	}
	if st.StagingPath != stagingPath {
		return fmt.Errorf("staging mount %s is down and the mount pod %s of volume %s mounts %q: reschedule the "+
			"pods using it on this node", stagingPath, st.Name, volumeID, st.StagingPath)
	}
	if _, _, err := s.recoverVolume(ctx, volumeID, true); err != nil {
		return err
	}
	if ok, err := healthy(); err != nil || !ok {
		return fmt.Errorf("staging mount %s is still down (%v): its mount pod %s is being restarted", stagingPath, err,
			st.Name)
	}

	return nil
}

// device formats a mount's device as major:minor.
func device(info mountutils.MountInfo) string {
	return fmt.Sprintf("%d:%d", info.Major, info.Minor)
}

// topMount returns the topmost mount on path. mountinfo lists mounts parent first, so the last
// entry for a mount point is its top layer.
func topMount(infos []mountutils.MountInfo, path string) (top mountutils.MountInfo, mounted bool) {
	if path == "" {
		return top, false
	}
	for _, info := range infos {
		if info.MountPoint == path {
			top, mounted = info, true
		}
	}

	return top, mounted
}

// deadTargets returns the mount points whose top layer is a bind mount of the volume's root from
// another device than alive (the current FUSE mount): the pod target paths NodePublishVolume bound
// to a mount that no longer answers. isTarget selects the pod target paths.
func deadTargets(
	infos []mountutils.MountInfo, volumeID, alive string, isTarget func(string) bool,
) []mountutils.MountInfo {
	var order []string
	top := map[string]mountutils.MountInfo{}
	for _, info := range infos {
		if !isTarget(info.MountPoint) {
			continue
		}
		if _, seen := top[info.MountPoint]; !seen {
			order = append(order, info.MountPoint)
		}
		top[info.MountPoint] = info
	}
	var targets []mountutils.MountInfo
	for _, mp := range order {
		info := top[mp]
		if device(info) != alive && info.Root == "/" && info.Source == mountSource(volumeID) {
			targets = append(targets, info)
		}
	}

	return targets
}

// Prepare readies the host for a new mount pod (mountpod.Manager.Prepare): the staging path exists
// with nothing mounted on it, a dead mount left by a former pod unmounted (a plain umount is enough
// for a FUSE mount whose process is gone), and the spool is emptied and recreated 0700.
func (s *NodeServer) Prepare(_ context.Context, spec *mountpod.Spec) error {
	staging := spec.StagingPath
	if err := s.unmountAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", staging, err)
	}
	spool := s.Pods.SpoolHostPath(spec.VolumeID)
	if err := os.RemoveAll(spool); err != nil {
		return fmt.Errorf("emptying %s: %w", spool, err)
	}
	if err := os.MkdirAll(filepath.Dir(spool), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(spool), err)
	}
	if err := os.Mkdir(spool, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", spool, err)
	}

	return os.Chmod(spool, 0o700) //nolint:gosec // G302: a directory, owner-only whatever the umask
}

// unmountAll unmounts every layer mounted on mnt, detaching the ones a plain unmount refuses, and
// fails when mnt is still mounted after maxMountLayers of them.
func (s *NodeServer) unmountAll(mnt string) error {
	for range maxMountLayers {
		mounted, _, err := s.Mounter.State(mnt)
		if err != nil {
			return err
		}
		if !mounted {
			return nil
		}
		klog.Infof("unmounting the former mount on %s", mnt)
		if err := s.Mounter.Unmount(mnt); err != nil {
			klog.Warningf("unmounting %s: %v; detaching it", mnt, err)
			if err := s.Mounter.Detach(mnt); err != nil {
				return err
			}
		}
	}
	if mounted, _, err := s.Mounter.State(mnt); err != nil || mounted {
		return fmt.Errorf("%s is still mounted after %d unmounts (%v)", mnt, maxMountLayers, err)
	}

	return nil
}

// maxMountLayers bounds the unmount loop of Prepare: a staging path may hold a few dead layers.
const maxMountLayers = 8
