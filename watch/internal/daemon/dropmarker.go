package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/matteobortolazzo/cenci/watch/v2/internal/errcode"
)

// UndeliveredEventsMarkerPath is the fixed, host-visible home-volume path an
// in-sandbox `cenci notify` writes to after a hook event fails to reach the
// host daemon and its one retry is also exhausted. It has no env-var
// override — it must be readable via the same short-lived home-volume read
// `cenci diagnose` already uses for every other startup/dind marker.
const UndeliveredEventsMarkerPath = "/home/dev/.cenci-events-undelivered"

// markerPath and mountInfoPath are package-level seams so tests can redirect
// both the marker file and the mountinfo probe to isolated temp paths
// (watch/docs/test-isolation.md #202) without touching the real
// /home/dev/.cenci-events-undelivered or /proc/self/mountinfo.
var (
	markerPath    = UndeliveredEventsMarkerPath
	mountInfoPath = "/proc/self/mountinfo"
)

// UndeliveredEventsMarker is the JSON shape written to UndeliveredEventsMarkerPath.
// It crosses the container->host seam (read by `cenci diagnose` on the host
// side via a short-lived container), so every field is validated at that
// seam (#1094) — see launcher's eventDeliveryFinding.
type UndeliveredEventsMarker struct {
	SessionID string       `json:"session_id"`
	Code      errcode.Code `json:"code"`
	Message   string       `json:"message"`
	Timestamp string       `json:"timestamp"`
}

// ParseUndeliveredEventsMarker parses data as an UndeliveredEventsMarker.
func ParseUndeliveredEventsMarker(data []byte) (*UndeliveredEventsMarker, error) {
	var m UndeliveredEventsMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// classifyDeliveryFailure classifies a failed in-sandbox hook-event delivery
// as either unrecoverable/container-scoped (errcode.SandboxSocketUnwired —
// the socket-dir bind mount is dangling or was never wired in) or
// recoverable/host-scoped (errcode.DaemonSocketMissing — the mount is
// healthy, so the failure must be the host daemon). mountinfo is the raw
// contents of /proc/self/mountinfo (or the test seam mountInfoPath);
// readOK is false when that file could not be read at all. Any inconclusive
// signal — an unreadable mountinfo, or a line that doesn't match the
// expected shape — defaults to the conservative, container-scoped verdict
// rather than silently falling through to the recoverable one.
func classifyDeliveryFailure(mountinfo string, readOK bool, socketPath string) (errcode.Code, string) {
	if !readOK {
		return errcode.SandboxSocketUnwired,
			"the sandbox socket mount probe was inconclusive: /proc/self/mountinfo could not be read"
	}

	socketDir := filepath.Clean(filepath.Dir(socketPath))

	for _, line := range strings.Split(mountinfo, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 || !strings.Contains(line, " - ") {
			return errcode.SandboxSocketUnwired,
				"the sandbox socket mount probe was inconclusive: a /proc/self/mountinfo line was malformed"
		}
		root := unescapeMountinfoField(fields[3])
		mountpoint := unescapeMountinfoField(fields[4])
		if filepath.Clean(mountpoint) != socketDir {
			continue
		}
		if strings.HasSuffix(root, "//deleted") {
			return errcode.SandboxSocketUnwired,
				"the sandbox socket directory's bind mount is dangling inside this container; recreate the container to restore it"
		}
		return errcode.DaemonSocketMissing,
			"the sandbox socket directory is mounted correctly; the failure is on the host — the cenci daemon is not reachable, run cenci daemon start"
	}

	return errcode.SandboxSocketUnwired,
		"no mount entry was found for the sandbox socket directory; recreate the container to restore the bind mount"
}

// mountinfoUnescaper reverses the kernel's octal escaping of special
// characters (space, tab, newline, backslash) in /proc/self/mountinfo's
// root and mountpoint fields.
var mountinfoUnescaper = strings.NewReplacer(
	`\040`, " ",
	`\011`, "\t",
	`\012`, "\n",
	`\134`, `\`,
)

func unescapeMountinfoField(s string) string {
	return mountinfoUnescaper.Replace(s)
}

// writeUndeliveredMarker records a failed in-sandbox hook-event delivery so
// `cenci diagnose` can surface it host-side. A second failed delivery for
// the same sessionID is a no-op (the marker is once-per-session): re-reading
// and re-parsing the existing marker avoids a pointless rewrite when nothing
// new has happened. Every error here (read, parse, write, rename) is
// swallowed — a hook must never break the calling agent session.
func writeUndeliveredMarker(sessionID string, code errcode.Code, message string) {
	if existing, err := os.ReadFile(markerPath); err == nil {
		if m, err := ParseUndeliveredEventsMarker(existing); err == nil && m.SessionID == sessionID {
			return
		}
	}

	marker := UndeliveredEventsMarker{
		SessionID: sessionID,
		Code:      code,
		Message:   message,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	b, err := json.Marshal(marker)
	if err != nil {
		return
	}

	tmpPath := markerPath + ".tmp"
	if err := os.WriteFile(tmpPath, b, 0o600); err != nil {
		return
	}
	// os.WriteFile only applies its mode argument when creating a new file;
	// if tmpPath already existed (e.g. left over from an interrupted prior
	// write) it was opened with O_TRUNC and kept its existing mode, which
	// os.Rename below would then carry onto the final marker. Chmod
	// unconditionally enforces 0600 regardless of whether tmpPath was freshly
	// created or pre-existing. Its error is swallowed, matching this
	// function's existing every-I/O-error-is-swallowed contract.
	_ = os.Chmod(tmpPath, 0o600)
	if err := os.Rename(tmpPath, markerPath); err != nil {
		_ = os.Remove(tmpPath)
	}
}

// clearUndeliveredMarker removes any stale undelivered-events marker after a
// successful delivery. ENOENT (no marker to clear) and every other error are
// silently ignored. It also attempts to remove a stray markerPath+".tmp" left
// over from a prior interrupted write — cheap cleanup, same
// error-swallowing contract.
func clearUndeliveredMarker() {
	_ = os.Remove(markerPath)
	_ = os.Remove(markerPath + ".tmp")
}
