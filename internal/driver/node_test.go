package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	mountutils "k8s.io/mount-utils"

	"github.com/retyc/retycfs-k8s-csi/internal/identity"
	"github.com/retyc/retycfs-k8s-csi/internal/mountpod"
)

// fakeMounter is an in-memory mount table: FUSE mounts added by the test (as a mount pod would),
// bind mounts by the driver, and dead devices.
type fakeMounter struct {
	mu     sync.Mutex
	mounts []mountutils.MountInfo
	dead   map[string]bool
	minor  int
}

func newFakeMounter() *fakeMounter { return &fakeMounter{dead: map[string]bool{}, minor: 100} }

// fuse mounts a new FUSE filesystem of volumeID on path and returns its device.
func (f *fakeMounter) fuse(path, volumeID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.minor++
	info := mountutils.MountInfo{MountPoint: path, Major: 0, Minor: f.minor, Root: "/", Source: mountSource(volumeID)}
	f.mounts = append(f.mounts, info)

	return device(info)
}

func (f *fakeMounter) kill(dev string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dead[dev] = true
}

func (f *fakeMounter) top(path string) (mountutils.MountInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return topMount(f.mounts, path)
}

func (f *fakeMounter) list() ([]mountutils.MountInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.mounts), nil
}

func (f *fakeMounter) bind(source, target string, readonly bool) error {
	src, ok := topMount(f.mounts, source)
	if !ok {
		return fmt.Errorf("bind %s: source not mounted", source)
	}
	src.MountPoint, src.MountOptions = target, []string{"rw"}
	if readonly {
		src.MountOptions = []string{"ro"}
	}
	f.mounts = append(f.mounts, src)

	return nil
}

func (f *fakeMounter) BindMount(source, target string, readonly bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if top, ok := topMount(f.mounts, target); ok {
		if !f.dead[device(top)] {
			return nil
		}
		f.removeTop(target)
	}

	return f.bind(source, target, readonly)
}

func (f *fakeMounter) Rebind(source, target string, readonly bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.bind(source, target, readonly)
}

func (f *fakeMounter) removeTop(target string) {
	for i := len(f.mounts) - 1; i >= 0; i-- {
		if f.mounts[i].MountPoint == target {
			f.mounts = slices.Delete(f.mounts, i, i+1)

			return
		}
	}
}

func (f *fakeMounter) Unpublish(target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounts = slices.DeleteFunc(f.mounts, func(m mountutils.MountInfo) bool { return m.MountPoint == target })

	return nil
}

func (f *fakeMounter) Unmount(target string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeTop(target)

	return nil
}

func (f *fakeMounter) Detach(target string) error { return f.Unmount(target) }

func (f *fakeMounter) State(target string) (mounted, corrupted bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	top, ok := topMount(f.mounts, target)

	return ok, ok && f.dead[device(top)], nil
}

const (
	testNS     = "retycfs-csi"
	testNode   = "node-a"
	testVolume = "dr-1"
	// Under the kubelet directory of each test: kubelet's layout since 1.25 (driver, then the
	// SHA-256 of the volume handle).
	testStagingRel = "plugins/kubernetes.io/csi/csi.retycfs.com/abc/globalmount"
	testTargetRel  = "pods/p1/volumes/kubernetes.io~csi/pv-1/mount"
)

var podsGVR = corev1.SchemeGroupVersion.WithResource("pods")

type nodeEnv struct {
	t      *testing.T
	kube   *fake.Clientset
	mnt    *fakeMounter
	s      *NodeServer
	events *record.FakeRecorder
	now    time.Time
	// kubelet is the kubelet directory, staging and target the volume's staging and target paths.
	kubelet, staging, target string

	mu        sync.Mutex
	readyCode int
}

func (e *nodeEnv) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.now
}

func (e *nodeEnv) advance(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = e.now.Add(d)
}

func (e *nodeEnv) setReady(code int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.readyCode = code
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newNodeEnv(t *testing.T) *nodeEnv {
	t.Helper()

	return newNodeEnvWith(t, func(*mountpod.Config) {})
}

// newNodeEnvWith is newNodeEnv with the mount pods' configuration changed by configure.
func newNodeEnvWith(t *testing.T, configure func(*mountpod.Config)) *nodeEnv {
	t.Helper()
	e := &nodeEnv{
		t: t, kube: fake.NewClientset(), mnt: newFakeMounter(), events: record.NewFakeRecorder(100),
		now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC), readyCode: http.StatusOK,
	}
	uids := 0
	e.kube.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		uids++
		pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.UID = types.UID(fmt.Sprintf("uid-%d", uids))
		pod.CreationTimestamp = metav1.Time{Time: e.clock()}

		return false, nil, nil
	})
	// Like the API server, a pod with finalizers is only marked as deleted.
	e.kube.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		obj, err := e.kube.Tracker().Get(podsGVR, a.GetNamespace(), a.(k8stesting.DeleteAction).GetName())
		if err != nil || len(obj.(*corev1.Pod).Finalizers) == 0 {
			return false, nil, nil
		}
		pod := obj.(*corev1.Pod)
		if pod.DeletionTimestamp == nil {
			pod.DeletionTimestamp = &metav1.Time{Time: e.clock().Add(5 * time.Minute)}
		}

		return true, nil, e.kube.Tracker().Update(podsGVR, pod, a.GetNamespace())
	})
	dir := t.TempDir()
	e.kubelet = dir + "/kubelet"
	e.staging, e.target = e.kubelet+"/"+testStagingRel, e.kubelet+"/"+testTargetRel
	cfg := mountpod.Config{
		Namespace: testNS, Image: "ghcr.io/retyc/retyc-cli:1.2.3", KubeletDir: e.kubelet, SpoolRoot: dir + "/spool",
	}
	configure(&cfg)
	pods := mountpod.New(e.kube, cfg)
	pods.Now = e.clock
	pods.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		e.mu.Lock()
		defer e.mu.Unlock()

		return &http.Response{StatusCode: e.readyCode, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	e.s = &NodeServer{
		NodeID: testNode, Mounter: e.mnt, Pods: pods, Kube: e.kube, Events: e.events,
		IdentityDir: identityDir(t, machineSecrets("default-key")),
		MountInfo:   e.mnt.list, Metrics: NewNodeMetrics(),
	}
	pods.Prepare = e.s.Prepare

	return e
}

// volume creates the volume's PV, without a class: kubelet only stages a volume that has a PV,
// and the driver checks it before using its own identity.
func (e *nodeEnv) volume() {
	e.t.Helper()
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-data"},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: DriverName, VolumeHandle: testVolume},
		}},
	}
	_, err := e.kube.CoreV1().PersistentVolumes().Create(context.Background(), pv, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		e.t.Fatal(err)
	}
}

func (e *nodeEnv) claim(annotations map[string]string) {
	e.t.Helper()
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data", Namespace: "team", UID: "pvc-uid", Annotations: annotations,
	}}
	ctx := context.Background()
	e.volume()
	if _, err := e.kube.CoreV1().PersistentVolumeClaims("team").Update(ctx, pvc, metav1.UpdateOptions{}); err == nil {
		return
	}
	if _, err := e.kube.CoreV1().PersistentVolumeClaims("team").Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *nodeEnv) stageRequest(secrets map[string]string) *csi.NodeStageVolumeRequest {
	return &csi.NodeStageVolumeRequest{
		VolumeId: testVolume, StagingTargetPath: e.staging,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		},
		VolumeContext: map[string]string{volumeContextPVCName: "data", volumeContextNamespace: "team"},
		Secrets:       secrets,
	}
}

// volData is kubelet's vol_data.json for a staging path.
func volData(staging string) string { return path.Join(path.Dir(staging), volDataFile) }

// setVolData writes or removes kubelet's vol_data.json of a staging path.
func (e *nodeEnv) setVolData(staging string, present bool) {
	e.t.Helper()
	if !present {
		if err := os.Remove(volData(staging)); err != nil && !os.IsNotExist(err) {
			e.t.Fatal(err)
		}

		return
	}
	if err := os.MkdirAll(path.Dir(staging), 0o750); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(volData(staging), []byte(`{"driverName":"csi.retycfs.com"}`), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// finalForKubelet says whether kubelet takes a CSI code as final (csi_client.go): it then marks
// the volume as not staged and removes the staging path and vol_data.json, unless it is mounted.
func finalForKubelet(c codes.Code) bool {
	switch c {
	case codes.OK, codes.Canceled, codes.DeadlineExceeded, codes.Unavailable, codes.ResourceExhausted, codes.Aborted:
		return false
	}

	return true
}

// stage calls NodeStageVolume as kubelet does: vol_data.json written first, removed after a final
// error when the staging path is not mounted.
func (e *nodeEnv) stage(req *csi.NodeStageVolumeRequest) codes.Code {
	e.t.Helper()
	e.setVolData(req.GetStagingTargetPath(), true)
	_, err := e.s.NodeStageVolume(context.Background(), req)
	if c := status.Code(err); finalForKubelet(c) {
		if _, mounted := e.mnt.top(req.GetStagingTargetPath()); !mounted {
			e.setVolData(req.GetStagingTargetPath(), false)
		}
	}

	return status.Code(err)
}

// unstage calls NodeUnstageVolume as kubelet does: vol_data.json removed once it succeeded.
func (e *nodeEnv) unstage() (codes.Code, error) {
	e.t.Helper()
	_, err := e.s.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId: testVolume, StagingTargetPath: e.staging,
	})
	if err == nil {
		e.setVolData(e.staging, false)
	}

	return status.Code(err), err
}

func (e *nodeEnv) pod() *corev1.Pod {
	e.t.Helper()
	pod, err := e.kube.CoreV1().Pods(testNS).Get(context.Background(), mountpod.Name(testNode, testVolume),
		metav1.GetOptions{})
	if err != nil {
		e.t.Fatalf("get mount pod: %v", err)
	}

	return pod
}

func (e *nodeEnv) podGone() bool {
	_, err := e.kube.CoreV1().Pods(testNS).Get(context.Background(), mountpod.Name(testNode, testVolume),
		metav1.GetOptions{})

	return err != nil
}

func (e *nodeEnv) setContainer(state corev1.ContainerState, phase corev1.PodPhase) {
	e.t.Helper()
	pod := e.pod()
	pod.Status = corev1.PodStatus{
		Phase: phase, PodIP: "10.0.0.7", Conditions: pod.Status.Conditions,
		ContainerStatuses: []corev1.ContainerStatus{{Name: "mount", State: state}},
	}
	// The Ready condition as kubelet keeps it: back to false, at that time, when the container stops.
	for i, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue && state.Running == nil {
			pod.Status.Conditions[i] = corev1.PodCondition{
				Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.Time{Time: e.clock()},
			}
		}
	}
	if _, err := e.kube.CoreV1().Pods(testNS).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		e.t.Fatal(err)
	}
}

// started makes the mount pod's container run, the volume not mounted yet.
func (e *nodeEnv) started() {
	e.t.Helper()
	e.setContainer(corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, corev1.PodRunning)
}

// running makes the mount pod run and mount the volume on the staging path, and returns the
// device of the mount.
func (e *nodeEnv) running() string {
	e.t.Helper()
	e.started()
	pod := e.pod()
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Time{Time: e.clock()},
	}}
	if _, err := e.kube.CoreV1().Pods(testNS).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		e.t.Fatal(err)
	}

	return e.mnt.fuse(e.staging, testVolume)
}

func (e *nodeEnv) exit(code int32, message string) {
	e.t.Helper()
	e.exitWith(code, "Error", message)
}

// exitWith ends the mount pod's container with a termination reason ("OOMKilled").
func (e *nodeEnv) exitWith(code int32, reason, message string) {
	e.t.Helper()
	phase := corev1.PodSucceeded
	if code != 0 {
		phase = corev1.PodFailed
	}
	e.setContainer(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: code, Message: message, Reason: reason,
		StartedAt: metav1.Time{Time: e.clock().Add(-time.Second)}, FinishedAt: metav1.Time{Time: e.clock()},
	}}, phase)
}

// stageReady stages the volume until it is mounted, and returns the device of its mount.
func (e *nodeEnv) stageReady() string {
	e.t.Helper()
	if c := e.stage(e.stageRequest(nil)); c != codes.Aborted {
		e.t.Fatalf("first stage = %v, want Aborted", c)
	}
	dev := e.running()
	if c := e.stage(e.stageRequest(nil)); c != codes.OK {
		e.t.Fatalf("stage once running = %v, want OK", c)
	}

	return dev
}

func (e *nodeEnv) publish(target string) {
	e.t.Helper()
	_, err := e.s.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId: testVolume, StagingTargetPath: e.staging, TargetPath: target,
	})
	if err != nil {
		e.t.Fatalf("publish: %v", err)
	}
}

func (e *nodeEnv) eventList() []string {
	var out []string
	for {
		select {
		case ev := <-e.events.Events:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func (e *nodeEnv) hasEvent(reason, substr string) bool {
	for _, ev := range e.eventList() {
		if strings.Contains(ev, reason) && strings.Contains(ev, substr) {
			return true
		}
	}

	return false
}

func TestNodeStageRequiresUID(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationGID: "1000"})
	if c := e.stage(e.stageRequest(nil)); c != codes.InvalidArgument {
		t.Fatalf("stage without uid = %v, want InvalidArgument", c)
	}
	e.claim(map[string]string{AnnotationUID: "1000", AnnotationCacheTTL: "soon"})
	if c := e.stage(e.stageRequest(nil)); c != codes.InvalidArgument {
		t.Fatalf("stage with a bad cache-ttl = %v, want InvalidArgument", c)
	}
	if !e.podGone() {
		t.Fatal("a mount pod was created for an invalid claim")
	}
}

func TestNodeStageConfigAndReadiness(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{
		AnnotationUID: "1000", AnnotationCacheTTL: "1m", AnnotationReadOnly: "true",
		AnnotationBackgroundDelete: "true", AnnotationLocalFile: "*.tmp  .~lock*",
		AnnotationMountMemoryLimit: "1Gi", AnnotationMountCPURequest: "200m",
	})
	tenant := machineSecrets("tenant-key")

	if c := e.stage(e.stageRequest(tenant)); c != codes.Aborted {
		t.Fatalf("stage of a new volume = %v, want Aborted", c)
	}
	pod := e.pod()
	args := strings.Join(pod.Spec.Containers[0].Args, " ")
	for _, want := range []string{
		"--uid=1000", "--gid=1000", "--cache-ttl=1m0s", "--read-only", "--background-delete",
		"--local-file=*.tmp --local-file=.~lock*",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("args %q lack %q", args, want)
		}
	}
	if strings.Contains(args, "--unsafe-write") {
		t.Errorf("args %q have --unsafe-write", args)
	}
	r := pod.Spec.Containers[0].Resources
	if r.Limits.Memory().String() != "1Gi" || r.Requests.Cpu().String() != "200m" {
		t.Errorf("resources = %v", r)
	}
	if pod.Annotations[mountpod.AnnotationStagingPath] != e.staging || pod.Annotations[mountpod.LabelPVC] != "data" {
		t.Errorf("annotations = %v", pod.Annotations)
	}
	secret, err := e.kube.CoreV1().Secrets(testNS).Get(context.Background(),
		pod.Annotations[mountpod.AnnotationSecret], metav1.GetOptions{})
	if err != nil || string(secret.Data[identity.ClientKeyFile]) != "tenant-key" {
		t.Fatalf("secret = %v, %v; want the tenant's identity", secret, err)
	}

	// Running, but /readyz not 200 yet: still Aborted, and nothing bound.
	e.setReady(http.StatusServiceUnavailable)
	dev := e.running()
	if c := e.stage(e.stageRequest(tenant)); c != codes.Aborted {
		t.Fatalf("stage while mounting = %v, want Aborted", c)
	}
	e.setReady(http.StatusOK)
	if c := e.stage(e.stageRequest(tenant)); c != codes.OK {
		t.Fatalf("stage once ready = %v, want OK", c)
	}
	if top, ok := e.mnt.top(e.staging); !ok || device(top) != dev {
		t.Fatalf("staging = %+v, %v; want a bind of %s", top, ok, dev)
	}
	// Idempotent.
	if c := e.stage(e.stageRequest(tenant)); c != codes.OK {
		t.Fatalf("second stage = %v", c)
	}
}

func TestNodeStageStaticPV(t *testing.T) {
	e := newNodeEnv(t)
	req := e.stageRequest(nil)
	// The claim's annotation wins over the PV's attributes.
	req.VolumeContext[AnnotationUID] = "5"
	req.VolumeContext[AnnotationUnsafeWrite] = "true"
	e.claim(map[string]string{AnnotationUID: "7"})
	if c := e.stage(req); c != codes.Aborted {
		t.Fatalf("stage = %v", c)
	}
	args := strings.Join(e.pod().Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--uid=7") || !strings.Contains(args, "--unsafe-write") {
		t.Fatalf("args = %q", args)
	}

	// Without a claim: the attributes alone, and the default identity.
	e2 := newNodeEnv(t)
	req = e2.stageRequest(nil)
	req.VolumeContext = map[string]string{AnnotationUID: "5", AnnotationGID: "6"}
	e2.volume()
	if c := e2.stage(req); c != codes.Aborted {
		t.Fatalf("stage of a static PV = %v", c)
	}
	pod := e2.pod()
	if args := strings.Join(pod.Spec.Containers[0].Args, " "); !strings.Contains(args, "--uid=5 --gid=6") {
		t.Fatalf("args = %q", args)
	}
	secret, _ := e2.kube.CoreV1().Secrets(testNS).Get(context.Background(),
		pod.Annotations[mountpod.AnnotationSecret], metav1.GetOptions{})
	if string(secret.Data[identity.ClientKeyFile]) != "default-key" {
		t.Fatal("the static PV is not mounted with the default identity")
	}
}

func TestNodeStageRecreatesOnConfigChange(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.stageReady()
	oldUID := e.pod().UID

	// Not published: the pod is recreated with the new settings.
	e.claim(map[string]string{AnnotationUID: "2000"})
	if c := e.stage(e.stageRequest(nil)); c != codes.Aborted {
		t.Fatalf("stage with new settings = %v, want Aborted (draining)", c)
	}
	if e.pod().DeletionTimestamp == nil {
		t.Fatal("the former pod is not being deleted")
	}
	if _, ok := e.mnt.top(e.staging); !ok {
		t.Fatal("staging path unmounted under the draining pod")
	}
	e.exit(0, "")
	if c := e.stage(e.stageRequest(nil)); c != codes.Aborted {
		t.Fatalf("stage once the former pod exited = %v, want Aborted (new pod starting)", c)
	}
	pod := e.pod()
	if pod.UID == oldUID || !strings.Contains(strings.Join(pod.Spec.Containers[0].Args, " "), "--uid=2000") {
		t.Fatalf("pod %s args %v: not recreated", pod.UID, pod.Spec.Containers[0].Args)
	}
	dev := e.running()
	if c := e.stage(e.stageRequest(nil)); c != codes.OK {
		t.Fatalf("stage = %v", c)
	}
	if top, _ := e.mnt.top(e.staging); device(top) != dev {
		t.Fatalf("staging bound to %s, want %s", device(top), dev)
	}

	// Published: the pod is kept and an event says why.
	e.publish(e.target)
	e.claim(map[string]string{AnnotationUID: "3000"})
	if c := e.stage(e.stageRequest(nil)); c != codes.OK {
		t.Fatalf("stage of a published volume with new settings = %v, want OK", c)
	}
	if p := e.pod(); p.UID != pod.UID || p.DeletionTimestamp != nil {
		t.Fatal("the pod of a published volume was replaced")
	}
	if !e.hasEvent(reasonConfigChanged, "still in use") {
		t.Fatal("no MountConfigChanged event")
	}
}

func TestNodeStageGivesUp(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	if c := e.stage(e.stageRequest(nil)); c != codes.Aborted {
		t.Fatalf("first stage = %v, want Aborted", c)
	}
	// A machine refused is not retried: the first exit 77 gives up.
	e.exit(77, "machine account refused: disabled")
	if c := e.stage(e.stageRequest(nil)); c != codes.FailedPrecondition {
		t.Fatalf("stage after a fatal exit = %v, want FailedPrecondition", c)
	}
	if !e.hasEvent(reasonMountGaveUp, "machine account refused") {
		t.Fatal("no MountGaveUp event with the exit message")
	}
}

func TestNodeUnstage(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.stageReady()

	if c, err := e.unstage(); c != codes.Aborted {
		t.Fatalf("unstage = %v (%v), want Aborted while draining", c, err)
	}
	// The staging mount is the FUSE mount: unmounting it would cut the drain short.
	if _, ok := e.mnt.top(e.staging); !ok {
		t.Fatal("staging path unmounted before the drain")
	}
	if e.pod().DeletionTimestamp == nil {
		t.Fatal("the mount pod is not being deleted")
	}
	if c, _ := e.unstage(); c != codes.Aborted {
		t.Fatalf("unstage while draining = %v, want Aborted", c)
	}
	if !e.s.isBusy(testVolume) {
		t.Fatal("a draining volume is not marked busy")
	}
	e.exit(75, "Error: 2 files being written were not sent before the filesystem was unmounted")
	if c, err := e.unstage(); c != codes.OK {
		t.Fatalf("unstage once exited = %v (%v)", c, err)
	}
	if !e.podGone() {
		t.Fatal("the mount pod is still there")
	}
	if _, ok := e.mnt.top(e.staging); ok {
		t.Fatal("staging path still mounted when unstage returns OK")
	}
	if e.s.isBusy(testVolume) {
		t.Fatal("busy mark left after the unstage")
	}
	if !e.hasEvent(reasonWritesLost, "2 files being written") {
		t.Fatal("no WritesLost event with the exit message")
	}
	if c, _ := e.unstage(); c != codes.OK {
		t.Fatalf("unstage again = %v", c)
	}
}

func TestNodeUnstageCleanExit(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.stageReady()
	_, _ = e.unstage()
	e.exit(0, "")
	if c, _ := e.unstage(); c != codes.OK {
		t.Fatalf("unstage = %v", c)
	}
	if evs := e.eventList(); len(evs) != 0 {
		t.Fatalf("events = %v, want none", evs)
	}
}

// A bind left on a target path (an unpublish that failed): the process cannot unmount, is killed
// at the end of its grace period, and the dead staging mount is unmounted by the node plugin.
func TestNodeUnstageWithBinds(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	dev := e.stageReady()
	e.publish(e.target)
	if c, _ := e.unstage(); c != codes.Aborted {
		t.Fatalf("unstage = %v, want Aborted", c)
	}
	e.exit(137, "")
	e.mnt.kill(dev)
	if c, err := e.unstage(); c != codes.OK {
		t.Fatalf("unstage once killed = %v (%v)", c, err)
	}
	if _, ok := e.mnt.top(e.staging); ok {
		t.Fatal("dead staging mount left")
	}
	if !e.podGone() {
		t.Fatal("the mount pod is still there")
	}
}

// One mount pod per volume and node: a second staging path of the same volume handle is refused
// while the first is mounted, and takes the pod over once it is not.
func TestNodeStageOtherStagingPath(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.stageReady()
	other := e.stageRequest(nil)
	other.StagingTargetPath = e.kubelet + "/plugins/kubernetes.io/csi/pv/pv-2/globalmount"
	if c := e.stage(other); c != codes.FailedPrecondition {
		t.Fatalf("stage on a second staging path = %v, want FailedPrecondition", c)
	}
	if e.pod().DeletionTimestamp != nil {
		t.Fatal("the pod of the staged volume is being deleted")
	}

	if err := e.mnt.Unpublish(e.staging); err != nil {
		t.Fatal(err)
	}
	if c := e.stage(other); c != codes.Aborted {
		t.Fatalf("stage on the second path, the first unmounted = %v, want Aborted (draining)", c)
	}
	e.exit(0, "")
	if c := e.stage(other); c != codes.Aborted {
		t.Fatalf("stage = %v, want Aborted (new pod starting)", c)
	}
	if p := e.pod(); p.Annotations[mountpod.AnnotationStagingPath] != other.StagingTargetPath {
		t.Fatalf("staging path of the new pod = %q", p.Annotations[mountpod.AnnotationStagingPath])
	}
}

// The busy mark stays only while kubelet retries (Aborted).
func TestNodeStageBusyMark(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationGID: "1000"})
	if c := e.stage(e.stageRequest(nil)); c != codes.InvalidArgument || e.s.isBusy(testVolume) {
		t.Fatalf("stage = %v, busy = %v; want InvalidArgument, not busy", c, e.s.isBusy(testVolume))
	}
	e.claim(map[string]string{AnnotationUID: "1000"})
	if c := e.stage(e.stageRequest(nil)); c != codes.Aborted || !e.s.isBusy(testVolume) {
		t.Fatalf("stage = %v, busy = %v; want Aborted, busy", c, e.s.isBusy(testVolume))
	}
}

func TestNodeGetVolumeStats(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.stageReady()
	target := t.TempDir() // statfs needs a real directory
	e.publish(target)
	stats := func() (*csi.NodeGetVolumeStatsResponse, error) {
		return e.s.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
			VolumeId: testVolume, VolumePath: target,
		})
	}

	resp, err := stats()
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetUsage()) != 2 || resp.GetUsage()[0].GetTotal() <= 0 || resp.GetVolumeCondition().GetAbnormal() {
		t.Fatalf("stats = %v", resp)
	}

	e.setReady(http.StatusServiceUnavailable)
	if resp, _ := stats(); !resp.GetVolumeCondition().GetAbnormal() {
		t.Fatalf("condition with /readyz failing = %v, want abnormal", resp.GetVolumeCondition())
	}
	e.exit(137, "")
	if resp, _ := stats(); !resp.GetVolumeCondition().GetAbnormal() ||
		!strings.Contains(resp.GetVolumeCondition().GetMessage(), "Exited") {
		t.Fatalf("condition with the pod exited = %v, want abnormal", resp.GetVolumeCondition())
	}
	_, err = e.s.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId: testVolume, VolumePath: t.TempDir(),
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("stats of an unmounted path = %v, want NotFound", err)
	}
}

func TestNodeCapabilities(t *testing.T) {
	resp, _ := (&NodeServer{}).NodeGetCapabilities(context.Background(), nil)
	var got []csi.NodeServiceCapability_RPC_Type
	for _, c := range resp.GetCapabilities() {
		got = append(got, c.GetRpc().GetType())
	}
	for _, want := range []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME, csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		csi.NodeServiceCapability_RPC_VOLUME_CONDITION, csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("capabilities %v lack %v", got, want)
		}
	}
}

// A transient error of the Kubernetes API or of the mount table is no final error: kubelet would
// remove the staging path and vol_data.json, under the mount pod already started.
func TestNodeStageTransientErrors(t *testing.T) {
	for _, tc := range []struct{ verb, resource string }{
		{"get", "pods"}, {"create", "pods"}, {"create", "secrets"}, {"patch", "secrets"},
	} {
		t.Run(tc.verb+" "+tc.resource, func(t *testing.T) {
			e := newNodeEnv(t)
			e.claim(map[string]string{AnnotationUID: "1000"})
			if tc.verb == "patch" { // the pod exists, its Secret is not owned yet
				e.stage(e.stageRequest(nil))
				pod := e.pod()
				secret, err := e.kube.CoreV1().Secrets(testNS).Get(context.Background(),
					pod.Annotations[mountpod.AnnotationSecret], metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				secret.OwnerReferences = nil
				if _, err := e.kube.CoreV1().Secrets(testNS).Update(context.Background(), secret,
					metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			e.kube.PrependReactor(tc.verb, tc.resource, func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("etcdserver: request timed out")
			})
			if c := e.stage(e.stageRequest(nil)); c != codes.Unavailable {
				t.Fatalf("stage = %v, want Unavailable", c)
			}
			if _, err := os.Stat(volData(e.staging)); err != nil {
				t.Fatalf("vol_data.json: %v; kubelet must keep it", err)
			}
		})
	}

	// The mount table unreadable once the pod is ready: Unavailable too.
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.stage(e.stageRequest(nil))
	e.running()
	e.s.MountInfo = func() ([]mountutils.MountInfo, error) { return nil, errors.New("EMFILE") }
	if c := e.stage(e.stageRequest(nil)); c != codes.Unavailable {
		t.Fatalf("stage with the mount table unreadable = %v, want Unavailable", c)
	}
	// Mounted for kubelet but not seen by the node plugin: Internal, a final error that leaves it.
	e.s.MountInfo = func() ([]mountutils.MountInfo, error) { return nil, nil }
	if c := e.stage(e.stageRequest(nil)); c != codes.Internal {
		t.Fatalf("stage of a mount the node plugin does not see = %v, want Internal", c)
	}
}

// A given-up volume (the e2e leak): the stage failed for good, so kubelet removed vol_data.json and
// never unstages it. The next sweep releases the exited pod, its finalizer and its Secret; kubelet's
// retries keep failing at once, with one MountGaveUp event, the last output and no new pod (no new
// login with the refused credentials); once the consumer, the claim and the PV are gone, the give-up
// goes after orphanGrace; new credentials start afresh.
func TestNodeStageGaveUpReleased(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.giveUp()
	creates := func() int {
		n := 0
		for _, a := range e.kube.Actions() {
			if a.GetVerb() == "create" && a.GetResource().Resource == "pods" {
				n++
			}
		}

		return n
	}
	secrets := func() int {
		list, err := e.kube.CoreV1().Secrets(testNS).List(context.Background(), metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}

		return len(list.Items)
	}
	if _, err := os.Stat(volData(e.staging)); !os.IsNotExist(err) {
		t.Fatalf("vol_data.json after a final error: %v", err)
	}
	before := creates()
	e.s.Recover(context.Background()) // the next sweep
	if !e.podGone() || secrets() != 0 {
		t.Fatalf("after the sweep: pod gone %t, %d secrets; want the pod and its secret released", e.podGone(),
			secrets())
	}
	for i := range 5 { // 10 minutes of kubelet retries (a 77 is tried again after 15), with the recovery in between
		_, err := e.s.NodeStageVolume(context.Background(), e.stageRequest(nil))
		if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "machine refused") {
			t.Fatalf("retry %d = %v, want FailedPrecondition with the last output", i, err)
		}
		e.advance(2 * time.Minute)
		e.s.Recover(context.Background())
	}
	if n := creates() - before; n != 0 {
		t.Fatalf("%d pods created by the retries, want none", n)
	}
	if n := strings.Count(strings.Join(e.eventList(), "\n"), reasonMountGaveUp); n != 1 {
		t.Fatalf("%d MountGaveUp events, want 1", n)
	}

	// The consumer, the claim and the PV deleted: kubelet stops retrying, the give-up goes.
	if err := e.kube.CoreV1().PersistentVolumeClaims("team").Delete(context.Background(), "data",
		metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	e.advance(orphanGrace)
	e.s.Recover(context.Background())
	if v := e.s.Pods.Volumes(); len(v) != 0 {
		t.Fatalf("restart state kept past orphanGrace: %v", v)
	}

	// New credentials while it is given up: a new pod at once.
	e2 := newNodeEnv(t)
	e2.claim(map[string]string{AnnotationUID: "1000"})
	e2.giveUp()
	e2.s.Recover(context.Background())
	tenant := machineSecrets("new-key")
	if c := e2.stage(e2.stageRequest(tenant)); c != codes.Aborted || e2.podGone() {
		t.Fatalf("stage with new credentials = %v, want Aborted with a new pod", c)
	}
}

// giveUp makes the volume's mount pod give up during kubelet's stages: one exit 77 is enough.
func (e *nodeEnv) giveUp() {
	e.t.Helper()
	e.stage(e.stageRequest(nil))
	e.exit(77, "machine refused")
	if c := e.stage(e.stageRequest(nil)); c != codes.FailedPrecondition {
		e.t.Fatalf("stage after a fatal exit = %v, want FailedPrecondition", c)
	}
}

// The ways out of a give-up besides new credentials: the exited pod deleted by hand, an explicit
// unstage, the volume gone from the node. Each allows a new try.
func TestNodeGaveUpWaysOut(t *testing.T) {
	t.Run("pod deleted by hand", func(t *testing.T) {
		e := newNodeEnv(t)
		e.claim(map[string]string{AnnotationUID: "1000"})
		e.giveUp()
		old := e.pod().UID
		// kubectl delete pod: the finalizer keeps it, terminating.
		if err := e.kube.CoreV1().Pods(testNS).Delete(context.Background(), e.pod().Name,
			metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		e.s.Recover(context.Background()) // between two retries: not staged, kept
		if c := e.stage(e.stageRequest(nil)); c != codes.Aborted || e.podGone() || e.pod().UID == old {
			t.Fatalf("stage after the deletion = %v, want Aborted with a new pod", c)
		}
		// A new try (the machine enabled again keeps its keys, so the same config), and a new
		// give-up is reported.
		e.exit(77, "machine refused again")
		if c := e.stage(e.stageRequest(nil)); c != codes.FailedPrecondition {
			t.Fatalf("stage after a new fatal exit = %v, want FailedPrecondition", c)
		}
		if !e.hasEvent(reasonMountGaveUp, "machine refused again") {
			t.Fatal("the new give-up was not reported")
		}
	})
	t.Run("unstage", func(t *testing.T) {
		e := newNodeEnv(t)
		e.claim(map[string]string{AnnotationUID: "1000"})
		e.giveUp()
		if c, err := e.unstage(); c != codes.OK {
			t.Fatalf("unstage = %v (%v)", c, err)
		}
		if c := e.stage(e.stageRequest(nil)); c != codes.Aborted || e.podGone() {
			t.Fatalf("stage after the unstage = %v, want Aborted with a new pod", c)
		}
	})
	t.Run("volume gone", func(t *testing.T) {
		e := newNodeEnv(t)
		e.claim(map[string]string{AnnotationUID: "1000"})
		e.giveUp()
		e.advance(orphanGrace)
		e.s.Recover(context.Background()) // the pod goes
		e.s.Recover(context.Background()) // and its restart state
		if !e.podGone() {
			t.Fatal("the given-up pod was kept past orphanGrace")
		}
		if c := e.stage(e.stageRequest(nil)); c != codes.Aborted || e.podGone() {
			t.Fatalf("stage once the volume was gone = %v, want Aborted with a new pod", c)
		}
	})
}

// An exit of a pod that served: 75 is lost writes whatever its output; 1 is lost writes only when its
// output says so (a CLI older than 75), MountFailed otherwise; a 77 that lost writes is reported too.
func TestReportExitAfterReady(t *testing.T) {
	const lost = "Error: 1 file being written was not sent before the filesystem was unmounted: its changes are lost"
	for _, tc := range []struct {
		name    string
		code    int32
		message string
		reason  string
	}{
		{"75", 75, "Error: something", reasonWritesLost},
		{"1 old CLI", 1, lost, reasonWritesLost},
		{"1", 1, "Error: redis: connection lost", reasonMountFailed},
		{"77 lost writes", 77, "Error: login lost\n" + lost, reasonWritesLost},
		{"77", 77, "Error: login lost", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newNodeEnv(t)
			e.claim(map[string]string{AnnotationUID: "1000"})
			e.stageReady()
			if c, _ := e.unstage(); c != codes.Aborted {
				t.Fatalf("unstage = %v, want Aborted while draining", c)
			}
			e.exit(tc.code, tc.message)
			if c, err := e.unstage(); c != codes.OK {
				t.Fatalf("unstage = %v (%v)", c, err)
			}
			evs := strings.Join(e.eventList(), "\n")
			for _, reason := range []string{reasonWritesLost, reasonMountFailed} {
				if strings.Contains(evs, reason) != (reason == tc.reason) {
					t.Fatalf("events = %q, want %q only", evs, tc.reason)
				}
			}
		})
	}
}

// A mount pod killed at its memory limit gets a MountOOMKilled on the claim, with the limit, and no
// other event; a 137 of another kill (kill -9, end of the grace period) gets none.
func TestReportExitOOMKilled(t *testing.T) {
	for _, tc := range []struct {
		reason string
		want   bool
	}{{"OOMKilled", true}, {"Error", false}} {
		t.Run(tc.reason, func(t *testing.T) {
			e := newNodeEnv(t)
			e.claim(map[string]string{AnnotationUID: "1000"})
			e.stageReady()
			if c, _ := e.unstage(); c != codes.Aborted {
				t.Fatalf("unstage = %v, want Aborted while draining", c)
			}
			e.exitWith(137, tc.reason, "")
			if c, err := e.unstage(); c != codes.OK {
				t.Fatalf("unstage = %v (%v)", c, err)
			}
			evs := strings.Join(e.eventList(), "\n")
			if strings.Contains(evs, reasonMountOOM) != tc.want || strings.Contains(evs, reasonWritesLost) ||
				strings.Contains(evs, reasonMountFailed) {
				t.Fatalf("events = %q, want MountOOMKilled: %v", evs, tc.want)
			}
			if tc.want && !strings.Contains(evs, "512Mi") {
				t.Fatalf("events = %q, want the limit 512Mi", evs)
			}
		})
	}
}

// An exit 1 before the pod was ever ready is a failure at start (Redis unreachable, a dead mount
// point): MountFailed, not WritesLost, unless its output reports lost writes.
func TestReportExitBeforeReady(t *testing.T) {
	for _, tc := range []struct {
		message, reason string
	}{
		{"Error: redis: dial tcp 10.0.0.9:6379: connect: connection refused", reasonMountFailed},
		{"Error: 1 file being written was not sent before the filesystem was unmounted: its changes are lost",
			reasonWritesLost},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			e := newNodeEnv(t)
			e.claim(map[string]string{AnnotationUID: "1000"})
			if c := e.stage(e.stageRequest(nil)); c != codes.Aborted {
				t.Fatalf("stage = %v", c)
			}
			e.started()
			e.exit(1, tc.message)
			if c, err := e.unstage(); c != codes.OK {
				t.Fatalf("unstage = %v (%v)", c, err)
			}
			evs := strings.Join(e.eventList(), "\n")
			other := reasonWritesLost
			if tc.reason == reasonWritesLost {
				other = reasonMountFailed
			}
			if !strings.Contains(evs, tc.reason) || strings.Contains(evs, other) {
				t.Fatalf("events = %q, want %s only", evs, tc.reason)
			}
		})
	}
}

// A mount pod that fails at start is reported while kubelet retries the stage, once, and not again
// when it is released.
func TestReportExitDuringStage(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	if c := e.stage(e.stageRequest(nil)); c != codes.Aborted {
		t.Fatalf("stage = %v", c)
	}
	e.started()
	e.exit(1, "Error: redis: dial tcp 10.0.0.9:6379: connect: connection refused")
	for range 3 {
		if c := e.stage(e.stageRequest(nil)); c != codes.Aborted {
			t.Fatalf("stage of a dead pod = %v", c)
		}
	}
	if n := strings.Count(strings.Join(e.eventList(), "\n"), reasonMountFailed); n != 1 {
		t.Fatalf("%d MountFailed events during the stage, want 1", n)
	}
	if c, err := e.unstage(); c != codes.OK {
		t.Fatalf("unstage = %v (%v)", c, err)
	}
	if n := strings.Count(strings.Join(e.eventList(), "\n"), reasonMountFailed); n != 0 {
		t.Fatalf("%d more MountFailed events at the release, want none", n)
	}
}

// A give-up on a 77 lasts mountpod.GiveUpRetryAfter, whatever kubelet's retries: then one new
// stage creates a pod (the machine may be enabled again); refused again, it waits as long again.
func TestNodeStageGaveUpOn77Expires(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.giveUp()
	e.s.Recover(context.Background()) // the sweep releases the exited pod
	for range 6 {
		e.advance(2 * time.Minute)
		if c := e.stage(e.stageRequest(nil)); c != codes.FailedPrecondition {
			t.Fatalf("stage before the expiry = %v, want FailedPrecondition", c)
		}
	}
	e.advance(4 * time.Minute) // 16 minutes since the exit
	if c := e.stage(e.stageRequest(nil)); c != codes.Aborted || e.podGone() {
		t.Fatalf("stage after the expiry = %v, want Aborted with a new pod", c)
	}
	e.exit(77, "machine refused")
	for range 3 {
		if c := e.stage(e.stageRequest(nil)); c != codes.FailedPrecondition {
			t.Fatalf("stage after the second 77 = %v, want FailedPrecondition", c)
		}
		e.advance(2 * time.Minute)
	}
}
