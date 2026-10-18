package enroller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"

	"github.com/retyc/retycfs-k8s-csi/internal/driver"
	"github.com/retyc/retycfs-k8s-csi/internal/identity"
	"github.com/retyc/retycfs-k8s-csi/internal/retycclient"
)

const (
	driverNS   = "retycfs"
	tenantNS   = "team-a"
	org        = "0190a7f4-0000-7000-8000-000000000001"
	kubeSysUID = "kube-system-uid"
	tenantUID  = "team-a-uid"
	driverUID  = "retycfs-uid"
)

// fakeRetyc is the CLI: it writes keys when the directory has none, then plays outcome.
type fakeRetyc struct {
	ctx     context.Context // of the last call
	calls   []retycclient.EnrollParams
	keys    []string // client.key seen by each call, after generation
	n       int
	outcome func(dir string, emit func(retycclient.EnrollEvent)) error
}

func (f *fakeRetyc) Enroll(ctx context.Context, p retycclient.EnrollParams, emit func(retycclient.EnrollEvent)) error {
	f.ctx = ctx
	f.calls = append(f.calls, p)
	keyPath := filepath.Join(p.Dir, identity.ClientKeyFile)
	if _, err := os.Stat(keyPath); err != nil {
		f.n++
		for _, name := range []string{identity.ClientKeyFile, identity.AgeKeyFile} {
			if err := os.WriteFile(filepath.Join(p.Dir, name), fmt.Appendf(nil, "%s-%d", name, f.n), 0o600); err != nil {
				return err
			}
		}
	}
	key, _ := os.ReadFile(keyPath) //nolint:gosec // test dir
	f.keys = append(f.keys, string(key))

	return f.outcome(p.Dir, emit)
}

func pendingEvent(dir string, emit func(retycclient.EnrollEvent)) {
	_ = os.WriteFile(filepath.Join(dir, identity.EnrollmentFile), []byte(`{"enrollment_id":"enr-1"}`), 0o600)
	emit(retycclient.EnrollEvent{Event: retycclient.EnrollPending, EnrollmentID: "enr-1", Fingerprint: "K7QF-2MXA",
		ApprovalURI: "https://retyc.example/approve", ExpiresAt: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)})
}

func approve(dir string, emit func(retycclient.EnrollEvent)) error {
	if err := os.WriteFile(filepath.Join(dir, identity.MachineFile),
		[]byte(`{"machine_id":"m-1","client_id":"machine-m-1","organization_id":"`+org+`"}`), 0o600); err != nil {
		return err
	}
	emit(retycclient.EnrollEvent{Event: retycclient.EnrollApproved, MachineID: "m-1", ClientID: "machine-m-1"})

	return nil
}

type env struct {
	e      *Enroller
	retyc  *fakeRetyc
	events *record.FakeRecorder
	auth   error
	now    time.Time
}

func namespace(name, uid string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)}}
}

func identityObj(ns, name, uid string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": GVR.GroupVersion().String(), "kind": Kind,
		"metadata": map[string]any{"name": name, "namespace": ns, "uid": uid, "generation": int64(1)},
		"spec":     spec,
	}}

	return u
}

func newEnv(t *testing.T, kubeObjs []runtime.Object, objs ...runtime.Object) *env {
	t.Helper()
	kubeObjs = append(kubeObjs, namespace("kube-system", kubeSysUID), namespace(tenantNS, tenantUID),
		namespace(driverNS, driverUID))
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{GVR: "RetycIdentityList"}, objs...)
	v := &env{retyc: &fakeRetyc{}, events: record.NewFakeRecorder(100), now: time.Now()}
	v.e = &Enroller{
		Kube: fake.NewClientset(kubeObjs...), Dynamic: dyn, Retyc: v.retyc, Events: v.events,
		Auth:      func(context.Context, *identity.Credentials) error { return v.auth },
		Namespace: driverNS, ClusterName: "prod", TempDir: t.TempDir(),
		Now: func() time.Time { return v.now },
	}

	return v
}

// run reconciles ns/default until no pass is asked at once, at most 10 times.
func (v *env) run(t *testing.T, ns string) result {
	t.Helper()
	var res result
	for range 10 {
		res = v.e.reconcile(context.Background(), nil, ns, ObjectName)
		if !res.again {
			return res
		}
	}
	t.Fatal("reconcile loops")

	return res
}

func (v *env) status(t *testing.T, ns string) *RetycIdentity {
	t.Helper()
	u, err := v.e.resource(ns).Get(context.Background(), ObjectName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ri, err := fromUnstructured(u)
	if err != nil {
		t.Fatal(err)
	}

	return ri
}

func (v *env) secret(t *testing.T, name string) *corev1.Secret {
	t.Helper()
	s, err := v.e.Kube.CoreV1().Secrets(driverNS).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Secret %s: %v", name, err)
	}

	return s
}

func (v *env) noSecret(t *testing.T, name string) {
	t.Helper()
	if _, err := v.e.Kube.CoreV1().Secrets(driverNS).Get(context.Background(), name, metav1.GetOptions{}); err == nil {
		t.Fatalf("Secret %s exists", name)
	}
}

func k8sOf(t *testing.T, s *corev1.Secret) identity.K8sInfo {
	t.Helper()
	var info identity.K8sInfo
	if err := json.Unmarshal(s.Data[identity.K8sFile], &info); err != nil {
		t.Fatal(err)
	}

	return info
}

// hasEvent returns the next event of reason, the ones before it dropped.
func (v *env) hasEvent(t *testing.T, reason string) string {
	t.Helper()
	for {
		select {
		case ev := <-v.events.Events:
			if strings.Contains(ev, " "+reason+" ") {
				return ev
			}
		default:
			t.Fatalf("no %s event", reason)

			return ""
		}
	}
}

func pvWith(name string, ref *corev1.SecretReference, ann map[string]string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: ann},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{Driver: driver.DriverName, VolumeHandle: name, NodeStageSecretRef: ref},
		}},
	}
}

func TestTenantPendingThenApproved(t *testing.T) {
	v := newEnv(t, nil, identityObj(tenantNS, ObjectName, "ri-1", map[string]any{"organizationID": org}))
	secretName := TenantSecretPrefix + tenantNS
	v.retyc.outcome = func(dir string, emit func(retycclient.EnrollEvent)) error {
		pendingEvent(dir, emit)
		// Saved as soon as pending: keys, request, k8s.json with the fingerprint.
		s := v.secret(t, secretName)
		if len(s.Data[identity.ClientKeyFile]) == 0 || len(s.Data[identity.EnrollmentFile]) == 0 ||
			len(s.Data[identity.MachineFile]) != 0 {
			t.Errorf("pending Secret keys = %v", keys(s.Data))
		}
		if fp := k8sOf(t, s).Fingerprint; fp != "K7QF-2MXA" {
			t.Errorf("k8s.json fingerprint = %q", fp)
		}
		if ri := v.status(t, tenantNS); ri.Status.Phase != PhasePending || ri.Status.ApprovalURI == "" {
			t.Errorf("pending status = %+v", ri.Status)
		}

		return approve(dir, emit)
	}
	v.run(t, tenantNS)

	p := v.retyc.calls[0]
	if p.ClusterUID != tenantUID || p.OrganizationID != org || p.Name != "prod/"+tenantNS {
		t.Errorf("enroll params = %+v, want the tenant namespace's UID as cluster UID", p)
	}
	s := v.secret(t, secretName)
	if s.Type != SecretType || s.Labels[LabelNamespace] != tenantNS || s.Labels[LabelNamespaceUID] != tenantUID {
		t.Errorf("Secret type %s labels %v", s.Type, s.Labels)
	}
	if len(s.Data[identity.MachineFile]) == 0 {
		t.Error("no machine.json once approved")
	}
	info := k8sOf(t, s)
	if info.Mode != identity.ModeTenant || info.Namespace != tenantNS || info.NamespaceUID != tenantUID ||
		info.ClusterUID != tenantUID {
		t.Errorf("k8s.json = %+v", info)
	}
	if err := identity.FromData(s.Data).CheckNamespace(tenantNS, tenantUID); err != nil {
		t.Error(err)
	}
	ri := v.status(t, tenantNS)
	st := ri.Status
	if st.Phase != PhaseApproved || st.MachineID != "m-1" || st.ClientID != "machine-m-1" ||
		st.SecretName != secretName || st.Fingerprint != "K7QF-2MXA" {
		t.Errorf("status = %+v", st)
	}
	if !meta.IsStatusConditionTrue(st.Conditions, ConditionActive) {
		t.Errorf("Active not true: %+v", st.Conditions)
	}
	if !slices.Contains(ri.Finalizers, Finalizer) {
		t.Errorf("finalizers = %v", ri.Finalizers)
	}
	v.hasEvent(t, PhasePending)
	v.hasEvent(t, PhaseApproved)

	// Approved: no more enrollment.
	v.run(t, tenantNS)
	if len(v.retyc.calls) != 1 {
		t.Errorf("%d enroll calls, want 1", len(v.retyc.calls))
	}
}

func TestClusterMode(t *testing.T) {
	v := newEnv(t, nil, identityObj(driverNS, ObjectName, "ri-c", map[string]any{}))
	v.e.DefaultOrganizationID = org
	v.retyc.outcome = func(dir string, emit func(retycclient.EnrollEvent)) error {
		pendingEvent(dir, emit)

		return approve(dir, emit)
	}
	v.run(t, driverNS)
	if p := v.retyc.calls[0]; p.ClusterUID != kubeSysUID || p.OrganizationID != org {
		t.Errorf("enroll params = %+v, want kube-system's UID and the default organization", p)
	}
	info := k8sOf(t, v.secret(t, ClusterSecretName))
	if info.Mode != identity.ModeCluster || info.ClusterUID != kubeSysUID || info.NamespaceUID != "" {
		t.Errorf("k8s.json = %+v", info)
	}
}

func TestNoOrganizationRejected(t *testing.T) {
	v := newEnv(t, nil, identityObj(tenantNS, ObjectName, "ri-1", map[string]any{}))
	v.run(t, tenantNS)
	if st := v.status(t, tenantNS).Status; st.Phase != PhaseRejected || len(v.retyc.calls) != 0 {
		t.Errorf("status %+v, %d calls", st, len(v.retyc.calls))
	}
}

func TestOtherNameIgnored(t *testing.T) {
	v := newEnv(t, nil, identityObj(tenantNS, "other", "ri-1", map[string]any{"organizationID": org}))
	if res := v.e.reconcile(context.Background(), nil, tenantNS, "other"); !res.gone || len(v.retyc.calls) != 0 {
		t.Errorf("result %+v, %d calls", res, len(v.retyc.calls))
	}
}

func TestResumeAfterRestart(t *testing.T) {
	obj := identityObj(tenantNS, ObjectName, "ri-1", map[string]any{"organizationID": org})
	v := newEnv(t, nil, obj)
	v.retyc.outcome = func(dir string, emit func(retycclient.EnrollEvent)) error {
		pendingEvent(dir, emit)

		return errors.New("signal: killed")
	}
	if res := v.run(t, tenantNS); res.after == 0 {
		t.Errorf("a failed enrollment is retried later: %+v", res)
	}
	// A new process (same API state) resumes with the saved keys and request.
	v.e = &Enroller{Kube: v.e.Kube, Dynamic: v.e.Dynamic, Retyc: v.retyc, Events: v.events, Namespace: driverNS,
		TempDir: t.TempDir(), Auth: v.e.Auth}
	v.retyc.outcome = func(dir string, emit func(retycclient.EnrollEvent)) error {
		if b, _ := os.ReadFile(filepath.Join(dir, identity.EnrollmentFile)); !strings.Contains(string(b), "enr-1") { //nolint:gosec,lll // test dir
			t.Error("enrollment.json not restored")
		}

		return approve(dir, emit)
	}
	v.run(t, tenantNS)
	if len(v.retyc.keys) != 2 || v.retyc.keys[0] != v.retyc.keys[1] || v.retyc.n != 1 {
		t.Errorf("keys %v (%d generated): want the same keys", v.retyc.keys, v.retyc.n)
	}
	if st := v.status(t, tenantNS).Status; st.Phase != PhaseApproved {
		t.Errorf("status = %+v", st)
	}
}

func TestExpiredFourTimesThenRetry(t *testing.T) {
	v := newEnv(t, nil, identityObj(tenantNS, ObjectName, "ri-1", map[string]any{"organizationID": org}))
	v.retyc.outcome = func(dir string, emit func(retycclient.EnrollEvent)) error {
		pendingEvent(dir, emit)

		return &retycclient.Error{Kind: retycclient.KindEnrollmentExpired, ExitCode: 1}
	}
	v.run(t, tenantNS)
	st := v.status(t, tenantNS).Status
	if st.Phase != PhaseExpired || st.Attempts != DefaultMaxAttempts || len(v.retyc.calls) != DefaultMaxAttempts {
		t.Fatalf("status %+v after %d calls", st, len(v.retyc.calls))
	}
	if v.retyc.n != 1 {
		t.Errorf("%d key generations, want 1: the keys are reused", v.retyc.n)
	}
	v.hasEvent(t, PhaseExpired)
	v.run(t, tenantNS)
	if len(v.retyc.calls) != DefaultMaxAttempts {
		t.Error("Expired enrolled again without a retry")
	}

	// The retry annotation starts a new series.
	u, _ := v.e.resource(tenantNS).Get(context.Background(), ObjectName, metav1.GetOptions{})
	u.SetAnnotations(map[string]string{AnnotationRetry: "1"})
	if _, err := v.e.resource(tenantNS).Update(context.Background(), u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	v.retyc.outcome = func(dir string, emit func(retycclient.EnrollEvent)) error {
		pendingEvent(dir, emit)

		return approve(dir, emit)
	}
	v.run(t, tenantNS)
	if st := v.status(t, tenantNS).Status; st.Phase != PhaseApproved || st.Attempts != 0 || st.ObservedRetry != "1" {
		t.Errorf("status after retry = %+v", st)
	}
}

func TestDenied(t *testing.T) {
	v := newEnv(t, nil, identityObj(tenantNS, ObjectName, "ri-1", map[string]any{"organizationID": org}))
	v.retyc.outcome = func(dir string, emit func(retycclient.EnrollEvent)) error {
		pendingEvent(dir, emit)
		emit(retycclient.EnrollEvent{Event: retycclient.EnrollDenied, EnrollmentID: "enr-1"})

		return &retycclient.Error{Kind: retycclient.KindEnrollmentDenied, ExitCode: retycclient.ExitConfig}
	}
	v.run(t, tenantNS)
	v.run(t, tenantNS)
	if st := v.status(t, tenantNS).Status; st.Phase != PhaseDenied || len(v.retyc.calls) != 1 {
		t.Errorf("status %+v after %d calls", st, len(v.retyc.calls))
	}
	v.hasEvent(t, PhaseDenied)
}

func TestConfigRefusedRejected(t *testing.T) {
	v := newEnv(t, nil, identityObj(tenantNS, ObjectName, "ri-1", map[string]any{"organizationID": org}))
	v.retyc.outcome = func(string, func(retycclient.EnrollEvent)) error {
		return &retycclient.Error{Kind: retycclient.KindConfig, ExitCode: retycclient.ExitConfig, Message: "unknown org"}
	}
	v.run(t, tenantNS)
	v.run(t, tenantNS)
	if st := v.status(t, tenantNS).Status; st.Phase != PhaseRejected || len(v.retyc.calls) != 1 {
		t.Errorf("status %+v after %d calls", st, len(v.retyc.calls))
	}
}

// oldSecret is a tenant Secret left by a namespace of the same name.
func oldSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: TenantSecretPrefix + tenantNS, Namespace: driverNS, UID: "old-secret",
			Labels: map[string]string{labelManagedBy: managedBy, LabelNamespace: tenantNS,
				LabelNamespaceUID: "old-ns-uid", LabelIdentityUID: "old-ri"},
			Annotations: map[string]string{AnnotationMachineID: "m-old"},
		},
		Type: SecretType,
		Data: map[string][]byte{identity.ClientKeyFile: []byte("old"), identity.AgeKeyFile: []byte("old"),
			identity.MachineFile: []byte(`{"machine_id":"m-old"}`)},
	}
}

func TestNamespaceRecreatedBlocked(t *testing.T) {
	pv := pvWith("pv-1", &corev1.SecretReference{Name: TenantSecretPrefix + tenantNS, Namespace: driverNS}, nil)
	v := newEnv(t, []runtime.Object{oldSecret(), pv},
		identityObj(tenantNS, ObjectName, "ri-1", map[string]any{"organizationID": org}))
	v.run(t, tenantNS)
	if st := v.status(t, tenantNS).Status; st.Phase != PhaseBlocked || len(v.retyc.calls) != 0 {
		t.Fatalf("status %+v after %d calls", st, len(v.retyc.calls))
	}
	v.hasEvent(t, PhaseBlocked)
	if string(v.secret(t, TenantSecretPrefix+tenantNS).Data[identity.ClientKeyFile]) != "old" {
		t.Error("the old Secret was changed")
	}

	// The PV gone, a new enrollment replaces it.
	if err := v.e.Kube.CoreV1().PersistentVolumes().Delete(context.Background(), "pv-1",
		metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	v.retyc.outcome = func(dir string, emit func(retycclient.EnrollEvent)) error {
		pendingEvent(dir, emit)

		return approve(dir, emit)
	}
	v.run(t, tenantNS)
	s := v.secret(t, TenantSecretPrefix+tenantNS)
	if s.Labels[LabelNamespaceUID] != tenantUID || string(s.Data[identity.ClientKeyFile]) == "old" {
		t.Errorf("Secret not replaced: labels %v", s.Labels)
	}
	if st := v.status(t, tenantNS).Status; st.Phase != PhaseApproved {
		t.Errorf("status = %+v", st)
	}
}

func TestDeletionAnnotationReferences(t *testing.T) {
	pv := pvWith("pv-1", nil, map[string]string{annotationDeletionSecretName: TenantSecretPrefix + tenantNS,
		annotationDeletionSecretNamespace: driverNS})
	v := newEnv(t, []runtime.Object{pv})
	if used, err := v.e.referenced(context.Background(), TenantSecretPrefix+tenantNS); err != nil || !used {
		t.Errorf("referenced = %v, %v", used, err)
	}
	// A PV without node secret uses the cluster-wide identity.
	if used, _ := v.e.referenced(context.Background(), ClusterSecretName); !used {
		t.Error("the cluster Secret is used by the PVs without node secret")
	}
}

// deleting is an approved tenant RetycIdentity being deleted, with its Secret.
func deleting(t *testing.T, pvs ...runtime.Object) *env {
	t.Helper()
	obj := identityObj(tenantNS, ObjectName, "ri-1", map[string]any{"organizationID": org})
	obj.SetFinalizers([]string{Finalizer})
	now := metav1.Now()
	obj.SetDeletionTimestamp(&now)
	s := oldSecret()
	s.Labels[LabelNamespaceUID], s.Labels[LabelIdentityUID] = tenantUID, "ri-1"

	return newEnv(t, append(pvs, s), obj)
}

func TestFinalizerWithoutPV(t *testing.T) {
	v := deleting(t)
	if res := v.run(t, tenantNS); !res.gone {
		t.Errorf("result %+v", res)
	}
	v.noSecret(t, TenantSecretPrefix+tenantNS)
	if f := v.status(t, tenantNS).Finalizers; len(f) != 0 {
		t.Errorf("finalizers = %v", f)
	}
	v.hasEvent(t, "MachineKept")
}

func TestFinalizerWithPVThenSweep(t *testing.T) {
	pv := pvWith("pv-1", &corev1.SecretReference{Name: TenantSecretPrefix + tenantNS, Namespace: driverNS}, nil)
	v := deleting(t, pv)
	v.run(t, tenantNS)
	if s := v.secret(t, TenantSecretPrefix+tenantNS); s.Labels[LabelOrphan] != "true" {
		t.Errorf("labels = %v", s.Labels)
	}
	if f := v.status(t, tenantNS).Finalizers; len(f) != 0 {
		t.Errorf("finalizers = %v", f)
	}
	v.e.Sweep(context.Background())
	v.secret(t, TenantSecretPrefix+tenantNS)
	if err := v.e.Kube.CoreV1().PersistentVolumes().Delete(context.Background(), "pv-1",
		metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	v.e.Sweep(context.Background())
	v.noSecret(t, TenantSecretPrefix+tenantNS)
}

func TestAuthStatusCondition(t *testing.T) {
	s := oldSecret()
	s.Labels[LabelNamespaceUID], s.Labels[LabelIdentityUID] = tenantUID, "ri-1"
	v := newEnv(t, []runtime.Object{s}, identityObj(tenantNS, ObjectName, "ri-1", map[string]any{"organizationID": org}))
	v.auth = fmt.Errorf("retyc not authenticated: %w", retycclient.ErrMachineRefused)
	v.run(t, tenantNS)
	st := v.status(t, tenantNS).Status
	c := meta.FindStatusCondition(st.Conditions, ConditionActive)
	if st.Phase != PhaseApproved || c == nil || c.Status != metav1.ConditionFalse || c.Reason != ReasonMachineRefused {
		t.Fatalf("status = %+v", st)
	}
	v.hasEvent(t, ReasonMachineRefused)

	// Not again before the interval.
	v.auth = nil
	v.run(t, tenantNS)
	if meta.IsStatusConditionTrue(v.status(t, tenantNS).Status.Conditions, ConditionActive) {
		t.Error("auth status checked again before the interval")
	}
	// Retyc unreachable keeps the refusal.
	v.now = v.now.Add(DefaultAuthInterval)
	v.auth = errors.New("connection refused")
	v.run(t, tenantNS)
	if c := meta.FindStatusCondition(v.status(t, tenantNS).Status.Conditions, ConditionActive); c.Reason !=
		ReasonMachineRefused {
		t.Errorf("condition = %+v", c)
	}
	v.now = v.now.Add(DefaultAuthInterval)
	v.auth = nil
	v.run(t, tenantNS)
	if !meta.IsStatusConditionTrue(v.status(t, tenantNS).Status.Conditions, ConditionActive) {
		t.Error("Active not true once accepted again")
	}
	if len(v.retyc.calls) != 0 {
		t.Error("an approved identity was enrolled")
	}
}

func TestMachineName(t *testing.T) {
	e := &Enroller{ClusterName: strings.Repeat("c", 60)}
	ri := &RetycIdentity{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant"}}
	if n := e.machineName(ri); len(n) != maxNameLength || !strings.HasPrefix(n, "ccc") {
		t.Errorf("name = %q", n)
	}
	ri.Spec.DisplayName = "mine"
	if n := e.machineName(ri); n != "mine" {
		t.Errorf("name = %q", n)
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}

	return out
}

// blockingRetyc blocks the tenant's enrollment until interrupted, approves the cluster's.
type blockingRetyc struct{ interrupted chan struct{} }

func (b *blockingRetyc) Enroll(
	ctx context.Context, p retycclient.EnrollParams, emit func(retycclient.EnrollEvent),
) error {
	for _, name := range []string{identity.ClientKeyFile, identity.AgeKeyFile} {
		if err := os.WriteFile(filepath.Join(p.Dir, name), []byte(name), 0o600); err != nil {
			return err
		}
	}
	pendingEvent(p.Dir, emit)
	if p.ClusterUID == kubeSysUID {
		return approve(p.Dir, emit)
	}
	<-ctx.Done()
	close(b.interrupted)

	return ctx.Err()
}

func TestRunConcurrentAndInterrupt(t *testing.T) {
	v := newEnv(t, nil, identityObj(tenantNS, ObjectName, "ri-1", map[string]any{"organizationID": org}),
		identityObj(driverNS, ObjectName, "ri-c", map[string]any{"organizationID": org}))
	b := &blockingRetyc{interrupted: make(chan struct{})}
	v.e.Retyc = b
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- v.e.Run(ctx) }()

	waitFor(t, func() bool { return v.status(t, driverNS).Status.Phase == PhaseApproved })
	waitFor(t, func() bool { return v.status(t, tenantNS).Status.EnrollmentID == "enr-1" })
	v.secret(t, TenantSecretPrefix+tenantNS)
	// Deleting the tenant's RetycIdentity (held by its finalizer: the fake API does not hold it)
	// interrupts its enrollment and drops its Secret.
	u, err := v.e.resource(tenantNS).Get(ctx, ObjectName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(u.GetFinalizers(), Finalizer) {
		t.Fatalf("finalizers = %v", u.GetFinalizers())
	}
	now := metav1.Now()
	u.SetDeletionTimestamp(&now)
	if _, err := v.e.resource(tenantNS).Update(ctx, u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.interrupted:
	case <-time.After(10 * time.Second):
		t.Fatal("the enrollment was not interrupted")
	}
	waitFor(t, func() bool { return len(v.status(t, tenantNS).Finalizers) == 0 })
	v.noSecret(t, TenantSecretPrefix+tenantNS)
	v.secret(t, ClusterSecretName)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if ev := v.hasEvent(t, ReasonEnrollmentAbandoned); !strings.Contains(ev, "retyc machine deny enr-1") {
		t.Errorf("event %q", ev)
	}

	// Run again once it returned.
	ctx, cancel = context.WithCancel(context.Background())
	go func() { done <- v.e.Run(ctx) }()
	waitFor(t, func() bool {
		v.e.mu.Lock()
		defer v.e.mu.Unlock()

		return v.e.workers[driverNS+"/"+ObjectName] != nil
	})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func (v *env) kube() *fake.Clientset {
	return v.e.Kube.(*fake.Clientset) //nolint:forcetypeassert // newEnv's
}

func TestPendingSaveFailsResumesSameKeys(t *testing.T) {
	v := newEnv(t, nil, identityObj(tenantNS, ObjectName, "ri-1", map[string]any{"organizationID": org}))
	failing := true
	v.kube().PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failing {
			return true, nil, errors.New("etcd unavailable")
		}

		return false, nil, nil
	})
	v.retyc.outcome = func(dir string, emit func(retycclient.EnrollEvent)) error {
		pendingEvent(dir, emit)
		ctx := v.retyc.ctx
		if ctx.Err() == nil {
			t.Error("the enrollment awaits approval with its keys unsaved")
		}
		if st := v.status(t, tenantNS).Status; st.ApprovalURI != "" {
			t.Errorf("approval URI published with the keys unsaved: %+v", st)
		}

		return ctx.Err() // the CLI killed
	}
	res := v.run(t, tenantNS)
	if res.after == 0 {
		t.Errorf("result %+v: want a pass later", res)
	}
	v.noSecret(t, TenantSecretPrefix+tenantNS)
	st := v.status(t, tenantNS).Status
	if st.Attempts != 0 || st.Phase != PhasePending || enrolledReason(&st) != ReasonEnrollFailed {
		t.Errorf("status = %+v", st)
	}

	// The next pass resumes the same request with the same keys, and saves them.
	failing = false
	v.retyc.outcome = func(dir string, emit func(retycclient.EnrollEvent)) error {
		if b, _ := os.ReadFile(filepath.Join(dir, identity.EnrollmentFile)); !strings.Contains(string(b), "enr-1") { //nolint:gosec,lll // test dir
			t.Error("enrollment.json not kept")
		}
		pendingEvent(dir, emit)

		return approve(dir, emit)
	}
	v.run(t, tenantNS)
	if len(v.retyc.keys) != 2 || v.retyc.keys[0] != v.retyc.keys[1] || v.retyc.n != 1 {
		t.Errorf("keys %v (%d generated): want the same keys", v.retyc.keys, v.retyc.n)
	}
	if st := v.status(t, tenantNS).Status; st.Phase != PhaseApproved {
		t.Errorf("status = %+v", st)
	}
	if string(v.secret(t, TenantSecretPrefix+tenantNS).Data[identity.ClientKeyFile]) != v.retyc.keys[0] {
		t.Error("Secret without the request's keys")
	}
	if len(v.e.keptDirs) != 0 {
		t.Errorf("kept directories %v", v.e.keptDirs)
	}
}

func TestApproveSaveFailsResumesFromSecret(t *testing.T) {
	v := newEnv(t, nil, identityObj(tenantNS, ObjectName, "ri-1", map[string]any{"organizationID": org}))
	failing := true
	v.kube().PrependReactor("update", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
		s, _ := a.(k8stesting.UpdateAction).GetObject().(*corev1.Secret)
		if failing && s != nil && len(s.Data[identity.MachineFile]) > 0 {
			return true, nil, errors.New("etcd unavailable")
		}

		return false, nil, nil
	})
	v.retyc.outcome = func(dir string, emit func(retycclient.EnrollEvent)) error {
		pendingEvent(dir, emit)

		return approve(dir, emit)
	}
	if res := v.run(t, tenantNS); res.after == 0 {
		t.Errorf("result %+v: want a pass later", res)
	}
	s := v.secret(t, TenantSecretPrefix+tenantNS)
	if len(s.Data[identity.ClientKeyFile]) == 0 || len(s.Data[identity.MachineFile]) != 0 {
		t.Errorf("Secret keys = %v", keys(s.Data))
	}
	if st := v.status(t, tenantNS).Status; st.Phase != PhasePending {
		t.Errorf("status = %+v", st)
	}

	// The CLI answers from the approved request, with the Secret's keys: no new request.
	failing = false
	v.retyc.outcome = approve
	v.run(t, tenantNS)
	if len(v.retyc.keys) != 2 || v.retyc.keys[0] != v.retyc.keys[1] || v.retyc.n != 1 {
		t.Errorf("keys %v (%d generated): want the Secret's keys", v.retyc.keys, v.retyc.n)
	}
	if st := v.status(t, tenantNS).Status; st.Phase != PhaseApproved || st.MachineID != "m-1" {
		t.Errorf("status = %+v", st)
	}
}

func TestResumedPendingKeepsStatus(t *testing.T) {
	v := newEnv(t, nil, identityObj(tenantNS, ObjectName, "ri-1", map[string]any{"organizationID": org}))
	v.retyc.outcome = func(dir string, emit func(retycclient.EnrollEvent)) error {
		pendingEvent(dir, emit)

		return errors.New("signal: killed")
	}
	v.run(t, tenantNS)
	expires := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	// Resumed, the CLI announces the request again without its URI, expiry or ID.
	v.retyc.outcome = func(dir string, emit func(retycclient.EnrollEvent)) error {
		emit(retycclient.EnrollEvent{Event: retycclient.EnrollPending, Fingerprint: "K7QF-2MXA"})
		st := v.status(t, tenantNS).Status
		if st.ApprovalURI != "https://retyc.example/approve" || st.EnrollmentID != "enr-1" ||
			st.ExpiresAt == nil || !st.ExpiresAt.Time.Equal(expires) || st.Fingerprint != "K7QF-2MXA" {
			t.Errorf("resumed status = %+v", st)
		}
		if strings.Contains(st.Message, "at  ") || strings.Contains(st.Message, "before 0001") ||
			!strings.Contains(st.Message, "approve at https://retyc.example/approve before 2026-10-18T12:00:00Z") {
			t.Errorf("message %q", st.Message)
		}

		return approve(dir, emit)
	}
	v.run(t, tenantNS)
	// Approved: the request's URI and expiry go, its ID stays.
	if st := v.status(t, tenantNS).Status; st.Phase != PhaseApproved || st.ApprovalURI != "" ||
		st.ExpiresAt != nil || st.EnrollmentID != "enr-1" {
		t.Errorf("approved status = %+v", st)
	}
}

func TestUnmanagedTenantSecretBlocked(t *testing.T) {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: TenantSecretPrefix + tenantNS, Namespace: driverNS},
		Data:       map[string][]byte{"token": []byte("mine")},
	}
	v := newEnv(t, []runtime.Object{s}, identityObj(tenantNS, ObjectName, "ri-1", map[string]any{"organizationID": org}))
	v.run(t, tenantNS)
	if st := v.status(t, tenantNS).Status; st.Phase != PhaseBlocked || len(v.retyc.calls) != 0 {
		t.Fatalf("status %+v after %d calls", st, len(v.retyc.calls))
	}
	v.hasEvent(t, PhaseBlocked)
	for _, a := range v.kube().Actions() {
		if a.GetResource().Resource == "secrets" && a.GetVerb() != "get" && a.GetVerb() != "list" {
			t.Errorf("Secret written: %s", a.GetVerb())
		}
	}
	if got := v.secret(t, s.Name); string(got.Data["token"]) != "mine" || len(got.Data) != 1 {
		t.Errorf("Secret changed: %v", keys(got.Data))
	}
}

func TestStopBeforeEnrollment(t *testing.T) {
	v := newEnv(t, nil, identityObj(tenantNS, ObjectName, "ri-1", map[string]any{"organizationID": org}))
	v.retyc.outcome = func(dir string, emit func(retycclient.EnrollEvent)) error {
		pendingEvent(dir, emit)

		return approve(dir, emit)
	}
	// The deletion seen after the pass read the object, before the enrollment started.
	w := &worker{kick: make(chan struct{}, 1)}
	w.stop()
	v.e.reconcile(context.Background(), w, tenantNS, ObjectName)
	if len(v.retyc.calls) != 0 {
		t.Fatalf("%d enroll calls after a stop", len(v.retyc.calls))
	}
	w.passStarting()
	v.e.reconcile(context.Background(), w, tenantNS, ObjectName)
	if len(v.retyc.calls) != 1 {
		t.Errorf("%d enroll calls, want 1 once the stop is cleared", len(v.retyc.calls))
	}
}

func TestClusterIdentityRecreatedReplacesSecret(t *testing.T) {
	ctx := context.Background()
	// A default PV: no node secret, the mounted cluster-wide identity.
	pv := pvWith("pv-default", nil, nil)
	obj := identityObj(driverNS, ObjectName, "ri-c", map[string]any{"organizationID": org})
	obj.SetFinalizers([]string{Finalizer})
	now := metav1.Now()
	obj.SetDeletionTimestamp(&now)
	s := oldSecret()
	s.Name = ClusterSecretName
	s.Labels[LabelNamespace], s.Labels[LabelNamespaceUID], s.Labels[LabelIdentityUID] = driverNS, driverUID, "ri-c"
	v := newEnv(t, []runtime.Object{pv, s}, obj)
	v.e.DefaultOrganizationID = org

	v.run(t, driverNS)
	if got := v.secret(t, ClusterSecretName); got.Labels[LabelOrphan] != "true" {
		t.Fatalf("labels = %v: kept while the default PVs exist", got.Labels)
	}
	// The API deletes it once its finalizer is gone; the next pass creates it again.
	if err := v.e.resource(driverNS).Delete(ctx, ObjectName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if res := v.e.reconcile(ctx, nil, driverNS, ObjectName); !res.gone {
		t.Fatalf("result %+v", res)
	}
	u, err := v.e.resource(driverNS).Get(ctx, ObjectName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	u.SetUID("ri-new") // the fake API sets none
	if _, err := v.e.resource(driverNS).Update(ctx, u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	v.retyc.outcome = func(dir string, emit func(retycclient.EnrollEvent)) error {
		pendingEvent(dir, emit)

		return approve(dir, emit)
	}
	v.run(t, driverNS)
	if st := v.status(t, driverNS).Status; st.Phase != PhaseApproved || len(v.retyc.calls) != 1 {
		t.Fatalf("status %+v after %d calls: want replaced, not Blocked", st, len(v.retyc.calls))
	}
	got := v.secret(t, ClusterSecretName)
	if got.Labels[LabelIdentityUID] != "ri-new" || got.Labels[LabelOrphan] != "" ||
		got.Annotations[AnnotationMachineID] != "m-1" {
		t.Errorf("Secret labels %v annotations %v", got.Labels, got.Annotations)
	}
	if ev := v.hasEvent(t, ReasonClusterIdentityReplaced); !strings.Contains(ev, "Warning") ||
		!strings.Contains(ev, "m-old") {
		t.Errorf("event %q", ev)
	}
}

func TestRetryOnDeniedDoesNotEnroll(t *testing.T) {
	v := newEnv(t, nil, identityObj(tenantNS, ObjectName, "ri-1", map[string]any{"organizationID": org}))
	v.retyc.outcome = func(dir string, emit func(retycclient.EnrollEvent)) error {
		pendingEvent(dir, emit)

		return &retycclient.Error{Kind: retycclient.KindEnrollmentDenied, ExitCode: retycclient.ExitConfig}
	}
	v.run(t, tenantNS)
	u, _ := v.e.resource(tenantNS).Get(context.Background(), ObjectName, metav1.GetOptions{})
	u.SetAnnotations(map[string]string{AnnotationRetry: "1"})
	if _, err := v.e.resource(tenantNS).Update(context.Background(), u, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	v.run(t, tenantNS)
	if st := v.status(t, tenantNS).Status; st.Phase != PhaseDenied || len(v.retyc.calls) != 1 {
		t.Errorf("status %+v after %d calls", st, len(v.retyc.calls))
	}
}
