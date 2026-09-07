package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matteobortolazzo/cenci/watch/v2/internal/errcode"
)

// isolateDropMarkerPaths redirects the package-level markerPath and
// mountInfoPath seams to fresh paths under a per-test temp dir, restoring
// the originals on cleanup. Every test in this package that exercises the
// drop-marker read/write/classify paths must call this first so it never
// touches the real /home/dev/.cenci-events-undelivered or
// /proc/self/mountinfo, mirroring the CENCI_SANDBOX ambient-isolation
// discipline in watch/docs/test-isolation.md (#202).
func isolateDropMarkerPaths(t *testing.T) (marker, mountinfo string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "marker")
	mountinfo = filepath.Join(dir, "mountinfo")
	origMarker, origMountInfo := markerPath, mountInfoPath
	markerPath = marker
	mountInfoPath = mountinfo
	t.Cleanup(func() {
		markerPath = origMarker
		mountInfoPath = origMountInfo
	})
	return marker, mountinfo
}

func TestUndeliveredEventsMarkerPath_IsHomeDevDotfile(t *testing.T) {
	if UndeliveredEventsMarkerPath != "/home/dev/.cenci-events-undelivered" {
		t.Errorf("UndeliveredEventsMarkerPath = %q, want %q", UndeliveredEventsMarkerPath, "/home/dev/.cenci-events-undelivered")
	}
}

// -- classifyDeliveryFailure ------------------------------------------------

const testSocketDir = "/run/user/1000/cenci-sock"
const testSocketPath = testSocketDir + "/cenci-events.sock"

// TestClassifyDeliveryFailure_DanglingBindMount pins the unrecoverable,
// container-scoped verdict: a socket-dir mount entry whose root field ends
// in "//deleted" (the kernel's marker for a bind-mount source that vanished
// from the mount namespace) means the container's bind mount is dangling and
// can never self-heal — recreating the container is the only remediation.
func TestClassifyDeliveryFailure_DanglingBindMount(t *testing.T) {
	mountinfo := "645 640 0:58 " + testSocketDir + "//deleted " + testSocketDir +
		" rw,nosuid,nodev,relatime shared:349 - tmpfs tmpfs rw,size=65536k\n"

	code, msg := classifyDeliveryFailure(mountinfo, true, testSocketPath)

	if code != errcode.SandboxSocketUnwired {
		t.Fatalf("code = %s, want %s", code, errcode.SandboxSocketUnwired)
	}
	if !strings.Contains(msg, "dangling") {
		t.Errorf("message %q does not mention the dangling bind mount", msg)
	}
	if !strings.Contains(msg, "recreate") {
		t.Errorf("message %q does not name the recreate-the-container remediation", msg)
	}
}

// TestClassifyDeliveryFailure_NoMountEntryForSocketDir pins the second
// unrecoverable, container-scoped verdict: no mountinfo entry at all for the
// socket dir (the bind mount was never wired in). It must be distinguished
// in wording from the dangling-mount case even though both map to the same
// code.
func TestClassifyDeliveryFailure_NoMountEntryForSocketDir(t *testing.T) {
	mountinfo := "645 640 0:58 / /some/unrelated/path rw,relatime shared:349 - tmpfs tmpfs rw\n"

	code, msg := classifyDeliveryFailure(mountinfo, true, testSocketPath)

	if code != errcode.SandboxSocketUnwired {
		t.Fatalf("code = %s, want %s", code, errcode.SandboxSocketUnwired)
	}
	if !strings.Contains(msg, "no mount entry") {
		t.Errorf("message %q does not mention the missing mount entry", msg)
	}
	if !strings.Contains(msg, "recreate") {
		t.Errorf("message %q does not name the recreate-the-container remediation", msg)
	}
	if strings.Contains(msg, "dangling") {
		t.Errorf("message %q must not reuse the dangling-mount wording for a wholly absent mount entry", msg)
	}
}

// TestClassifyDeliveryFailure_HealthyMount pins the recoverable, host-scoped
// verdict: the socket dir is mounted correctly, so the failure must be the
// host daemon, not the container's wiring.
func TestClassifyDeliveryFailure_HealthyMount(t *testing.T) {
	mountinfo := "645 640 0:58 / " + testSocketDir + " rw,nosuid,nodev,relatime shared:349 - tmpfs tmpfs rw,size=65536k\n"

	code, msg := classifyDeliveryFailure(mountinfo, true, testSocketPath)

	if code != errcode.DaemonSocketMissing {
		t.Fatalf("code = %s, want %s", code, errcode.DaemonSocketMissing)
	}
	if !strings.Contains(msg, "cenci daemon") {
		t.Errorf("message %q does not name the host cenci daemon", msg)
	}
	if strings.Contains(msg, "recreate") {
		t.Errorf("message %q must not suggest recreating the container for a recoverable host-side failure", msg)
	}
}

// TestClassifyDeliveryFailure_UnreadableMountinfo pins the AC #3 regression:
// an unreadable /proc/self/mountinfo must never silently fall through to the
// recoverable classification — it defaults to the conservative,
// container-scoped verdict with an "inconclusive" message.
func TestClassifyDeliveryFailure_UnreadableMountinfo(t *testing.T) {
	code, msg := classifyDeliveryFailure("", false, testSocketPath)

	if code != errcode.SandboxSocketUnwired {
		t.Fatalf("code = %s, want %s", code, errcode.SandboxSocketUnwired)
	}
	if !strings.Contains(msg, "inconclusive") {
		t.Errorf("message %q does not state the mount probe was inconclusive", msg)
	}
}

// TestClassifyDeliveryFailure_MalformedMountinfoLine pins the other AC #3
// regression case: a line that fails the field-count/separator shape check
// is malformed, not "no match" — it must also classify as inconclusive
// rather than being silently skipped and falling through to "no mount
// entry".
func TestClassifyDeliveryFailure_MalformedMountinfoLine(t *testing.T) {
	mountinfo := "not a valid mountinfo line\n"

	code, msg := classifyDeliveryFailure(mountinfo, true, testSocketPath)

	if code != errcode.SandboxSocketUnwired {
		t.Fatalf("code = %s, want %s", code, errcode.SandboxSocketUnwired)
	}
	if !strings.Contains(msg, "inconclusive") {
		t.Errorf("message %q does not state the mount probe was inconclusive", msg)
	}
}

// TestClassifyDeliveryFailure_UnescapesOctalMountPoint pins the mountinfo
// octal-unescaping requirement: the kernel encodes special characters in
// mount point paths (e.g. a literal space as \040) — classifyDeliveryFailure
// must unescape before comparing against the resolved socket dir, or a
// perfectly healthy mount with an unusual path would be misclassified as "no
// mount entry".
func TestClassifyDeliveryFailure_UnescapesOctalMountPoint(t *testing.T) {
	socketDir := "/run/user/1000/my dir"
	socketPath := socketDir + "/cenci-events.sock"
	mountinfo := `645 640 0:58 / /run/user/1000/my\040dir rw,relatime shared:349 - tmpfs tmpfs rw` + "\n"

	code, msg := classifyDeliveryFailure(mountinfo, true, socketPath)

	if code != errcode.DaemonSocketMissing {
		t.Fatalf("code = %s, want %s (the escaped mount point must unescape to match the socket dir)", code, errcode.DaemonSocketMissing)
	}
	if !strings.Contains(msg, "cenci daemon") {
		t.Errorf("message %q does not name the host cenci daemon", msg)
	}
}

// -- writeUndeliveredMarker / clearUndeliveredMarker / ParseUndeliveredEventsMarker

func TestWriteUndeliveredMarker_WritesSessionCodeMessageAndTimestamp(t *testing.T) {
	marker, _ := isolateDropMarkerPaths(t)

	writeUndeliveredMarker("session-1", errcode.DaemonSocketMissing, "the cenci daemon is not reachable from inside this container")

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	m, err := ParseUndeliveredEventsMarker(data)
	if err != nil {
		t.Fatalf("parse marker: %v", err)
	}
	if m.SessionID != "session-1" {
		t.Errorf("SessionID = %q, want %q", m.SessionID, "session-1")
	}
	if m.Code != errcode.DaemonSocketMissing {
		t.Errorf("Code = %q, want %q", m.Code, errcode.DaemonSocketMissing)
	}
	if m.Message == "" {
		t.Error("Message is empty")
	}
	if _, err := time.Parse(time.RFC3339, m.Timestamp); err != nil {
		t.Errorf("Timestamp %q is not RFC3339: %v", m.Timestamp, err)
	}
}

func TestWriteUndeliveredMarker_FileModeIs0600(t *testing.T) {
	marker, _ := isolateDropMarkerPaths(t)

	writeUndeliveredMarker("session-1", errcode.DaemonSocketMissing, "message")

	info, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("stat marker: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("marker mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestWriteUndeliveredMarker_LeavesNoStrayTmpFile(t *testing.T) {
	marker, _ := isolateDropMarkerPaths(t)

	writeUndeliveredMarker("session-1", errcode.DaemonSocketMissing, "message")

	if _, err := os.Stat(marker + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected no stray .tmp file after a successful write, stat err = %v", err)
	}
}

func TestWriteUndeliveredMarker_WritesCompactSingleLineJSON(t *testing.T) {
	marker, _ := isolateDropMarkerPaths(t)

	writeUndeliveredMarker("session-1", errcode.DaemonSocketMissing, "message")

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if strings.Contains(strings.TrimRight(string(data), "\n"), "\n") {
		t.Errorf("marker JSON is not compact/single-line: %s", data)
	}
}

// TestWriteUndeliveredMarker_SameSessionDoesNotRewrite pins the once-per-
// session contract: a second failed delivery carrying the same session_id
// must leave the existing marker byte-identical, with no rewrite at all —
// not just coincidentally-identical content, but the same mtime, proving the
// file was never touched.
func TestWriteUndeliveredMarker_SameSessionDoesNotRewrite(t *testing.T) {
	marker, _ := isolateDropMarkerPaths(t)

	writeUndeliveredMarker("session-1", errcode.SandboxSocketUnwired, "first message")
	first, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	firstInfo, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("stat marker: %v", err)
	}

	// A second failed delivery for the SAME session, with a different
	// code/message, must not rewrite the marker at all.
	writeUndeliveredMarker("session-1", errcode.DaemonSocketMissing, "second message")

	second, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker (2nd): %v", err)
	}
	secondInfo, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("stat marker (2nd): %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Errorf("marker content changed for the same session_id:\nfirst:  %s\nsecond: %s", first, second)
	}
	if !secondInfo.ModTime().Equal(firstInfo.ModTime()) {
		t.Errorf("marker mtime changed (%v -> %v); the same-session write must not touch the file at all", firstInfo.ModTime(), secondInfo.ModTime())
	}
}

// TestWriteUndeliveredMarker_DifferentSessionReplaces pins the other half of
// the once-per-session contract: a failed delivery for a DIFFERENT
// session_id must replace the marker.
func TestWriteUndeliveredMarker_DifferentSessionReplaces(t *testing.T) {
	marker, _ := isolateDropMarkerPaths(t)

	writeUndeliveredMarker("session-1", errcode.SandboxSocketUnwired, "first message")
	writeUndeliveredMarker("session-2", errcode.DaemonSocketMissing, "second message")

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	m, err := ParseUndeliveredEventsMarker(data)
	if err != nil {
		t.Fatalf("parse marker: %v", err)
	}
	if m.SessionID != "session-2" {
		t.Errorf("SessionID = %q, want %q (a different session_id must replace the marker)", m.SessionID, "session-2")
	}
	if m.Code != errcode.DaemonSocketMissing {
		t.Errorf("Code = %q, want %q", m.Code, errcode.DaemonSocketMissing)
	}
	if m.Message != "second message" {
		t.Errorf("Message = %q, want %q", m.Message, "second message")
	}
}

// TestWriteUndeliveredMarker_UnwritableDirIsNonFatal asserts the hook-safety
// contract: an unwritable marker directory must never panic or otherwise
// break the caller — the write is silently swallowed.
func TestWriteUndeliveredMarker_UnwritableDirIsNonFatal(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // allow TempDir cleanup to remove it

	origMarker := markerPath
	markerPath = filepath.Join(dir, "marker")
	t.Cleanup(func() { markerPath = origMarker })

	// Must not panic, and must not leave a stray .tmp file behind either.
	writeUndeliveredMarker("session-1", errcode.DaemonSocketMissing, "message")

	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("expected no marker file to appear in an unwritable directory, stat err = %v", err)
	}
}

// TestWriteUndeliveredMarker_StaleTmpFileModeGetsCorrected pins fix 5: if a
// ".tmp" file already exists (e.g. left over from an interrupted prior
// write) with a mode other than 0600, os.WriteFile alone would keep that
// stale mode (it only applies the mode argument when creating a new file),
// and os.Rename would carry it onto the final marker. writeUndeliveredMarker
// must chmod the .tmp file to 0600 before renaming so the final marker's
// mode is enforced regardless of what the .tmp file started with.
func TestWriteUndeliveredMarker_StaleTmpFileModeGetsCorrected(t *testing.T) {
	marker, _ := isolateDropMarkerPaths(t)

	if err := os.WriteFile(marker+".tmp", []byte("stale"), 0o644); err != nil {
		t.Fatalf("seed stale .tmp file: %v", err)
	}

	writeUndeliveredMarker("session-1", errcode.DaemonSocketMissing, "message")

	info, err := os.Stat(marker)
	if err != nil {
		t.Fatalf("stat marker: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("marker mode = %o, want 0600 (a pre-existing .tmp file's stale mode must not survive the rename)", info.Mode().Perm())
	}
}

func TestClearUndeliveredMarker_RemovesStrayTmpFile(t *testing.T) {
	marker, _ := isolateDropMarkerPaths(t)
	if err := os.WriteFile(marker+".tmp", []byte("stale"), 0o600); err != nil {
		t.Fatalf("seed stray .tmp file: %v", err)
	}

	clearUndeliveredMarker()

	if _, err := os.Stat(marker + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("expected the stray .tmp file to be removed, stat err = %v", err)
	}
}

func TestClearUndeliveredMarker_RemovesFile(t *testing.T) {
	marker, _ := isolateDropMarkerPaths(t)
	writeUndeliveredMarker("session-1", errcode.DaemonSocketMissing, "message")

	clearUndeliveredMarker()

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("expected marker to be removed, stat err = %v", err)
	}
}

// TestClearUndeliveredMarker_MissingFileIsNotAnError asserts ENOENT on
// removal is tolerated, not treated as an error.
func TestClearUndeliveredMarker_MissingFileIsNotAnError(t *testing.T) {
	isolateDropMarkerPaths(t) // marker was never written

	// Must not panic or otherwise misbehave on ENOENT.
	clearUndeliveredMarker()
}

func TestParseUndeliveredEventsMarker_UnparseableContentReturnsError(t *testing.T) {
	if _, err := ParseUndeliveredEventsMarker([]byte("not json")); err == nil {
		t.Error("expected an error for unparseable marker content, got nil")
	}
}
