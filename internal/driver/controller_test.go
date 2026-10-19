package driver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"github.com/retyc/retycfs-k8s-csi/internal/identity"
	"github.com/retyc/retycfs-k8s-csi/internal/retycclient"
)

// fakeRetyc is a shell stand-in for the retyc CLI. Each command (ls, quota, create, set, rm) prints
// <dir>/<command>.json on stdout, or <dir>/<command>.err on stderr with exit code 1 when that file
// exists. Every call is logged as "<client.key of its identity> <args>" in <dir>/calls.
type fakeRetyc struct {
	t   *testing.T
	dir string
}

func newFakeRetyc(t *testing.T) *fakeRetyc {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	f := &fakeRetyc{t: t, dir: t.TempDir()}
	script := `#!/bin/sh
d=` + f.dir + `
echo "$(cat "$RETYC_MACHINE_DIR/client.key" 2>/dev/null) $*" >> "$d/calls"
case "$2 $3" in
  "dataroom user") c=member ;;
  "dataroom ls") c=ls ;;
  "user quota") c=quota ;;
  "dataroom create") c=create ;;
  "dataroom set") c=set ;;
  "dataroom rm") c=rm ;;
  *) echo "{\"error\":\"unexpected: $*\"}" >&2; exit 2 ;;
esac
if [ -f "$d/$c.err" ]; then cat "$d/$c.err" >&2; exit 1; fi
cat "$d/$c.json"
`
	//nolint:gosec // G306: the fixture is a script and must be executable
	if err := os.WriteFile(filepath.Join(f.dir, "retyc"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	f.set("ls.json", `{"items":[],"total":0,"page":1,"pages":1}`)
	f.set("quota.json", `{"count_dataroom":1,"max_count_dataroom":10,"used_storage":0,"max_storage":0}`)
	f.set("create.json", `{"id":"dr-new","title":"pvc-1"}`)
	f.set("set.json", `{"id":"dr-1"}`)
	f.set("rm.json", `{"dataroom_id":"dr-1","uri":"retyc://dr-1","deleted_count":1}`)
	f.set("member.json", `{}`)

	return f
}

func (f *fakeRetyc) set(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// calls returns the logged invocations, without the leading --json.
func (f *fakeRetyc) calls() []string {
	data, err := os.ReadFile(filepath.Join(f.dir, "calls")) //nolint:gosec // G304: test fixture path
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for i, line := range lines {
		lines[i] = strings.Replace(line, " --json ", " ", 1)
	}

	return lines
}

// machineSecrets are the CSI secrets of a complete machine identity whose signing key is key.
func machineSecrets(key string) map[string]string {
	return map[string]string{
		identity.ClientKeyFile: key, identity.AgeKeyFile: "AGE-SECRET-KEY-1", identity.MachineFile: `{"version":1}`,
	}
}

// identityDir writes an identity directory, as the driver's Secret mounted, from secrets.
func identityDir(t *testing.T, secrets map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, v := range secrets {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// controller returns a ControllerServer running the fake CLI with the default identity "default",
// and a fake Kubernetes API holding pvcs and the namespace team-a.
func (f *fakeRetyc) controller(pvcs ...*corev1.PersistentVolumeClaim) *ControllerServer {
	kube := fake.NewClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", UID: "uid-team-a"}})
	for _, pvc := range pvcs {
		if _, err := kube.CoreV1().PersistentVolumeClaims(pvc.Namespace).Create(
			context.Background(), pvc, metav1.CreateOptions{}); err != nil {
			f.t.Fatal(err)
		}
	}

	return &ControllerServer{
		Retyc:       retycclient.New(filepath.Join(f.dir, "retyc"), []string{"RETYC_TOKEN=inherited"}),
		IdentityDir: identityDir(f.t, machineSecrets("default")),
		Kube:        kube,
		Events:      record.NewFakeRecorder(100),
	}
}

func claim(annotations map[string]string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "team-a", Annotations: annotations},
	}
}

func createRequest() *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:          "pvc-1",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 25 << 30},
		VolumeCapabilities: []*csi.VolumeCapability{{
			AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
			AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
		}},
		Parameters: map[string]string{
			pvcNameParameter:                  "data",
			pvcNamespaceParameter:             "team-a",
			"csi.storage.k8s.io/pv/name":      "pvc-1",
			"csi.storage.k8s.io/unknown-meta": "accepted: reserved for the sidecars",
		},
	}
}

func wantCode(t *testing.T, err error, code codes.Code) {
	t.Helper()
	if status.Code(err) != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func TestCreateVolume_CreatesWithCapacityAndVersioning(t *testing.T) {
	f := newFakeRetyc(t)
	s := f.controller(claim(map[string]string{AnnotationUID: "1000"}))
	req := createRequest()
	req.MutableParameters = map[string]string{versioningParameter: "false"}

	resp, err := s.CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	vol := resp.GetVolume()
	if vol.GetVolumeId() != "dr-new" || vol.GetCapacityBytes() != 25<<30 {
		t.Fatalf("volume = %+v", vol)
	}
	ctx := vol.GetVolumeContext()
	if len(ctx) != 2 || ctx[volumeContextPVCName] != "data" || ctx[volumeContextNamespace] != "team-a" {
		t.Fatalf("volume context = %v", ctx)
	}
	want := []string{
		"default dataroom ls",
		"default user quota",
		"default dataroom create --title pvc-1 --storage-capacity 25GiB --versioning=false",
	}
	if got := f.calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q, want %q", got, want)
	}
}

func TestCreateVolume_TenantSecretsAndDefaultVersioning(t *testing.T) {
	f := newFakeRetyc(t)
	s := f.controller(claim(map[string]string{AnnotationUID: "0", AnnotationGID: "0"}))
	req := createRequest()
	req.Secrets = machineSecrets("tenant")

	if _, err := s.CreateVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	calls := f.calls()
	// No mutable parameter: the CLI's default versioning applies.
	if last := calls[len(calls)-1]; last != "tenant dataroom create --title pvc-1 --storage-capacity 25GiB" {
		t.Fatalf("calls = %q", calls)
	}
}

func TestCreateVolume_ReusesAnExistingDataroom(t *testing.T) {
	f := newFakeRetyc(t)
	f.set("ls.json", `{"items":[{"id":"dr-other","title":"pvc-0"},{"id":"dr-1","title":"pvc-1"}]}`)
	// The account is full, but the retried create needs no new slot.
	f.set("quota.json", `{"count_dataroom":10,"max_count_dataroom":10}`)
	s := f.controller(claim(map[string]string{AnnotationUID: "1000"}))

	resp, err := s.CreateVolume(context.Background(), createRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetVolume().GetVolumeId() != "dr-1" {
		t.Fatalf("volume = %+v", resp.GetVolume())
	}
	if got := f.calls(); len(got) != 1 {
		t.Fatalf("calls = %q, want the listing only", got)
	}
}

func TestCreateVolume_ExistingDataroomCapacity(t *testing.T) {
	ok := claim(map[string]string{AnnotationUID: "1000"})

	// Reservation larger than requested: reused, the real capacity is reported.
	f := newFakeRetyc(t)
	f.set("ls.json", `{"items":[{"id":"dr-1","title":"pvc-1","storage_capacity":32212254720}]}`)
	resp, err := f.controller(ok).CreateVolume(context.Background(), createRequest())
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetVolume().GetCapacityBytes(); got != 30<<30 {
		t.Fatalf("capacity = %d, want the existing reservation", got)
	}

	// Reservation smaller than requested: AlreadyExists, nothing is created or changed.
	f = newFakeRetyc(t)
	f.set("ls.json", `{"items":[{"id":"dr-1","title":"pvc-1","storage_capacity":1073741824}]}`)
	_, err = f.controller(ok).CreateVolume(context.Background(), createRequest())
	wantCode(t, err, codes.AlreadyExists)
	if got := f.calls(); len(got) != 1 {
		t.Fatalf("calls = %q, want the listing only", got)
	}

	// Reservation above limit_bytes: AlreadyExists too.
	f = newFakeRetyc(t)
	f.set("ls.json", `{"items":[{"id":"dr-1","title":"pvc-1","storage_capacity":32212254720}]}`)
	req := createRequest()
	req.CapacityRange.LimitBytes = 26 << 30
	_, err = f.controller(ok).CreateVolume(context.Background(), req)
	wantCode(t, err, codes.AlreadyExists)
}

func TestCreateVolume_RefusesBeforeCallingRetyc(t *testing.T) {
	type edit = func(*csi.CreateVolumeRequest)
	invalid := codes.InvalidArgument
	for name, tc := range map[string]struct {
		annotations map[string]string
		edit        edit
		code        codes.Code
	}{
		"no uid annotation": {annotations: map[string]string{AnnotationGID: "1000"}, code: invalid},
		"uid not a number":  {annotations: map[string]string{AnnotationUID: "alice"}, code: invalid},
		"negative uid":      {annotations: map[string]string{AnnotationUID: "-1"}, code: invalid},
		"uid above 32 bits": {annotations: map[string]string{AnnotationUID: "4294967296"}, code: invalid},
		"empty uid":         {annotations: map[string]string{AnnotationUID: ""}, code: invalid},
		"plus-signed uid":   {annotations: map[string]string{AnnotationUID: "+1"}, code: invalid},
		"padded uid":        {annotations: map[string]string{AnnotationUID: " 1000"}, code: invalid},
		"exponent uid":      {annotations: map[string]string{AnnotationUID: "1e3"}, code: invalid},
		"invalid gid": {
			annotations: map[string]string{AnnotationUID: "1000", AnnotationGID: "x"}, code: invalid,
		},
		"unknown parameter": {edit: func(r *csi.CreateVolumeRequest) { r.Parameters["foo"] = "bar" }, code: invalid},
		"invalid versioning": {
			edit: func(r *csi.CreateVolumeRequest) { r.MutableParameters = map[string]string{"versioning": "maybe"} },
			code: invalid,
		},
		"unknown mutable": {
			edit: func(r *csi.CreateVolumeRequest) { r.MutableParameters = map[string]string{"iops": "1"} },
			code: invalid,
		},
		"no claim metadata": {
			edit: func(r *csi.CreateVolumeRequest) { delete(r.Parameters, pvcNameParameter) }, code: invalid,
		},
		"claim not found": {
			edit: func(r *csi.CreateVolumeRequest) { r.Parameters[pvcNameParameter] = "gone" }, code: codes.NotFound,
		},
		"required above limit": {
			edit: func(r *csi.CreateVolumeRequest) { r.CapacityRange.LimitBytes = 1 }, code: codes.OutOfRange,
		},
		"limit below the floor": {
			edit: func(r *csi.CreateVolumeRequest) { r.CapacityRange = &csi.CapacityRange{LimitBytes: 10 << 30} },
			code: codes.OutOfRange,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeRetyc(t)
			annotations := tc.annotations
			if annotations == nil {
				annotations = map[string]string{AnnotationUID: "1000"}
			}
			s := f.controller(claim(annotations))
			req := createRequest()
			if tc.edit != nil {
				tc.edit(req)
			}
			_, err := s.CreateVolume(context.Background(), req)
			wantCode(t, err, tc.code)
			if got := f.calls(); len(got) != 0 {
				t.Fatalf("the CLI ran: %q", got)
			}
		})
	}
}

func TestCreateVolume_QuotaAndCLIErrors(t *testing.T) {
	ok := claim(map[string]string{AnnotationUID: "1000"})

	f := newFakeRetyc(t)
	f.set("quota.json", `{"count_dataroom":10,"max_count_dataroom":10}`)
	_, err := f.controller(ok).CreateVolume(context.Background(), createRequest())
	wantCode(t, err, codes.ResourceExhausted)

	f = newFakeRetyc(t)
	f.set("create.err", `{"error":"API error 403: plan storage exceeded"}`)
	_, err = f.controller(ok).CreateVolume(context.Background(), createRequest())
	wantCode(t, err, codes.Internal)
	if !strings.Contains(err.Error(), "plan storage exceeded") {
		t.Fatalf("error = %v, want the CLI's message", err)
	}

	f = newFakeRetyc(t)
	f.set("ls.err", `{"error":"not authenticated"}`)
	_, err = f.controller(ok).CreateVolume(context.Background(), createRequest())
	wantCode(t, err, codes.Internal)

	// The API's refusals of a reservation.
	for msg, code := range map[string]codes.Code{
		`API error 400: {\"detail\":\"Not enough storage left on your plan\"}`:                codes.ResourceExhausted,
		`API error 422: {\"detail\":\"storage_capacity must be at least 21474836480 bytes\"}`: codes.OutOfRange,
		`API error 422: {\"detail\":\"title: field required\"}`:                               codes.InvalidArgument,
	} {
		f = newFakeRetyc(t)
		f.set("create.err", `{"error":"`+msg+`"}`)
		_, err = f.controller(ok).CreateVolume(context.Background(), createRequest())
		wantCode(t, err, code)
		if !strings.Contains(err.Error(), "API error") {
			t.Fatalf("error = %v, want the API's message", err)
		}
	}
}

// The API refuses a reservation under 20 GiB: smaller requests are rounded up to it.
func TestCreateVolume_CapacityFloor(t *testing.T) {
	ok := claim(map[string]string{AnnotationUID: "1000"})
	f := newFakeRetyc(t)
	req := createRequest()
	req.CapacityRange = &csi.CapacityRange{RequiredBytes: 1 << 30}
	resp, err := f.controller(ok).CreateVolume(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetVolume().GetCapacityBytes(); got != MinDataroomCapacity {
		t.Fatalf("capacity = %d, want %d", got, MinDataroomCapacity)
	}
	if calls := f.calls(); calls[len(calls)-1] != "default dataroom create --title pvc-1 --storage-capacity 20GiB" {
		t.Fatalf("calls = %q", calls)
	}

	// A limit at the floor is met; no range, no reservation.
	for _, tc := range []struct {
		r    *csi.CapacityRange
		want string
	}{
		{&csi.CapacityRange{RequiredBytes: 1 << 30, LimitBytes: 20 << 30}, " --storage-capacity 20GiB"},
		{nil, ""},
	} {
		f = newFakeRetyc(t)
		req.CapacityRange = tc.r
		if _, err := f.controller(ok).CreateVolume(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if calls := f.calls(); calls[len(calls)-1] != "default dataroom create --title pvc-1"+tc.want {
			t.Fatalf("range %v: calls = %q", tc.r, calls)
		}
	}
}

func TestControllerExpandVolume(t *testing.T) {
	f := newFakeRetyc(t)
	s := f.controller()
	resp, err := s.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId:      "dr-1",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 20 << 30},
		Secrets:       machineSecrets("tenant"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCapacityBytes() != 20<<30 || resp.GetNodeExpansionRequired() {
		t.Fatalf("response = %+v", resp)
	}
	if got := f.calls(); len(got) != 2 || got[1] != "tenant dataroom set retyc://dr-1 --storage-capacity 20GiB -y" {
		t.Fatalf("calls = %q", got)
	}

	_, err = s.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{VolumeId: "dr-1"})
	wantCode(t, err, codes.InvalidArgument)

	f.set("set.err", `{"error":"API error 400: a storage reservation can only be enlarged"}`)
	_, err = s.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "dr-1", CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30},
	})
	wantCode(t, err, codes.OutOfRange)

	f.set("set.err", `{"error":"API error 404: {\"detail\":\"Dataroom not found\"}"}`)
	_, err = s.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "dr-404", CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30},
	})
	wantCode(t, err, codes.NotFound)

	f.set("set.err", `{"error":"API error 400: {\"detail\":\"Not enough storage left on your plan\"}"}`)
	_, err = s.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "dr-1", CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 40},
	})
	wantCode(t, err, codes.ResourceExhausted)
}

// The kinds of a CLI with structured errors decide the code, whatever the text says; without a kind
// (an older CLI), the text does (see TestCreateVolume_QuotaAndCLIErrors).
func TestCLIErrorKinds(t *testing.T) {
	ok := claim(map[string]string{AnnotationUID: "1000"})
	stderr := func(kind string) string {
		return `{"error":"API error 400: {\"detail\":\"something\"}","kind":"` + kind + `","status":400}`
	}
	for kind, code := range map[string]codes.Code{
		"plan_exceeded":      codes.ResourceExhausted,
		"storage_exceeded":   codes.ResourceExhausted,
		"capacity_too_small": codes.OutOfRange,
		"capacity_shrink":    codes.OutOfRange,
		"invalid":            codes.InvalidArgument,
		"auth_required":      codes.FailedPrecondition,
		"unauthenticated":    codes.FailedPrecondition,
		"config":             codes.FailedPrecondition,
		"cli_too_old":        codes.FailedPrecondition,
		"forbidden":          codes.PermissionDenied,
		"rate_limited":       codes.Unavailable,
		"api_error":          codes.Internal,
		"error":              codes.Internal,
		"some_future_kind":   codes.Internal,
	} {
		t.Run(kind, func(t *testing.T) {
			f := newFakeRetyc(t)
			f.set("create.err", stderr(kind))
			_, err := f.controller(ok).CreateVolume(context.Background(), createRequest())
			wantCode(t, err, code)
			if !strings.Contains(err.Error(), "API error 400") {
				t.Fatalf("error = %v, want the CLI's message", err)
			}
		})
	}

	// The listing, the quota and the delete go through the same mapping.
	f := newFakeRetyc(t)
	f.set("ls.err", `{"error":"not logged in: run retyc auth login","kind":"auth_required"}`)
	_, err := f.controller(ok).CreateVolume(context.Background(), createRequest())
	wantCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "Organization > Machines") {
		t.Fatalf("error = %v, want a hint on the machine account", err)
	}
	f = newFakeRetyc(t)
	f.set("quota.err", `{"error":"API error 426: upgrade","kind":"cli_too_old","status":426}`)
	_, err = f.controller(ok).CreateVolume(context.Background(), createRequest())
	wantCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "too old") {
		t.Fatalf("error = %v, want a hint on the CLI version", err)
	}
	f = newFakeRetyc(t)
	f.set("rm.err", `{"error":"deleting dataroom: API error 404: {\"detail\":\"Dataroom not found\"}",`+
		`"kind":"not_found","status":404,"detail":"Dataroom not found"}`)
	deleteVolume := func() error {
		_, err := f.controller().DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: "dr-1"})

		return err
	}
	if err := deleteVolume(); err != nil {
		t.Fatalf("DeleteVolume of a dataroom gone: %v", err)
	}
	f.set("rm.err", `{"error":"API error 429: slow down","kind":"rate_limited","status":429}`)
	wantCode(t, deleteVolume(), codes.Unavailable)

	// A kind wins over the text: a 404 the CLI classifies as something else is no missing dataroom.
	f.set("rm.err", `{"error":"deleting dataroom: API error 404: x","kind":"api_error","status":404}`)
	wantCode(t, deleteVolume(), codes.Internal)

	f = newFakeRetyc(t)
	f.set("set.err", `{"error":"updating dataroom: API error 404: x","kind":"not_found","status":404}`)
	_, err = f.controller().ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "dr-404", CapacityRange: &csi.CapacityRange{RequiredBytes: 30 << 30},
	})
	wantCode(t, err, codes.NotFound)
}

func TestControllerExpandVolume_CapacityFloor(t *testing.T) {
	f := newFakeRetyc(t)
	s := f.controller()
	resp, err := s.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "dr-1", CapacityRange: &csi.CapacityRange{RequiredBytes: 5 << 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetCapacityBytes() != MinDataroomCapacity {
		t.Fatalf("capacity = %d, want %d", resp.GetCapacityBytes(), MinDataroomCapacity)
	}
	if calls := f.calls(); calls[len(calls)-1] != "default dataroom set retyc://dr-1 --storage-capacity 20GiB -y" {
		t.Fatalf("calls = %q", calls)
	}
	_, err = s.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "dr-1", CapacityRange: &csi.CapacityRange{RequiredBytes: 5 << 30, LimitBytes: 10 << 30},
	})
	wantCode(t, err, codes.OutOfRange)
}

func TestControllerExpandVolume_AlreadyReserved(t *testing.T) {
	f := newFakeRetyc(t)
	f.set("ls.json", `{"items":[{"id":"dr-1","title":"pvc-1","storage_capacity":21474836480}]}`)
	s := f.controller()
	// Equal and smaller requests succeed without `set`, reporting the real reservation.
	for _, required := range []int64{20 << 30, 10 << 30} {
		resp, err := s.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
			VolumeId: "dr-1", CapacityRange: &csi.CapacityRange{RequiredBytes: required},
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetCapacityBytes() != 20<<30 {
			t.Fatalf("capacity = %d", resp.GetCapacityBytes())
		}
	}
	for _, c := range f.calls() {
		if strings.Contains(c, "dataroom set") {
			t.Fatalf("set ran: %q", f.calls())
		}
	}
}

func TestControllerUpdates_TenantSecrets(t *testing.T) {
	ctx := context.Background()
	partial := map[string]string{"RETYC_TOKEN": "tenant"} // a former token secret
	expand := func(s *ControllerServer, secrets map[string]string) error {
		_, err := s.ControllerExpandVolume(ctx, &csi.ControllerExpandVolumeRequest{
			VolumeId: "dr-1", CapacityRange: &csi.CapacityRange{RequiredBytes: 1 << 30}, Secrets: secrets,
		})

		return err
	}
	modify := func(s *ControllerServer, secrets map[string]string) error {
		_, err := s.ControllerModifyVolume(ctx, &csi.ControllerModifyVolumeRequest{
			VolumeId: "dr-1", MutableParameters: map[string]string{versioningParameter: "true"}, Secrets: secrets,
		})

		return err
	}
	for name, op := range map[string]func(*ControllerServer, map[string]string) error{
		"expand": expand, "modify": modify,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeRetyc(t)
			// Partial tenant secrets are refused rather than silently falling back to the default.
			wantCode(t, op(f.controller(), partial), codes.InvalidArgument)

			// No secrets and no default identity: nothing to authenticate with.
			s := f.controller()
			s.IdentityDir = t.TempDir()
			wantCode(t, op(s, nil), codes.FailedPrecondition)

			if got := f.calls(); len(got) != 0 {
				t.Fatalf("the CLI ran: %q", got)
			}
		})
	}
}

func TestControllerModifyVolume(t *testing.T) {
	f := newFakeRetyc(t)
	s := f.controller()
	ctx := context.Background()

	_, err := s.ControllerModifyVolume(ctx, &csi.ControllerModifyVolumeRequest{
		VolumeId: "dr-1",
		// Added by csi-resizer --extra-modify-metadata: ignored.
		MutableParameters: map[string]string{versioningParameter: "false", pvcNameParameter: "data"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.calls(); len(got) != 1 || got[0] != "default dataroom set retyc://dr-1 --versioning=false -y" {
		t.Fatalf("calls = %q", got)
	}

	// No versioning parameter: nothing the driver manages changes, so OK (the resizer then
	// settles the claim) and the CLI does not run.
	if _, err := s.ControllerModifyVolume(ctx, &csi.ControllerModifyVolumeRequest{VolumeId: "dr-1"}); err != nil {
		t.Fatal(err)
	}
	if got := f.calls(); len(got) != 1 {
		t.Fatalf("calls = %q", got)
	}

	_, err = s.ControllerModifyVolume(ctx, &csi.ControllerModifyVolumeRequest{
		VolumeId: "dr-1", MutableParameters: map[string]string{"compression": "on"},
	})
	wantCode(t, err, codes.InvalidArgument)

	f.set("set.err", `{"error":"API error 500: boom"}`)
	_, err = s.ControllerModifyVolume(ctx, &csi.ControllerModifyVolumeRequest{
		VolumeId: "dr-1", MutableParameters: map[string]string{versioningParameter: "true"},
	})
	wantCode(t, err, codes.Internal)
}

func TestControllerGetCapabilities(t *testing.T) {
	resp, err := (&ControllerServer{}).ControllerGetCapabilities(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[csi.ControllerServiceCapability_RPC_Type]bool{}
	for _, c := range resp.GetCapabilities() {
		got[c.GetRpc().GetType()] = true
	}
	for _, want := range []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		csi.ControllerServiceCapability_RPC_MODIFY_VOLUME,
	} {
		if !got[want] {
			t.Errorf("missing capability %s", want)
		}
	}
}

func TestClaimOwner(t *testing.T) {
	uid, gid, err := claimOwner(map[string]string{AnnotationUID: "1000"})
	if err != nil || uid != 1000 || gid != 1000 {
		t.Fatalf("gid must default to the uid: %d, %d, %v", uid, gid, err)
	}
	uid, gid, err = claimOwner(map[string]string{AnnotationUID: "0", AnnotationGID: "33"})
	if err != nil || uid != 0 || gid != 33 {
		t.Fatalf("explicit root uid and gid: %d, %d, %v", uid, gid, err)
	}
}

func TestClaimNamespace_OnlyNamespaceNames(t *testing.T) {
	for value, want := range map[string]string{
		"team-a":   "team-a",
		"":         "",
		"_default": "", // would read as the cluster-wide identity's tenant
		"a,b":      "",
		"Team-A":   "",
		"-team":    "",
	} {
		if got := claimNamespace(map[string]string{volumeContextNamespace: value}); got != want {
			t.Errorf("claimNamespace(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestIsSupportedCapability_AccessModes(t *testing.T) {
	mount := &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}
	for mode, want := range map[csi.VolumeCapability_AccessMode_Mode]bool{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:        true,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER: true, // ReadWriteOncePod
		csi.VolumeCapability_AccessMode_SINGLE_NODE_MULTI_WRITER:  true,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER:   true,
		csi.VolumeCapability_AccessMode_UNKNOWN:                   false,
	} {
		am := &csi.VolumeCapability_AccessMode{Mode: mode}
		got := isSupportedCapability(&csi.VolumeCapability{AccessType: mount, AccessMode: am})
		if got != want {
			t.Errorf("mode %v supported = %v, want %v", mode, got, want)
		}
	}
	block := &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_SINGLE_WRITER},
	}
	if isSupportedCapability(block) {
		t.Error("block volume accepted")
	}
}

// A claim of a tenant class (provisioner-secret-name) whose request carries empty secrets is
// refused, never created with the driver's identity; a plain class, or secrets, go through.
func TestCreateVolume_TenantClassWithoutSecrets(t *testing.T) {
	f := newFakeRetyc(t)
	class := "tenant"
	pvc := claim(map[string]string{AnnotationUID: "1000"})
	pvc.Spec.StorageClassName = &class
	s := f.controller(pvc)
	ctx := context.Background()
	mkClass := func(params map[string]string) {
		t.Helper()
		sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: class}, Provisioner: DriverName, Parameters: params}
		if _, err := s.Kube.StorageV1().StorageClasses().Create(ctx, sc, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	// Class not found: nothing to check.
	if _, err := s.CreateVolume(ctx, createRequest()); err != nil {
		t.Fatalf("class not found: %v", err)
	}

	mkClass(map[string]string{providerSecretParam: "creds"})
	before := len(f.calls())
	for _, secrets := range []map[string]string{nil, {}} {
		req := createRequest()
		req.Secrets = secrets
		_, err := s.CreateVolume(ctx, req)
		wantCode(t, err, codes.FailedPrecondition)
		if !strings.Contains(err.Error(), "tenant class") {
			t.Fatalf("error = %v", err)
		}
	}
	if n := len(f.calls()); n != before {
		t.Fatalf("the CLI ran %d times for a refused request", n-before)
	}

	// With the tenant's secrets: accepted.
	req := createRequest()
	req.Secrets = machineSecrets("tenant-key")
	if _, err := s.CreateVolume(ctx, req); err != nil {
		t.Fatalf("with secrets: %v", err)
	}
}
