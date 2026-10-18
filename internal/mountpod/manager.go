// Package mountpod manages the retycfs mount pods: one pod per (volume, node), in the driver's
// namespace, running `retyc fs mount` directly on kubelet's staging path, whose parent directory
// it mounts with Bidirectional propagation: the FUSE mount is the staging mount, seen by kubelet and
// by the node plugin, and the only mount of the volume on the host besides the pods' bind mounts.
// The node plugin creates them at stage, deletes them at unstage and recreates the ones that died;
// the pods, not the node plugin, hold the FUSE mounts, so a node plugin restart cuts none.
package mountpod

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/retyc/retycfs-k8s-csi/internal/identity"
)

// FinalizerInUse keeps a mount pod object after its container stopped, until the node plugin has
// read how it ended (its exit code): set at creation, removed by Delete or Ensure once the pod is
// finished. It does not stop a drain or an eviction from killing the pod.
const FinalizerInUse = driverName + "/in-use"

// DefaultMetricsPort is the port of the CLI's /healthz, /readyz and /metrics in the mount pods.
const DefaultMetricsPort = 9567

// terminationMargin is added to the drain timeout for the pod's grace period: up to 5 s of unmount
// retries and 5 s of slack before kubelet sends SIGKILL.
const terminationMargin = 10 * time.Second

// stuckAfter is how long a pod may stay terminating past its deletion timestamp (which already
// includes the grace period) before it is taken as finished: its kubelet is gone or hung, and no
// status will ever come.
const stuckAfter = time.Minute

// Defaults applied by New to the zero fields of Config.
const (
	defaultKubeletDir        = "/var/lib/kubelet"
	defaultSpoolRoot         = "/var/lib/retycfs-csi/spool"
	defaultPriorityClass     = "system-node-critical"
	defaultDrainTimeout      = 5 * time.Minute
	defaultRestartBackoff    = 5 * time.Second
	defaultMaxRestartBackoff = 5 * time.Minute
	defaultStableAfter       = time.Minute
	// defaultMaxFatalExits gives up at the first 77 or 78: the CLI exits 77 only once the machine is
	// refused for good (at once by the token endpoint, after a minute of 401 from the API) and 78 on
	// a configuration no retry fixes: a new pod with the same identity would only fail again. A 77
	// is not final though (see GiveUpRetryAfter).
	defaultMaxFatalExits = 1
	readyTimeout         = 2 * time.Second
)

// DefaultResources are the mount container's resources when Config.Resources is empty: Burstable,
// with a memory limit and no CPU limit.
func DefaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
	}
}

// GiveUpRetryAfter is how long a give-up on a 77 lasts, from the exit that caused it: the admin may
// enable the machine again with the same keys, so the volume is tried once more then, and gives up
// for as long again if refused still. Kubelet's retries do not extend it. A 78 never expires.
const GiveUpRetryAfter = 15 * time.Minute

// ErrGaveUp is returned by Ensure once a volume's pods exited MaxFatalExits times in a row with a
// code saying the credentials are unusable: only new credentials (another config hash), or the
// exited pod deleted by hand (a machine enabled again keeps its keys, so its hash), start over.
var ErrGaveUp = errors.New("mount pod gave up")

// ErrInvalidSpec is returned by Ensure for a Spec no pod can be built from: retrying is no use.
var ErrInvalidSpec = errors.New("mount pod spec")

// Config is the node-wide configuration of the mount pods (chart values).
type Config struct {
	// Namespace is the driver's namespace, where the pods and their Secrets live.
	Namespace string
	// Image is the retyc-cli image; CLIVersion is recorded on the pods, the image tag by default.
	Image           string
	CLIVersion      string
	ImagePullPolicy corev1.PullPolicy
	// ImagePullSecrets, ExtraEnv and HostAliases are copied as is into every pod (ExtraEnv after
	// the driver's variables, so it can override RETYC_* settings such as RETYC_API_BASE_URL).
	ImagePullSecrets []corev1.LocalObjectReference
	ExtraEnv         []corev1.EnvVar
	HostAliases      []corev1.HostAlias
	HostPID          bool
	// Resources are the default resources of the mount container (DefaultResources when empty);
	// Spec.Resources overrides them one request or limit at a time.
	Resources corev1.ResourceRequirements
	// PriorityClassName defaults to system-node-critical.
	PriorityClassName string
	// KubeletDir defaults to /var/lib/kubelet. The staging paths must be under <KubeletDir>/plugins,
	// and the pods' target paths are under <KubeletDir>/pods.
	KubeletDir string
	// SpoolRoot defaults to /var/lib/retycfs-csi/spool; each volume spools in <SpoolRoot>/<volume>.
	SpoolRoot string
	// DrainTimeout is `--drain-timeout`, 5 min by default; the pods' grace period is 10 s more.
	DrainTimeout time.Duration
	// MetricsPort defaults to DefaultMetricsPort. It applies to the pods created from now on: Ready
	// reads the port of each pod from its spec.
	MetricsPort int

	// Restart policy: the delay starts at RestartBackoff (5 s) and
	// doubles with every consecutive failure up to MaxRestartBackoff (5 min); a pod that ran for
	// StableAfter (1 min) restarts after RestartBackoff again; MaxFatalExits (1) consecutive 77/78
	// exits give up.
	RestartBackoff    time.Duration
	MaxRestartBackoff time.Duration
	StableAfter       time.Duration
	MaxFatalExits     int
}

// Manager creates, inspects and deletes the mount pods of one node.
//
// The caller MUST serialize every call (Ensure, Delete, RemoveFinalizer) for a given volume with a
// per-volume lock held across the whole call, such as the node plugin's lock on the volume. The
// Manager only protects its own memory: two concurrent Ensure for one volume would both run Prepare
// (clearing the spool of a pod being created) and both create a Secret. Calls for different volumes
// may run concurrently.
type Manager struct {
	Kube kubernetes.Interface
	// Prepare runs before every pod creation, once the pod is known to be absent: it must unmount a
	// dead mount left on Spec.StagingPath by a former pod, create that directory (the CLI mounts on
	// an existing directory) and an empty SpoolHostPath (mode 0700). Nil skips it.
	Prepare func(ctx context.Context, spec *Spec) error
	// HTTP probes /readyz; nil means a client with a 2 s timeout.
	HTTP *http.Client
	// Now is the clock of the backoff and of the stuck-pod detection; nil means time.Now.
	Now func() time.Time

	cfg      Config
	mu       sync.Mutex
	backoffs map[string]*backoffEntry // by volume ID
}

// New returns a Manager with the defaults applied to cfg.
func New(kube kubernetes.Interface, cfg Config) *Manager {
	if cfg.CLIVersion == "" {
		cfg.CLIVersion = imageTag(cfg.Image)
	}
	if len(cfg.Resources.Requests) == 0 && len(cfg.Resources.Limits) == 0 {
		cfg.Resources = DefaultResources()
	}
	setDefault(&cfg.PriorityClassName, defaultPriorityClass)
	setDefault(&cfg.KubeletDir, defaultKubeletDir)
	setDefault(&cfg.SpoolRoot, defaultSpoolRoot)
	setDefault(&cfg.DrainTimeout, defaultDrainTimeout)
	setDefault(&cfg.MetricsPort, DefaultMetricsPort)
	setDefault(&cfg.RestartBackoff, defaultRestartBackoff)
	setDefault(&cfg.MaxRestartBackoff, defaultMaxRestartBackoff)
	setDefault(&cfg.StableAfter, defaultStableAfter)
	setDefault(&cfg.MaxFatalExits, defaultMaxFatalExits)

	return &Manager{Kube: kube, cfg: cfg, backoffs: map[string]*backoffEntry{}}
}

func setDefault[T comparable](v *T, def T) {
	var zero T
	if *v == zero {
		*v = def
	}
}

// imageTag is the tag of an image reference, "" without one (a digest is not a version).
func imageTag(image string) string {
	name, _, _ := strings.Cut(image, "@")
	if i := strings.LastIndex(name, ":"); i > strings.LastIndex(name, "/") {
		return name[i+1:]
	}

	return ""
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}

	return time.Now()
}

// Config returns the configuration with its defaults.
func (m *Manager) Config() Config { return m.cfg }

// Clock returns the time of the Manager's clock (Now), for the callers that compare it with the
// pods' timestamps.
func (m *Manager) Clock() time.Time { return m.now() }

// checkStagingPath accepts a clean absolute path at least two levels under <KubeletDir>/plugins: the
// pod mounts its parent directory from the host, which must be neither kubelet's plugin directory
// nor anything outside it.
func (m *Manager) checkStagingPath(p string) error {
	plugins := path.Join(m.cfg.KubeletDir, "plugins")
	if !path.IsAbs(p) || path.Clean(p) != p || !strings.HasPrefix(path.Dir(p), plugins+"/") {
		return fmt.Errorf("%w: staging path %q is not a clean path under %s/<dir>/", ErrInvalidSpec, p, plugins)
	}

	return nil
}

// ConfigHash is the hash recorded on the pod built from spec (AnnotationConfigHash).
func (m *Manager) ConfigHash(spec *Spec) string {
	return configHash(spec, m.resources(spec), m.cfg.Image)
}

// Phase sums up a mount pod.
type Phase string

const (
	// PhaseAbsent: no pod. From Ensure, with Backoff.NextRestart set: waiting before a new one.
	PhaseAbsent Phase = "Absent"
	// PhasePending: created, the container is not running yet (scheduling, image pull; see
	// WaitingReason).
	PhasePending Phase = "Pending"
	// PhaseRunning: the container runs; Ready tells whether the volume is mounted.
	PhaseRunning Phase = "Running"
	// PhaseTerminating: deleted, the container has not exited yet (drain), or the object is about to go.
	PhaseTerminating Phase = "Terminating"
	// PhaseExited: the mount process is gone (see ExitCode). From Delete: the pod is released, the
	// unstage is done.
	PhaseExited Phase = "Exited"
)

// State is what a call found, or left, for a volume's mount pod.
type State struct {
	Name  string
	Phase Phase
	// Pod is the pod as last read, nil when absent.
	Pod *corev1.Pod
	// VolumeID, NodeName, PVCName and PVCNamespace are read from the pod's annotations.
	VolumeID, NodeName, PVCName, PVCNamespace string
	// ConfigHash, CLIVersion and StagingPath are the pod's annotations.
	ConfigHash, CLIVersion, StagingPath string
	// ConfigChanged is set by Ensure when the pod runs another config than the spec: the caller
	// recreates it (Delete, then Ensure) once the volume is no longer published.
	ConfigChanged bool
	// WaitingReason and WaitingMessage say why the container does not run yet (ImagePullBackOff,
	// CreateContainerConfigError...), "" otherwise.
	WaitingReason, WaitingMessage string
	// ExitCode is the container's exit code once it terminated, nil before or when it left none
	// (pod evicted before start, node lost, pod stuck terminating).
	ExitCode *int32
	// ExitReason is the termination reason ("Error", "OOMKilled"...), or the pod's when the
	// container left none ("Evicted", "NodeLost"...). ExitMessage is the termination message: the
	// end of the CLI's log when it failed (FallbackToLogsOnError), or the pod's status message.
	ExitReason, ExitMessage string
	// WasReady says whether the mount container passed its readiness probe at least once (see
	// wasReady): an exit 1 before that is a failure at start, with nothing written (see ExitLabel).
	WasReady bool
	// Backoff is the volume's restart state (Ensure only).
	Backoff Backoff
}

// Get returns the state of a volume's mount pod on a node.
func (m *Manager) Get(ctx context.Context, nodeName, volumeID string) (State, error) {
	name := Name(nodeName, volumeID)
	pod, err := m.Kube.CoreV1().Pods(m.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return State{Name: name, Phase: PhaseAbsent, VolumeID: volumeID, NodeName: nodeName}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("getting mount pod %s: %w", name, err)
	}

	return m.stateOf(pod), nil
}

// NodeSelector is the label selector of the mount pods of a node. A hashed label value could match
// another node: the pods' LabelNode annotation holds the exact name.
func NodeSelector(nodeName string) string {
	return labels.SelectorFromSet(labels.Set{LabelManagedBy: managedBy, LabelNode: labelValue(nodeName)}).String()
}

// List returns the states of the mount pods of a node, for the recovery loop.
func (m *Manager) List(ctx context.Context, nodeName string) ([]State, error) {
	pods, err := m.Kube.CoreV1().Pods(m.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: NodeSelector(nodeName)})
	if err != nil {
		return nil, fmt.Errorf("listing the mount pods of node %s: %w", nodeName, err)
	}
	states := make([]State, 0, len(pods.Items))
	for i := range pods.Items {
		// A hashed label value could match another node: the annotation holds the exact name.
		if st := m.stateOf(&pods.Items[i]); st.NodeName == nodeName {
			states = append(states, st)
		}
	}

	return states, nil
}

// Ensure makes sure the mount pod of spec exists, and is idempotent:
//   - a pod that runs or starts is left as is; ConfigChanged says whether it runs spec's config,
//     State.StagingPath where it mounts the volume (the caller compares it with spec's);
//   - an exited pod is counted in the backoff and kept (for `kubectl logs`) until its restart
//     time, then replaced; after MaxFatalExits exits with 77/78, ErrGaveUp;
//   - a pod being deleted is waited for (PhaseTerminating); once finished, it is handled as an
//     exited one: kept until its restart time, so that the recovery can still rebuild its Spec
//     (SpecOf) from it, then replaced;
//   - otherwise the Secret and the pod are created, after Prepare.
//
// The volume is usable once Ready succeeds on the returned pod. See Manager for the locking.
func (m *Manager) Ensure(ctx context.Context, spec *Spec) (State, error) {
	if err := spec.validate(); err != nil {
		return State{}, err
	}
	if err := m.checkStagingPath(spec.StagingPath); err != nil {
		return State{}, err
	}
	hash := m.ConfigHash(spec)
	name := Name(spec.NodeName, spec.VolumeID)

	pod, err := m.Kube.CoreV1().Pods(m.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return State{}, fmt.Errorf("getting mount pod %s: %w", name, err)
	default:
		st, created := m.ensureExisting(ctx, spec, hash, pod)
		if !created {
			return st.State, st.err
		}
	}

	b := m.backoffState(spec.VolumeID, hash)
	if b.GaveUp {
		return absent(spec, b), gaveUpError(name, b)
	}
	if m.now().Before(b.NextRestart) {
		return absent(spec, b), nil
	}

	return m.create(ctx, spec, hash)
}

type stateErr struct {
	State
	err error
}

// ensureExisting handles Ensure for a pod that exists. It returns create = true when the pod was
// released and a new one is due.
func (m *Manager) ensureExisting(ctx context.Context, spec *Spec, hash string, pod *corev1.Pod) (stateErr, bool) {
	st := m.stateOf(pod)
	st.ConfigChanged = st.ConfigHash != hash
	if m.finished(pod) {
		m.countExit(spec.VolumeID, hash, &st)
	}
	st.Backoff = m.backoffState(spec.VolumeID, hash)
	if st.Backoff.GaveUp && pod.DeletionTimestamp != nil && m.finished(pod) {
		klog.Infof("mountpod: %s (volume %s) gave up and was deleted by hand: trying again", st.Name, spec.VolumeID)
		m.startOver(spec.VolumeID, hash)
		st.Backoff = m.backoffState(spec.VolumeID, hash)
	}
	if st.Phase == PhaseTerminating && !m.finished(pod) {
		return stateErr{st, nil}, false
	}
	switch st.Phase {
	case PhaseTerminating, PhaseExited:
		if st.Backoff.GaveUp {
			return stateErr{st, gaveUpError(st.Name, st.Backoff)}, false
		}
		if m.now().Before(st.Backoff.NextRestart) {
			return stateErr{st, nil}, false
		}
		if err := m.release(ctx, pod); err != nil {
			return stateErr{st, err}, false
		}

		return stateErr{}, true
	default:
		return stateErr{st, m.ownSecret(ctx, pod)}, false
	}
}

func absent(spec *Spec, b Backoff) State {
	return State{
		Name: Name(spec.NodeName, spec.VolumeID), Phase: PhaseAbsent,
		VolumeID: spec.VolumeID, NodeName: spec.NodeName, PVCName: spec.PVCName, PVCNamespace: spec.PVCNamespace,
		StagingPath: spec.StagingPath, Backoff: b,
	}
}

// countExit records the exit of a pod in the backoff, when it ran the config being ensured: an exit
// with another config says nothing about the new one.
func (m *Manager) countExit(volumeID, hash string, st *State) {
	if st.ConfigHash != hash {
		return
	}
	code, ran := int32(-1), time.Duration(0)
	if st.ExitCode != nil {
		code = *st.ExitCode
	}
	if t := terminated(st.Pod); t != nil && !t.StartedAt.IsZero() {
		ran = t.FinishedAt.Sub(t.StartedAt.Time)
	}
	b := m.recordExit(volumeID, hash, st.StagingPath, st.Pod.UID, code, st.ExitMessage, ran)
	klog.Warningf("mountpod: %s (volume %s) exited with code %d (%s: %s); %d consecutive failures",
		st.Name, volumeID, code, st.ExitReason, ExitLabel(*st), b.ConsecutiveFailures)
}

func (m *Manager) create(ctx context.Context, spec *Spec, hash string) (State, error) {
	name := Name(spec.NodeName, spec.VolumeID)
	pods := m.Kube.CoreV1().Pods(m.cfg.Namespace)
	// Right before Prepare, which clears the spool: a pod that showed up since (a previous one still
	// on its way out) must not lose its files.
	if pod, err := pods.Get(ctx, name, metav1.GetOptions{}); err == nil {
		st := m.stateOf(pod)
		st.ConfigChanged = st.ConfigHash != hash

		return st, nil
	} else if !apierrors.IsNotFound(err) {
		return State{}, fmt.Errorf("getting mount pod %s: %w", name, err)
	}
	if m.Prepare != nil {
		if err := m.Prepare(ctx, spec); err != nil {
			return State{}, fmt.Errorf("preparing the host for mount pod %s: %w", name, err)
		}
	}
	// Secrets left by a creation that failed midway: nothing uses them.
	if err := m.deleteSecrets(ctx, spec.NodeName, spec.VolumeID, true); err != nil {
		return State{}, err
	}
	secret, err := m.Kube.CoreV1().Secrets(m.cfg.Namespace).Create(ctx, m.buildSecret(spec, hash), metav1.CreateOptions{})
	if err != nil {
		return State{}, fmt.Errorf("creating the secret of mount pod %s: %w", name, err)
	}
	pod, err := pods.Create(ctx, m.buildPod(spec, hash, secret.Name), metav1.CreateOptions{})
	if err != nil {
		m.deleteSecret(ctx, secret.Name)
		if apierrors.IsAlreadyExists(err) {
			// The previous pod is still on its way out: retry later.
			return State{Name: name, Phase: PhaseTerminating, VolumeID: spec.VolumeID, NodeName: spec.NodeName}, nil
		}

		return State{}, fmt.Errorf("creating mount pod %s: %w", name, err)
	}
	klog.Infof("mountpod: created %s for volume %s on node %s", name, spec.VolumeID, spec.NodeName)
	st := m.stateOf(pod)
	st.Backoff = m.backoffState(spec.VolumeID, hash)

	return st, m.ownSecret(ctx, pod)
}

// ownSecret makes the pod the owner of its Secret: the Secret then goes with the pod, whoever
// deletes it. Each pod has its own Secret (a generated name), so an owner is never moved.
func (m *Manager) ownSecret(ctx context.Context, pod *corev1.Pod) error {
	name := pod.Annotations[AnnotationSecret]
	if name == "" {
		return nil
	}
	secrets := m.Kube.CoreV1().Secrets(m.cfg.Namespace)
	secret, err := secrets.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("getting secret %s: %w", name, err)
	}
	if slices.ContainsFunc(secret.OwnerReferences, func(r metav1.OwnerReference) bool { return r.UID == pod.UID }) {
		return nil
	}
	owner := metav1.OwnerReference{APIVersion: "v1", Kind: "Pod", Name: pod.Name, UID: pod.UID}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"ownerReferences": []any{owner}}})
	if err != nil {
		return err
	}
	if _, err := secrets.Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("setting the owner of secret %s: %w", name, err)
	}

	return nil
}

// deleteSecrets deletes the Secrets of a volume's mount pods on a node: all of them, or only those
// without owner (onlyOrphans).
func (m *Manager) deleteSecrets(ctx context.Context, nodeName, volumeID string, onlyOrphans bool) error {
	selector := labels.SelectorFromSet(labels.Set{
		LabelManagedBy: managedBy, LabelNode: labelValue(nodeName), LabelVolume: labelValue(volumeID),
	})
	list, err := m.Kube.CoreV1().Secrets(m.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return fmt.Errorf("listing the secrets of volume %s: %w", volumeID, err)
	}
	for _, s := range list.Items {
		if s.Annotations[LabelNode] != nodeName || s.Annotations[LabelVolume] != volumeID ||
			onlyOrphans && len(s.OwnerReferences) > 0 {
			continue
		}
		m.deleteSecret(ctx, s.Name)
	}

	return nil
}

// deleteSecret deletes a Secret, logging a failure: the garbage collector or the next sweep retries.
func (m *Manager) deleteSecret(ctx context.Context, name string) {
	err := m.Kube.CoreV1().Secrets(m.cfg.Namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		klog.Warningf("mountpod: deleting secret %s: %v", name, err)
	}
}

// Delete stops a volume's mount pod and is meant to be called until it is done, which is when the
// returned Phase is PhaseExited (the pod is released: ExitCode says how it ended, 1 when writes
// were lost) or PhaseAbsent. Before that it is PhaseTerminating: SIGTERM was sent and the pod
// drains, for up to DrainTimeout. The Secret goes with the pod; the backoff is forgotten at the end,
// unless the volume gave up (see forget) and the finished pod was not deleted by someone else
// meanwhile (`kubectl delete pod`, the manual way out of a give-up: see startOver).
// See Manager for the locking.
func (m *Manager) Delete(ctx context.Context, nodeName, volumeID string) (State, error) {
	st, err := m.Get(ctx, nodeName, volumeID)
	if err != nil {
		return st, err
	}
	switch {
	case st.Phase == PhaseAbsent:
		m.forget(volumeID)

		return st, m.deleteSecrets(ctx, nodeName, volumeID, false)
	case m.finished(st.Pod):
		deleted := st.Pod.DeletionTimestamp != nil
		if err := m.release(ctx, st.Pod); err != nil {
			return st, err
		}
		if deleted {
			m.Forget(volumeID)
		} else {
			m.forget(volumeID)
		}
		st.Phase = PhaseExited
		klog.Infof("mountpod: %s (volume %s) released, exit code %s", st.Name, volumeID, fmtCode(st.ExitCode))

		return st, m.deleteSecrets(ctx, nodeName, volumeID, false)
	case st.Pod.DeletionTimestamp == nil:
		// The pod's own grace period: drain timeout + margin.
		err := m.Kube.CoreV1().Pods(m.cfg.Namespace).Delete(ctx, st.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &st.Pod.UID},
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return st, fmt.Errorf("deleting mount pod %s: %w", st.Name, err)
		}
		klog.Infof("mountpod: deleting %s (volume %s), draining for up to %s", st.Name, volumeID, m.cfg.DrainTimeout)
	}
	st.Phase = PhaseTerminating

	return st, nil
}

// release removes the finalizer of a finished pod and deletes it without grace: its mount process
// is gone, there is nothing to wait for (and a stuck pod has no kubelet left to wait for).
func (m *Manager) release(ctx context.Context, pod *corev1.Pod) error {
	if err := m.RemoveFinalizer(ctx, pod); err != nil {
		return err
	}
	grace := int64(0)
	err := m.Kube.CoreV1().Pods(m.cfg.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		GracePeriodSeconds: &grace, Preconditions: &metav1.Preconditions{UID: &pod.UID},
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting mount pod %s: %w", pod.Name, err)
	}

	return nil
}

// SpecOf rebuilds the Spec of an existing pod, with cache as the cache settings, so that the
// recovery can recreate a pod that died without the claim or the stage request at hand: the
// owner, options and resources come from AnnotationSpec, the credentials from the pod's Secret,
// which the pod keeps as long as it exists (its finalizer holds it once it exited).
func (m *Manager) SpecOf(ctx context.Context, st State, cache Cache) (*Spec, error) {
	if st.Pod == nil {
		return nil, fmt.Errorf("mount pod %s is gone: nothing to rebuild its spec from", st.Name)
	}
	if st.StagingPath == "" {
		return nil, fmt.Errorf("mount pod %s has no %s annotation: nothing to mount on", st.Name, AnnotationStagingPath)
	}
	var stored storedSpec
	if err := json.Unmarshal([]byte(st.Pod.Annotations[AnnotationSpec]), &stored); err != nil {
		return nil, fmt.Errorf("mount pod %s: annotation %s: %w", st.Name, AnnotationSpec, err)
	}
	name := st.Pod.Annotations[AnnotationSecret]
	secret, err := m.Kube.CoreV1().Secrets(m.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("mount pod %s: reading its secret %q: %w", st.Name, name, err)
	}

	return &Spec{
		VolumeID: st.VolumeID, NodeName: st.NodeName, PVCName: st.PVCName, PVCNamespace: st.PVCNamespace,
		StagingPath: st.StagingPath,
		Credentials: credentialsOf(secret),
		Cache:       cache, UID: stored.UID, GID: stored.GID, Options: stored.Options, Resources: stored.Resources,
	}, nil
}

// credentialsOf reads the identity files back from a pod's Secret (see buildSecret).
func credentialsOf(secret *corev1.Secret) identity.Credentials {
	if c := identity.FromData(secret.Data); c != nil {
		return *c
	}

	return identity.Credentials{}
}

// RemoveFinalizer removes FinalizerInUse from a pod. The JSON patch tests the current list, so a
// concurrent change fails the call instead of being overwritten.
func (m *Manager) RemoveFinalizer(ctx context.Context, pod *corev1.Pod) error {
	if !slices.Contains(pod.Finalizers, FinalizerInUse) {
		return nil
	}
	kept := slices.DeleteFunc(slices.Clone(pod.Finalizers), func(f string) bool { return f == FinalizerInUse })
	patch, err := json.Marshal([]map[string]any{
		{"op": "test", "path": "/metadata/finalizers", "value": pod.Finalizers},
		{"op": "replace", "path": "/metadata/finalizers", "value": kept},
	})
	if err != nil {
		return err
	}
	_, err = m.Kube.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, types.JSONPatchType, patch, metav1.PatchOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("removing finalizer %s from mount pod %s: %w", FinalizerInUse, pod.Name, err)
	}
	pod.Finalizers = kept

	return nil
}

// Ready GETs the pod's /readyz: nil once the CLI has mounted the volume, an error while it starts,
// after it began to stop, or when it does not answer. A success resets the volume's backoff.
func (m *Manager) Ready(ctx context.Context, pod *corev1.Pod) error {
	if pod == nil || pod.Status.PodIP == "" {
		return errors.New("mount pod has no IP yet")
	}
	port := metricsPort(pod)
	if port == 0 {
		return fmt.Errorf("mount pod %s declares no %q port", pod.Name, metricsPortName)
	}
	url := "http://" + net.JoinHostPort(pod.Status.PodIP, strconv.Itoa(int(port))) + "/readyz"
	client := m.HTTP
	if client == nil {
		client = &http.Client{Timeout: readyTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("mount pod %s not answering on %s: %w", pod.Name, url, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mount pod %s not ready: HTTP %d on %s (mounting or stopping)", pod.Name, resp.StatusCode, url)
	}
	if v := pod.Annotations[LabelVolume]; v != "" {
		m.resetBackoff(v)
	}

	return nil
}

// metricsPort is the port the pod's mount container declares for its probes, 0 when none.
func metricsPort(pod *corev1.Pod) int32 {
	for _, c := range pod.Spec.Containers {
		if c.Name != containerName {
			continue
		}
		for _, p := range c.Ports {
			if p.Name == metricsPortName {
				return p.ContainerPort
			}
		}
	}

	return 0
}

// stateOf reads a pod's state, without the backoff.
func (m *Manager) stateOf(pod *corev1.Pod) State {
	st := State{
		Name: pod.Name, Pod: pod,
		VolumeID: pod.Annotations[LabelVolume], NodeName: pod.Annotations[LabelNode],
		PVCName: pod.Annotations[LabelPVC], PVCNamespace: pod.Annotations[LabelNamespace],
		ConfigHash: pod.Annotations[AnnotationConfigHash], CLIVersion: pod.Annotations[AnnotationCLIVersion],
		StagingPath: pod.Annotations[AnnotationStagingPath],
		ExitReason:  pod.Status.Reason, ExitMessage: pod.Status.Message,
	}
	running := false
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != containerName {
			continue
		}
		switch {
		case cs.State.Terminated != nil:
			t := cs.State.Terminated
			code := t.ExitCode
			st.ExitCode = &code
			if t.Reason != "" || t.Message != "" {
				st.ExitReason, st.ExitMessage = t.Reason, t.Message
			}
		case cs.State.Waiting != nil:
			st.WaitingReason, st.WaitingMessage = cs.State.Waiting.Reason, cs.State.Waiting.Message
		case cs.State.Running != nil:
			running = true
		}
	}
	st.WasReady = wasReady(pod)
	switch {
	case pod.DeletionTimestamp != nil:
		st.Phase = PhaseTerminating
	case m.finished(pod):
		st.Phase = PhaseExited
	case running:
		st.Phase = PhaseRunning
	default:
		st.Phase = PhasePending
	}

	return st
}

// wasReady says whether the pod was ready at some point, from its Ready condition: true now, or false
// since a transition after the mount container started. A pod that never became ready keeps the
// transition to false of its creation, before the container started.
func wasReady(pod *corev1.Pod) bool {
	var started time.Time
	for _, cs := range pod.Status.ContainerStatuses {
		switch {
		case cs.Name != containerName:
		case cs.State.Terminated != nil:
			started = cs.State.Terminated.StartedAt.Time
		case cs.State.Running != nil:
			started = cs.State.Running.StartedAt.Time
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue ||
				!started.IsZero() && c.LastTransitionTime.After(started)
		}
	}

	return false
}

// terminated is the mount container's termination state, nil while it has none.
func terminated(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	if pod == nil {
		return nil
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == containerName {
			return cs.State.Terminated
		}
	}

	return nil
}

// finished says whether the mount process is gone for good (with restartPolicy Never, it never
// comes back): the container terminated, the pod failed without one (evicted before start), its
// node was lost, or it stayed terminating stuckAfter past its deletion timestamp.
func (m *Manager) finished(pod *corev1.Pod) bool {
	switch {
	case terminated(pod) != nil, pod.Status.Phase == corev1.PodFailed, pod.Status.Phase == corev1.PodSucceeded,
		pod.Status.Reason == "NodeLost":
		return true
	case pod.DeletionTimestamp != nil:
		return m.now().After(pod.DeletionTimestamp.Add(stuckAfter))
	}

	return false
}

func gaveUpError(name string, b Backoff) error {
	if !b.GiveUpUntil.IsZero() {
		return fmt.Errorf("%w: %s exited %d times in a row, last with code %d (%s); once the machine is enabled "+
			"again (or with new credentials), the mount is tried again automatically within %s, at %s at the latest",
			ErrGaveUp, name, b.FatalExits, b.LastExitCode, ExitMeaning(b.LastExitCode), GiveUpRetryAfter,
			b.GiveUpUntil.UTC().Format(time.RFC3339))
	}

	return fmt.Errorf("%w: %s exited %d times in a row, last with code %d (%s); fix the configuration (a changed "+
		"one is used at the next stage), then delete the pod using the volume or unstage it to try again",
		ErrGaveUp, name, b.FatalExits, b.LastExitCode, ExitMeaning(b.LastExitCode))
}

func fmtCode(code *int32) string {
	if code == nil {
		return "unknown"
	}

	return strconv.Itoa(int(*code))
}
