package daemon

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// This file implements the process-control half of `cenci daemon
// start|stop|restart|status`: PID file read/write, liveness probing of an
// already-running daemon process, and the SIGTERM/SIGKILL stop sequence. It
// is distinct from lifecycle_test.go in this package, which covers the
// session-hook event lifecycle (SessionStart/Stop/SessionEnd), not the OS
// process lifecycle covered here.

// stopGraceTimeout bounds how long Stop waits after SIGTERM before
// escalating to SIGKILL. stopPollInterval is the polling cadence within that
// window. Both are vars so tests can shrink them.
var stopGraceTimeout = 3 * time.Second
var stopPollInterval = 50 * time.Millisecond

// daemonProcessPattern is the pgrep -f pattern used to find a running daemon
// process when the PID file is missing or stale. The leading "[/]" avoids
// pgrep matching its own invocation (a literal "[/]" never appears as a raw
// "/" in pgrep's own argv). The trailing "( start)?$" anchors on the two
// forms `cenci daemon` (bare, e.g. hand-started in a pane) and
// `cenci daemon start` (the canonical form Spawn uses) without matching
// `daemon stop`/`daemon status`/`daemon restart` invocations, including our
// own.
const daemonProcessPattern = `[/]cenci daemon( start)?$`

// pgrepDaemon is the process-table fallback used by Stop when the PID file is
// missing or stale but the daemon's socket is still alive. It is a package
// var so tests can stub it without depending on a real "cenci daemon"
// process existing in the table.
var pgrepDaemon = func() (int, error) {
	out, err := exec.Command("pgrep", "-f", daemonProcessPattern).Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return 0, nil // pgrep's documented "no processes matched" exit code
		}
		return 0, err
	}
	for _, field := range strings.Fields(string(out)) {
		pid, perr := strconv.Atoi(field)
		if perr != nil || pid == os.Getpid() {
			continue
		}
		return pid, nil
	}
	return 0, nil
}

// pidFileOwner returns the uid that ReadPIDFile requires the PID file to be
// owned by (normally the current process's uid). A package var, mirroring
// the pgrepDaemon precedent above, so tests can stub it without needing a
// second real system user.
var pidFileOwner = os.Getuid

// notRegularFileErr formats the post-open regular-file rejection shared by
// WritePIDFile and ReadPIDFile: an opened path that survived the
// O_NOFOLLOW/O_NONBLOCK open (so it's neither a symlink nor a blocking FIFO)
// but is still not a plain file (e.g. a device or a non-blocking FIFO/socket).
func notRegularFileErr(path string, mode os.FileMode) error {
	return fmt.Errorf("pid file %q is not a regular file (mode %v)", path, mode)
}

// WritePIDFile writes the current process's PID to path (0600: owner-only,
// matching the 0700 socket dir it lives alongside). The open refuses to
// follow a symlink (O_NOFOLLOW — a planted symlink at path errors instead of
// truncating/overwriting whatever it points at) and refuses to block on a
// pre-planted FIFO (O_NONBLOCK, which makes open(2) fail ENXIO immediately
// instead of hanging while waiting for a reader). The opened file is then
// rejected unless it is a regular file. Note: O_TRUNC fires at open time, so
// the regular-file check cannot protect an existing regular file from
// truncation — but O_NOFOLLOW already blocks the arbitrary-symlink-target
// case, O_WRONLY fails EISDIR on a directory, and O_TRUNC is a no-op on
// devices, so this guard introduces no new truncation reach.
func WritePIDFile(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return err
	}

	fi, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		return statErr
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return notRegularFileErr(path, fi.Mode())
	}

	// O_CREATE's mode argument only applies on fresh creation; a
	// pre-existing file (e.g. acquired with a looser umask outside cenci's
	// control) keeps whatever permissions it already had. Normalize to 0600
	// on every write so ReadPIDFile's perm check (which rejects
	// group/other-writable) never rejects a file WritePIDFile itself just
	// produced (#1147).
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}

	_, err = f.WriteString(strconv.Itoa(os.Getpid()))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// RemovePIDFile best-effort removes the PID file at path. A missing file is
// not an error — removal is idempotent so callers can defer it unconditionally.
func RemovePIDFile(path string) {
	_ = os.Remove(path)
}

// ReadPIDFile reads and parses the PID file at path. The open refuses to
// follow a symlink (O_NOFOLLOW) and uses O_NONBLOCK so a pre-planted FIFO's
// open(2) returns immediately instead of blocking for a writer to appear.
// Ordering is load-bearing: f.Stat() and the non-regular-file rejection MUST
// run before any read. Unlike O_WRONLY, O_RDONLY|O_NONBLOCK on a FIFO
// succeeds at open time, and Go registers the resulting fd as
// pollable/non-blocking with the runtime poller — so a subsequent
// io.ReadAll would hang exactly like the pre-hardening bug unless the
// non-regular rejection runs first. Once the file is confirmed regular, its
// owning uid must match pidFileOwner() (the current process's uid,
// normally) — this rejects a foreign-owned regular file planted on a
// world-writable fallback path, which would otherwise let ReadPIDFile hand
// Stop an attacker-chosen PID to signal. The file's permission bits are then
// checked (mirroring hardenDir's own rule): group- or other-writable
// (perm&0022 != 0) is rejected even though we own the file, since an
// owned-but-world-writable file can still be rewritten by any other local
// user. Accepted residual: an in-container
// `cenci daemon status` reading a host bind-mounted cenci.pid
// (internal/sandbox/launcher/launch.go's cenciSocketMountDest mount) under a
// uid-remapping runtime reports PID 0 instead of the host PID —
// StatusInfo.Running is unaffected because it comes from the socket dial,
// not the PID file.
func ReadPIDFile(path string) (int, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if !fi.Mode().IsRegular() {
		return 0, notRegularFileErr(path, fi.Mode())
	}

	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != pidFileOwner() {
		return 0, fmt.Errorf("pid file %q is not owned by the current user (uid %d)", path, pidFileOwner())
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		return 0, fmt.Errorf("pid file %q has insecure permissions %v (group/other-writable)", path, fi.Mode())
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parsing pid file %s: %w", path, err)
	}
	return pid, nil
}

// ProcessAlive reports whether a process with the given PID currently
// exists, using signal 0 (a null signal that performs existence/permission
// checks without actually signaling the process).
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// StatusInfo is the result of Status: whether a daemon is running, and its
// PID if known.
type StatusInfo struct {
	// Running is authoritative — it reflects a live socket dial.
	Running bool
	// PID is best-effort informational context from the PID file. It may be
	// 0 even when Running is true (e.g. the PID file could not be read).
	PID int
}

// Status reports whether a daemon is currently reachable at eventSocketPath.
// The PID, read from pidPath, is included for display but is never used to
// determine Running — a stale PID file must never report "not running" as
// "running", nor vice versa.
func Status(eventSocketPath, pidPath string) StatusInfo {
	info := StatusInfo{Running: Alive(eventSocketPath)}
	if pid, err := ReadPIDFile(pidPath); err == nil {
		info.PID = pid
	}
	return info
}

// StopOutcome describes what Stop did.
type StopOutcome struct {
	// WasRunning reports whether a daemon was found and stopped. False means
	// Stop found nothing to do (already-idempotent no-op).
	WasRunning bool
	// PID is the process that was signaled. Zero when WasRunning is false.
	PID int
}

// Stop stops a running daemon. Liveness is determined the same way
// EnsureRunning's on-demand startup does: a dial against eventSocketPath.
// Given a live daemon, it reads pidPath for the PID, sends SIGTERM, and
// polls (bounded by stopGraceTimeout) for the process to exit, escalating to
// SIGKILL if it is still alive afterward. If the PID file is missing or
// stale (points at a dead/foreign process) but the socket reports a live
// daemon, it falls back to a pgrep -f process-table scan
// (daemonProcessPattern) to find the process to signal. A stale PID file is
// always removed. Safe to call unconditionally: when nothing is running, it
// returns a zero StopOutcome and a nil error.
func Stop(eventSocketPath, pidPath string) (StopOutcome, error) {
	pid, pidErr := ReadPIDFile(pidPath)
	if pidErr != nil && !os.IsNotExist(pidErr) {
		// Named so a genuine attack signature (symlink, FIFO, foreign-owned,
		// insecure-permissions) is distinguishable in logs from routine
		// staleness (file doesn't exist) — Stop still falls through to the
		// pgrepDaemon scan below regardless of the reason. A plain
		// not-exist (the common case: no daemon has ever run, or a prior
		// clean stop already removed it) is excluded so it doesn't drown
		// out the genuine attack signatures on every idle-host stop/restart
		// (#1147).
		log.Printf("warning: pid file %q unusable: %v", pidPath, pidErr)
	}
	pidFileValid := pidErr == nil && ProcessAlive(pid)

	if pidErr == nil && !pidFileValid {
		// The file exists but points at a dead/foreign process — stale.
		RemovePIDFile(pidPath)
	}

	if !pidFileValid {
		if !Alive(eventSocketPath) {
			// Nothing running by either signal — idempotent no-op.
			return StopOutcome{}, nil
		}
		found, err := pgrepDaemon()
		if err != nil {
			return StopOutcome{}, fmt.Errorf("daemon socket is alive but locating its process failed: %w", err)
		}
		if found == 0 {
			return StopOutcome{}, fmt.Errorf("daemon socket is alive but no matching process was found (pid file missing/stale)")
		}
		pid = found
	}

	if err := terminateAndWait(pid); err != nil {
		return StopOutcome{}, err
	}
	RemovePIDFile(pidPath)
	return StopOutcome{WasRunning: true, PID: pid}, nil
}

// terminateAndWait sends SIGTERM to pid and polls (bounded by
// stopGraceTimeout) for it to exit, escalating to SIGKILL if it is still
// alive once the grace period elapses.
func terminateAndWait(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}

	if !ProcessAlive(pid) {
		return nil // already gone
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		if !ProcessAlive(pid) {
			return nil // exited between the check above and the signal
		}
		return fmt.Errorf("signaling pid %d: %w", pid, err)
	}

	deadline := time.Now().Add(stopGraceTimeout)
	for time.Now().Before(deadline) {
		if !ProcessAlive(pid) {
			return nil
		}
		time.Sleep(stopPollInterval)
	}

	if !ProcessAlive(pid) {
		return nil
	}
	_ = proc.Signal(syscall.SIGKILL)
	killDeadline := time.Now().Add(stopGraceTimeout)
	for time.Now().Before(killDeadline) {
		if !ProcessAlive(pid) {
			return nil
		}
		time.Sleep(stopPollInterval)
	}
	return fmt.Errorf("pid %d did not exit after SIGTERM and SIGKILL", pid)
}
