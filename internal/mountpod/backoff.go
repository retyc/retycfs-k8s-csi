package mountpod

import (
	"math/rand/v2"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// Exit codes of `retyc fs mount` (sysexits.h, a stable contract of retyc-cli). 77 and 78 say a new
// pod with the same machine account fails again; any other code may be transient (Retyc API or
// identity provider unreachable), 75 included: the mount worked, a restart may.
const (
	// ExitFailed is any other failure, a failure to start included (Redis unreachable, a mount point
	// left dead). A CLI older than ExitWritesLost also exits with it when writes were lost: see
	// LostWrites.
	ExitFailed       = 1
	ExitWritesLost   = 75 // EX_TEMPFAIL: files being written, or background deletes, never reached the API
	ExitAuthRequired = 77 // EX_NOPERM: the machine account is disabled or deleted on the Retyc side
	ExitConfig       = 78 // EX_CONFIG: the machine account's configuration is refused
)

// ExitMeaning explains an exit code of `retyc fs mount`, or returns "" for 0 and unknown codes.
func ExitMeaning(code int32) string {
	switch code {
	case ExitFailed:
		return "error"
	case ExitWritesLost:
		return "writes lost: files being written could not be sent to the Retyc API"
	case ExitAuthRequired:
		return "machine disabled or deleted on the Retyc side: enable it again (Organization > Machines) " +
			"or recreate the RetycIdentity"
	case ExitConfig:
		return "machine account configuration refused: its AGE key is not the account's active key, a file " +
			"of it is readable by others, or RETYC_TOKEN/RETYC_KEY_PASSPHRASE are set alongside it"
	}

	return ""
}

// lostWritesText are the CLI's words for files or deletes lost at the end of a drain
// (retycfs.LostWritesError, and the log of the drain), found in the termination message.
var lostWritesText = []string{
	"before the filesystem was unmounted", "abandoned (no drain timeout)",
	"what they held is lost", // uploads cancelled when the drain timeout is over
}

// LostWrites says whether a mount pod lost writes: it exited with ExitWritesLost or, without a
// code that says so, its termination message reports a loss in the CLI's words (the error it exits
// with, unchanged since: "N files being written were not sent before the filesystem was
// unmounted..."). The words are read on two codes only: ExitFailed, for a CLI older than
// ExitWritesLost, or a newer one whose drain ran out before it counted the cancelled uploads (it
// then exits 1 after logging that they are lost), and ExitAuthRequired, which a login lost at the
// same time keeps.
func LostWrites(st State) bool {
	if st.ExitCode == nil {
		return false
	}
	switch *st.ExitCode {
	case ExitWritesLost:
		return true
	case ExitFailed, ExitAuthRequired:
		return lostWritesReported(st.ExitMessage)
	}

	return false
}

// lostWritesReported reports whether a termination message tells of writes lost.
func lostWritesReported(message string) bool {
	for _, text := range lostWritesText {
		if strings.Contains(message, text) {
			return true
		}
	}

	return strings.Contains(message, "drain over after") && strings.Contains(message, " not sent")
}

// ExitLabel is ExitMeaning of a pod that has exited, with an exit 1 told apart by LostWrites (an
// older CLI) and by whether the pod was ever ready: "writes lost" when it reports a loss, a failure
// at start when it never served, a plain failure otherwise. A 77 that lost writes says both.
func ExitLabel(st State) string {
	if st.ExitCode == nil {
		return ""
	}
	code := *st.ExitCode
	switch {
	case code == ExitAuthRequired && LostWrites(st):
		return ExitMeaning(ExitAuthRequired) + "; " + ExitMeaning(ExitWritesLost)
	case LostWrites(st):
		return ExitMeaning(ExitWritesLost)
	case code == ExitFailed && !st.WasReady:
		return "failed before it was ready, nothing written"
	case code == ExitFailed:
		return "failed after it was ready, no lost writes reported"
	}

	return ExitMeaning(code)
}

func isFatal(code int32) bool {
	return code == ExitAuthRequired || code == ExitConfig
}

// Backoff is the restart state of one volume's mount pod on this node, kept in memory: a node
// plugin restart forgets it, as a restart of the old supervisor did.
type Backoff struct {
	// ConsecutiveFailures counts the pod exits since a pod was last seen ready or ran for
	// StableAfter; it drives the delay.
	ConsecutiveFailures int
	// FatalExits counts the consecutive exits with code 77 or 78.
	FatalExits int
	// LastExitCode is the code of the last exit, -1 when the container left none (pod evicted or
	// lost with its node).
	LastExitCode int32
	// LastExitMessage is the termination message of the last exit, kept once its pod is gone.
	LastExitMessage string
	// NextRestart is when Ensure may create the next pod; zero when it may now.
	NextRestart time.Time
	// GaveUp is true after MaxFatalExits consecutive 77/78 exits. Delete keeps it (see forget);
	// another config hash (another identity), Forget (an explicit unstage, or the volume gone from
	// the node), a deletion of the exited pod by hand (see startOver) or a node plugin restart
	// clears it.
	GaveUp bool
	// GiveUpUntil is when a give-up on a 77 expires, for one more try (see GiveUpRetryAfter); zero
	// for a give-up that never does (78).
	GiveUpUntil time.Time
}

// backoffEntry is a Backoff tied to the config it was counted for and to the last pod seen exit,
// so that the same pod is never counted twice.
type backoffEntry struct {
	Backoff
	hash    string
	lastPod types.UID
	// staging is the staging path of the last pod seen exit, for the sweep of the volumes gone
	// (see Volumes).
	staging string
}

// backoff returns the entry of a volume for a config, starting a fresh one when the config changed.
// Callers hold m.mu.
func (m *Manager) backoff(volumeID, hash string) *backoffEntry {
	e := m.backoffs[volumeID]
	if e == nil || e.hash != hash {
		e = &backoffEntry{hash: hash}
		m.backoffs[volumeID] = e
	}

	return e
}

// recordExit counts the exit of a pod running the given config on staging, once per pod, and
// returns the resulting state.
func (m *Manager) recordExit(
	volumeID, hash, staging string, uid types.UID, code int32, message string, ran time.Duration,
) Backoff {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.backoff(volumeID, hash)
	if uid != "" && uid == e.lastPod {
		return e.Backoff
	}
	e.lastPod, e.staging = uid, staging
	if ran >= m.cfg.StableAfter {
		e.ConsecutiveFailures = 0
	}
	e.ConsecutiveFailures++
	e.LastExitCode, e.LastExitMessage = code, message
	if isFatal(code) {
		e.FatalExits++
	} else {
		e.FatalExits = 0
	}
	if e.FatalExits >= m.cfg.MaxFatalExits {
		e.GaveUp, e.NextRestart, e.GiveUpUntil = true, time.Time{}, time.Time{}
		if code == ExitAuthRequired {
			e.GiveUpUntil = m.now().Add(GiveUpRetryAfter)
		}

		return e.Backoff
	}
	delay := restartDelay(m.cfg.RestartBackoff, m.cfg.MaxRestartBackoff, e.ConsecutiveFailures)
	// Up to 10% of jitter, so the nodes of a cluster that failed together do not retry in step.
	delay += rand.N(delay/10 + 1) //nolint:gosec // G404: jitter, not a secret
	e.NextRestart = m.now().Add(delay)

	return e.Backoff
}

// backoffState returns the current restart state of a volume for a config.
func (m *Manager) backoffState(volumeID, hash string) Backoff {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.backoff(volumeID, hash)
	if e.GaveUp && !e.GiveUpUntil.IsZero() && !m.now().Before(e.GiveUpUntil) {
		// Expired: one more try. If the machine is still refused, its pod exits with 77 and the
		// give-up starts again for GiveUpRetryAfter.
		klog.Infof("mountpod: volume %s: the give-up on code %d expired: trying again", volumeID, e.LastExitCode)
		e.Backoff = Backoff{}
	}

	return e.Backoff
}

// resetBackoff records that the volume's pod was seen ready: the next failure starts a fresh backoff.
func (m *Manager) resetBackoff(volumeID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.backoffs[volumeID]; e != nil {
		e.Backoff = Backoff{}
	}
}

// forget drops the restart state of a volume, after a Delete, but keeps a give-up: kubelet retries
// a failed stage for as long as a pod waits for the volume, and each new pod would log in with the
// same refused credentials again. Another config hash (new credentials), Forget or startOver starts
// afresh.
func (m *Manager) forget(volumeID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.backoffs[volumeID]; e != nil && !e.GaveUp {
		delete(m.backoffs, volumeID)
	}
}

// startOver clears a give-up once someone deleted the exited pod that gave up (`kubectl delete pod`):
// the manual way out after fixing what the exit code says, without waiting for a new stage or
// restarting the node plugin. Our own code never deletes a finished pod but by releasing it (its
// finalizer removed first), so a finished pod being deleted while it still holds its finalizer was
// deleted by hand (or by us while it still ran, before its last exit: one more series of tries at
// worst). The pod stays counted, and the next pod gets a full series of tries.
func (m *Manager) startOver(volumeID, hash string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.backoff(volumeID, hash); e.GaveUp {
		e.Backoff = Backoff{}
	}
}

// Forget drops the restart state of a volume, a give-up included: after an explicit
// NodeUnstageVolume (the volume was staged, its users are gone), or once the volume is gone from
// the node (no pod, no stage under way). See Manager for the locking.
func (m *Manager) Forget(volumeID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.backoffs, volumeID)
}

// Volumes returns the volumes with a restart state, with the staging path of their last pod seen
// exit ("" if none), for the sweep of the volumes gone (see Forget).
func (m *Manager) Volumes() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.backoffs))
	for id, e := range m.backoffs {
		out[id] = e.staging
	}

	return out
}

// restartDelay is base doubled for every consecutive failure after the first, capped at maxDelay.
func restartDelay(base, maxDelay time.Duration, consecutiveFailures int) time.Duration {
	delay := base
	for i := 1; i < consecutiveFailures && delay < maxDelay; i++ {
		delay *= 2
	}

	return min(delay, maxDelay)
}
