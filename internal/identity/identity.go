// Package identity models one Retyc identity, a machine account: the set of files `retyc machine
// enroll` writes (signing key, AGE key, enrollment and machine records), plus the k8s.json record
// of the driver's enroller. It comes from the CSI secrets that kubelet and external-provisioner
// resolve from a StorageClass's secret parameters, or from the directory where the driver's own
// Secret is mounted (the cluster-wide default identity).
package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

// Names of the files of an identity, also the keys of a credentials Secret. The first four are
// the CLI's (`retyc machine enroll --output-dir`); K8sFile is the driver's own.
const (
	ClientKeyFile  = "client.key"
	AgeKeyFile     = "age.key"
	EnrollmentFile = "enrollment.json"
	MachineFile    = "machine.json"
	K8sFile        = "k8s.json"
)

// MachineDirKey is the environment variable that points the retyc CLI at a machine account
// directory.
const MachineDirKey = "RETYC_MACHINE_DIR"

// FileNames are every file an identity may hold.
var FileNames = []string{ClientKeyFile, AgeKeyFile, EnrollmentFile, MachineFile, K8sFile}

// cliFiles are the files the CLI reads in machine mode, and those that make up Key.
var cliFiles = []string{ClientKeyFile, AgeKeyFile, MachineFile}

// ErrIncomplete is wrapped when an identity lacks a file the CLI needs. ErrPending is wrapped
// (with ErrIncomplete) when only machine.json is missing: the enrollment awaits an approval.
var (
	ErrIncomplete = errors.New("incomplete Retyc machine identity")
	ErrPending    = errors.New("machine enrollment awaiting approval")
)

// ErrNamespaceMismatch is wrapped when a tenant identity is used for another namespace than the
// one it was enrolled for.
var ErrNamespaceMismatch = errors.New("identity enrolled for another namespace")

// Credentials is one Retyc machine identity: file name → content. Never log the contents.
type Credentials struct {
	Files map[string][]byte
}

// FromSecrets extracts an identity from a CSI request's Secrets map. An empty map means "use the
// driver's default identity" and yields nil, nil. Keys other than FileNames are ignored; a map
// with none of the files is a misconfigured Secret and is an error.
func FromSecrets(secrets map[string]string) (*Credentials, error) {
	if len(secrets) == 0 {
		return nil, nil //nolint:nilnil // nil credentials mean "default identity", a valid outcome
	}
	data := make(map[string][]byte, len(secrets))
	for k, v := range secrets {
		data[k] = []byte(v)
	}
	if c := FromData(data); c != nil {
		return c, nil
	}

	return nil, fmt.Errorf("credentials secret holds none of %s, %s, %s: a machine account from `retyc machine "+
		"enroll` is expected (RETYC_TOKEN and RETYC_KEY_PASSPHRASE are no longer supported)",
		ClientKeyFile, AgeKeyFile, MachineFile)
}

// FromData keeps the identity files of a Secret's data, or returns nil when it holds none.
func FromData(data map[string][]byte) *Credentials {
	c := &Credentials{Files: map[string][]byte{}}
	for _, name := range FileNames {
		if v, ok := data[name]; ok && len(v) > 0 {
			c.Files[name] = slices.Clone(v)
		}
	}
	if len(c.Files) == 0 {
		return nil
	}

	return c
}

// FromDir reads the identity files present in dir, where the driver's Secret is mounted (an
// optional volume: it may be missing or partial). It returns nil, nil when dir holds none of
// them, an error only when a file exists but cannot be read.
func FromDir(dir string) (*Credentials, error) {
	data := map[string][]byte{}
	for _, name := range FileNames {
		// A mounted Secret's files are symlinks into a ..data directory: ReadFile follows them.
		v, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: the operator's identity directory
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("reading identity file %s: %w", name, err)
		}
		data[name] = v
	}

	return FromData(data), nil
}

// Complete reports whether the CLI can run with the identity: the keys and machine.json. Without
// machine.json only, the enrollment awaits an administrator's approval: the error wraps ErrPending
// and names the fingerprint to compare when it is known.
func (c *Credentials) Complete() error {
	if c == nil {
		return fmt.Errorf("%w: no identity files", ErrIncomplete)
	}
	for _, name := range []string{ClientKeyFile, AgeKeyFile} {
		if len(c.Files[name]) == 0 {
			return fmt.Errorf("%w: %s is missing", ErrIncomplete, name)
		}
	}
	if len(c.Files[MachineFile]) == 0 {
		msg := "approve it in Organization > Machines of the Retyc web app"
		if fp := c.fingerprint(); fp != "" {
			msg += ", after checking its fingerprint " + fp
		}

		return fmt.Errorf("%w: %w (%s is missing): %s", ErrIncomplete, ErrPending, MachineFile, msg)
	}
	if !json.Valid(c.Files[MachineFile]) {
		return fmt.Errorf("%w: %s is not valid JSON", ErrIncomplete, MachineFile)
	}

	return nil
}

// fingerprint is the enrollment's fingerprint, from enrollment.json or k8s.json, "" if neither
// records it (the CLI's enrollment.json does not, as of retyc-cli 1.5).
func (c *Credentials) fingerprint() string {
	for _, name := range []string{EnrollmentFile, K8sFile} {
		var v struct {
			Fingerprint string `json:"fingerprint"`
		}
		if json.Unmarshal(c.Files[name], &v) == nil && v.Fingerprint != "" {
			return v.Fingerprint
		}
	}

	return ""
}

// Key is a short, stable, non-reversible identifier of the identity, used to name and hash the
// per-identity resources. It covers the keys and machine.json only: enrollment.json and k8s.json
// may be rewritten without the identity changing. Never log the inputs; the key is safe to log.
func (c *Credentials) Key() string {
	h := sha256.New()
	for _, name := range cliFiles {
		v := c.Files[name]
		// Length-prefixed, so that no two sets of contents hash alike.
		h.Write([]byte(name + "\x00" + strconv.Itoa(len(v)) + "\x00"))
		h.Write(v)
	}

	return hex.EncodeToString(h.Sum(nil)[:8])
}

// MachineFiles returns the files the CLI reads in machine mode (client.key, age.key, machine.json),
// those present.
func (c *Credentials) MachineFiles() map[string][]byte {
	out := make(map[string][]byte, len(cliFiles))
	for _, name := range cliFiles {
		if v, ok := c.Files[name]; ok {
			out[name] = v
		}
	}

	return out
}

// Materialize writes the files the CLI reads into dir, created 0700 if needed, each 0600: the CLI
// refuses a key file that others can access. An existing dir that group or others can access is
// refused.
func (c *Credentials) Materialize(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating identity directory: %w", err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("identity directory: %w", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("identity directory %s has mode %04o, want 0700", dir, st.Mode().Perm())
	}
	for name, v := range c.MachineFiles() {
		if err := os.WriteFile(filepath.Join(dir, name), v, 0o600); err != nil {
			return fmt.Errorf("writing identity file %s: %w", name, err)
		}
	}

	return nil
}

// Modes of an identity (k8s.json): one for the whole cluster, or one per tenant namespace.
const (
	ModeCluster = "cluster"
	ModeTenant  = "tenant"
)

// K8sInfo is the content of k8s.json, written by the driver's enroller next to the CLI's files.
// It ties a tenant identity to the namespace it was enrolled for.
type K8sInfo struct {
	// Mode is ModeCluster or ModeTenant.
	Mode string `json:"mode"`
	// Namespace and NamespaceUID are the tenant's namespace (ModeTenant only); the UID tells a
	// namespace from a later one of the same name.
	Namespace    string `json:"namespace,omitempty"`
	NamespaceUID string `json:"namespaceUID,omitempty"`
	// ClusterUID is the value sent as `retyc machine enroll --cluster-uid`: the UID of kube-system
	// for the cluster identity, the UID of the tenant's namespace for a tenant one.
	ClusterUID string `json:"clusterUID,omitempty"`
	// Fingerprint is the one the enrollment printed (pending event), for the administrator to
	// compare while it awaits approval.
	Fingerprint string `json:"fingerprint,omitempty"`
}

// K8s returns the identity's k8s.json, nil when it has none (an identity enrolled by hand).
func (c *Credentials) K8s() (*K8sInfo, error) {
	raw, ok := c.Files[K8sFile]
	if !ok {
		return nil, nil //nolint:nilnil // no k8s.json is a valid outcome
	}
	var info K8sInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON", K8sFile)
	}
	switch info.Mode {
	case ModeCluster:
	case ModeTenant:
		if info.NamespaceUID == "" {
			return nil, fmt.Errorf("%s: mode %q without namespaceUID", K8sFile, ModeTenant)
		}
	default:
		return nil, fmt.Errorf("%s: mode %q: want %q or %q", K8sFile, info.Mode, ModeCluster, ModeTenant)
	}

	return &info, nil
}

// CheckNamespace refuses a tenant identity used for a volume of another namespace than its own:
// a Secret copied elsewhere does not carry the identity along. namespaceUID is the UID of the
// volume's namespace. An identity without k8s.json, or of the cluster mode, is not restricted.
func (c *Credentials) CheckNamespace(namespace, namespaceUID string) error {
	info, err := c.K8s()
	if err != nil {
		return err
	}
	if info == nil || info.Mode != ModeTenant {
		return nil
	}
	if info.NamespaceUID != namespaceUID {
		return fmt.Errorf("%w: enrolled for namespace %q (UID %s), used in namespace %q (UID %s): enroll one "+
			"for this namespace", ErrNamespaceMismatch, info.Namespace, info.NamespaceUID, namespace, namespaceUID)
	}

	return nil
}
