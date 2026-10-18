package enroller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	"github.com/retyc/retycfs-k8s-csi/internal/identity"
	"github.com/retyc/retycfs-k8s-csi/internal/retycclient"
)

// Finalizer holds a RetycIdentity until its Secret is dealt with.
const Finalizer = "csi.retycfs.com/identity"

// AnnotationRetry on a RetycIdentity asks for a new series of enrollment requests after an
// expiry or a refused configuration: any value that changes.
const AnnotationRetry = "csi.retycfs.com/retry"

// Defaults of the Enroller's tunables.
const (
	DefaultMaxAttempts   = 4
	DefaultAuthInterval  = 5 * time.Minute
	DefaultSweepInterval = time.Hour
	DefaultRetryDelay    = time.Minute
	DefaultClusterName   = "kubernetes"
	// maxNameLength is Retyc's bound on a machine's display name and cluster UID.
	maxNameLength = 63
)

// Enrollment runs `retyc machine enroll` (retycclient.Client.Enroll).
type Enrollment interface {
	Enroll(ctx context.Context, p retycclient.EnrollParams, emit func(retycclient.EnrollEvent)) error
}

// AuthChecker runs `retyc auth status` as an identity: nil when Retyc accepts it, an error wrapping
// retycclient.ErrMachineRefused when the machine is disabled or deleted.
type AuthChecker func(ctx context.Context, creds *identity.Credentials) error

// Enroller reconciles the RetycIdentity objects of the cluster. One pass at a time per object, each
// in its own goroutine: an enrollment blocks for as long as its request lives (15 minutes).
type Enroller struct {
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	Retyc   Enrollment
	Auth    AuthChecker
	Events  record.EventRecorder
	// Namespace is the driver's: a RetycIdentity there is the cluster-wide identity, and every
	// identity's Secret lives there.
	Namespace string
	// DefaultOrganizationID applies to a RetycIdentity without spec.organizationID.
	DefaultOrganizationID string
	// ClusterName prefixes the machines' default display name.
	ClusterName string
	// TempDir holds the enrollments' working directories ("" for os.TempDir()).
	TempDir string

	MaxAttempts   int
	AuthInterval  time.Duration
	SweepInterval time.Duration
	RetryDelay    time.Duration
	Now           func() time.Time

	mu            sync.Mutex
	workers       map[string]*worker
	lastAuth      map[types.UID]time.Time
	kubeSystemUID string
	// keptDirs are the working directories of enrollments whose pending Secret could not be saved,
	// by RetycIdentity: the next pass resumes from their keys and request.
	keptDirs map[types.UID]string
	wg       sync.WaitGroup
	stopped  bool
}

func (e *Enroller) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}

	return time.Now()
}

func (e *Enroller) maxAttempts() int32 {
	if e.MaxAttempts > 0 {
		return int32(e.MaxAttempts) //nolint:gosec // G115: a small configured count
	}

	return DefaultMaxAttempts
}

func (e *Enroller) authInterval() time.Duration {
	if e.AuthInterval > 0 {
		return e.AuthInterval
	}

	return DefaultAuthInterval
}

func (e *Enroller) retryDelay() time.Duration {
	if e.RetryDelay > 0 {
		return e.RetryDelay
	}

	return DefaultRetryDelay
}

func (e *Enroller) resource(namespace string) dynamic.ResourceInterface {
	return e.Dynamic.Resource(GVR).Namespace(namespace)
}

// result tells the worker what follows a pass: another one now, one after a delay, or none and the
// object gone.
type result struct {
	again bool
	after time.Duration
	gone  bool
}

// pass is one reconciliation of a RetycIdentity: obj as read (for the events), ri decoded, st the
// status being built.
type pass struct {
	e   *Enroller
	obj *unstructured.Unstructured
	ri  *RetycIdentity
	st  Status
}

// reconcile runs one pass on the RetycIdentity namespace/name. w is the worker running it (nil in
// tests): an enrollment under way registers its cancel function there.
func (e *Enroller) reconcile(ctx context.Context, w *worker, namespace, name string) result {
	if name != ObjectName {
		klog.Warningf("enroller: RetycIdentity %s/%s ignored: only one named %q is acted on", namespace, name, ObjectName)

		return result{gone: true}
	}
	obj, err := e.resource(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// The cluster-wide identity deleted: created again (its informer event starts the pass), its
		// previous Secret replaced by a new enrollment (clearSecret).
		if namespace == e.Namespace {
			if _, err := e.EnsureClusterIdentity(ctx); err != nil {
				klog.Errorf("enroller: %v", err)

				return result{after: e.retryDelay()}
			}
		}

		return result{gone: true}
	}
	if err != nil {
		klog.Errorf("enroller: reading RetycIdentity %s/%s: %v", namespace, name, err)

		return result{after: e.retryDelay()}
	}
	ri, err := fromUnstructured(obj)
	if err != nil {
		klog.Errorf("enroller: %v", err)

		return result{}
	}
	p := &pass{e: e, obj: obj, ri: ri}
	if ri.DeletionTimestamp != nil {
		return p.finalize(ctx)
	}
	if !slices.Contains(ri.Finalizers, Finalizer) {
		if err := p.updateFinalizers(ctx, true); err != nil {
			klog.Errorf("enroller: adding the finalizer to %s/%s: %v", namespace, name, err)

			return result{after: e.retryDelay()}
		}
	}
	p.st = *ri.Status.DeepCopy()
	res := p.sync(ctx, w)
	if err := p.writeStatus(ctx); err != nil {
		klog.Errorf("enroller: writing the status of %s/%s: %v", namespace, name, err)
		if res.after == 0 && !res.again {
			res.after = e.retryDelay()
		}
	}

	return res
}

// sync brings the identity forward: Secret checks, enrollment, auth status.
func (p *pass) sync(ctx context.Context, w *worker) result {
	e, ri, st := p.e, p.ri, &p.st
	reason := enrolledReason(st)
	if reason == ReasonConfigRefused && st.ObservedGeneration != ri.Generation {
		meta.RemoveStatusCondition(&st.Conditions, ConditionEnrolled)
	}
	st.ObservedGeneration = ri.Generation
	if v := ri.Annotations[AnnotationRetry]; v != "" && v != st.ObservedRetry {
		st.ObservedRetry, st.Attempts = v, 0
		if reason == ReasonExpired || reason == ReasonConfigRefused {
			meta.RemoveStatusCondition(&st.Conditions, ConditionEnrolled)
		}
		klog.Infof("enroller: %s/%s: retry asked", ri.Namespace, ri.Name)
	}
	st.SecretName = e.SecretName(ri.Namespace)

	org := ri.Spec.OrganizationID
	if org == "" {
		org = e.DefaultOrganizationID
	}
	if org == "" {
		p.setEnrolled(PhaseRejected, ReasonNoOrganization, "no organization: set spec.organizationID, or the "+
			"enroller's --default-organization-id")

		return result{}
	}
	ns, err := e.Kube.CoreV1().Namespaces().Get(ctx, ri.Namespace, metav1.GetOptions{})
	if err != nil {
		st.Message = fmt.Sprintf("reading namespace %s: %v", ri.Namespace, err)

		return result{after: e.retryDelay()}
	}
	nsUID := string(ns.UID)
	// A tenant's machine is bound to its namespace: with the cluster's UID, Retyc would present
	// its request as replacing the cluster's machine.
	clusterUID := nsUID
	if ri.Namespace == e.Namespace {
		if clusterUID, err = e.clusterUID(ctx); err != nil {
			st.Message = err.Error()

			return result{after: e.retryDelay()}
		}
	}

	secret, err := e.Kube.CoreV1().Secrets(e.Namespace).Get(ctx, st.SecretName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		secret = nil
	case err != nil:
		st.Message = fmt.Sprintf("reading Secret %s/%s: %v", e.Namespace, st.SecretName, err)

		return result{after: e.retryDelay()}
	}
	if secret != nil && !ours(secret, nsUID, ri.UID) {
		blocked, err := p.clearSecret(ctx, secret)
		if err != nil {
			st.Message = err.Error()

			return result{after: e.retryDelay()}
		}
		if blocked != "" {
			if st.Phase != PhaseBlocked {
				e.Events.Event(p.obj, corev1.EventTypeWarning, PhaseBlocked, blocked)
			}
			p.setEnrolled(PhaseBlocked, ReasonBlocked, blocked)

			return result{}
		}
		secret = nil
	}
	if secret != nil && len(secret.Data[identity.MachineFile]) > 0 {
		p.approved(ctx, secret)

		return result{}
	}
	if enrolledReason(st) == ReasonBlocked {
		meta.RemoveStatusCondition(&st.Conditions, ConditionEnrolled)
	}
	if terminal(st) {
		return result{}
	}

	return p.enroll(ctx, w, org, nsUID, clusterUID, secret)
}

// ours says whether s holds the identity of the RetycIdentity uid, of the namespace nsUID.
func ours(s *corev1.Secret, nsUID string, uid types.UID) bool {
	return managed(s) && s.Labels[LabelNamespaceUID] == nsUID && s.Labels[LabelIdentityUID] == string(uid)
}

// clusterUID is the UID of the kube-system namespace, the cluster's.
func (e *Enroller) clusterUID(ctx context.Context) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.kubeSystemUID == "" {
		ns, err := e.Kube.CoreV1().Namespaces().Get(ctx, metav1.NamespaceSystem, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("reading namespace %s, the cluster's UID: %w", metav1.NamespaceSystem, err)
		}
		e.kubeSystemUID = string(ns.UID)
	}

	return e.kubeSystemUID, nil
}

// clearSecret deletes a Secret that is not this RetycIdentity's (left by a previous one, or by a
// namespace of the same name), so that it enrolls anew. It returns why it cannot instead: a Secret
// the enroller did not make, or a tenant's one PVs still use. The cluster-wide Secret is replaced
// whatever the PVs: the default ones name no Secret (the driver mounts it), blocking would only
// leave them without identity.
func (p *pass) clearSecret(ctx context.Context, s *corev1.Secret) (string, error) {
	e := p.e
	if !managed(s) {
		return fmt.Sprintf("Secret %s/%s exists and was not made by the enroller (type %s): delete or rename it",
			s.Namespace, s.Name, SecretType), nil
	}
	machine := s.Annotations[AnnotationMachineID]
	cluster := s.Name == ClusterSecretName
	if !cluster {
		used, err := e.referenced(ctx, s.Name)
		if err != nil {
			return "", err
		}
		if used {
			return fmt.Sprintf("Secret %s/%s holds the identity of a previous RetycIdentity or namespace (machine %q) "+
				"and PVs still use it: delete those PVs (or this RetycIdentity)", s.Namespace, s.Name, machine), nil
		}
	}
	uid := s.UID
	err := e.Kube.CoreV1().Secrets(s.Namespace).Delete(ctx, s.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("deleting the previous identity's Secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	klog.Infof("enroller: %s/%s: deleted the previous identity's Secret %s/%s, machine_id=%q stays in Retyc",
		p.ri.Namespace, p.ri.Name, s.Namespace, s.Name, machine)
	switch {
	case cluster && machine != "":
		msg := fmt.Sprintf("The cluster-wide identity was recreated: Secret %s/%s of machine %s is replaced by a new "+
			"enrollment. The existing datarooms belong to machine %s: once the new machine is approved, an "+
			"administrator must add it to those datarooms (re-pairing), then disable or delete machine %s in "+
			"Organization > Machines", s.Namespace, s.Name, machine, machine, machine)
		e.Events.Event(p.obj, corev1.EventTypeWarning, ReasonClusterIdentityReplaced, msg)
		klog.Warningf("enroller: %s/%s: %s", p.ri.Namespace, p.ri.Name, msg)
	case machine != "":
		e.Events.Eventf(p.obj, corev1.EventTypeNormal, "MachineKept", "The previous identity's Secret was deleted: "+
			"its machine %s stays in Retyc, disable or delete it in Organization > Machines", machine)
	}

	return "", nil
}

// keepFingerprint fills an empty status fingerprint from the k8s.json of the Secret: a status lost
// or reset keeps showing the one the administrator compared.
func keepFingerprint(st *Status, secret *corev1.Secret) {
	if info, err := identity.FromData(secret.Data).K8s(); err == nil && info != nil && st.Fingerprint == "" {
		st.Fingerprint = info.Fingerprint
	}
}

// approved fills the status of an identity whose Secret holds machine.json, and checks the machine
// with Retyc when due.
func (p *pass) approved(ctx context.Context, secret *corev1.Secret) {
	st := &p.st
	m := readMachine(secret.Data[identity.MachineFile])
	st.MachineID, st.ClientID = m.MachineID, m.ClientID
	creds := identity.FromData(secret.Data)
	keepFingerprint(st, secret)
	p.setEnrolled(PhaseApproved, ReasonApproved, fmt.Sprintf("machine %s approved, in Secret %s/%s",
		st.MachineID, secret.Namespace, secret.Name))
	p.checkAuth(ctx, creds)
	if c := meta.FindStatusCondition(st.Conditions, ConditionActive); c != nil && c.Status != metav1.ConditionTrue {
		st.Message = c.Message
	}
}

// enroll runs one enrollment, from secret's keys when it holds them, and records its outcome.
func (p *pass) enroll(ctx context.Context, w *worker, org, nsUID, clusterUID string, secret *corev1.Secret) result {
	e, ri, st := p.e, p.ri, &p.st
	dir, kept := e.takeKeptDir(ri.UID)
	if !kept {
		var err error
		if dir, err = os.MkdirTemp(e.TempDir, "enroll-"); err != nil { // 0700
			st.Message = fmt.Sprintf("creating the working directory: %v", err)

			return result{after: e.retryDelay()}
		}
	}
	keep := false
	defer func() {
		if keep {
			e.keepDir(ri.UID, dir)
		} else {
			_ = os.RemoveAll(dir)
		}
	}()
	// The keys of a kept directory are newer than the Secret's, which may hold none.
	if secret != nil && !kept {
		for _, name := range []string{identity.ClientKeyFile, identity.AgeKeyFile, identity.EnrollmentFile} {
			if v := secret.Data[name]; len(v) > 0 {
				if err := os.WriteFile(filepath.Join(dir, name), v, 0o600); err != nil {
					st.Message = fmt.Sprintf("writing %s: %v", name, err)

					return result{after: e.retryDelay()}
				}
			}
		}
		keepFingerprint(st, secret)
	}
	if enrolledReason(st) != ReasonPending {
		p.setEnrolled(PhasePending, ReasonPending, "enrolling")
		if err := p.writeStatus(ctx); err != nil {
			klog.Errorf("enroller: writing the status of %s/%s: %v", ri.Namespace, ri.Name, err)
		}
	}

	params := retycclient.EnrollParams{OrganizationID: org, Dir: dir, Name: e.machineName(ri), ClusterUID: clusterUID}
	enrollCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if w != nil {
		var done func()
		enrollCtx, done = w.enrolling(enrollCtx)
		defer done()
	}
	if enrollCtx.Err() != nil {
		// Deleted since the pass read it: the next pass finalizes it.
		klog.Infof("enroller: %s/%s: enrollment not started, the RetycIdentity is being deleted", ri.Namespace, ri.Name)

		return result{}
	}
	var last retycclient.EnrollEvent
	var saveErr error
	err := e.Retyc.Enroll(enrollCtx, params, func(ev retycclient.EnrollEvent) {
		if ev.Event != retycclient.EnrollPending {
			last = ev

			return
		}
		if saveErr = p.pending(ctx, ev, dir, nsUID, clusterUID); saveErr != nil {
			// Nobody may approve a request whose keys a restart would lose: stopped, resumed from
			// the kept directory by the next pass.
			cancel()
		}
	})

	switch {
	case err == nil:
		res := p.approve(ctx, dir, nsUID, clusterUID, last)
		// Approved without its keys in the Secret: only the directory holds them.
		keep = saveErr != nil && res.after > 0

		return res
	case saveErr != nil && ctx.Err() == nil:
		keep = true
		msg := fmt.Sprintf("saving the pending request into Secret %s/%s: %v: enrolling again", e.Namespace,
			st.SecretName, saveErr)
		p.setEnrolled(PhasePending, ReasonEnrollFailed, msg)
		klog.Errorf("enroller: %s/%s: %s", ri.Namespace, ri.Name, msg)

		return result{after: e.retryDelay()}
	case enrollCtx.Err() != nil:
		klog.Infof("enroller: %s/%s: enrollment interrupted", ri.Namespace, ri.Name)

		return result{}
	case last.Event == retycclient.EnrollExpired || errors.Is(err, retycclient.ErrEnrollmentExpired):
		st.Attempts++
		if st.Attempts >= e.maxAttempts() {
			msg := fmt.Sprintf("the enrollment request expired unanswered %d times: annotate the RetycIdentity with "+
				"%s=<new value> to ask again", st.Attempts, AnnotationRetry)
			p.setEnrolled(PhaseExpired, ReasonExpired, msg)
			e.Events.Event(p.obj, corev1.EventTypeWarning, PhaseExpired, msg)
			klog.Warningf("enroller: %s/%s: %s", ri.Namespace, ri.Name, msg)

			return result{}
		}
		p.setEnrolled(PhasePending, ReasonPending, fmt.Sprintf("the enrollment request expired unanswered (%d/%d): "+
			"asking again", st.Attempts, e.maxAttempts()))

		return result{again: true}
	case last.Event == retycclient.EnrollDenied || errors.Is(err, retycclient.ErrEnrollmentDenied):
		msg := "an administrator denied the enrollment request: delete and recreate the RetycIdentity to enroll " +
			"with new keys"
		p.setEnrolled(PhaseDenied, ReasonDenied, msg)
		e.Events.Event(p.obj, corev1.EventTypeWarning, PhaseDenied, msg)
		klog.Warningf("enroller: %s/%s: enrollment %s denied", ri.Namespace, ri.Name, st.EnrollmentID)

		return result{}
	case retycclient.IsConfig(err):
		msg := fmt.Sprintf("Retyc refused the enrollment: %v", err)
		p.setEnrolled(PhaseRejected, ReasonConfigRefused, msg)
		e.Events.Event(p.obj, corev1.EventTypeWarning, PhaseRejected, msg)
		klog.Warningf("enroller: %s/%s: %s", ri.Namespace, ri.Name, msg)

		return result{}
	default:
		msg := fmt.Sprintf("enrolling: %v", err)
		p.setEnrolled(PhasePending, ReasonEnrollFailed, msg)
		klog.Errorf("enroller: %s/%s: %s", ri.Namespace, ri.Name, msg)

		return result{after: e.retryDelay()}
	}
}

// pending records a pending event: the keys and the request into the Secret at once, so that a
// restart resumes it, then the request into the status. A resumed request is announced again with
// less (no approval URI, at times no expiry or ID): only the fields it carries change. It returns
// the Secret's error, the status untouched then.
func (p *pass) pending(ctx context.Context, ev retycclient.EnrollEvent, dir, nsUID, clusterUID string) error {
	e, ri, st := p.e, p.ri, &p.st
	fingerprint := st.Fingerprint
	if ev.Fingerprint != "" {
		fingerprint = ev.Fingerprint
	}
	if err := e.saveSecret(ctx, ri, dir, nsUID, clusterUID, fingerprint, ""); err != nil {
		return err
	}
	st.Fingerprint = fingerprint
	if ev.ApprovalURI != "" {
		st.ApprovalURI = ev.ApprovalURI
	}
	if ev.EnrollmentID != "" {
		st.EnrollmentID = ev.EnrollmentID
	}
	if !ev.ExpiresAt.IsZero() {
		st.ExpiresAt = &metav1.Time{Time: ev.ExpiresAt}
	}
	msg := "awaiting approval in Retyc (Organization > Machines): compare the fingerprint " + st.Fingerprint
	if st.ApprovalURI != "" {
		msg += ", approve at " + st.ApprovalURI
	}
	expires := ""
	if st.ExpiresAt != nil {
		expires = st.ExpiresAt.UTC().Format(time.RFC3339)
		msg += " before " + expires
	}
	p.setEnrolled(PhasePending, ReasonPending, msg)
	if err := p.writeStatus(ctx); err != nil {
		klog.Errorf("enroller: writing the status of %s/%s: %v", ri.Namespace, ri.Name, err)
	}
	e.Events.Eventf(p.obj, corev1.EventTypeNormal, PhasePending, "Enrollment %s %s", st.EnrollmentID, msg)
	klog.Infof("enroller: %s/%s: enrollment %s pending, fingerprint %s, expires %s", ri.Namespace, ri.Name,
		st.EnrollmentID, st.Fingerprint, expires)

	return nil
}

// takeKeptDir returns the kept working directory of uid, if any, which is no longer kept.
func (e *Enroller) takeKeptDir(uid types.UID) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	dir, ok := e.keptDirs[uid]
	delete(e.keptDirs, uid)
	if ok {
		if _, err := os.Stat(dir); err != nil {
			return "", false
		}
	}

	return dir, ok
}

// keepDir keeps the working directory of uid for its next pass.
func (e *Enroller) keepDir(uid types.UID, dir string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.keptDirs == nil {
		e.keptDirs = map[types.UID]string{}
	}
	e.keptDirs[uid] = dir
}

// dropKeptDir removes the kept working directory of uid, if any.
func (e *Enroller) dropKeptDir(uid types.UID) {
	if dir, ok := e.takeKeptDir(uid); ok {
		_ = os.RemoveAll(dir)
	}
}

// approve stores the approved identity (machine.json included) and records it.
func (p *pass) approve(ctx context.Context, dir, nsUID, clusterUID string, ev retycclient.EnrollEvent) result {
	e, ri, st := p.e, p.ri, &p.st
	raw, err := os.ReadFile(filepath.Join(dir, identity.MachineFile)) //nolint:gosec // G304: our temp dir
	if err != nil {
		st.Message = fmt.Sprintf("enrollment approved, but %s: %v", identity.MachineFile, err)

		return result{after: e.retryDelay()}
	}
	m := readMachine(raw)
	if m.MachineID == "" {
		m.MachineID, m.ClientID = ev.MachineID, ev.ClientID
	}
	if err := e.saveSecret(ctx, ri, dir, nsUID, clusterUID, st.Fingerprint, m.MachineID); err != nil {
		// The keys were saved at the pending event: the next pass enrolls with them again, which
		// the CLI answers from the approved request.
		st.Message = fmt.Sprintf("enrollment approved, but saving it: %v", err)

		return result{after: e.retryDelay()}
	}
	st.MachineID, st.ClientID = m.MachineID, m.ClientID
	p.setEnrolled(PhaseApproved, ReasonApproved, fmt.Sprintf("machine %s approved, in Secret %s/%s",
		m.MachineID, e.Namespace, st.SecretName))
	e.Events.Eventf(p.obj, corev1.EventTypeNormal, PhaseApproved, "Machine %s approved, in Secret %s/%s",
		m.MachineID, e.Namespace, st.SecretName)
	klog.Infof("enroller: %s/%s: enrollment approved, machine_id=%s, Secret %s/%s", ri.Namespace, ri.Name,
		m.MachineID, e.Namespace, st.SecretName)

	return result{again: true} // the auth status check
}

// saveSecret creates or updates ri's Secret from the files in dir, plus its k8s.json.
func (e *Enroller) saveSecret(ctx context.Context, ri *RetycIdentity, dir, nsUID, clusterUID, fingerprint,
	machineID string) error {
	files := map[string][]byte{}
	for _, name := range identity.FileNames {
		if name == identity.K8sFile {
			continue
		}
		v, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: our temp dir
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", name, err)
		}
		files[name] = v
	}
	k8s, err := e.k8sInfo(ri.Namespace, nsUID, clusterUID, fingerprint)
	if err != nil {
		return err
	}
	files[identity.K8sFile] = k8s
	want := e.secretFor(ri, nsUID, files)
	if machineID != "" {
		want.Annotations = map[string]string{AnnotationMachineID: machineID}
	}
	secrets := e.Kube.CoreV1().Secrets(e.Namespace)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := secrets.Get(ctx, want.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = secrets.Create(ctx, want, metav1.CreateOptions{})

			return err
		}
		if err != nil {
			return err
		}
		if !managed(cur) || cur.Labels[LabelIdentityUID] != string(ri.UID) {
			return fmt.Errorf("the Secret %s/%s belongs to another identity", cur.Namespace, cur.Name)
		}
		cur.Data = want.Data
		for k, v := range want.Labels {
			cur.Labels[k] = v
		}
		if machineID != "" {
			if cur.Annotations == nil {
				cur.Annotations = map[string]string{}
			}
			cur.Annotations[AnnotationMachineID] = machineID
		}
		_, err = secrets.Update(ctx, cur, metav1.UpdateOptions{})

		return err
	})
}

// machineName is the display name of ri's machine: its spec's, or "<cluster name>/<namespace>",
// cut to Retyc's bound.
func (e *Enroller) machineName(ri *RetycIdentity) string {
	name := ri.Spec.DisplayName
	if name == "" {
		cluster := e.ClusterName
		if cluster == "" {
			cluster = DefaultClusterName
		}
		name = cluster + "/" + ri.Namespace
	}
	if r := []rune(name); len(r) > maxNameLength {
		name = string(r[:maxNameLength])
	}

	return name
}

// setEnrolled sets the phase, the message and the Enrolled condition. Approved drops the request's
// approval URI and expiry (its ID stays, for the audit).
func (p *pass) setEnrolled(phase, reason, msg string) {
	p.st.Phase, p.st.Message = phase, msg
	status := metav1.ConditionFalse
	if phase == PhaseApproved {
		status = metav1.ConditionTrue
		p.st.ApprovalURI, p.st.ExpiresAt = "", nil
	}
	meta.SetStatusCondition(&p.st.Conditions, metav1.Condition{
		Type: ConditionEnrolled, Status: status, Reason: reason, Message: msg, ObservedGeneration: p.ri.Generation,
	})
}

func enrolledReason(st *Status) string {
	if c := meta.FindStatusCondition(st.Conditions, ConditionEnrolled); c != nil {
		return c.Reason
	}

	return ""
}

// terminal says whether the enrollment stopped for good: denied, expired too often or refused (until
// a retry, or a new RetycIdentity).
func terminal(st *Status) bool {
	switch enrolledReason(st) {
	case ReasonDenied, ReasonExpired, ReasonConfigRefused:
		return true
	}

	return false
}

// writeStatus stores the pass's status, unless unchanged.
func (p *pass) writeStatus(ctx context.Context) error {
	res := p.e.resource(p.ri.Namespace)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj, err := res.Get(ctx, p.ri.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		cur, err := fromUnstructured(obj)
		if err != nil {
			return err
		}
		if cur.UID != p.ri.UID || equality.Semantic.DeepEqual(cur.Status, p.st) {
			return nil
		}
		m, err := statusMap(&p.st)
		if err != nil {
			return err
		}
		obj.Object["status"] = m
		_, err = res.UpdateStatus(ctx, obj, metav1.UpdateOptions{})

		return err
	})
}

// updateFinalizers adds or removes the enroller's finalizer.
func (p *pass) updateFinalizers(ctx context.Context, add bool) error {
	res := p.e.resource(p.ri.Namespace)

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		obj, err := res.Get(ctx, p.ri.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if obj.GetUID() != p.ri.UID {
			return nil
		}
		fins := slices.DeleteFunc(obj.GetFinalizers(), func(f string) bool { return f == Finalizer })
		if add {
			fins = append(fins, Finalizer)
		}
		obj.SetFinalizers(fins)
		_, err = res.Update(ctx, obj, metav1.UpdateOptions{})

		return err
	})
}

// finalize deals with the Secret of a RetycIdentity being deleted: deleted when no PV uses it,
// otherwise kept and marked orphan for the sweep. The machine stays in Retyc: the administrator
// disables or deletes it.
func (p *pass) finalize(ctx context.Context) result {
	e, ri := p.e, p.ri
	if !slices.Contains(ri.Finalizers, Finalizer) {
		return result{gone: true}
	}
	name := e.SecretName(ri.Namespace)
	secrets := e.Kube.CoreV1().Secrets(e.Namespace)
	s, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		klog.Errorf("enroller: %s/%s: reading Secret %s: %v", ri.Namespace, ri.Name, name, err)

		return result{after: e.retryDelay()}
	}
	if err == nil && managed(s) && s.Labels[LabelIdentityUID] == string(ri.UID) {
		machine := s.Annotations[AnnotationMachineID]
		used, err := e.referenced(ctx, name)
		if err != nil {
			klog.Errorf("enroller: %s/%s: %v", ri.Namespace, ri.Name, err)

			return result{after: e.retryDelay()}
		}
		if used {
			s.Labels[LabelOrphan] = "true"
			if _, err := secrets.Update(ctx, s, metav1.UpdateOptions{}); err != nil {
				klog.Errorf("enroller: %s/%s: marking Secret %s orphan: %v", ri.Namespace, ri.Name, name, err)

				return result{after: e.retryDelay()}
			}
			klog.Infof("enroller: %s/%s deleted: Secret %s/%s kept while PVs use it", ri.Namespace, ri.Name,
				e.Namespace, name)
		} else {
			uid := s.UID
			err := secrets.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
			if err != nil && !apierrors.IsNotFound(err) {
				klog.Errorf("enroller: %s/%s: deleting Secret %s: %v", ri.Namespace, ri.Name, name, err)

				return result{after: e.retryDelay()}
			}
			klog.Infof("enroller: %s/%s deleted: Secret %s/%s deleted", ri.Namespace, ri.Name, e.Namespace, name)
		}
		if machine != "" {
			e.Events.Eventf(p.obj, corev1.EventTypeNormal, "MachineKept", "Machine %s stays in Retyc: disable or "+
				"delete it in Organization > Machines", machine)
			klog.Infof("enroller: %s/%s deleted, machine_id=%s stays in Retyc: the administrator disables or "+
				"deletes it", ri.Namespace, ri.Name, machine)
		}
	}
	if err := p.updateFinalizers(ctx, false); err != nil && !apierrors.IsNotFound(err) {
		klog.Errorf("enroller: %s/%s: removing the finalizer: %v", ri.Namespace, ri.Name, err)

		return result{after: e.retryDelay()}
	}
	if ri.Status.Phase == PhasePending && ri.Status.EnrollmentID != "" {
		// Its request may still await approval in Retyc, with keys now gone.
		e.Events.Eventf(p.obj, corev1.EventTypeWarning, ReasonEnrollmentAbandoned, "Enrollment request %s was "+
			"pending: deny it in Retyc (Organization > Machines, or `retyc machine deny %s`)",
			ri.Status.EnrollmentID, ri.Status.EnrollmentID)
		klog.Warningf("enroller: %s/%s deleted while enrollment %s was pending: deny it in Retyc (retyc machine "+
			"deny %s)", ri.Namespace, ri.Name, ri.Status.EnrollmentID, ri.Status.EnrollmentID)
	}
	e.dropKeptDir(ri.UID)
	e.mu.Lock()
	delete(e.lastAuth, ri.UID)
	e.mu.Unlock()

	return result{gone: true}
}

// Sweep deletes the orphan Secrets no PV uses any more.
func (e *Enroller) Sweep(ctx context.Context) {
	secrets := e.Kube.CoreV1().Secrets(e.Namespace)
	list, err := secrets.List(ctx, metav1.ListOptions{LabelSelector: LabelOrphan + "=true"})
	if err != nil {
		klog.Errorf("enroller: listing orphan Secrets: %v", err)

		return
	}
	for i := range list.Items {
		s := &list.Items[i]
		if !managed(s) {
			continue
		}
		used, err := e.referenced(ctx, s.Name)
		if err != nil {
			klog.Errorf("enroller: %v", err)

			return
		}
		if used {
			continue
		}
		uid := s.UID
		err = secrets.Delete(ctx, s.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			klog.Errorf("enroller: deleting orphan Secret %s: %v", s.Name, err)

			continue
		}
		klog.Infof("enroller: orphan Secret %s/%s deleted, no PV uses it; machine_id=%q stays in Retyc",
			s.Namespace, s.Name, s.Annotations[AnnotationMachineID])
	}
}
