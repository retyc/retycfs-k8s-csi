package identity

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func complete() *Credentials {
	return &Credentials{Files: map[string][]byte{
		ClientKeyFile:  []byte("signing"),
		AgeKeyFile:     []byte("AGE-SECRET-KEY-1"),
		MachineFile:    []byte(`{"version":1,"client_id":"machine-1"}`),
		EnrollmentFile: []byte(`{"version":1,"enrollment_id":"e-1"}`),
	}}
}

func TestFromSecrets(t *testing.T) {
	if c, err := FromSecrets(nil); c != nil || err != nil {
		t.Fatalf("empty secrets must mean default identity, got %+v, %v", c, err)
	}
	if _, err := FromSecrets(map[string]string{"RETYC_TOKEN": "t", "RETYC_KEY_PASSPHRASE": "p"}); err == nil ||
		!strings.Contains(err.Error(), "no longer supported") {
		t.Fatalf("a token secret must be refused with an explanation: %v", err)
	}
	c, err := FromSecrets(map[string]string{ClientKeyFile: "k", AgeKeyFile: "a", "extra": "ignored"})
	if err != nil || string(c.Files[ClientKeyFile]) != "k" || len(c.Files) != 2 {
		t.Fatalf("FromSecrets: %+v, %v", c, err)
	}
}

func TestFromDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "id")
	if c, err := FromDir(dir); c != nil || err != nil {
		t.Fatalf("a missing directory is no identity: %+v, %v", c, err)
	}
	if err := complete().Materialize(dir); err != nil {
		t.Fatal(err)
	}
	c, err := FromDir(dir)
	if err != nil || c.Complete() != nil || c.Key() != complete().Key() {
		t.Fatalf("FromDir: %v, %v", c, err)
	}
}

func TestComplete(t *testing.T) {
	if err := complete().Complete(); err != nil {
		t.Fatal(err)
	}
	var nilCreds *Credentials
	if err := nilCreds.Complete(); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("nil: %v", err)
	}
	c := complete()
	delete(c.Files, AgeKeyFile)
	if err := c.Complete(); !errors.Is(err, ErrIncomplete) || errors.Is(err, ErrPending) {
		t.Fatalf("no age.key: %v", err)
	}
	c = complete()
	delete(c.Files, MachineFile)
	if err := c.Complete(); !errors.Is(err, ErrPending) || strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("pending: %v", err)
	}
	c.Files[K8sFile] = []byte(`{"mode":"cluster","fingerprint":"K7QF-2MXA"}`)
	if err := c.Complete(); !errors.Is(err, ErrPending) || !strings.Contains(err.Error(), "K7QF-2MXA") {
		t.Fatalf("pending with a fingerprint: %v", err)
	}
	c = complete()
	c.Files[MachineFile] = []byte("{")
	if err := c.Complete(); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("bad machine.json: %v", err)
	}
}

func TestKey(t *testing.T) {
	a, b := complete(), complete()
	if a.Key() != b.Key() || len(a.Key()) != 16 {
		t.Fatalf("Key must be stable and short: %q vs %q", a.Key(), b.Key())
	}
	// enrollment.json and k8s.json may change without the identity changing.
	b.Files[EnrollmentFile] = []byte(`{"version":1,"enrollment_id":"e-2"}`)
	b.Files[K8sFile] = []byte(`{"mode":"cluster"}`)
	if a.Key() != b.Key() {
		t.Fatal("the records must not change the key")
	}
	for _, name := range []string{ClientKeyFile, AgeKeyFile, MachineFile} {
		c := complete()
		c.Files[name] = append(c.Files[name], 'x')
		if c.Key() == a.Key() {
			t.Errorf("a change of %s must change the key", name)
		}
	}
	// No ambiguity between files: moving bytes from one to the next changes the key.
	c := complete()
	c.Files[ClientKeyFile] = []byte("signingA")
	c.Files[AgeKeyFile] = []byte("GE-SECRET-KEY-1")
	if c.Key() == a.Key() {
		t.Fatal("contents must be delimited")
	}
}

func TestMaterialize(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "id")
	c := complete()
	c.Files[K8sFile] = []byte(`{"mode":"cluster"}`)
	if err := c.Materialize(dir); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dir); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("dir: %v, %v", st, err)
	}
	for _, name := range []string{ClientKeyFile, AgeKeyFile, MachineFile} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v, %v", name, st, err)
		}
	}
	for _, name := range []string{EnrollmentFile, K8sFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s is not read by the CLI and must not be written: %v", name, err)
		}
	}
	open := t.TempDir()
	if err := os.Chmod(open, 0o755); err != nil { //nolint:gosec // G302: the open directory under test
		t.Fatal(err)
	}
	if err := c.Materialize(open); err == nil {
		t.Fatal("a directory others can read must be refused")
	}
}

func TestCheckNamespace(t *testing.T) {
	c := complete()
	if err := c.CheckNamespace("team-a", "uid-a"); err != nil {
		t.Fatalf("no k8s.json: %v", err)
	}
	c.Files[K8sFile] = []byte(`{"mode":"cluster","clusterUID":"c"}`)
	if err := c.CheckNamespace("team-a", "uid-a"); err != nil {
		t.Fatalf("cluster mode: %v", err)
	}
	c.Files[K8sFile] = []byte(`{"mode":"tenant","namespace":"team-a","namespaceUID":"uid-a"}`)
	if err := c.CheckNamespace("team-a", "uid-a"); err != nil {
		t.Fatalf("own namespace: %v", err)
	}
	if err := c.CheckNamespace("team-a", "uid-recreated"); !errors.Is(err, ErrNamespaceMismatch) {
		t.Fatalf("namespace recreated: %v", err)
	}
	if err := c.CheckNamespace("team-b", "uid-b"); !errors.Is(err, ErrNamespaceMismatch) {
		t.Fatalf("other namespace: %v", err)
	}
	for _, raw := range []string{`{"mode":"tenant"}`, `{"mode":"other"}`, `{`} {
		c.Files[K8sFile] = []byte(raw)
		if err := c.CheckNamespace("team-a", "uid-a"); err == nil || errors.Is(err, ErrNamespaceMismatch) {
			t.Errorf("k8s.json %s: %v, want a malformed-record error", raw, err)
		}
	}
}
