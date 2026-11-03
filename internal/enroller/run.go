package enroller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/retyc/retycfs-k8s-csi/internal/identity"
	"github.com/retyc/retycfs-k8s-csi/internal/mountpod"
	"github.com/retyc/retycfs-k8s-csi/internal/retycclient"
)

// worker runs the passes of one RetycIdentity, one at a time.
type worker struct {
	kick chan struct{}

	mu           sync.Mutex
	cancelEnroll context.CancelFunc
	// later is the pass scheduled after a delay, if any: one at most, the last scheduled wins, so that
	// the passes queued by events do not each start a series of delayed passes.
	later *time.Timer
	// stopping is set when the object is being deleted: an enrollment of the pass under way, even
	// one not started yet (the deletion seen between its read and its start), stops at once.
	stopping bool
}

// schedule runs f after d, replacing the pass scheduled before, if any.
func (w *worker) schedule(d time.Duration, f func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.later != nil {
		w.later.Stop()
	}
	w.later = time.AfterFunc(d, f)
}

// passStarting clears the stop of the previous pass: the next one reads the object, deletion
// included, after it.
func (w *worker) passStarting() {
	w.mu.Lock()
	w.stopping = false
	w.mu.Unlock()
}

// enrolling returns the context of an enrollment, which interrupt cancels (already cancelled when
// a stop was asked), and the function to call once it is over.
func (w *worker) enrolling(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	w.mu.Lock()
	w.cancelEnroll = cancel
	if w.stopping {
		cancel()
	}
	w.mu.Unlock()

	return ctx, func() {
		w.mu.Lock()
		w.cancelEnroll = nil
		w.mu.Unlock()
		cancel()
	}
}

// stop asks the pass under way to stop its enrollment, started or not.
func (w *worker) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopping = true
	if w.cancelEnroll != nil {
		w.cancelEnroll()
	}
}

// Run watches the RetycIdentity objects until ctx is done. Every AuthInterval each one gets a pass
// (an approved machine's auth status, a Blocked one's PVs); every SweepInterval the orphan Secrets
// are swept and the cluster-wide RetycIdentity is created if missing (EnsureClusterIdentity). Run
// it in a single replica: the passes are not coordinated across processes. Run may be called again
// once it returned, never concurrently.
func (e *Enroller) Run(ctx context.Context) error {
	e.mu.Lock()
	e.stopped = false
	e.workers = map[string]*worker{}
	e.lastAuth = map[types.UID]time.Time{}
	e.authDue = map[types.UID]bool{}
	e.mu.Unlock()

	factory := dynamicinformer.NewDynamicSharedInformerFactory(e.Dynamic, e.authInterval())
	informer := factory.ForResource(GVR).Informer()
	_, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { e.event(ctx, obj, false) },
		UpdateFunc: func(_, obj any) {
			m, err := meta.Accessor(obj)
			e.event(ctx, obj, err == nil && m.GetDeletionTimestamp() != nil)
		},
		DeleteFunc: func(obj any) { e.event(ctx, obj, true) },
	})
	if err != nil {
		return fmt.Errorf("watching RetycIdentities: %w", err)
	}
	e.mu.Lock()
	e.identities = informer.GetStore()
	e.mu.Unlock()
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		factory.Shutdown()
		e.stop()
		if ctx.Err() != nil {
			return nil
		}

		return errors.New("RetycIdentities: the informer did not sync")
	}
	// Once the RetycIdentities are known: a pod found exited at the start finds its identity.
	pods, err := e.watchMountPods(ctx)
	if err != nil {
		factory.Shutdown()
		e.stop()

		return err
	}
	defer pods.Shutdown()
	klog.Infof("enroller: watching RetycIdentities, Secrets and mount pods in namespace %s", e.Namespace)

	sweep := e.SweepInterval
	if sweep <= 0 {
		sweep = DefaultSweepInterval
	}
	ticker := time.NewTicker(sweep)
	defer ticker.Stop()
	e.ensureClusterIdentity(ctx)
	e.Sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			factory.Shutdown()
			e.stop()

			return nil
		case <-ticker.C:
			e.ensureClusterIdentity(ctx)
			e.Sweep(ctx)
		}
	}
}

// event queues a pass for an object of the informer; stop interrupts its enrollment under way
// (the object is being deleted).
func (e *Enroller) event(ctx context.Context, obj any, stop bool) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		klog.Errorf("enroller: %v", err)

		return
	}
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil || name != ObjectName {
		if _, isNew := obj.(*cache.DeletedFinalStateUnknown); !isNew && err == nil {
			klog.V(2).Infof("enroller: RetycIdentity %s ignored: only %q is acted on", key, ObjectName)
		}

		return
	}
	if stop {
		e.interrupt(key)
	}
	e.enqueue(ctx, namespace, name)
}

// enqueue asks for a pass on namespace/name, starting its worker if needed.
func (e *Enroller) enqueue(ctx context.Context, namespace, name string) {
	if ctx.Err() != nil {
		return
	}
	key := namespace + "/" + name
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopped {
		return
	}
	w := e.workers[key]
	if w == nil {
		w = &worker{kick: make(chan struct{}, 1)}
		e.workers[key] = w
		e.wg.Add(1)
		go e.work(ctx, namespace, name, w)
	}
	// Under e.mu: a worker exits only with no kick pending.
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// stop waits for the workers, once no new one may start.
func (e *Enroller) stop() {
	e.mu.Lock()
	e.stopped = true
	e.mu.Unlock()
	e.wg.Wait()
}

// interrupt stops the enrollment of key's pass under way, if any; without a worker, no pass runs
// and the next one reads the deletion.
func (e *Enroller) interrupt(key string) {
	e.mu.Lock()
	w := e.workers[key]
	e.mu.Unlock()
	if w != nil {
		w.stop()
	}
}

// work runs the passes of namespace/name until it is gone with nothing pending, or ctx is done.
func (e *Enroller) work(ctx context.Context, namespace, name string, w *worker) {
	defer e.wg.Done()
	key := namespace + "/" + name
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.kick:
		}
		w.passStarting()
		res := e.reconcile(ctx, w, namespace, name)
		switch {
		case res.again:
			select {
			case w.kick <- struct{}{}:
			default:
			}
		case res.after > 0:
			w.schedule(res.after, func() { e.enqueue(ctx, namespace, name) })
		case res.gone:
			e.mu.Lock()
			if len(w.kick) == 0 {
				delete(e.workers, key)
				e.mu.Unlock()

				return
			}
			e.mu.Unlock()
		}
	}
}

// watchMountPods starts an informer on the mount pods of the driver's namespace: one that exits with
// 77 (Retyc refuses its machine) has its machine checked at once (see recheck), so that Active goes
// false within seconds, not at the next AuthInterval; the node plugins then revive its volumes as
// soon as it is accepted again. Without it, a machine disabled and enabled again between two checks
// is never seen refused, and its mounts wait for their give-up to expire.
func (e *Enroller) watchMountPods(ctx context.Context) (informers.SharedInformerFactory, error) {
	factory := informers.NewSharedInformerFactoryWithOptions(e.Kube, 0, informers.WithNamespace(e.Namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = mountpod.Selector() }))
	_, err := factory.Core().V1().Pods().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			pod, _ := obj.(*corev1.Pod)
			if machineID := mountpod.RefusedMachine(pod); machineID != "" {
				e.recheckMachine(ctx, machineID, pod.Name)
			}
		},
		UpdateFunc: func(oldObj, newObj any) {
			oldPod, _ := oldObj.(*corev1.Pod)
			newPod, _ := newObj.(*corev1.Pod)
			if machineID := mountpod.RefusedMachine(newPod); machineID != "" && mountpod.RefusedMachine(oldPod) == "" {
				e.recheckMachine(ctx, machineID, newPod.Name)
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("watching the mount pods: %w", err)
	}
	factory.Start(ctx.Done())

	return factory, nil
}

// recheckMachine checks at once the RetycIdentity of machineID, which the mount pod podName found
// refused.
func (e *Enroller) recheckMachine(ctx context.Context, machineID, podName string) {
	e.mu.Lock()
	store := e.identities
	e.mu.Unlock()
	if store == nil {
		return
	}
	for _, obj := range store.List() {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			continue
		}
		if id, _, _ := unstructured.NestedString(u.Object, "status", "machineID"); id != machineID {
			continue
		}
		klog.Infof("enroller: %s/%s: mount pod %s exited with 77: checking machine_id=%s now", u.GetNamespace(),
			u.GetName(), podName, machineID)
		e.recheck(ctx, u.GetNamespace(), u.GetName(), u.GetUID())
	}
}

// recheck makes the next auth check of an identity due RefusedAuthInterval after the last one and
// queues a pass: it checks at once if the last check is that old, and otherwise schedules itself
// for when it is (see nextAuth). Several mount pods refused together make one check.
func (e *Enroller) recheck(ctx context.Context, namespace, name string, uid types.UID) {
	e.mu.Lock()
	if e.authDue == nil {
		e.authDue = map[types.UID]bool{}
	}
	e.authDue[uid] = true
	e.mu.Unlock()
	e.enqueue(ctx, namespace, name)
}

// nextAuth returns how long until the next auth check of an approved identity is due when it comes
// sooner than the informer's resync: the machine refused, or a recheck asked by a mount pod. The
// delay runs from the last check, so that the passes in between (status writes, resyncs) do not push
// it back. ok is false when the resync is soon enough.
func (e *Enroller) nextAuth(uid types.UID, st *Status) (time.Duration, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !machineRefused(st) && !e.authDue[uid] {
		return 0, false
	}
	wait := e.refusedAuthInterval()
	if last, ok := e.lastAuth[uid]; ok {
		wait = last.Add(e.refusedAuthInterval()).Sub(e.now())
	}

	return max(wait, time.Second), true
}

// machineRefused says whether the last auth check found the machine disabled or deleted in Retyc.
func machineRefused(st *Status) bool {
	c := meta.FindStatusCondition(st.Conditions, ConditionActive)

	return c != nil && c.Status == metav1.ConditionFalse && c.Reason == ReasonMachineRefused
}

// checkAuth runs `retyc auth status` as the approved identity, at most once per AuthInterval (per
// RefusedAuthInterval while the machine is refused, or once a mount pod found it refused), and sets
// the Active condition.
func (p *pass) checkAuth(ctx context.Context, creds *identity.Credentials) {
	e, ri, st := p.e, p.ri, &p.st
	if e.Auth == nil {
		return
	}
	wasRefused := machineRefused(st)
	now := e.now()
	e.mu.Lock()
	if e.lastAuth == nil {
		e.lastAuth = map[types.UID]time.Time{}
	}
	interval := e.authInterval()
	if wasRefused || e.authDue[ri.UID] {
		interval = e.refusedAuthInterval()
	}
	// A tenth of slack: the informer's resync, or the pass after a refusal, comes every interval too.
	last, ok := e.lastAuth[ri.UID]
	if ok && now.Sub(last) < interval*9/10 {
		e.mu.Unlock()

		return
	}
	e.lastAuth[ri.UID] = now
	delete(e.authDue, ri.UID)
	e.mu.Unlock()

	err := e.Auth(ctx, creds)
	cond := metav1.Condition{Type: ConditionActive, ObservedGeneration: ri.Generation}
	switch {
	case err == nil:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionTrue, ReasonAuthenticated, "Retyc accepts the machine"
	case retycclient.IsMachineRefused(err):
		cond.Status, cond.Reason = metav1.ConditionFalse, ReasonMachineRefused
		cond.Message = fmt.Sprintf("Retyc refuses machine %s, disabled or deleted: an administrator tells which "+
			"(Organization > Machines, or `retyc machine ls`). Disabled: enable it again. Deleted: delete the "+
			"RetycIdentity to enroll a new one", st.MachineID)
		if !wasRefused {
			e.Events.Event(p.obj, corev1.EventTypeWarning, ReasonMachineRefused, cond.Message)
			klog.Warningf("enroller: %s/%s: machine_id=%s refused by Retyc (disabled or deleted)", ri.Namespace,
				ri.Name, st.MachineID)
		}
	case wasRefused:
		// Retyc unreachable: the refusal stands until Retyc answers.
		klog.Warningf("enroller: %s/%s: auth status: %v", ri.Namespace, ri.Name, err)

		return
	default:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionUnknown, ReasonAuthCheckFailed, err.Error()
		klog.Warningf("enroller: %s/%s: auth status: %v", ri.Namespace, ri.Name, err)
	}
	meta.SetStatusCondition(&st.Conditions, cond)
	if cond.Status != metav1.ConditionUnknown {
		st.CheckedAt = &metav1.Time{Time: now}
	}
	if err == nil && wasRefused {
		klog.Infof("enroller: %s/%s: machine_id=%s accepted again by Retyc", ri.Namespace, ri.Name, st.MachineID)
	}
}
