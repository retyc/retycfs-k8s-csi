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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/retyc/retycfs-k8s-csi/internal/identity"
	"github.com/retyc/retycfs-k8s-csi/internal/retycclient"
)

// worker runs the passes of one RetycIdentity, one at a time.
type worker struct {
	kick chan struct{}

	mu           sync.Mutex
	cancelEnroll context.CancelFunc
	// stopping is set when the object is being deleted: an enrollment of the pass under way, even
	// one not started yet (the deletion seen between its read and its start), stops at once.
	stopping bool
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
	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		factory.Shutdown()
		e.stop()
		if ctx.Err() != nil {
			return nil
		}

		return errors.New("RetycIdentities: the informer did not sync")
	}
	klog.Infof("enroller: watching RetycIdentities, Secrets in namespace %s", e.Namespace)

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
			time.AfterFunc(res.after, func() { e.enqueue(ctx, namespace, name) })
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

// checkAuth runs `retyc auth status` as the approved identity, at most once per AuthInterval, and
// sets the Active condition.
func (p *pass) checkAuth(ctx context.Context, creds *identity.Credentials) {
	e, ri, st := p.e, p.ri, &p.st
	if e.Auth == nil {
		return
	}
	now := e.now()
	e.mu.Lock()
	if e.lastAuth == nil {
		e.lastAuth = map[types.UID]time.Time{}
	}
	// A tenth of slack: the informer's resync comes every AuthInterval too.
	last, ok := e.lastAuth[ri.UID]
	if ok && now.Sub(last) < e.authInterval()*9/10 {
		e.mu.Unlock()

		return
	}
	e.lastAuth[ri.UID] = now
	e.mu.Unlock()

	err := e.Auth(ctx, creds)
	prev := meta.FindStatusCondition(st.Conditions, ConditionActive)
	wasRefused := prev != nil && prev.Status == metav1.ConditionFalse && prev.Reason == ReasonMachineRefused
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
	if err == nil && wasRefused {
		klog.Infof("enroller: %s/%s: machine_id=%s accepted again by Retyc", ri.Namespace, ri.Name, st.MachineID)
	}
}
