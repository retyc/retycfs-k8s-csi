package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	mountutils "k8s.io/mount-utils"

	"github.com/retyc/retycfs-k8s-csi/internal/mountpod"
)

func TestRecoverRecreatesDeadMountPod(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000", AnnotationCacheTTL: "1m"})
	dev := e.stageReady()
	e.publish(e.target)
	old := e.pod()
	// A file the dead pod had not sent: the spool is emptied for the new one.
	spool := e.s.Pods.SpoolHostPath(testVolume)
	if err := os.WriteFile(spool+"/pending", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	// OOM kill: the mount process is gone, the staging path and every bind answer ENOTCONN.
	e.exit(137, "")
	e.mnt.kill(dev)
	e.s.Recover(context.Background()) // counts the exit, waits for the backoff
	if e.pod().UID != old.UID {
		t.Fatal("pod recreated before its backoff")
	}
	e.advance(time.Minute)
	e.s.Recover(context.Background())
	pod := e.pod()
	if pod.UID == old.UID {
		t.Fatal("dead mount pod not recreated")
	}
	if pod.Annotations[mountpod.AnnotationConfigHash] != old.Annotations[mountpod.AnnotationConfigHash] ||
		pod.Annotations[mountpod.AnnotationStagingPath] != e.staging {
		t.Fatal("recreated pod runs another config")
	}
	if _, err := os.Stat(spool + "/pending"); !os.IsNotExist(err) {
		t.Fatalf("spool not emptied: %v", err)
	}
	if mounted, _, _ := e.mnt.State(e.staging); mounted {
		t.Fatal("the dead mount was not unmounted from the staging path before the new pod")
	}

	// Once the new pod mounted the volume on the staging path: the target is bound again.
	newDev := e.running()
	e.s.Recover(context.Background())
	if top, _ := e.mnt.top(e.target); device(top) != newDev {
		t.Fatalf("target on %s, want %s", device(top), newDev)
	}
	if v := testutil.ToFloat64(e.s.Metrics.pods.WithLabelValues(stateReady)); v != 1 {
		t.Fatalf("retycfs_csi_mount_pods{state=ready} = %v", v)
	}
	if v := testutil.ToFloat64(e.s.Metrics.outdated.WithLabelValues("cli_version")); v != 0 {
		t.Fatalf("retycfs_csi_mount_pod_outdated = %v", v)
	}
	// Nothing else to do.
	before, _ := e.mnt.list()
	e.s.Recover(context.Background())
	if after, _ := e.mnt.list(); len(after) != len(before) {
		t.Fatalf("a healthy volume was rebound: %d mounts, then %d", len(before), len(after))
	}
}

// A mount pod deleted by hand while its volume is staged and published: recreated once it exited.
// A 137 nobody asked for, while the pod served, is reported as a likely OOM kill even when containerd
// says Error rather than OOMKilled (a race it has), once.
func TestRecoverReportsUnaskedKill(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	dev := e.stageReady()
	e.exitWith(137, "Error", "")
	e.mnt.kill(dev)
	e.s.Recover(context.Background())
	e.advance(time.Minute)
	e.s.Recover(context.Background())
	evs := strings.Join(e.eventList(), "\n")
	if strings.Count(evs, reasonMountOOM) != 1 || !strings.Contains(evs, "most likely at its memory limit (512Mi)") {
		t.Fatalf("events = %q, want one MountOOMKilled", evs)
	}
}

func TestRecoverRecreatesDeletedMountPod(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	dev := e.stageReady()
	e.publish(e.target)
	old := e.pod()
	if err := e.kube.CoreV1().Pods(testNS).Delete(context.Background(), old.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	// Still draining: left alone.
	e.s.Recover(context.Background())
	if p := e.pod(); p.UID != old.UID {
		t.Fatal("a draining pod was replaced")
	}
	// Killed at the end of its grace period (the bind kept it from unmounting): a dead mount.
	e.exit(137, "")
	e.mnt.kill(dev)
	e.s.Recover(context.Background())
	if p := e.pod(); p.UID != old.UID {
		t.Fatal("the deleted pod was released before its restart time: nothing left to rebuild it from")
	}
	e.advance(time.Minute)
	e.s.Recover(context.Background())
	if p := e.pod(); p.UID == old.UID || p.DeletionTimestamp != nil {
		t.Fatal("the deleted mount pod of a staged volume was not recreated")
	}
	newDev := e.running()
	e.s.Recover(context.Background())
	if top, _ := e.mnt.top(e.target); device(top) != newDev {
		t.Fatalf("target on %s, want %s", device(top), newDev)
	}
}

func TestRecoverDeletesOrphans(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.stageReady()
	e.publish(e.target)
	// kubelet forgot the volume (vol_data.json gone), but a pod still binds it: never deleted.
	e.setVolData(e.staging, false)
	if err := e.mnt.Unpublish(e.staging); err != nil {
		t.Fatal(err)
	}
	e.s.Recover(context.Background())
	if e.pod().DeletionTimestamp != nil {
		t.Fatal("a mount pod with binds was deleted")
	}

	// A copy of the mount outside the pod target paths (a private mount of the node plugin) is no bind.
	e.mnt.fuse("/csi/mnt/"+testVolume, testVolume)
	if err := e.mnt.Unpublish(e.target); err != nil {
		t.Fatal(err)
	}
	e.s.Recover(context.Background())
	if e.pod().DeletionTimestamp == nil {
		t.Fatal("orphan mount pod not deleted")
	}
	e.exit(75, "drain over after 5m0s: 3 files not sent")
	e.s.Recover(context.Background())
	if !e.podGone() {
		t.Fatal("orphan mount pod not released")
	}
	if !e.hasEvent(reasonWritesLost, "3 files not sent") {
		t.Fatal("no WritesLost event")
	}
}

// kubelet's directory of the driver out of the node plugin's sight (hostPath missing, kubeletDir
// wrong): no volume looks staged, and none is swept.
func TestRecoverDriverDirUnreadable(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.stageReady()
	e.setVolData(e.staging, false)
	if err := e.mnt.Unpublish(e.staging); err != nil {
		t.Fatal(err)
	}
	driverDir := path.Dir(path.Dir(e.staging))
	if err := os.Rename(driverDir, driverDir+".away"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		e.s.Recover(context.Background())
		if e.pod().DeletionTimestamp != nil {
			t.Fatal("a mount pod was deleted although kubelet's directory cannot be read")
		}
	}
	if !e.s.driverDirUnreadable.Load() {
		t.Fatal("the unreadable driver directory was not reported")
	}

	if err := os.Rename(driverDir+".away", driverDir); err != nil {
		t.Fatal(err)
	}
	e.s.Recover(context.Background())
	if e.pod().DeletionTimestamp == nil || e.s.driverDirUnreadable.Load() {
		t.Fatal("the orphan was kept once the driver directory is readable again")
	}
}

// kubelet's vol_data.json tells a staged volume: its pod is kept whatever its age, even unmounted
// and bound nowhere (a stage under way whose busy mark a node plugin restart lost); without it, the
// pod is an orphan whatever its phase, and goes at once.
func TestRecoverVolDataDecides(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	if c := e.stage(e.stageRequest(nil)); c != codes.Aborted {
		t.Fatalf("stage = %v", c)
	}
	e.s.busy.Delete(testVolume) // the node plugin restarted
	e.started()
	e.advance(orphanGrace + time.Hour)
	e.s.Recover(context.Background())
	if e.pod().DeletionTimestamp != nil {
		t.Fatal("the running pod of a volume kubelet is staging was deleted")
	}
	e.setVolData(e.staging, false)
	e.s.Recover(context.Background())
	if e.pod().DeletionTimestamp == nil {
		t.Fatal("a running pod of a volume kubelet does not hold staged was kept")
	}

	// A young pending pod is no exception.
	e2 := newNodeEnv(t)
	e2.claim(map[string]string{AnnotationUID: "1000"})
	e2.stage(e2.stageRequest(nil))
	e2.s.busy.Delete(testVolume)
	e2.setVolData(e2.staging, false)
	e2.s.Recover(context.Background())
	if e2.pod().DeletionTimestamp == nil {
		t.Fatal("a pending pod of a volume kubelet does not hold staged was kept")
	}
}

// An evicted mount pod drained and unmounted the volume: nothing is mounted nor bound any more.
// With vol_data.json, kubelet still holds the volume staged and the pod is recreated; without it,
// the pod is an orphan.
func TestRecoverEvictedMountPod(t *testing.T) {
	for _, staged := range []bool{true, false} {
		t.Run(fmt.Sprintf("staged=%t", staged), func(t *testing.T) {
			e := newNodeEnv(t)
			e.claim(map[string]string{AnnotationUID: "1000"})
			e.stageReady()
			old := e.pod()
			if err := e.mnt.Unpublish(e.staging); err != nil { // the drained process unmounted it
				t.Fatal(err)
			}
			e.setContainer(corev1.ContainerState{}, corev1.PodFailed)
			pod := e.pod()
			pod.Status.Reason, pod.Status.Message = "Evicted", "The node was low on resource: memory."
			if _, err := e.kube.CoreV1().Pods(testNS).UpdateStatus(context.Background(), pod,
				metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			e.setVolData(e.staging, staged)

			e.s.Recover(context.Background())
			if !staged {
				if !e.podGone() {
					t.Fatal("the evicted pod of an unstaged volume was kept")
				}

				return
			}
			e.advance(time.Minute)
			e.s.Recover(context.Background())
			if p := e.pod(); p.UID == old.UID || p.Annotations[mountpod.AnnotationStagingPath] != e.staging {
				t.Fatal("the evicted pod of a staged volume was not recreated on its staging path")
			}
		})
	}
}

func TestRecoverSkipsStageInProgress(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.stage(e.stageRequest(nil)) // Aborted: the pod starts, nothing mounted yet
	e.started()
	e.advance(orphanGrace + time.Minute)
	e.s.Recover(context.Background())
	if e.pod().DeletionTimestamp != nil {
		t.Fatal("the mount pod of a stage under way was taken for an orphan")
	}
}

// The Kubernetes API down: the recovery deletes and creates nothing.
func TestRecoverWithoutAPI(t *testing.T) {
	for _, verb := range []string{"list", "get"} {
		t.Run(verb, func(t *testing.T) {
			e := newNodeEnv(t)
			e.claim(map[string]string{AnnotationUID: "1000"})
			dev := e.stageReady()
			e.publish(e.target)
			e.exit(137, "")
			e.mnt.kill(dev)
			e.advance(orphanGrace + time.Minute)
			e.kube.PrependReactor(verb, "*", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("connection refused")
			})
			e.kube.ClearActions()
			e.s.Recover(context.Background())
			for _, a := range e.kube.Actions() {
				if v := a.GetVerb(); v != "list" && v != "get" {
					t.Errorf("%s %s with the API down", v, a.GetResource().Resource)
				}
			}
		})
	}
}

func TestRecoverGaveUpReportedOnce(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	dev := e.stageReady()
	e.publish(e.target)
	e.mnt.kill(dev)
	for range 3 {
		e.exit(77, "machine refused")
		e.s.Recover(context.Background())
		e.advance(4 * time.Minute) // under mountpod.GiveUpRetryAfter in all
		e.s.Recover(context.Background())
	}
	if n := strings.Count(strings.Join(e.eventList(), "\n"), reasonMountGaveUp); n != 1 {
		t.Fatalf("%d MountGaveUp events after the give-up, want 1", n)
	}
	for range 3 {
		e.s.Recover(context.Background())
	}
	if evs := e.eventList(); len(evs) != 0 {
		t.Fatalf("events at later rounds = %v, want none", evs)
	}
}

// NodePublishVolume repairs a dead staging mount, the mount pod having exited.
func TestPublishRepairsStaging(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	dev := e.stageReady()
	e.exit(137, "")
	e.mnt.kill(dev)
	publish := func() error {
		_, err := e.s.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
			VolumeId: testVolume, StagingTargetPath: e.staging, TargetPath: e.target,
		})

		return err
	}
	if err := publish(); status.Code(err) != codes.Unavailable {
		t.Fatalf("publish within the backoff = %v, want Unavailable", err)
	}
	e.advance(time.Minute)
	if err := publish(); status.Code(err) != codes.Unavailable {
		t.Fatalf("publish while the new pod starts = %v, want Unavailable", err)
	}
	newDev := e.running()
	if err := publish(); err != nil {
		t.Fatalf("publish once remounted: %v", err)
	}
	if top, ok := e.mnt.top(e.target); !ok || device(top) != newDev {
		t.Fatalf("target = %+v, %v; want a bind of %s", top, ok, newDev)
	}
}

func TestDeadTargets(t *testing.T) {
	const staging = "/var/lib/kubelet/plugins/kubernetes.io/csi/csi.retycfs.com/abc/globalmount"
	src := mountSource("dr-1")
	podA := "/var/lib/kubelet/pods/a/volumes/kubernetes.io~csi/pvc-1/mount"
	podB := "/var/lib/kubelet/pods/b/volumes/kubernetes.io~csi/pvc-1/mount"
	podC := "/var/lib/kubelet/pods/c/volumes/kubernetes.io~csi/pvc-2/mount"
	infos := []mountutils.MountInfo{
		{MountPoint: "/", Major: 8, Minor: 1, Root: "/", Source: "/dev/sda1"},
		{MountPoint: staging, Major: 0, Minor: 60, Root: "/", Source: src},
		// A copy left in a private mount of the node plugin, on a former device.
		{MountPoint: "/csi/mnt/dr-1", Major: 0, Minor: 50, Root: "/", Source: src},
		{MountPoint: podA, Major: 0, Minor: 50, Root: "/", Source: src, MountOptions: []string{"rw"}},
		// Recovered once already: an older dead layer, then the current one on top.
		{MountPoint: podB, Major: 0, Minor: 40, Root: "/", Source: src},
		{MountPoint: podB, Major: 0, Minor: 60, Root: "/", Source: src, MountOptions: []string{"ro"}},
		// Another volume, on a device number pvc-1 used before.
		{MountPoint: podC, Major: 0, Minor: 50, Root: "/", Source: mountSource("dr-2")},
		// A subdirectory of the same filesystem is not a NodePublishVolume bind mount.
		{MountPoint: "/var/lib/kubelet/pods/d/sub", Major: 0, Minor: 50, Root: "/sub", Source: src},
	}
	isTarget := func(mp string) bool { return strings.HasPrefix(mp, "/var/lib/kubelet/pods/") }

	if top, ok := topMount(infos, podB); !ok || device(top) != "0:60" {
		t.Fatalf("topMount(podB) = %v, %v", top, ok)
	}
	if _, ok := topMount(infos, "/not/mounted"); ok {
		t.Fatal("topMount() reports an unmounted path as mounted")
	}
	targets := deadTargets(infos, "dr-1", "0:60", isTarget)
	if len(targets) != 1 || targets[0].MountPoint != podA {
		t.Fatalf("deadTargets = %v; want %s only", targets, podA)
	}
	// podB's top layer is the dead one now; podC is another volume.
	if targets := deadTargets(infos, "dr-1", "0:50", isTarget); len(targets) != 1 || targets[0].MountPoint != podB {
		t.Fatalf("deadTargets(alive 0:50) = %v; want %s only", targets, podB)
	}
}

func TestPrepare(t *testing.T) {
	e := newNodeEnv(t)
	dev := e.mnt.fuse(e.staging, testVolume)
	e.mnt.kill(dev)
	e.mnt.fuse(e.staging, testVolume) // two dead layers
	spec := &mountpod.Spec{VolumeID: testVolume, NodeName: testNode, StagingPath: e.staging}
	if err := e.s.Prepare(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if mounted, _, _ := e.mnt.State(e.staging); mounted {
		t.Fatal("dead mount left in place")
	}
	info, err := os.Stat(e.s.Pods.SpoolHostPath(testVolume))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("spool = %v, %v; want a 0700 directory", info, err)
	}
	if _, err := os.Stat(e.staging); err != nil {
		t.Fatalf("staging path: %v", err)
	}

	// A mount that does not go: an error, not a pod started on top of it.
	for range maxMountLayers + 1 {
		e.mnt.fuse(e.staging, testVolume)
	}
	if err := e.s.Prepare(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "still mounted") {
		t.Fatalf("Prepare with %d layers = %v, want an error", maxMountLayers+1, err)
	}
}

// Each replacement of the mount pod stacks a bind on the dead layers of a target path, so that
// containers mounting it with HostToContainer see it come back; past maxTargetLayers, the dead
// layers on top are unmounted first, down to the bottom one.
func TestRecoverRestacksTargets(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	dev := e.stageReady()
	e.publish(e.target)
	count := func() (n int, top string) {
		infos, _ := e.mnt.list()
		t, _ := topMount(infos, e.target)

		return layers(infos, e.target), device(t)
	}
	for i := 1; ; i++ {
		uid := e.pod().UID
		e.exit(137, "")
		e.mnt.kill(dev)
		e.s.Recover(context.Background()) // counts the exit
		e.advance(10 * time.Minute)
		e.s.Recover(context.Background()) // recreates the pod
		if e.pod().UID == uid {
			t.Fatalf("replacement %d: the pod was not recreated", i)
		}
		dev = e.running()
		before, _ := count()
		e.s.Recover(context.Background()) // rebinds
		n, top := count()
		if top != dev {
			t.Fatalf("replacement %d: target on %s, want %s", i, top, dev)
		}
		if before < maxTargetLayers {
			if n != before+1 {
				t.Fatalf("replacement %d: %d layers, then %d; want one more", i, before, n)
			}

			continue
		}
		if n != 2 {
			t.Fatalf("replacement %d with %d layers: %d left, want the bottom one and the new one", i, before, n)
		}

		break
	}
	// Unpublish takes every layer.
	if _, err := e.s.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: testVolume, TargetPath: e.target,
	}); err != nil {
		t.Fatal(err)
	}
	if n, _ := count(); n != 0 {
		t.Fatalf("%d layers left after Unpublish", n)
	}
}

// A mount pod of a published volume that exits 75 is replaced by the recovery, which reports the
// lost writes on the claim once, through the rounds that wait for its restart.
func TestRecoverReportsLostWrites(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	dev := e.stageReady()
	e.publish(e.target)
	e.exit(75, "Error: 1 file being written was not sent before the filesystem was unmounted: its changes are lost")
	e.mnt.kill(dev)
	e.s.Recover(context.Background())
	e.s.Recover(context.Background())
	e.advance(time.Minute)
	e.s.Recover(context.Background())
	if n := strings.Count(strings.Join(e.eventList(), "\n"), reasonWritesLost); n != 1 {
		t.Fatalf("%d WritesLost events, want 1", n)
	}
}

// The same, the mount pod deleted by hand (kubectl delete): its finalizer keeps it Terminating with
// the exit code of its container, and the recovery reports the lost writes before replacing it.
func TestRecoverReportsLostWritesOfDeletedPod(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	dev := e.stageReady()
	e.publish(e.target)
	pod := e.pod()
	if err := e.kube.CoreV1().Pods(testNS).Delete(context.Background(), pod.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	e.exit(75, "Error: 1 file being written was not sent before the filesystem was unmounted: its changes are lost")
	if p := e.pod(); p.DeletionTimestamp == nil {
		t.Fatal("the pod is not Terminating")
	}
	e.mnt.kill(dev)
	for range 3 {
		e.s.Recover(context.Background())
		e.advance(time.Minute)
	}
	if n := strings.Count(strings.Join(e.eventList(), "\n"), reasonWritesLost); n != 1 {
		t.Fatalf("%d WritesLost events, want 1", n)
	}
}
