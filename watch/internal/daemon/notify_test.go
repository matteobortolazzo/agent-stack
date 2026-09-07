package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/matteobortolazzo/cenci/watch/v2/internal/errcode"
	"github.com/matteobortolazzo/cenci/watch/v2/internal/ipc"
)

// The four pre-existing tests below must isolate CENCI_SANDBOX (ambient in
// this dev container, per watch/docs/test-isolation.md #202) plus the
// socket-dir and drop-marker seams, or they silently stop exercising the
// host-branch code they claim to test.

func TestDeliverEventSucceedsWithoutRecovery(t *testing.T) {
	t.Setenv("CENCI_SANDBOX", "")
	useTempSocketDir(t)
	isolateDropMarkerPaths(t)

	event := ipc.HookEvent{EventType: "Stop", SessionID: "session-1"}
	var sent []ipc.HookEvent
	ensured := false

	deliverEvent(ipc.DefaultEventSocketPath(), event, func(_ string, got ipc.HookEvent) error {
		sent = append(sent, got)
		return nil
	}, func() { ensured = true })

	if ensured {
		t.Error("successful delivery must not ensure the daemon")
	}
	if !reflect.DeepEqual(sent, []ipc.HookEvent{event}) {
		t.Errorf("sent events = %#v, want original event once", sent)
	}
}

func TestDeliverEventRecoversAndRetriesSameEvent(t *testing.T) {
	t.Setenv("CENCI_SANDBOX", "")
	useTempSocketDir(t)
	isolateDropMarkerPaths(t)

	event := ipc.HookEvent{EventType: "PreToolUse", SessionID: "session-2", ToolName: "shell"}
	var sent []ipc.HookEvent
	ensured := 0

	deliverEvent(ipc.DefaultEventSocketPath(), event, func(_ string, got ipc.HookEvent) error {
		sent = append(sent, got)
		if len(sent) == 1 {
			return errors.New("daemon absent")
		}
		return nil
	}, func() { ensured++ })

	if ensured != 1 {
		t.Errorf("ensure calls = %d, want 1", ensured)
	}
	if !reflect.DeepEqual(sent, []ipc.HookEvent{event, event}) {
		t.Errorf("sent events = %#v, want exact event twice", sent)
	}
}

func TestDeliverEventFailedRecoveryRemainsNonFatal(t *testing.T) {
	t.Setenv("CENCI_SANDBOX", "")
	useTempSocketDir(t)
	isolateDropMarkerPaths(t)

	event := ipc.HookEvent{EventType: "UserPromptSubmit", SessionID: "session-3"}
	sends := 0
	ensured := 0

	deliverEvent(ipc.DefaultEventSocketPath(), event, func(string, ipc.HookEvent) error {
		sends++
		return errors.New("still unavailable")
	}, func() { ensured++ })

	if sends != 2 {
		t.Errorf("send attempts = %d, want 2", sends)
	}
	if ensured != 1 {
		t.Errorf("ensure calls = %d, want 1", ensured)
	}
}

func TestDeliverEventCustomSocketDoesNotStartDaemon(t *testing.T) {
	t.Setenv("CENCI_SANDBOX", "")
	useTempSocketDir(t)
	isolateDropMarkerPaths(t)

	event := ipc.HookEvent{EventType: "Stop", SessionID: "managed-instance"}
	sends := 0
	ensured := false

	deliverEvent("/tmp/custom-cenci.sock", event, func(string, ipc.HookEvent) error {
		sends++
		return errors.New("unavailable")
	}, func() { ensured = true })

	if ensured {
		t.Error("custom event socket must not start the default daemon")
	}
	if sends != 1 {
		t.Errorf("send attempts = %d, want only the initial attempt", sends)
	}
}

// -- sandbox-branch vs host-branch drop-marker coverage (#1122) ------------

// TestDeliverEventSandboxBranch_MarksMarkerAfterRetryExhausted asserts that
// inside a sandbox (CENCI_SANDBOX=1), a terminal delivery failure against the
// default event socket classifies the failure via mountInfoPath and writes
// an undelivered-events marker carrying the event's session id and the
// classified code.
func TestDeliverEventSandboxBranch_MarksMarkerAfterRetryExhausted(t *testing.T) {
	t.Setenv("CENCI_SANDBOX", "1")
	useTempSocketDir(t)
	marker, mountinfo := isolateDropMarkerPaths(t)

	// A healthy socket-dir mount, so the classifier attaches the
	// recoverable, host-scoped code.
	socketPath := ipc.DefaultEventSocketPath()
	socketDir := filepath.Dir(socketPath)
	content := "645 640 0:58 / " + socketDir + " rw,relatime shared:349 - tmpfs tmpfs rw\n"
	if err := os.WriteFile(mountinfo, []byte(content), 0o644); err != nil {
		t.Fatalf("write fake mountinfo: %v", err)
	}

	event := ipc.HookEvent{EventType: "Stop", SessionID: "sandbox-session-1"}
	sends := 0

	deliverEvent(socketPath, event, func(string, ipc.HookEvent) error {
		sends++
		return errors.New("event socket unreachable")
	}, func() {})

	if sends != 2 {
		t.Fatalf("send attempts = %d, want 2", sends)
	}

	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("expected a marker to be written after the retry was exhausted, read err: %v", err)
	}
	m, err := ParseUndeliveredEventsMarker(data)
	if err != nil {
		t.Fatalf("parse marker: %v", err)
	}
	if m.SessionID != event.SessionID {
		t.Errorf("marker SessionID = %q, want %q", m.SessionID, event.SessionID)
	}
	if m.Code != errcode.DaemonSocketMissing {
		t.Errorf("marker Code = %q, want %q", m.Code, errcode.DaemonSocketMissing)
	}
}

// TestDeliverEventSandboxBranch_ClearsMarkerOnSuccess asserts a successful
// in-sandbox delivery clears any stale marker left by an earlier failure.
func TestDeliverEventSandboxBranch_ClearsMarkerOnSuccess(t *testing.T) {
	t.Setenv("CENCI_SANDBOX", "1")
	useTempSocketDir(t)
	marker, _ := isolateDropMarkerPaths(t)

	writeUndeliveredMarker("stale-session", errcode.DaemonSocketMissing, "stale failure from an earlier attempt")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("precondition: marker must exist before the successful delivery: %v", err)
	}

	socketPath := ipc.DefaultEventSocketPath()
	event := ipc.HookEvent{EventType: "Stop", SessionID: "sandbox-session-2"}

	deliverEvent(socketPath, event, func(string, ipc.HookEvent) error {
		return nil
	}, func() {})

	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("expected the marker to be cleared after a successful delivery, stat err = %v", err)
	}
}

// TestDeliverEventHostBranch_NeverTouchesMountinfoOrMarker asserts the host
// branch (CENCI_SANDBOX unset) never enters the classify/write flow at all —
// not just that it happens to fail. mountInfoPath is pointed at a sentinel
// path that does not exist: if the host branch ever read it (a
// CENCI_SANDBOX-gate regression), the resulting "unreadable mountinfo"
// classification would still be a valid, writable marker — so the marker's
// absence is proof the gate held, not just that a read happened to fail
// silently.
func TestDeliverEventHostBranch_NeverTouchesMountinfoOrMarker(t *testing.T) {
	t.Setenv("CENCI_SANDBOX", "")
	useTempSocketDir(t)
	marker, _ := isolateDropMarkerPaths(t)
	mountInfoPath = filepath.Join(t.TempDir(), "does-not-exist")

	socketPath := ipc.DefaultEventSocketPath()
	event := ipc.HookEvent{EventType: "Stop", SessionID: "host-session-1"}
	sends := 0

	deliverEvent(socketPath, event, func(string, ipc.HookEvent) error {
		sends++
		return errors.New("daemon unreachable")
	}, func() {})

	if sends != 2 {
		t.Fatalf("send attempts = %d, want 2 (unchanged host retry behavior)", sends)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("host branch must never write an undelivered-events marker, stat err = %v", err)
	}
}

// TestDeliverEventSandboxBranch_UnwritableMarkerDirNeverBreaksDelivery
// exercises AC #5's non-fatality requirement end to end: an unreachable
// socket combined with an unwritable marker directory must not panic or
// otherwise break delivery. This is deliberately a package-internal test
// (not a black-box `cenci notify` subprocess test): the marker path is a
// hardcoded /home/dev/... constant with no env-var override, and this dev
// container's own HOME is /home/dev — a subprocess-level equivalent would
// risk writing to (or already having) a real file on the host outside test
// isolation. See watch/notify_test.go for the black-box host-branch
// regression coverage this ticket also requires.
func TestDeliverEventSandboxBranch_UnwritableMarkerDirNeverBreaksDelivery(t *testing.T) {
	t.Setenv("CENCI_SANDBOX", "1")
	useTempSocketDir(t)
	_, mountinfo := isolateDropMarkerPaths(t)
	if err := os.WriteFile(mountinfo, []byte(""), 0o644); err != nil {
		t.Fatalf("write fake mountinfo: %v", err)
	}

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	markerPath = filepath.Join(dir, "marker")

	socketPath := ipc.DefaultEventSocketPath()
	event := ipc.HookEvent{EventType: "Stop", SessionID: "sandbox-session-3"}

	deliverEvent(socketPath, event, func(string, ipc.HookEvent) error {
		return errors.New("event socket unreachable")
	}, func() {})

	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("expected no marker file in an unwritable directory, stat err = %v", err)
	}
}
