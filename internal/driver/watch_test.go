package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/retyc/retycfs-k8s-csi/internal/identity"
	"github.com/retyc/retycfs-k8s-csi/internal/mountpod"
)

// ready makes the mount pod run, mount the volume and pass its readiness probe, and returns the
// device of the mount.
func (e *nodeEnv) ready() string {
	e.t.Helper()
	dev := e.mnt.fuse(e.staging, testVolume)
	pod := e.pod()
	pod.Status = corev1.PodStatus{
		Phase: corev1.PodRunning, PodIP: "10.0.0.7",
		Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "mount", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}
	if _, err := e.kube.CoreV1().Pods(testNS).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		e.t.Fatal(err)
	}

	return dev
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting: %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The pod watch repairs a volume without waiting for the recovery's round: a killed mount pod is
// replaced once its restart delay is over, and the target is bound again once the new one is ready.
func TestWatchRepairsAtOnce(t *testing.T) {
	e := newNodeEnvWith(t, func(c *mountpod.Config) { c.RestartBackoff = 50 * time.Millisecond })
	e.s.Pods.Now = time.Now // the queue's delays are real ones
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.stage(e.stageRequest(nil))
	dev := e.ready()
	if c := e.stage(e.stageRequest(nil)); c != codes.OK {
		t.Fatalf("stage = %v", c)
	}
	e.publish(e.target)
	old := e.pod().UID

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queue := newRepairQueue()
	defer queue.ShutDown()
	if err := e.s.watchPods(ctx, queue); err != nil {
		t.Fatal(err)
	}
	go e.s.repairWorker(ctx, queue)

	// kill -9: the container exits 137, the mount is dead.
	e.mnt.kill(dev)
	e.exit(137, "")
	eventually(t, "a new mount pod", func() bool {
		p, err := e.kube.CoreV1().Pods(testNS).Get(ctx, mountpod.Name(testNode, testVolume), metav1.GetOptions{})

		return err == nil && p.UID != old && p.DeletionTimestamp == nil
	})
	newDev := e.ready()
	eventually(t, "the target bound to the new mount", func() bool {
		top, ok := e.mnt.top(e.target)

		return ok && device(top) == newDev
	})
}

// A repair that fails (the API refusing to release an exited pod whose restart time is over) is
// retried with a growing delay, not at once in a loop.
func TestWatchRetryRateLimited(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.stageReady()
	e.exit(137, "")
	e.s.Recover(context.Background()) // counts the exit
	e.advance(time.Hour)              // the restart time is long over
	deletes := 0
	e.kube.PrependReactor("delete", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		e.mu.Lock()
		defer e.mu.Unlock()
		deletes++

		return true, nil, errors.New("connection refused")
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queue := newRepairQueue()
	defer queue.ShutDown()
	go e.s.repairWorker(ctx, queue)
	queue.Add(testVolume)
	eventually(t, "a first repair", func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()

		return deletes > 0
	})
	time.Sleep(minRetryDelay + minRetryDelay/2)
	e.mu.Lock()
	defer e.mu.Unlock()
	// 0 s, 1 s, then 3 s: two tries in 1.5 s, three with a slow first one.
	if deletes < 2 || deletes > 3 {
		t.Fatalf("%d tries in %s, want 2 or 3", deletes, minRetryDelay+minRetryDelay/2)
	}
}

// identityWith is a RetycIdentity of machine m-1 whose Active condition is status/reason, none
// when status is "", last checked at checked (none if zero).
func identityWith(status metav1.ConditionStatus, reason string, checked time.Time) *unstructured.Unstructured {
	st := map[string]any{"machineID": "m-1"}
	if status != "" {
		st["conditions"] = []any{
			map[string]any{"type": "Enrolled", "status": "True", "reason": "Approved"},
			map[string]any{"type": identity.ConditionActive, "status": string(status), "reason": reason},
		}
	}
	if !checked.IsZero() {
		st["checkedAt"] = checked.UTC().Format(time.RFC3339)
	}
	u := &unstructured.Unstructured{Object: map[string]any{"status": st}}
	u.SetAPIVersion("csi.retycfs.com/v1alpha1")
	u.SetKind("RetycIdentity")
	u.SetNamespace(testNS)
	u.SetName("default")

	return u
}

// A machine counts as accepted at its last check, only when Active is true.
func TestMachineAccepted(t *testing.T) {
	at := time.Date(2026, 11, 3, 14, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		obj     any
		machine string
	}{
		{"accepted", identityWith(metav1.ConditionTrue, "Authenticated", at), "m-1"},
		{"refused", identityWith(metav1.ConditionFalse, identity.ReasonMachineRefused, at), ""},
		{"check failed", identityWith(metav1.ConditionUnknown, "AuthCheckFailed", at), ""},
		{"never checked", identityWith("", "", time.Time{}), ""},
		{"no check time (older enroller)", identityWith(metav1.ConditionTrue, "Authenticated", time.Time{}), ""},
		{"not an object", "x", ""},
	} {
		machine, when := machineAccepted(tc.obj)
		if machine != tc.machine || (machine != "" && !when.Equal(at)) {
			t.Errorf("%s: machine = %q at %s, want %q", tc.name, machine, when, tc.machine)
		}
	}
}

// watchIdentitiesEnv stages the volume with machine m-1 and starts the RetycIdentity watch on ri,
// with a repair worker.
func watchIdentitiesEnv(t *testing.T, ri *unstructured.Unstructured) (*nodeEnv, dynamic.Interface, context.Context) {
	t.Helper()
	e := newNodeEnv(t)
	machine := filepath.Join(e.s.IdentityDir, identity.MachineFile)
	if err := os.WriteFile(machine, []byte(`{"machine_id":"m-1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.stageReady()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{identity.RetycIdentityGVR: "RetycIdentityList"}, ri)
	e.s.Dynamic = dyn
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	queue := newRepairQueue()
	t.Cleanup(queue.ShutDown)
	if err := e.s.watchIdentities(ctx, queue); err != nil {
		t.Fatal(err)
	}
	go e.s.repairWorker(ctx, queue)

	return e, dyn, ctx
}

// A volume that gave up on a 77 is tried again as soon as a later check finds its machine accepted,
// long before the give-up expires, whether the enroller saw the refusal (Active false, then true) or
// not (the machine enabled again before its next check: Active stays true, checkedAt moves).
func TestWatchIdentitiesRevives(t *testing.T) {
	for _, sawRefusal := range []bool{true, false} {
		t.Run(fmt.Sprintf("refusal seen %t", sawRefusal), func(t *testing.T) {
			e := newNodeEnv(t) // for its clock only
			before := e.clock().Add(-time.Minute)
			ri := identityWith(metav1.ConditionTrue, "Authenticated", before)
			if sawRefusal {
				ri = identityWith(metav1.ConditionFalse, identity.ReasonMachineRefused, before)
			}
			e, dyn, ctx := watchIdentitiesEnv(t, ri)
			old := e.pod().UID
			e.exit(mountpod.ExitAuthRequired, "machine disabled")
			e.s.Recover(ctx) // counts the exit: gives up (accepted only before it)
			if !e.hasEvent(reasonMountGaveUp, "machine disabled") {
				t.Fatalf("after the 77: no MountGaveUp event in %q", e.eventList())
			}

			ri = identityWith(metav1.ConditionTrue, "Authenticated", e.clock().Add(5*time.Second))
			if _, err := dyn.Resource(identity.RetycIdentityGVR).Namespace(testNS).UpdateStatus(ctx, ri,
				metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			eventually(t, "a new mount pod", func() bool {
				p, err := e.kube.CoreV1().Pods(testNS).Get(ctx, mountpod.Name(testNode, testVolume), metav1.GetOptions{})

				return err == nil && p.UID != old && p.DeletionTimestamp == nil
			})
		})
	}
}

// A 77 no later than a check that already found the machine accepted (the event came first) is not
// given up on.
func TestWatchIdentitiesAcceptedBeforeExit(t *testing.T) {
	e := newNodeEnv(t)
	e, _, ctx := watchIdentitiesEnv(t, identityWith(metav1.ConditionTrue, "Authenticated", e.clock().Add(time.Second)))
	eventually(t, "the identity seen", func() bool { return !e.s.MachineAccepted("m-1").IsZero() })
	e.exit(mountpod.ExitAuthRequired, "machine disabled")
	old := e.pod().UID
	e.s.Recover(ctx) // counts the exit
	if e.hasEvent(reasonMountGaveUp, "") {
		t.Fatal("gave up on a 77 older than the acceptance")
	}
	// Restarted after the ordinary backoff, not after GiveUpRetryAfter.
	e.advance(time.Minute)
	e.s.Recover(ctx)
	if p := e.pod(); p.UID == old {
		t.Fatal("no new mount pod after the backoff")
	}
}
