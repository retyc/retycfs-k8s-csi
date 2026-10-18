package driver

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/retyc/retycfs-k8s-csi/internal/identity"
)

// tenantSecrets are machineSecrets with a k8s.json enrolling them for the namespace of UID nsUID.
func tenantSecrets(key, nsUID string) map[string]string {
	s := machineSecrets(key)
	s[identity.K8sFile] = `{"mode":"tenant","namespace":"team-a","namespaceUID":"` + nsUID + `"}`

	return s
}

// recorded drains the events of a fake recorder.
func recorded(s *ControllerServer) []string {
	s.members.Wait()
	var out []string
	ch := s.Events.(*record.FakeRecorder).Events
	for {
		select {
		case ev := <-ch:
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestParseInitialMembers(t *testing.T) {
	members, err := parseInitialMembers(map[string]string{
		AnnotationInitialMembers: "alice@acme.fr:admin, bob@acme.fr:editor ,carol@acme.fr",
	})
	want := []member{{"alice@acme.fr", "admin"}, {"bob@acme.fr", "editor"}, {"carol@acme.fr", "viewer"}}
	if err != nil || len(members) != 3 || members[0] != want[0] || members[1] != want[1] || members[2] != want[2] {
		t.Fatalf("members = %v, %v", members, err)
	}
	if m, err := parseInitialMembers(map[string]string{}); m != nil || err != nil {
		t.Fatalf("no annotation: %v, %v", m, err)
	}
	for _, raw := range []string{
		"", " ", "alice@acme.fr,", ",alice@acme.fr", "alice@acme.fr,,bob@acme.fr",
		"alice@acme.fr:owner", "alice@acme.fr:", "alice@acme.fr:Admin", "alice@acme.fr:admin:x",
		"alice", "Alice <alice@acme.fr>", "<alice@acme.fr>",
		"alice@acme.fr,ALICE@acme.fr:admin",
		"-alice@acme.fr", "alice@acme.fr, -x@acme.fr:admin", "--role@acme.fr",
	} {
		if m, err := parseInitialMembers(map[string]string{AnnotationInitialMembers: raw}); err == nil {
			t.Errorf("%q accepted as %v", raw, m)
		}
	}
}

func TestCreateVolume_InitialMembers(t *testing.T) {
	f := newFakeRetyc(t)
	s := f.controller(claim(map[string]string{
		AnnotationUID: "1000", AnnotationInitialMembers: "alice@acme.fr:admin,bob@acme.fr",
	}))
	if _, err := s.CreateVolume(context.Background(), createRequest()); err != nil {
		t.Fatal(err)
	}
	s.members.Wait()
	calls := f.calls()
	want := []string{
		"default dataroom user add --role admin -- dr-new alice@acme.fr",
		"default dataroom user add --role viewer -- dr-new bob@acme.fr",
	}
	if len(calls) != 5 || strings.Join(calls[3:], "|") != strings.Join(want, "|") {
		t.Fatalf("calls = %q", calls)
	}
	if evs := recorded(s); len(evs) != 0 {
		t.Fatalf("events = %q, want none", evs)
	}

	// A retry finding the dataroom does not add them again, and says so.
	f.set("ls.json", `{"items":[{"id":"dr-new","title":"pvc-1"}],"total":1,"page":1,"pages":1}`)
	if _, err := s.CreateVolume(context.Background(), createRequest()); err != nil {
		t.Fatal(err)
	}
	s.members.Wait()
	if n := len(f.calls()); n != 6 {
		t.Fatalf("calls after the retry = %q", f.calls())
	}
	if evs := recorded(s); len(evs) != 1 || !strings.HasPrefix(evs[0], "Normal "+reasonInitialMembers+" ") ||
		!strings.Contains(evs[0], "already exists") {
		t.Fatalf("events = %q", evs)
	}
}

// A member that cannot be added does not fail the volume, is reported and not retried; without
// an admin added, the claim is warned.
func TestCreateVolume_InitialMembersFailure(t *testing.T) {
	f := newFakeRetyc(t)
	f.set("member.err", `{"error":"adding user: API error 404: unknown user","kind":"not_found","status":404}`)
	s := f.controller(claim(map[string]string{
		AnnotationUID: "1000", AnnotationInitialMembers: "alice@acme.fr:admin",
	}))
	resp, err := s.CreateVolume(context.Background(), createRequest())
	if err != nil || resp.GetVolume().GetVolumeId() != "dr-new" {
		t.Fatalf("CreateVolume = %v, %v; want the volume despite the member", resp, err)
	}
	s.members.Wait()
	if n := strings.Count(strings.Join(f.calls(), "\n"), "user add"); n != 1 {
		t.Fatalf("calls = %q, want one try", f.calls())
	}
	evs := strings.Join(recorded(s), "\n")
	if !strings.Contains(evs, "Warning "+reasonInitialMembers+" ") || !strings.Contains(evs, "alice@acme.fr") ||
		!strings.Contains(evs, "Warning "+reasonNoAdmin+" ") {
		t.Fatalf("events = %q", evs)
	}

	// No annotation: no member, and the warning that only the machine can transfer the dataroom.
	f = newFakeRetyc(t)
	s = f.controller(claim(map[string]string{AnnotationUID: "1000"}))
	if _, err := s.CreateVolume(context.Background(), createRequest()); err != nil {
		t.Fatal(err)
	}
	if evs := recorded(s); len(evs) != 1 || !strings.Contains(evs[0], reasonNoAdmin) {
		t.Fatalf("events = %q", evs)
	}
}

func TestCreateVolume_InitialMembersInvalid(t *testing.T) {
	f := newFakeRetyc(t)
	s := f.controller(claim(map[string]string{AnnotationUID: "1000", AnnotationInitialMembers: "alice:boss"}))
	_, err := s.CreateVolume(context.Background(), createRequest())
	wantCode(t, err, codes.InvalidArgument)
	if got := f.calls(); len(got) != 0 {
		t.Fatalf("the CLI ran: %q", got)
	}
}

// A tenant identity is used only in the namespace it was enrolled for, told by its UID.
func TestCreateVolume_TenantNamespace(t *testing.T) {
	f := newFakeRetyc(t)
	s := f.controller(claim(map[string]string{AnnotationUID: "1000"}))
	req := createRequest()
	req.Secrets = tenantSecrets("tenant", "uid-team-a")
	if _, err := s.CreateVolume(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	f = newFakeRetyc(t)
	s = f.controller(claim(map[string]string{AnnotationUID: "1000"}))
	req.Secrets = tenantSecrets("tenant", "uid-of-a-former-team-a")
	_, err := s.CreateVolume(context.Background(), req)
	wantCode(t, err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), "another namespace") {
		t.Fatalf("error = %v", err)
	}
	if got := f.calls(); len(got) != 0 {
		t.Fatalf("the CLI ran: %q", got)
	}
}

// The default identity awaiting approval: Unavailable, with the fingerprint, nothing run.
func TestCreateVolume_PendingIdentity(t *testing.T) {
	f := newFakeRetyc(t)
	s := f.controller(claim(map[string]string{AnnotationUID: "1000"}))
	s.IdentityDir = identityDir(t, map[string]string{
		identity.ClientKeyFile: "k", identity.AgeKeyFile: "a",
		identity.EnrollmentFile: `{"version":1,"enrollment_id":"e-1"}`,
		identity.K8sFile:        `{"mode":"cluster","fingerprint":"K7QF-2MXA-PD9R-H3TB"}`,
	})
	_, err := s.CreateVolume(context.Background(), createRequest())
	wantCode(t, err, codes.Unavailable)
	if !strings.Contains(err.Error(), "K7QF-2MXA-PD9R-H3TB") {
		t.Fatalf("error = %v, want the fingerprint", err)
	}
	if got := f.calls(); len(got) != 0 {
		t.Fatalf("the CLI ran: %q", got)
	}
}

func TestNodeStageTenantNamespace(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	if _, err := e.kube.CoreV1().Namespaces().Create(context.Background(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "uid-team"}},
		metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := e.s.NodeStageVolume(context.Background(), e.stageRequest(tenantSecrets("t", "uid-elsewhere")))
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "another namespace") {
		t.Fatalf("stage with a tenant identity of another namespace = %v", err)
	}
	if !e.podGone() {
		t.Fatal("a mount pod was created")
	}
	if c := e.stage(e.stageRequest(tenantSecrets("t", "uid-team"))); c != codes.Aborted {
		t.Fatalf("stage in its own namespace = %v, want Aborted (pod starting)", c)
	}

	// Without a claim, a tenant identity cannot be checked: refused.
	e2 := newNodeEnv(t)
	e2.volume()
	req := e2.stageRequest(tenantSecrets("t", "uid-team"))
	req.VolumeContext = map[string]string{AnnotationUID: "1000"}
	if _, err := e2.s.NodeStageVolume(context.Background(), req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("stage of a tenant identity without a claim = %v", err)
	}
}

func TestNodeStagePendingIdentity(t *testing.T) {
	e := newNodeEnv(t)
	e.claim(map[string]string{AnnotationUID: "1000"})
	e.s.IdentityDir = identityDir(t, map[string]string{identity.ClientKeyFile: "k", identity.AgeKeyFile: "a"})
	_, err := e.s.NodeStageVolume(context.Background(), e.stageRequest(nil))
	if status.Code(err) != codes.Unavailable || !strings.Contains(err.Error(), "awaiting approval") {
		t.Fatalf("stage with a pending identity = %v, want Unavailable", err)
	}
	e.s.IdentityDir = t.TempDir()
	if _, err := e.s.NodeStageVolume(context.Background(), e.stageRequest(nil)); status.Code(err) !=
		codes.FailedPrecondition {
		t.Fatalf("stage without any identity = %v, want FailedPrecondition", err)
	}
	if !e.podGone() {
		t.Fatal("a mount pod was created")
	}
}
