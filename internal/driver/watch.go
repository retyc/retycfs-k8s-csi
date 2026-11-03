package driver

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	"github.com/retyc/retycfs-k8s-csi/internal/identity"
	"github.com/retyc/retycfs-k8s-csi/internal/mountpod"
)

// repairWorkers is how many volumes the pod watch repairs at once: a repair may wait for a mount
// pod's /readyz.
const repairWorkers = 4

// A failed repair is queued again after minRetryDelay, doubled at each failure in a row up to
// maxRetryDelay; the rounds of Recover still run in between. A restart time already passed is also
// deferred by minRetryDelay at least: never a repair right after the last one.
const (
	minRetryDelay = time.Second
	maxRetryDelay = 5 * time.Minute
)

// newRepairQueue returns the queue of the volumes to repair, rate limited per volume (see
// minRetryDelay).
func newRepairQueue() workqueue.TypedRateLimitingInterface[string] {
	return workqueue.NewTypedRateLimitingQueue(
		workqueue.NewTypedItemExponentialFailureRateLimiter[string](minRetryDelay, maxRetryDelay))
}

// Watch keeps the volumes of the node repaired until ctx is done: a mount pod can also die on its
// own (OOM kill, eviction, kill -9) while the node plugin keeps running. A watch on the node's mount
// pods (list and watch of the pods in the driver's namespace) repairs a volume as soon as its pod
// exits, is deleted or becomes ready: a dead pod is recreated once its restart delay is over, and
// the binds of the pods' target paths are stacked again once the new one mounted the volume.
// Recover runs every interval as well, as a safety net for what an event did not cure (an API
// error, a pod stuck terminating).
func (s *NodeServer) Watch(ctx context.Context, interval time.Duration) {
	queue := newRepairQueue()
	go func() {
		<-ctx.Done()
		queue.ShutDown()
	}()
	// Without blocking the rounds: the informer only syncs once the API answers.
	go func() {
		if err := s.watchPods(ctx, queue); err != nil {
			klog.Warningf("watching the mount pods: %v; repairing every %s only", err, interval)
		}
	}()
	go func() {
		if err := s.watchIdentities(ctx, queue); err != nil {
			klog.Warningf("watching the RetycIdentities: %v; volumes refused by Retyc are tried again every %s only",
				err, mountpod.GiveUpRetryAfter)
		}
	}()
	for range repairWorkers {
		go s.repairWorker(ctx, queue)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Recover(ctx)
		}
	}
}

// watchPods starts an informer on the mount pods of the node that queues the volume of every pod
// whose state changed in a way a repair acts on (see podSignature), and returns once it is synced.
func (s *NodeServer) watchPods(ctx context.Context, queue workqueue.TypedDelayingInterface[string]) error {
	if s.Pods == nil || s.Pods.Kube == nil {
		return errors.New("no Kubernetes client")
	}
	factory := informers.NewSharedInformerFactoryWithOptions(s.Pods.Kube, 0,
		informers.WithNamespace(s.Pods.Config().Namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = mountpod.NodeSelector(s.NodeID) }))
	informer := factory.Core().V1().Pods().Informer()
	enqueue := func(obj any, why string) {
		if pod := s.ownPod(obj); pod != nil {
			klog.V(2).Infof("recovery: mount pod %s of volume %s %s", pod.Name, pod.Annotations[mountpod.LabelVolume], why)
			queue.Add(pod.Annotations[mountpod.LabelVolume])
		}
	}
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		// At the start: the pods that died while the informer was not watching yet.
		AddFunc: func(obj any) {
			if pod, ok := obj.(*corev1.Pod); ok && (pod.DeletionTimestamp != nil || podFinished(pod)) {
				enqueue(obj, "found finished")
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldPod, ok1 := oldObj.(*corev1.Pod)
			newPod, ok2 := newObj.(*corev1.Pod)
			if ok1 && ok2 && podSignature(oldPod) != podSignature(newPod) {
				enqueue(newObj, "is now "+podSignature(newPod))
			}
		},
		DeleteFunc: func(obj any) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			enqueue(obj, "deleted")
		},
	})
	if err != nil {
		return err
	}
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return fmt.Errorf("the pod informer did not sync: %w", ctx.Err())
	}

	return nil
}

// watchIdentities starts an informer on the RetycIdentities that revives the volumes of a machine
// enabled again in Retyc. Each auth check of the enroller that finds a machine accepted sets the
// identity's checkedAt (Active true): the volumes of the node that gave up on a 77 with that machine
// before that check are tried again at once, without waiting for their give-up to expire
// (mountpod.GiveUpRetryAfter). It works whether or not the enroller saw the refusal (a machine
// disabled and enabled again between two checks), and whatever the order: a 77 recorded after the
// event is not given up on (see MachineAccepted).
func (s *NodeServer) watchIdentities(ctx context.Context, queue workqueue.TypedDelayingInterface[string]) error {
	if s.Pods == nil || s.Dynamic == nil {
		return errors.New("no Kubernetes client")
	}
	factory := dynamicinformer.NewDynamicSharedInformerFactory(s.Dynamic, 0)
	informer := factory.ForResource(identity.RetycIdentityGVR).Informer()
	seen := func(obj any) {
		machineID, at := machineAccepted(obj)
		if machineID == "" {
			return
		}
		s.noteAccepted(machineID, at)
		for _, volumeID := range s.Pods.Revive(machineID, at) {
			queue.Add(volumeID)
		}
	}
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    seen,
		UpdateFunc: func(_, obj any) { seen(obj) },
	})
	if err != nil {
		return err
	}
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		return fmt.Errorf("the RetycIdentity informer did not sync: %w", ctx.Err())
	}

	return nil
}

// noteAccepted records that Retyc accepted machineID at at, for MachineAccepted.
func (s *NodeServer) noteAccepted(machineID string, at time.Time) {
	for {
		prev, loaded := s.accepted.LoadOrStore(machineID, at)
		if !loaded || !at.After(prev.(time.Time)) { //nolint:forcetypeassert // only times are stored
			return
		}
		if s.accepted.CompareAndSwap(machineID, prev, at) {
			return
		}
	}
}

// MachineAccepted returns when Retyc last accepted machineID, as the RetycIdentities seen by the
// node plugin tell (zero if never): mountpod.Manager's MachineAccepted.
func (s *NodeServer) MachineAccepted(machineID string) time.Time {
	if v, ok := s.accepted.Load(machineID); ok {
		return v.(time.Time) //nolint:forcetypeassert // only times are stored
	}

	return time.Time{}
}

// machineAccepted returns the machine ID of a RetycIdentity whose machine Retyc accepts (Active
// true) and the time of the check that said so (status.checkedAt), "" when it is not accepted or the
// enroller records no check time (an older enroller).
func machineAccepted(obj any) (string, time.Time) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return "", time.Time{}
	}
	if c := activeCondition(u); c == nil || c.Status != metav1.ConditionTrue {
		return "", time.Time{}
	}
	raw, _, _ := unstructured.NestedString(u.Object, "status", "checkedAt")
	at, err := time.Parse(time.RFC3339, raw)
	machineID, _, _ := unstructured.NestedString(u.Object, "status", "machineID")
	if err != nil || machineID == "" {
		return "", time.Time{}
	}

	return machineID, at
}

// activeCondition is the Active condition of a RetycIdentity, nil when it has none.
func activeCondition(u *unstructured.Unstructured) *metav1.Condition {
	raw, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	var conditions []metav1.Condition
	for _, c := range raw {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		var cond metav1.Condition
		if runtime.DefaultUnstructuredConverter.FromUnstructured(m, &cond) == nil {
			conditions = append(conditions, cond)
		}
	}

	return meta.FindStatusCondition(conditions, identity.ConditionActive)
}

// ownPod returns obj when it is a mount pod of this node, nil otherwise: a hashed node label may
// match another node, the annotation holds the exact name.
func (s *NodeServer) ownPod(obj any) *corev1.Pod {
	pod, ok := obj.(*corev1.Pod)
	if !ok || pod.Annotations[mountpod.LabelNode] != s.NodeID || pod.Annotations[mountpod.LabelVolume] == "" {
		return nil
	}

	return pod
}

// podFinished says whether a pod's mount process is gone: failed, succeeded or its container
// terminated (restartPolicy Never).
func podFinished(pod *corev1.Pod) bool {
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		return true
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil {
			return true
		}
	}

	return false
}

// podSignature sums up what a repair acts on: being deleted, the mount process gone, the pod ready
// (its readiness probe on /readyz, every 5 s).
func podSignature(pod *corev1.Pod) string {
	ready := false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}

	return fmt.Sprintf("deleting=%t finished=%t ready=%t", pod.DeletionTimestamp != nil, podFinished(pod), ready)
}

// repairWorker repairs the volumes of the queue, one at a time and under the volume's lock. A
// volume whose repair failed (the API down, say) is queued again with a growing delay; one whose
// mount pod waits for its restart time, for that time.
func (s *NodeServer) repairWorker(ctx context.Context, queue workqueue.TypedRateLimitingInterface[string]) {
	for {
		volumeID, shutdown := queue.Get()
		if shutdown {
			return
		}
		retryAt, err := s.repair(ctx, volumeID)
		switch {
		case err != nil:
			queue.AddRateLimited(volumeID)
		case !retryAt.IsZero():
			queue.Forget(volumeID)
			queue.AddAfter(volumeID, max(retryAt.Sub(s.Pods.Clock()), minRetryDelay))
		default:
			queue.Forget(volumeID)
		}
		queue.Done(volumeID)
	}
}

// repair runs the recovery of one volume and returns when to run it again (zero: at the next event).
func (s *NodeServer) repair(ctx context.Context, volumeID string) (time.Time, error) {
	defer s.locks.Lock(volumeID)()
	_, retryAt, err := s.recoverVolume(ctx, volumeID, false)
	if err != nil {
		klog.Errorf("recovery: volume %s: %v", volumeID, err)
	}

	return retryAt, err
}
