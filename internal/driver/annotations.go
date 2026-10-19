package driver

import (
	"fmt"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/retyc/retycfs-k8s-csi/internal/mountpod"
)

// PVC annotations read by the driver (see doc/configuration.md).
const (
	// AnnotationUID is the owner UID of every file in the mounted volume (`retyc fs mount --uid`).
	// Mandatory: CreateVolume refuses a claim without it.
	AnnotationUID = "csi.retycfs.com/uid"
	// AnnotationGID is the owner GID (`--gid`). Optional, defaults to the UID.
	AnnotationGID = "csi.retycfs.com/gid"

	// Mount options, read at stage (`retyc fs mount` flags). Without them the CLI defaults apply.
	AnnotationCacheTTL         = "csi.retycfs.com/cache-ttl"         // --cache-ttl, a Go duration
	AnnotationBackgroundDelete = "csi.retycfs.com/background-delete" // --background-delete, a boolean
	AnnotationUnsafeWrite      = "csi.retycfs.com/unsafe-write"      // --unsafe-write, a boolean
	AnnotationReadOnly         = "csi.retycfs.com/read-only"         // --read-only, a boolean
	// AnnotationLocalFile replaces the patterns of files never sent (--local-file), separated by
	// spaces; "none" sends every file.
	AnnotationLocalFile = "csi.retycfs.com/local-file"

	// Resources of the mount pod, over the node plugin's defaults (Kubernetes quantities).
	AnnotationMountCPURequest    = "csi.retycfs.com/mount-cpu-request"
	AnnotationMountCPULimit      = "csi.retycfs.com/mount-cpu-limit"
	AnnotationMountMemoryRequest = "csi.retycfs.com/mount-memory-request"
	AnnotationMountMemoryLimit   = "csi.retycfs.com/mount-memory-limit"

	// AnnotationInitialMembers lists the people CreateVolume adds to the dataroom it creates:
	// "alice@acme.fr:admin,bob@acme.fr:editor,carol@acme.fr", role admin, editor or viewer (the
	// default). Read at creation only: changing it later changes nothing.
	AnnotationInitialMembers = "csi.retycfs.com/initial-members"
)

// Roles of an initial member, as `retyc dataroom user add --role` takes them.
const (
	roleAdmin  = "admin"
	roleEditor = "editor"
	roleViewer = "viewer"
)

// member is one entry of AnnotationInitialMembers.
type member struct {
	Email, Role string
}

// parseInitialMembers reads AnnotationInitialMembers, strictly: comma-separated entries, each a
// bare e-mail address, optionally followed by ":" and a role; no empty entry, no address twice.
// Spaces around an entry are ignored. A claim without the annotation has no initial member.
func parseInitialMembers(annotations map[string]string) ([]member, error) {
	raw, ok := annotations[AnnotationInitialMembers]
	if !ok {
		return nil, nil
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("annotation %s = %q: %s", AnnotationInitialMembers, raw, fmt.Sprintf(format, args...))
	}
	var members []member
	seen := map[string]bool{}
	for entry := range strings.SplitSeq(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, fail("empty entry: want email[:role] separated by commas")
		}
		email, role, hasRole := strings.Cut(entry, ":")
		if !hasRole {
			role = roleViewer
		}
		switch role {
		case roleAdmin, roleEditor, roleViewer:
		default:
			return nil, fail("entry %q: role %q, want %s, %s or %s", entry, role, roleAdmin, roleEditor, roleViewer)
		}
		// A bare address only: no display name, no angle brackets, and nothing a CLI could take for a flag.
		if strings.HasPrefix(email, "-") {
			return nil, fail("entry %q: %q is not an e-mail address (it must not start with \"-\")", entry, email)
		}
		if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email || addr.Name != "" {
			return nil, fail("entry %q: %q is not an e-mail address", entry, email)
		}
		key := strings.ToLower(email)
		if seen[key] {
			return nil, fail("%s is listed twice", email)
		}
		seen[key] = true
		members = append(members, member{Email: email, Role: role})
	}

	return members, nil
}

// mountSettings are the per-volume settings of a mount pod, read from the claim's annotations.
type mountSettings struct {
	UID, GID  uint32
	Options   mountpod.Options
	Resources corev1.ResourceRequirements
}

// parseMountSettings reads the owner, the mount options and the mount pod resources from a
// volume's settings (the claim's annotations over the PV's volumeAttributes).
func parseMountSettings(settings map[string]string) (mountSettings, error) {
	var ms mountSettings
	var err error
	if ms.UID, ms.GID, err = claimOwner(settings); err != nil {
		return ms, err
	}
	if raw, ok := settings[AnnotationCacheTTL]; ok {
		d, err := time.ParseDuration(raw)
		if err != nil || d < 0 {
			return ms, fmt.Errorf("annotation %s = %q: want a positive Go duration (30s, 5m)", AnnotationCacheTTL, raw)
		}
		ms.Options.CacheTTL = d
	}
	for key, dst := range map[string]*bool{
		AnnotationBackgroundDelete: &ms.Options.BackgroundDelete,
		AnnotationUnsafeWrite:      &ms.Options.UnsafeWrite,
		AnnotationReadOnly:         &ms.Options.ReadOnly,
	} {
		if raw, ok := settings[key]; ok {
			if *dst, err = strconv.ParseBool(raw); err != nil {
				return ms, fmt.Errorf("annotation %s = %q: want true or false", key, raw)
			}
		}
	}
	if raw, ok := settings[AnnotationLocalFile]; ok {
		ms.Options.LocalFile = strings.Fields(raw)
		// As the chart's ValidatingAdmissionPolicy: "none" stands alone.
		if len(ms.Options.LocalFile) > 1 && slices.Contains(ms.Options.LocalFile, "none") {
			return ms, fmt.Errorf("annotation %s = %q: \"none\" cannot be combined with patterns",
				AnnotationLocalFile, raw)
		}
	}
	ms.Resources, err = parseMountResources(settings)

	return ms, err
}

func parseMountResources(settings map[string]string) (corev1.ResourceRequirements, error) {
	r := corev1.ResourceRequirements{}
	for _, a := range []struct {
		key  string
		list *corev1.ResourceList
		name corev1.ResourceName
	}{
		{AnnotationMountCPURequest, &r.Requests, corev1.ResourceCPU},
		{AnnotationMountCPULimit, &r.Limits, corev1.ResourceCPU},
		{AnnotationMountMemoryRequest, &r.Requests, corev1.ResourceMemory},
		{AnnotationMountMemoryLimit, &r.Limits, corev1.ResourceMemory},
	} {
		raw, ok := settings[a.key]
		if !ok {
			continue
		}
		q, err := resource.ParseQuantity(raw)
		if err != nil || q.Sign() <= 0 {
			return r, fmt.Errorf("annotation %s = %q: want a positive quantity", a.key, raw)
		}
		if *a.list == nil {
			*a.list = corev1.ResourceList{}
		}
		(*a.list)[a.name] = q
	}

	return r, nil
}

// claimOwner reads the owner UID/GID from a claim's annotations. The UID is mandatory, the GID
// defaults to the UID; both must be decimal integers in [0, 2^32-1], and 0 (root) is accepted
// when it is written explicitly.
func claimOwner(annotations map[string]string) (uid, gid uint32, err error) {
	raw, ok := annotations[AnnotationUID]
	if !ok {
		return 0, 0, fmt.Errorf("annotation %s is required", AnnotationUID)
	}
	uid, err = parseID(AnnotationUID, raw)
	if err != nil {
		return 0, 0, err
	}
	gid = uid
	if raw, ok := annotations[AnnotationGID]; ok {
		if gid, err = parseID(AnnotationGID, raw); err != nil {
			return 0, 0, err
		}
	}

	return uid, gid, nil
}

func parseID(key, raw string) (uint32, error) {
	id, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("annotation %s = %q: want an integer between 0 and %d", key, raw, uint32(1<<32-1))
	}

	return uint32(id), nil
}
