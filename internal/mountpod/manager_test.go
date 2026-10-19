package mountpod

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/retyc/retycfs-k8s-csi/internal/identity"
)

const testNS = "retycfs-csi"

var podsGVR = corev1.SchemeGroupVersion.WithResource("pods")

const testStaging = "/var/lib/kubelet/plugins/kubernetes.io/csi/csi.retycfs.com/abc/globalmount"

// env is a Manager on a fake API server that assigns UIDs and, like the real one, only marks a
// pod with finalizers as deleted, at now + its grace period.
type env struct {
	t        *testing.T
	kube     *fake.Clientset
	m        *Manager
	now      time.Time
	prepared int
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, kube: fake.NewClientset(), now: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	uids := 0
	e.kube.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		uids++
		a.(k8stesting.CreateAction).GetObject().(*corev1.Pod).UID = types.UID(fmt.Sprintf("uid-%d", uids))

		return false, nil, nil
	})
	e.kube.PrependReactor("delete", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
		obj, err := e.kube.Tracker().Get(podsGVR, a.GetNamespace(), a.(k8stesting.DeleteAction).GetName())
		if err != nil || len(obj.(*corev1.Pod).Finalizers) == 0 {
			return false, nil, nil
		}
		pod := obj.(*corev1.Pod)
		if pod.DeletionTimestamp == nil {
			grace := time.Duration(*pod.Spec.TerminationGracePeriodSeconds) * time.Second
			pod.DeletionTimestamp = &metav1.Time{Time: e.now.Add(grace)}
		}

		return true, nil, e.kube.Tracker().Update(podsGVR, pod, a.GetNamespace())
	})
	e.m = New(e.kube, Config{Namespace: testNS, Image: "ghcr.io/retyc/retyc-cli:1.2.3"})
	e.m.Now = func() time.Time { return e.now }
	e.m.Prepare = func(context.Context, *Spec) error {
		e.prepared++

		return nil
	}

	return e
}

func testSpec() *Spec {
	return &Spec{
		VolumeID: "dr-1", NodeName: "node-a", PVCName: "data", PVCNamespace: "team",
		StagingPath: testStaging,
		Credentials: testCredentials("key"),
		UID:         1000, GID: 2000,
		Options: Options{CacheTTL: 30 * time.Second, ReadOnly: true, LocalFile: []string{"*.tmp"}},
		Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("1Gi"), corev1.ResourceCPU: resource.MustParse("1"),
		}},
	}
}

// testCredentials is a complete machine identity whose signing key is key.
func testCredentials(key string) identity.Credentials {
	return identity.Credentials{Files: map[string][]byte{
		identity.ClientKeyFile: []byte(key), identity.AgeKeyFile: []byte("age"),
		identity.MachineFile: []byte(`{"version":1}`), identity.K8sFile: []byte(`{"mode":"cluster"}`),
	}}
}

func (e *env) ensure(spec *Spec) State {
	e.t.Helper()
	st, err := e.m.Ensure(context.Background(), spec)
	if err != nil {
		e.t.Fatalf("Ensure: %v", err)
	}

	return st
}

func (e *env) pod(name string) *corev1.Pod {
	e.t.Helper()
	pod, err := e.kube.CoreV1().Pods(testNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		e.t.Fatalf("get pod: %v", err)
	}

	return pod
}

func (e *env) gone(name string) bool {
	_, err := e.kube.CoreV1().Pods(testNS).Get(context.Background(), name, metav1.GetOptions{})

	return err != nil
}

func (e *env) secret(name string) *corev1.Secret {
	e.t.Helper()
	s, err := e.kube.CoreV1().Secrets(testNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		e.t.Fatalf("get secret: %v", err)
	}

	return s
}

func (e *env) secretCount() int {
	e.t.Helper()
	l, err := e.kube.CoreV1().Secrets(testNS).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		e.t.Fatal(err)
	}

	return len(l.Items)
}

// setStatus sets the pod's status, as kubelet would.
func (e *env) setStatus(name string, status corev1.PodStatus) {
	e.t.Helper()
	pod := e.pod(name)
	pod.Status = status
	if _, err := e.kube.CoreV1().Pods(testNS).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) setContainer(name string, state corev1.ContainerState, phase corev1.PodPhase) {
	e.t.Helper()
	e.setStatus(name, corev1.PodStatus{
		Phase: phase, PodIP: "127.0.0.1",
		ContainerStatuses: []corev1.ContainerStatus{{Name: containerName, State: state}},
	})
}

func (e *env) running(name string) {
	e.t.Helper()
	e.setContainer(name, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, corev1.PodRunning)
}

// exit makes the mount container exit with code after having run for ran.
func (e *env) exit(name string, code int32, ran time.Duration) {
	e.t.Helper()
	phase := corev1.PodFailed
	if code == 0 {
		phase = corev1.PodSucceeded
	}
	e.setContainer(name, corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: code, Reason: "Error", Message: "mount: login lost",
		StartedAt: metav1.Time{Time: e.now.Add(-ran)}, FinishedAt: metav1.Time{Time: e.now},
	}}, phase)
}

func TestNameAndLabels(t *testing.T) {
	if a, b := Name("n", "v"), Name("n", "v"); a != b || a == Name("n", "w") || Name("n1", "v") == Name("n", "1v") {
		t.Fatal("names must be stable and distinct")
	}
	if got := Name("node-a", "dr-1"); !strings.HasPrefix(got, "retycfs-mount-") || len(got) != 30 {
		t.Fatalf("Name = %q", got)
	}
	for _, v := range []string{"", "node-a", strings.Repeat("n.", 100) + "x", "_a b/c-", strings.Repeat("-", 70)} {
		got := labelValue(v)
		if errs := validation.IsValidLabelValue(got); len(errs) > 0 {
			t.Errorf("labelValue(%q) = %q: %v", v, got, errs)
		}
	}
	if labelValue("node-a") != "node-a" {
		t.Error("a valid value must be kept")
	}
	long := strings.Repeat("a", 100)
	if labelValue(long) == labelValue(long+"b") {
		t.Error("hashed values must differ")
	}
}

func TestImageTag(t *testing.T) {
	for image, want := range map[string]string{
		"ghcr.io/retyc/retyc-cli:1.2.3": "1.2.3", "localhost:5000/retyc-cli": "", "retyc-cli:retycfs": "retycfs",
		"retyc-cli:v1@sha256:abc": "v1",
	} {
		if got := imageTag(image); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", image, got, want)
		}
	}
}

func TestEnsureCreatesPodAndSecret(t *testing.T) {
	e := newEnv(t)
	e.m.cfg.HostPID = true
	e.m.cfg.ExtraEnv = []corev1.EnvVar{{Name: "RETYC_INSECURE", Value: "true"}}
	e.m.cfg.HostAliases = []corev1.HostAlias{{IP: "10.0.0.1", Hostnames: []string{"api.example"}}}
	spec := testSpec()
	spec.Cache = Cache{RedisURL: "redis://:pw@redis:6379/0", HighConfidentiality: true}

	st := e.ensure(spec)
	if st.Phase != PhasePending || st.ConfigChanged || e.prepared != 1 {
		t.Fatalf("phase = %s, changed = %v, prepared = %d", st.Phase, st.ConfigChanged, e.prepared)
	}
	pod := e.pod(Name("node-a", "dr-1"))
	ps, c := pod.Spec, pod.Spec.Containers[0]

	if ps.NodeName != "node-a" || ps.RestartPolicy != corev1.RestartPolicyNever || !ps.HostPID ||
		ps.PriorityClassName != "system-node-critical" || *ps.TerminationGracePeriodSeconds != 310 ||
		len(ps.Tolerations) != 1 || ps.Tolerations[0].Operator != corev1.TolerationOpExists ||
		*ps.EnableServiceLinks || *ps.AutomountServiceAccountToken || len(ps.HostAliases) != 1 {
		t.Errorf("pod spec = %+v", ps)
	}
	if !slices.Contains(pod.Finalizers, FinalizerInUse) {
		t.Errorf("finalizers = %v", pod.Finalizers)
	}
	a := pod.Annotations
	if pod.Labels[LabelVolume] != "dr-1" || pod.Labels[LabelNode] != "node-a" || pod.Labels[LabelPVC] != "data" ||
		pod.Labels[LabelNamespace] != "team" || a[AnnotationCLIVersion] != "1.2.3" ||
		a[AnnotationConfigHash] != e.m.ConfigHash(spec) || a["karpenter.sh/do-not-disrupt"] != "true" ||
		a["cluster-autoscaler.kubernetes.io/safe-to-evict"] != "false" {
		t.Errorf("labels = %v, annotations = %v", pod.Labels, a)
	}
	if c.Image != "ghcr.io/retyc/retyc-cli:1.2.3" || !*c.SecurityContext.Privileged || *c.SecurityContext.RunAsUser != 0 ||
		c.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Errorf("container = %+v", c)
	}
	if p := c.ReadinessProbe; p == nil || p.HTTPGet.Path != "/readyz" || p.HTTPGet.Port.StrVal != "metrics" ||
		len(c.Ports) != 1 || c.Ports[0].ContainerPort != 9567 {
		t.Errorf("probe = %+v, ports = %+v", c.ReadinessProbe, c.Ports)
	}
	wantArgs := "fs mount retyc://dr-1 /staging/globalmount --allow-other --uid=1000 --gid=2000 --spool-dir=/spool " +
		"--metrics-addr=0.0.0.0:9567 --drain-timeout=5m0s --cache-ttl=30s --read-only --local-file=*.tmp " +
		"--cache-store=redis --cache-redis-high-confidentiality"
	if got := strings.Join(c.Args, " "); got != wantArgs {
		t.Errorf("args =\n%s\nwant\n%s", got, wantArgs)
	}
	// Defaults (50m, 256Mi / 512Mi) with the spec's limits on top.
	r := c.Resources
	if r.Requests.Cpu().String() != "50m" || r.Requests.Memory().String() != "256Mi" ||
		r.Limits.Cpu().String() != "1" || r.Limits.Memory().String() != "1Gi" {
		t.Errorf("resources = %v", r)
	}
	envs := map[string]corev1.EnvVar{}
	for _, v := range c.Env {
		envs[v.Name] = v
	}
	secretName := a[AnnotationSecret]
	if !strings.HasPrefix(secretName, pod.Name+"-") {
		t.Fatalf("secret name = %q", secretName)
	}
	if ref := envs[secretRedisURL].ValueFrom; ref == nil || ref.SecretKeyRef.Name != secretName ||
		ref.SecretKeyRef.Key != secretRedisURL {
		t.Errorf("env %s = %+v", secretRedisURL, envs[secretRedisURL].ValueFrom)
	}
	for _, k := range []string{"RETYC_TOKEN", "RETYC_KEY_PASSPHRASE", "RETYC_KEYRING_ENABLED"} {
		if _, ok := envs[k]; ok {
			t.Errorf("env %s must not be set in machine mode", k)
		}
	}
	if envs[identity.MachineDirKey].Value != "/machine" || envs["RETYC_CONFIG_DIR"].Value != "/config" ||
		envs["RETYC_INSECURE"].Value != "true" {
		t.Errorf("env names = %v", slices.Collect(func(yield func(string) bool) {
			for k := range envs {
				yield(k)
			}
		}))
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, vm := range c.VolumeMounts {
		mounts[vm.Name] = vm
	}
	if m := mounts["staging"]; m.MountPath != "/staging" || m.MountPropagation == nil ||
		*m.MountPropagation != corev1.MountPropagationBidirectional {
		t.Errorf("staging mount = %+v", m)
	}
	if m := mounts["machine"]; m.MountPath != "/machine" || !m.ReadOnly {
		t.Errorf("machine mount = %+v", m)
	}
	hostPaths := map[string]string{}
	for _, v := range ps.Volumes {
		switch {
		case v.HostPath != nil:
			hostPaths[v.Name] = v.HostPath.Path
		case v.Secret != nil:
			// The identity files only, owner read only: the CLI refuses a file others can read, and
			// the container runs as root.
			sv := v.Secret
			var keys []string
			for _, it := range sv.Items {
				if it.Key != it.Path {
					t.Errorf("item %+v must keep its name", it)
				}
				keys = append(keys, it.Key)
			}
			if v.Name != "machine" || sv.SecretName != secretName || sv.DefaultMode == nil || *sv.DefaultMode != 0o400 ||
				strings.Join(keys, " ") != "client.key age.key machine.json" {
				t.Errorf("secret volume %s = %+v, items %v", v.Name, sv, keys)
			}
		case v.EmptyDir == nil:
			t.Errorf("volume %s is neither hostPath, secret nor emptyDir", v.Name)
		}
	}
	want := map[string]string{
		"staging": "/var/lib/kubelet/plugins/kubernetes.io/csi/csi.retycfs.com/abc",
		"spool":   "/var/lib/retycfs-csi/spool/dr-1",
		"fuse":    "/dev/fuse",
	}
	if fmt.Sprint(hostPaths) != fmt.Sprint(want) {
		t.Errorf("hostPaths = %v, want %v", hostPaths, want)
	}

	secret := e.secret(secretName)
	if string(secret.Data[identity.ClientKeyFile]) != "key" || string(secret.Data[identity.AgeKeyFile]) != "age" ||
		string(secret.Data[identity.MachineFile]) != `{"version":1}` || len(secret.Data) != 4 ||
		string(secret.Data[secretRedisURL]) != spec.Cache.RedisURL {
		t.Errorf("secret data keys = %v", slices.Sorted(maps.Keys(secret.Data)))
	}
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].UID != pod.UID ||
		secret.OwnerReferences[0].Kind != "Pod" {
		t.Errorf("secret owners = %+v", secret.OwnerReferences)
	}
}

func TestResources(t *testing.T) {
	m := New(nil, Config{})
	spec := &Spec{Resources: corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
		Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
	}}
	r := m.resources(spec)
	// The default request above the new limit follows it; the explicit CPU request is kept.
	if r.Requests.Cpu().String() != "200m" || r.Requests.Memory().String() != "128Mi" ||
		r.Limits.Memory().String() != "128Mi" || !r.Limits.Cpu().IsZero() {
		t.Errorf("resources = %v", r)
	}
	if r := m.resources(&Spec{}); r.Requests.Memory().String() != "256Mi" || r.Limits.Memory().String() != "512Mi" {
		t.Errorf("defaults = %v", r)
	}
}

func TestEnsureIdempotentAndConfigChange(t *testing.T) {
	e := newEnv(t)
	first := e.ensure(testSpec())
	e.running(first.Name)

	st := e.ensure(testSpec())
	if st.Phase != PhaseRunning || st.ConfigChanged || e.prepared != 1 || st.Pod.UID != first.Pod.UID {
		t.Fatalf("second Ensure: phase = %s, changed = %v, prepared = %d", st.Phase, st.ConfigChanged, e.prepared)
	}

	for name, change := range map[string]func(*Spec){
		"uid":         func(s *Spec) { s.UID = 0 },
		"credentials": func(s *Spec) { s.Credentials = testCredentials("other") },
		"options":     func(s *Spec) { s.Options.BackgroundDelete = true },
		"resources":   func(s *Spec) { s.Resources = corev1.ResourceRequirements{} },
	} {
		spec := testSpec()
		change(spec)
		st := e.ensure(spec)
		if !st.ConfigChanged || st.Phase != PhaseRunning || st.Pod.UID != first.Pod.UID {
			t.Errorf("%s: phase = %s, changed = %v: want the same pod with ConfigChanged", name, st.Phase, st.ConfigChanged)
		}
	}
	other := New(e.kube, Config{Namespace: testNS, Image: "ghcr.io/retyc/retyc-cli:1.2.4"})
	st, err := other.Ensure(context.Background(), testSpec())
	if err != nil || !st.ConfigChanged || st.CLIVersion != "1.2.3" {
		t.Errorf("new image: changed = %v, version = %s, %v", st.ConfigChanged, st.CLIVersion, err)
	}
	if e.prepared != 1 || e.secretCount() != 1 {
		t.Errorf("prepared = %d, secrets = %d, want no new pod", e.prepared, e.secretCount())
	}
}

func TestEnsureValidates(t *testing.T) {
	e := newEnv(t)
	for i, change := range []func(*Spec){
		func(s *Spec) { s.VolumeID = "../x" }, func(s *Spec) { s.NodeName = "" },
		func(s *Spec) { delete(s.Credentials.Files, identity.MachineFile) },
		func(s *Spec) { s.StagingPath = "" }, func(s *Spec) { s.StagingPath = "relative/globalmount" },
		func(s *Spec) { s.StagingPath = "/var/lib/kubelet/plugins/globalmount" },
		func(s *Spec) { s.StagingPath = "/etc/x/globalmount" },
		func(s *Spec) { s.StagingPath = "/var/lib/kubelet/plugins/../pods/x/globalmount" },
	} {
		spec := testSpec()
		change(spec)
		if _, err := e.m.Ensure(context.Background(), spec); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("case %d: Ensure = %v, want ErrInvalidSpec", i, err)
		}
	}
}

func TestSecretPerPod(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	spec := testSpec()
	spec.Cache.RedisURL = "redis://redis:6379"
	// Left by a creation that failed between the Secret and the pod.
	orphan := e.m.buildSecret(spec, "x")
	if _, err := e.kube.CoreV1().Secrets(testNS).Create(ctx, orphan, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	first := e.ensure(spec)
	if _, err := e.kube.CoreV1().Secrets(testNS).Get(ctx, orphan.Name, metav1.GetOptions{}); err == nil {
		t.Error("the orphan secret must be swept")
	}
	firstSecret := first.Pod.Annotations[AnnotationSecret]

	// The next pod, with another config, gets a Secret of its own, without the dropped key.
	e.exit(first.Name, ExitFailed, time.Hour)
	spec.Cache.RedisURL = ""
	next := e.ensure(spec)
	if next.Phase != PhasePending || next.Pod.UID == first.Pod.UID {
		t.Fatalf("recreate: phase = %s", next.Phase)
	}
	nextSecret := next.Pod.Annotations[AnnotationSecret]
	if nextSecret == firstSecret {
		t.Fatal("each pod must have its own secret")
	}
	s := e.secret(nextSecret)
	if _, ok := s.Data[secretRedisURL]; ok || len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != next.Pod.UID {
		t.Errorf("new secret: keys %d, owners %+v", len(s.Data), s.OwnerReferences)
	}
	// The former Secret keeps its owner: the garbage collector deletes it with the former pod.
	if old := e.secret(firstSecret); old.OwnerReferences[0].UID != first.Pod.UID {
		t.Errorf("former secret owners = %+v", old.OwnerReferences)
	}
}

func TestDeleteReadsExitCode(t *testing.T) {
	e := newEnv(t)
	st := e.ensure(testSpec())
	e.running(st.Name)
	ctx := context.Background()

	st, err := e.m.Delete(ctx, "node-a", "dr-1")
	if err != nil || st.Phase != PhaseTerminating {
		t.Fatalf("Delete: phase = %s, %v", st.Phase, err)
	}
	pod := e.pod(st.Name)
	if pod.DeletionTimestamp == nil || !slices.Contains(pod.Finalizers, FinalizerInUse) {
		t.Fatalf("pod after the first Delete = %+v", pod.ObjectMeta)
	}
	if e.secretCount() != 1 {
		t.Error("the secret must stay while the pod exists")
	}
	if st, err := e.m.Delete(ctx, "node-a", "dr-1"); err != nil || st.Phase != PhaseTerminating {
		t.Fatalf("Delete while draining: phase = %s, %v", st.Phase, err)
	}

	e.exit(st.Name, ExitFailed, time.Hour)
	st, err = e.m.Delete(ctx, "node-a", "dr-1")
	if err != nil || st.Phase != PhaseExited || st.ExitCode == nil || *st.ExitCode != ExitFailed {
		t.Fatalf("Delete after exit: phase = %s, code = %s, %v", st.Phase, fmtCode(st.ExitCode), err)
	}
	if st.ExitReason != "Error" || st.ExitMessage != "mount: login lost" || ExitMeaning(*st.ExitCode) == "" {
		t.Errorf("reason = %q, message = %q", st.ExitReason, st.ExitMessage)
	}
	if !e.gone(st.Name) || e.secretCount() != 0 {
		t.Fatalf("pod gone = %v, secrets = %d", e.gone(st.Name), e.secretCount())
	}
	if st, err := e.m.Delete(ctx, "node-a", "dr-1"); err != nil || st.Phase != PhaseAbsent {
		t.Fatalf("Delete when gone: phase = %s, %v", st.Phase, err)
	}
}

func TestDeleteExitedPod(t *testing.T) {
	e := newEnv(t)
	st := e.ensure(testSpec())
	e.exit(st.Name, 0, time.Hour)
	st, err := e.m.Delete(context.Background(), "node-a", "dr-1")
	if err != nil || st.Phase != PhaseExited || *st.ExitCode != 0 || !e.gone(st.Name) {
		t.Fatalf("Delete: phase = %s, %v", st.Phase, err)
	}
}

// A pod deleted before its container ever started, whose kubelet then never reports: taken as
// finished once stuck past its deletion timestamp.
func TestDeletePendingPodStuck(t *testing.T) {
	e := newEnv(t)
	st := e.ensure(testSpec())
	e.setContainer(st.Name, corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
		Reason: "ImagePullBackOff", Message: "back-off pulling image",
	}}, corev1.PodPending)
	if got, _ := e.m.Get(context.Background(), "node-a", "dr-1"); got.Phase != PhasePending ||
		got.WaitingReason != "ImagePullBackOff" || got.WaitingMessage == "" {
		t.Fatalf("pending: phase = %s, waiting = %q", got.Phase, got.WaitingReason)
	}

	ctx := context.Background()
	if st, err := e.m.Delete(ctx, "node-a", "dr-1"); err != nil || st.Phase != PhaseTerminating {
		t.Fatalf("Delete: phase = %s, %v", st.Phase, err)
	}
	e.now = e.now.Add(310*time.Second + stuckAfter/2)
	if st, _ := e.m.Delete(ctx, "node-a", "dr-1"); st.Phase != PhaseTerminating {
		t.Fatalf("Delete within the margin: phase = %s", st.Phase)
	}
	e.now = e.now.Add(stuckAfter)
	st, err := e.m.Delete(ctx, "node-a", "dr-1")
	if err != nil || st.Phase != PhaseExited || st.ExitCode != nil || !e.gone(st.Name) {
		t.Fatalf("Delete when stuck: phase = %s, code = %s, %v", st.Phase, fmtCode(st.ExitCode), err)
	}
}

// A running pod evicted, whose node then disappears: Ensure releases it at its restart time and a
// new one follows.
func TestEnsureReleasesStuckPod(t *testing.T) {
	e := newEnv(t)
	spec := testSpec()
	st := e.ensure(spec)
	e.running(st.Name)
	if err := e.kube.CoreV1().Pods(testNS).Delete(context.Background(), st.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := e.ensure(spec); got.Phase != PhaseTerminating || e.gone(st.Name) {
		t.Fatalf("draining pod: phase = %s", got.Phase)
	}
	e.now = e.now.Add(310*time.Second + 2*stuckAfter)
	got := e.ensure(spec)
	// Kept until its restart time, as an exited pod.
	if got.Phase != PhaseTerminating || e.gone(st.Name) || got.Backoff.LastExitCode != -1 {
		t.Fatalf("stuck pod: phase = %s, gone = %v, backoff = %+v", got.Phase, e.gone(st.Name), got.Backoff)
	}
	e.now = e.now.Add(time.Minute)
	if got := e.ensure(spec); got.Phase != PhasePending {
		t.Fatalf("recreate: phase = %s", got.Phase)
	}
}

func TestNodeLostPodIsFinished(t *testing.T) {
	e := newEnv(t)
	st := e.ensure(testSpec())
	e.setStatus(st.Name, corev1.PodStatus{Phase: corev1.PodRunning, Reason: "NodeLost", Message: "node vanished"})
	st, err := e.m.Delete(context.Background(), "node-a", "dr-1")
	if err != nil || st.Phase != PhaseExited || st.ExitCode != nil || st.ExitReason != "NodeLost" {
		t.Fatalf("Delete: phase = %s, reason = %q, %v", st.Phase, st.ExitReason, err)
	}
}

// Evicted before the container started: Failed, no container status, the reason on the pod.
func TestFailedPodWithoutContainerStatus(t *testing.T) {
	e := newEnv(t)
	spec := testSpec()
	st := e.ensure(spec)
	e.setStatus(st.Name, corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Message: "low on memory"})
	got := e.ensure(spec)
	if got.Phase != PhaseExited || got.ExitCode != nil || got.ExitReason != "Evicted" ||
		got.ExitMessage != "low on memory" || got.Backoff.LastExitCode != -1 || got.Backoff.ConsecutiveFailures != 1 {
		t.Fatalf("phase = %s, reason = %q, backoff = %+v", got.Phase, got.ExitReason, got.Backoff)
	}
	e.now = got.Backoff.NextRestart
	if got := e.ensure(spec); got.Phase != PhasePending || got.Pod.UID == st.Pod.UID {
		t.Fatalf("recreate: phase = %s", got.Phase)
	}
}

func TestBackoffGivesUpOnFatalExits(t *testing.T) {
	e := newEnv(t)
	// The default gives up at once (see TestBackoffGivesUpAtOnce): three tries exercise the count.
	e.m.cfg.MaxFatalExits = 3
	spec := testSpec()
	st := e.ensure(spec)
	ctx := context.Background()

	wantDelays := []time.Duration{5 * time.Second, 10 * time.Second}
	for i, code := range []int32{ExitAuthRequired, ExitConfig} {
		e.exit(st.Name, code, time.Second)
		st = e.ensure(spec)
		b := st.Backoff
		if st.Phase != PhaseExited || b.FatalExits != i+1 || b.ConsecutiveFailures != i+1 || b.GaveUp {
			t.Fatalf("exit %d: phase = %s, backoff = %+v", i, st.Phase, b)
		}
		if d := b.NextRestart.Sub(e.now); d < wantDelays[i] || d > wantDelays[i]*11/10 {
			t.Fatalf("exit %d: delay = %s, want %s + 10%%", i, d, wantDelays[i])
		}
		// Counted once, however many times it is seen.
		if again := e.ensure(spec); again.Backoff.ConsecutiveFailures != i+1 || again.Pod.UID != st.Pod.UID {
			t.Fatalf("exit %d counted twice: %+v", i, again.Backoff)
		}
		e.now = b.NextRestart
		next := e.ensure(spec)
		if next.Phase != PhasePending || next.Pod.UID == st.Pod.UID {
			t.Fatalf("exit %d: restart phase = %s", i, next.Phase)
		}
		st = next
	}

	e.exit(st.Name, ExitConfig, time.Second) // a 78 never expires (see TestGiveUpOn77Expires)
	if st, err := e.m.Ensure(ctx, spec); !errors.Is(err, ErrGaveUp) || !st.Backoff.GaveUp || st.Phase != PhaseExited {
		t.Fatalf("third fatal exit: phase = %s, %v", st.Phase, err)
	}
	e.now = e.now.Add(time.Hour)
	if _, err := e.m.Ensure(ctx, spec); !errors.Is(err, ErrGaveUp) {
		t.Fatalf("a given-up volume must stay given up: %v", err)
	}
	// Deleting the pod (an orphan sweep, an unstage) keeps the give-up: no new pod, no new login.
	if st, err := e.m.Delete(ctx, spec.NodeName, spec.VolumeID); err != nil || st.Phase != PhaseExited {
		t.Fatalf("delete: %s, %v", st.Phase, err)
	}
	if st, err := e.m.Ensure(ctx, spec); !errors.Is(err, ErrGaveUp) || st.Phase != PhaseAbsent {
		t.Fatalf("ensure after the delete of a given-up pod: phase = %s, %v; want Absent, ErrGaveUp", st.Phase, err)
	}

	// New credentials: a new config, a fresh backoff and a new pod right away.
	spec.Credentials = testCredentials("new")
	st = e.ensure(spec)
	if st.Phase != PhasePending || st.Backoff != (Backoff{}) {
		t.Fatalf("new credentials: phase = %s, backoff = %+v", st.Phase, st.Backoff)
	}
}

// A 77 or a 78 gives up at once by default: a machine refused, or misconfigured, is not retried.
func TestBackoffGivesUpAtOnce(t *testing.T) {
	for _, code := range []int32{ExitAuthRequired, ExitConfig} {
		e := newEnv(t)
		spec := testSpec()
		st := e.ensure(spec)
		e.exit(st.Name, code, time.Second)
		st, err := e.m.Ensure(context.Background(), spec)
		if !errors.Is(err, ErrGaveUp) || !st.Backoff.GaveUp || !strings.Contains(err.Error(), ExitMeaning(code)) {
			t.Errorf("exit %d: %+v, %v", code, st.Backoff, err)
		}
	}
	if !strings.Contains(ExitMeaning(ExitAuthRequired), "Organization > Machines") {
		t.Errorf("77 must say how to enable the machine again: %q", ExitMeaning(ExitAuthRequired))
	}
}

// A give-up on a 77 expires GiveUpRetryAfter after the exit, however often it is looked at, for one
// try; refused again, it gives up for as long again. A 78 does not expire.
func TestGiveUpOn77Expires(t *testing.T) {
	e := newEnv(t)
	spec := testSpec()
	ctx := context.Background()
	st := e.ensure(spec)
	e.exit(st.Name, ExitAuthRequired, time.Second)
	_, err := e.m.Ensure(ctx, spec)
	if !errors.Is(err, ErrGaveUp) || !strings.Contains(err.Error(), "automatically") {
		t.Fatalf("first 77: %v", err)
	}
	until := e.now.Add(GiveUpRetryAfter)

	// Retries do not extend it.
	for range 3 {
		e.now = e.now.Add(GiveUpRetryAfter / 4)
		if e.now.Before(until) {
			if st, err := e.m.Ensure(ctx, spec); !errors.Is(err, ErrGaveUp) || st.Backoff.GiveUpUntil != until {
				t.Fatalf("before the expiry: %+v, %v", st.Backoff, err)
			}
		}
	}
	e.now = until
	st = e.ensure(spec)
	if st.Phase != PhasePending || st.Backoff.GaveUp {
		t.Fatalf("after the expiry: phase = %s, backoff = %+v; want one new try", st.Phase, st.Backoff)
	}

	// Refused again: gives up again, for a full period from the new exit.
	e.exit(st.Name, ExitAuthRequired, time.Second)
	if _, err := e.m.Ensure(ctx, spec); !errors.Is(err, ErrGaveUp) {
		t.Fatalf("second 77: %v", err)
	}
	e.now = e.now.Add(GiveUpRetryAfter - time.Second)
	if _, err := e.m.Ensure(ctx, spec); !errors.Is(err, ErrGaveUp) {
		t.Fatalf("a second give-up must last a full period: %v", err)
	}

	// A 78 stays.
	e = newEnv(t)
	st = e.ensure(spec)
	e.exit(st.Name, ExitConfig, time.Second)
	_, err = e.m.Ensure(ctx, spec)
	if !errors.Is(err, ErrGaveUp) || !strings.Contains(err.Error(), "fix the configuration") {
		t.Fatalf("78: %v", err)
	}
	e.now = e.now.Add(24 * time.Hour)
	if _, err := e.m.Ensure(ctx, spec); !errors.Is(err, ErrGaveUp) {
		t.Fatalf("a 78 must not expire: %v", err)
	}
}

func TestBackoffResets(t *testing.T) {
	e := newEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	e.m.cfg.MetricsPort = serverPort(t, srv)
	e.m.cfg.MaxFatalExits = 3 // to see the count of fatal exits reset
	spec := testSpec()
	st := e.ensure(spec)
	restart := func(code int32, ran time.Duration) Backoff {
		t.Helper()
		e.exit(st.Name, code, ran)
		b := e.ensure(spec).Backoff
		e.now = b.NextRestart
		st = e.ensure(spec)
		if st.Phase != PhasePending {
			t.Fatalf("restart: phase = %s", st.Phase)
		}

		return b
	}

	// A non-fatal exit between two fatal ones resets the fatal count, not the delay.
	restart(ExitAuthRequired, time.Second)
	restart(ExitAuthRequired, time.Second)
	if b := restart(ExitFailed, time.Second); b.FatalExits != 0 || b.ConsecutiveFailures != 3 {
		t.Fatalf("after exit 1: %+v", b)
	}
	// Lost writes (75) are no fatal exit either: the mount worked, a new pod may.
	restart(ExitAuthRequired, time.Second)
	if b := restart(ExitWritesLost, time.Second); b.FatalExits != 0 || b.ConsecutiveFailures != 5 || b.GaveUp {
		t.Fatalf("after exit 75: %+v", b)
	}
	// A pod that ran for StableAfter starts a fresh backoff.
	if b := restart(ExitAuthRequired, time.Hour); b.ConsecutiveFailures != 1 || b.FatalExits != 1 {
		t.Fatalf("after a stable run: %+v", b)
	}

	// A pod seen ready resets the backoff.
	e.running(st.Name)
	if err := e.m.Ready(context.Background(), e.pod(st.Name)); err != nil {
		t.Fatal(err)
	}
	if b := e.m.backoffState("dr-1", e.m.ConfigHash(spec)); b != (Backoff{}) {
		t.Fatalf("after Ready: %+v", b)
	}
	if b := restart(ExitAuthRequired, time.Second); b.ConsecutiveFailures != 1 || b.FatalExits != 1 {
		t.Fatalf("after Ready then exit: %+v", b)
	}
}

func TestRestartDelay(t *testing.T) {
	for failures, want := range map[int]time.Duration{
		1: 5 * time.Second, 2: 10 * time.Second, 4: 40 * time.Second, 7: 5 * time.Minute, 50: 5 * time.Minute,
	} {
		if got := restartDelay(5*time.Second, 5*time.Minute, failures); got != want {
			t.Errorf("restartDelay(%d) = %s, want %s", failures, got, want)
		}
	}
}

func TestEvictedPodIsRecreated(t *testing.T) {
	e := newEnv(t)
	spec := testSpec()
	st := e.ensure(spec)
	e.running(st.Name)
	// Evicted by a drain: deleted, then the container stops.
	if err := e.kube.CoreV1().Pods(testNS).Delete(context.Background(), st.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := e.ensure(spec); got.Phase != PhaseTerminating {
		t.Fatalf("draining pod: phase = %s", got.Phase)
	}
	e.exit(st.Name, 0, time.Hour)
	// Kept until its restart time, as an exited pod: the recovery rebuilds the next one's spec from it.
	got := e.ensure(spec)
	if got.Phase != PhaseTerminating || got.Backoff.ConsecutiveFailures != 1 || e.gone(st.Name) {
		t.Fatalf("terminated pod: phase = %s, backoff = %+v, gone = %v", got.Phase, got.Backoff, e.gone(st.Name))
	}
	e.now = e.now.Add(time.Minute)
	got = e.ensure(spec)
	if got.Phase != PhasePending || got.Pod.UID == st.Pod.UID {
		t.Fatalf("after the backoff: phase = %s, uid = %s", got.Phase, got.Pod.UID)
	}
}

func TestList(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("node-", 20)
	for _, s := range []struct{ node, vol string }{{"node-a", "dr-1"}, {"node-a", "dr-2"}, {"node-b", "dr-1"},
		{long + "x", "dr-3"}, {long + "y", "dr-4"}} {
		spec := testSpec()
		spec.NodeName, spec.VolumeID = s.node, s.vol
		e.ensure(spec)
	}
	// A pod of another node whose label value collides with node-a's.
	spec := testSpec()
	spec.NodeName, spec.VolumeID = "node-z", "dr-9"
	collide := e.m.buildPod(spec, "h", "s")
	collide.Labels[LabelNode] = "node-a"
	if _, err := e.kube.CoreV1().Pods(testNS).Create(context.Background(), collide, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	for node, want := range map[string][]string{"node-a": {"dr-1", "dr-2"}, long + "x": {"dr-3"}, "node-c": {}} {
		states, err := e.m.List(context.Background(), node)
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, st := range states {
			got = append(got, st.VolumeID)
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("List(%s) = %v, want %v", node, got, want)
		}
	}
}

func TestReady(t *testing.T) {
	e := newEnv(t)
	code := http.StatusServiceUnavailable
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)

			return
		}
		w.WriteHeader(code)
	}))
	defer srv.Close()
	e.m.HTTP = srv.Client()
	ctx := context.Background()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}}
	if err := e.m.Ready(ctx, pod); err == nil {
		t.Error("a pod without IP is not ready")
	}
	pod.Status.PodIP = "127.0.0.1"
	if err := e.m.Ready(ctx, pod); err == nil {
		t.Error("a pod without metrics port is not ready")
	}
	// The port comes from the pod, not from the current configuration.
	pod.Spec.Containers = []corev1.Container{{Name: containerName, Ports: []corev1.ContainerPort{
		{Name: metricsPortName, ContainerPort: int32(serverPort(t, srv))}, //nolint:gosec // G115: a port
	}}}
	if err := e.m.Ready(ctx, pod); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("503: %v", err)
	}
	code = http.StatusOK
	if err := e.m.Ready(ctx, pod); err != nil {
		t.Errorf("200: %v", err)
	}
	srv.Close()
	if err := e.m.Ready(ctx, pod); err == nil {
		t.Error("a closed port is not ready")
	}
}

func serverPort(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}

	return p
}

func TestSpecOf(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	spec := testSpec()
	st := e.ensure(spec)

	got, err := e.m.SpecOf(ctx, st, spec.Cache)
	if err != nil {
		t.Fatal(err)
	}
	if e.m.ConfigHash(got) != e.m.ConfigHash(spec) || got.StagingPath != testStaging || got.PVCName != "data" {
		t.Fatalf("SpecOf = %+v, want the spec the pod was built from", got)
	}

	// Without its spec or its staging path, a pod cannot be rebuilt: the recovery leaves it.
	for _, key := range []string{AnnotationSpec, AnnotationStagingPath} {
		pod := st.Pod.DeepCopy()
		delete(pod.Annotations, key)
		if _, err := e.m.SpecOf(ctx, e.m.stateOf(pod), spec.Cache); err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("SpecOf without %s: %v, want an error naming it", key, err)
		}
	}
	if _, err := e.m.SpecOf(ctx, State{Name: st.Name}, spec.Cache); err == nil {
		t.Error("SpecOf of an absent pod succeeded")
	}
}

func TestExitMeaning(t *testing.T) {
	if got := ExitMeaning(ExitFailed); strings.Contains(got, "writes lost") || got == "" {
		t.Errorf("ExitMeaning(1) = %q, want a neutral label", got)
	}
	if got := ExitMeaning(ExitWritesLost); !strings.HasPrefix(got, "writes lost") {
		t.Errorf("ExitMeaning(75) = %q", got)
	}
	if got := ExitLabel(State{}); got != "" {
		t.Errorf("ExitLabel without exit code = %q", got)
	}
	if isFatal(ExitWritesLost) || isFatal(ExitFailed) || !isFatal(ExitAuthRequired) || !isFatal(ExitConfig) {
		t.Error("only 77 and 78 are fatal")
	}
}

func TestLostWrites(t *testing.T) {
	created := metav1.Time{Time: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	started := metav1.Time{Time: created.Add(2 * time.Second)}
	exited := func(code int32, ready corev1.ConditionStatus, transition metav1.Time, message string) State {
		pod := &corev1.Pod{Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: ready, LastTransitionTime: transition}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: containerName, State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: code, StartedAt: started, Message: message},
			}}},
		}}
		m := New(nil, Config{})

		return m.stateOf(pod)
	}
	later := metav1.Time{Time: started.Add(time.Hour)}
	const lost = "Error: 2 files being written were not sent before the filesystem was unmounted: their changes are lost"
	for _, tc := range []struct {
		name  string
		st    State
		want  bool
		label string
	}{
		{"75", exited(ExitWritesLost, corev1.ConditionFalse, later, "Error: something"), true, "writes lost"},
		{"75 never ready", exited(ExitWritesLost, corev1.ConditionFalse, created, ""), true, "writes lost"},
		{"never ready", exited(ExitFailed, corev1.ConditionFalse, created, "Error: redis: connection refused"), false,
			"failed before it was ready"},
		// A newer CLI exits 75 on lost writes: a 1 after serving with no loss reported lost nothing.
		{"ready then exited", exited(ExitFailed, corev1.ConditionFalse, later, "Error: something"), false,
			"failed after it was ready"},
		// A CLI older than 75: its words.
		{"loss reported", exited(ExitFailed, corev1.ConditionFalse, created, lost), true, "writes lost"},
		{"loss reported after serving", exited(ExitFailed, corev1.ConditionFalse, later, lost), true, "writes lost"},
		{"drain log", exited(ExitFailed, corev1.ConditionFalse, later, "drain over after 5m0s: 3 files not sent"), true,
			"writes lost"},
		// A newer CLI whose drain ran out before it counted the cancelled uploads (seen in e2e).
		{"drain timeout over", exited(ExitFailed, corev1.ConditionFalse, later, "stopping: the drain timeout is over: "+
			"the uploads still in progress are cancelled, what they held is lost"), true, "writes lost"},
		{"everything sent", exited(ExitFailed, corev1.ConditionFalse, later, "drain over after 1s: everything was sent"),
			false, "failed after it was ready"},
		// A login lost with the writes keeps 77.
		{"77 lost writes", exited(ExitAuthRequired, corev1.ConditionFalse, later, lost), true, "machine disabled"},
		{"77", exited(ExitAuthRequired, corev1.ConditionFalse, later, "Error: login lost"), false,
			"machine disabled"},
		{"78 with the words", exited(ExitConfig, corev1.ConditionFalse, created, lost), false,
			"machine account configuration"},
	} {
		if got := LostWrites(tc.st); got != tc.want {
			t.Errorf("%s: LostWrites = %t, want %t", tc.name, got, tc.want)
		}
		label := ExitLabel(tc.st)
		if !strings.HasPrefix(label, tc.label) || strings.Contains(label, "writes lost") != tc.want {
			t.Errorf("%s: ExitLabel = %q, want %q..., writes lost = %t", tc.name, label, tc.label, tc.want)
		}
	}
}
