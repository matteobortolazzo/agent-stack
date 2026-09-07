package daemon

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func withShortStopTimeouts(t *testing.T) {
	t.Helper()
	restoreGrace, restorePoll := stopGraceTimeout, stopPollInterval
	stopGraceTimeout = 300 * time.Millisecond
	stopPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { stopGraceTimeout, stopPollInterval = restoreGrace, restorePoll })
}

func TestPIDFile_WriteReadRemoveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cenci.pid")

	if err := WritePIDFile(path); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}
	pid, err := ReadPIDFile(path)
	if err != nil {
		t.Fatalf("ReadPIDFile: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("ReadPIDFile = %d, want %d (own pid)", pid, os.Getpid())
	}

	RemovePIDFile(path)
	if _, err := ReadPIDFile(path); err == nil {
		t.Error("expected ReadPIDFile to fail after RemovePIDFile")
	}
}

// TestWritePIDFile_NormalizesPreExistingLoosePermissions covers #1147 Fix 4:
// a pre-existing PID file with loose permissions (0644, acquired somehow
// outside cenci's control -- e.g. a looser umask) must have its permissions
// normalized to 0600 on write, so ReadPIDFile's perm check (which rejects
// group/other-writable) never rejects a file WritePIDFile itself just
// produced.
func TestWritePIDFile_NormalizesPreExistingLoosePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cenci.pid")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Chmod bypasses umask, ensuring the file is actually group/other
	// readable regardless of the process umask.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := WritePIDFile(path); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat(%q): %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %04o, want 0600 (normalized)", perm)
	}

	pid, err := ReadPIDFile(path)
	if err != nil {
		t.Fatalf("ReadPIDFile: unexpected error after normalization: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("ReadPIDFile = %d, want %d (own pid)", pid, os.Getpid())
	}
}

func TestRemovePIDFile_MissingFileIsNoop(t *testing.T) {
	// Must not panic or error on a path that was never written.
	RemovePIDFile(filepath.Join(t.TempDir(), "never-written.pid"))
}

func TestReadPIDFile_MalformedContentErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cenci.pid")
	if err := os.WriteFile(path, []byte("not-a-pid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPIDFile(path); err == nil {
		t.Error("expected ReadPIDFile to error on malformed content")
	}
}

func TestProcessAlive(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Error("expected own process to be reported alive")
	}
	if ProcessAlive(0) {
		t.Error("expected pid 0 to be reported not alive")
	}
	if ProcessAlive(-1) {
		t.Error("expected negative pid to be reported not alive")
	}

	// A short-lived child process, waited on so it's reaped: its pid must
	// then report not-alive.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run 'true': %v", err)
	}
	if ProcessAlive(cmd.Process.Pid) {
		t.Errorf("expected exited+reaped pid %d to be reported not alive", cmd.Process.Pid)
	}
}

func TestStatus_NotRunning(t *testing.T) {
	dir := t.TempDir()
	info := Status(filepath.Join(dir, "events.sock"), filepath.Join(dir, "cenci.pid"))
	if info.Running {
		t.Error("expected Running=false with no listener")
	}
}

func TestStatus_RunningReportsPID(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "events.sock")
	pidPath := filepath.Join(dir, "cenci.pid")

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	if err := WritePIDFile(pidPath); err != nil {
		t.Fatal(err)
	}

	info := Status(socketPath, pidPath)
	if !info.Running {
		t.Error("expected Running=true with a live listener")
	}
	if info.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", info.PID, os.Getpid())
	}
}

func TestStatus_RunningWithMissingPIDFileReportsZeroPID(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "events.sock")

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	info := Status(socketPath, filepath.Join(dir, "missing.pid"))
	if !info.Running {
		t.Error("expected Running=true with a live listener even if the pid file is missing")
	}
	if info.PID != 0 {
		t.Errorf("PID = %d, want 0 (unknown)", info.PID)
	}
}

func TestStop_NothingRunningIsNoopNoError(t *testing.T) {
	dir := t.TempDir()
	outcome, err := Stop(filepath.Join(dir, "events.sock"), filepath.Join(dir, "cenci.pid"))
	if err != nil {
		t.Fatalf("Stop: unexpected error: %v", err)
	}
	if outcome.WasRunning {
		t.Error("expected WasRunning=false when nothing is running")
	}
}

func TestStop_StaleUnreachablePIDFileIsRemoved(t *testing.T) {
	// PID file points at a definitely-dead pid, and no socket is alive: Stop
	// must clean up the stale file and report nothing was running.
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "cenci.pid")

	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	deadPID := cmd.Process.Pid
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(deadPID)), 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := Stop(filepath.Join(dir, "events.sock"), pidPath)
	if err != nil {
		t.Fatalf("Stop: unexpected error: %v", err)
	}
	if outcome.WasRunning {
		t.Error("expected WasRunning=false for a stale pid file with no live socket")
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("expected stale pid file to be removed")
	}
}

func TestStop_ValidPIDFileSendsSIGTERMAndRemovesPIDFile(t *testing.T) {
	withShortStopTimeouts(t)
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "events.sock")
	pidPath := filepath.Join(dir, "cenci.pid")

	// A real long-running child process, so terminateAndWait has something
	// to signal and wait on. "sleep" ignores nothing special about SIGTERM —
	// the default disposition terminates the process, which is exactly the
	// behavior being exercised.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	defer func() {
		_ = cmd.Process.Kill()
		<-done
	}()

	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	outcome, err := Stop(socketPath, pidPath)
	if err != nil {
		t.Fatalf("Stop: unexpected error: %v", err)
	}
	if !outcome.WasRunning {
		t.Error("expected WasRunning=true")
	}
	if outcome.PID != cmd.Process.Pid {
		t.Errorf("PID = %d, want %d", outcome.PID, cmd.Process.Pid)
	}
	if ProcessAlive(cmd.Process.Pid) {
		t.Error("expected process to be gone after Stop")
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("expected pid file to be removed after Stop")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("expected the child process's Wait() to have returned")
	}
}

func TestStop_EscalatesToSIGKILLWhenProcessIgnoresSIGTERM(t *testing.T) {
	withShortStopTimeouts(t)
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "events.sock")
	pidPath := filepath.Join(dir, "cenci.pid")

	// A shell that traps and ignores SIGTERM, forcing the SIGKILL escalation
	// path.
	cmd := exec.Command("sh", "-c", "trap '' TERM; sleep 30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	defer func() {
		_ = cmd.Process.Kill()
		<-done
	}()

	// Give the shell a moment to install the trap before it's signaled.
	time.Sleep(100 * time.Millisecond)

	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	outcome, err := Stop(socketPath, pidPath)
	if err != nil {
		t.Fatalf("Stop: unexpected error: %v", err)
	}
	if !outcome.WasRunning {
		t.Error("expected WasRunning=true")
	}
	if ProcessAlive(cmd.Process.Pid) {
		t.Error("expected process to be gone after SIGKILL escalation")
	}
}

func TestStop_StaleUnreadablePIDFileButLiveSocketFallsBackToPgrep(t *testing.T) {
	withShortStopTimeouts(t)
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "events.sock")
	pidPath := filepath.Join(dir, "cenci.pid") // never written: missing

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	defer func() {
		_ = cmd.Process.Kill()
		<-done
	}()

	restore := pgrepDaemon
	pgrepDaemon = func() (int, error) { return cmd.Process.Pid, nil }
	defer func() { pgrepDaemon = restore }()

	outcome, err := Stop(socketPath, pidPath)
	if err != nil {
		t.Fatalf("Stop: unexpected error: %v", err)
	}
	if !outcome.WasRunning || outcome.PID != cmd.Process.Pid {
		t.Errorf("outcome = %+v, want WasRunning=true PID=%d", outcome, cmd.Process.Pid)
	}
	if ProcessAlive(cmd.Process.Pid) {
		t.Error("expected process found via pgrep fallback to be stopped")
	}
}

func TestStop_LiveSocketButPgrepFindsNothingErrors(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "events.sock")
	pidPath := filepath.Join(dir, "cenci.pid") // missing

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	restore := pgrepDaemon
	pgrepDaemon = func() (int, error) { return 0, nil }
	defer func() { pgrepDaemon = restore }()

	_, err = Stop(socketPath, pidPath)
	if err == nil {
		t.Error("expected an error when the socket is alive but no process can be found")
	}
}

func TestStop_PgrepErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "events.sock")
	pidPath := filepath.Join(dir, "cenci.pid")

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	boom := errors.New("boom")
	restore := pgrepDaemon
	pgrepDaemon = func() (int, error) { return 0, boom }
	defer func() { pgrepDaemon = restore }()

	_, err = Stop(socketPath, pidPath)
	if err == nil {
		t.Fatal("expected an error to propagate from pgrepDaemon")
	}
}

// -- #1147: PID-file hardening against the full plant class -----------------

// TestWritePIDFile_RefusesSymlink covers the O_NOFOLLOW guard on the write
// path: a symlink at the PID path must make WritePIDFile error, naming the
// path, and must leave the symlink's target file byte-identical (not
// truncated, not overwritten).
func TestWritePIDFile_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	original := []byte("do-not-touch")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cenci.pid")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("planting symlink %q -> %q: %v", path, target, err)
	}

	err := WritePIDFile(path)
	if err == nil {
		t.Fatal("WritePIDFile() error = nil, want error for a symlinked pid path")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it to name the path %q", err.Error(), path)
	}

	got, rerr := os.ReadFile(target)
	if rerr != nil {
		t.Fatalf("reading symlink target %q: %v", target, rerr)
	}
	if string(got) != string(original) {
		t.Errorf("symlink target was modified: got %q, want %q", got, original)
	}

	linkInfo, lerr := os.Lstat(path)
	if lerr != nil {
		t.Fatalf("Lstat(%q): %v", path, lerr)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("expected %q to still be a symlink, got mode %v", path, linkInfo.Mode())
	}
	gotTarget, rerr2 := os.Readlink(path)
	if rerr2 != nil {
		t.Fatalf("Readlink(%q): %v", path, rerr2)
	}
	if gotTarget != target {
		t.Errorf("symlink target changed: got %q, want %q", gotTarget, target)
	}
}

// TestReadPIDFile_RefusesSymlink covers the O_NOFOLLOW guard on the read
// path: a symlink at the PID path must make ReadPIDFile error rather than
// returning the target's PID.
func TestReadPIDFile_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	const plantedPID = 99999
	if err := os.WriteFile(target, []byte(strconv.Itoa(plantedPID)), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cenci.pid")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("planting symlink %q -> %q: %v", path, target, err)
	}

	pid, err := ReadPIDFile(path)
	if err == nil {
		t.Fatalf("ReadPIDFile() error = nil, want error for a symlinked pid path (got pid %d)", pid)
	}
	if pid == plantedPID {
		t.Errorf("ReadPIDFile returned the planted target's PID %d instead of erroring", plantedPID)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it to name the path %q", err.Error(), path)
	}
}

// TestWritePIDFile_RefusesFIFOWithoutHanging covers the O_NONBLOCK guard: a
// pre-planted FIFO at the PID path must make WritePIDFile return ENXIO (or a
// non-regular-file rejection) immediately, never block in open(2) waiting
// for a reader. Bounded to 5s wall clock — a hung open(2) produces no error
// at all, so only a bound actually distinguishes the fix from the bug. The
// goroutine never calls t.Fatal; the timeout fires on the main test
// goroutine.
func TestWritePIDFile_RefusesFIFOWithoutHanging(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cenci.pid")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("Mkfifo(%q): %v", path, err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- WritePIDFile(path)
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("WritePIDFile() error = nil, want error for a planted FIFO")
		}
		lower := strings.ToLower(err.Error())
		if !strings.Contains(lower, "not a regular file") &&
			!strings.Contains(lower, "device not configured") &&
			!strings.Contains(lower, "no such device") &&
			!strings.Contains(err.Error(), "ENXIO") {
			t.Errorf("error = %q, want it to mention %q or the ENXIO open failure", err.Error(), "not a regular file")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WritePIDFile hung on a planted FIFO")
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%q): %v", path, err)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("expected %q to still be a FIFO, got mode %v", path, info.Mode())
	}
}

// TestReadPIDFile_RefusesFIFOWithoutHanging exercises the read path's
// O_RDONLY|O_NONBLOCK-then-Stat ordering specifically: unlike the write
// path, O_RDONLY|O_NONBLOCK on a FIFO *succeeds* at open time, so the open
// itself never fails here — only the f.Stat()-before-read non-regular
// rejection prevents the hang that a subsequent io.ReadAll on a pollable
// non-blocking FIFO fd would otherwise reintroduce. Bounded to 5s wall
// clock, same rationale as the write-path FIFO test.
func TestReadPIDFile_RefusesFIFOWithoutHanging(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cenci.pid")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("Mkfifo(%q): %v", path, err)
	}

	type result struct {
		pid int
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		pid, err := ReadPIDFile(path)
		resCh <- result{pid, err}
	}()

	select {
	case res := <-resCh:
		if res.err == nil {
			t.Fatalf("ReadPIDFile() error = nil, want error for a planted FIFO (got pid %d)", res.pid)
		}
		lower := strings.ToLower(res.err.Error())
		if !strings.Contains(lower, "not a regular file") {
			t.Errorf("error = %q, want it to mention %q", res.err.Error(), "not a regular file")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadPIDFile hung on a planted FIFO")
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%q): %v", path, err)
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("expected %q to still be a FIFO, got mode %v", path, info.Mode())
	}
}

// TestReadPIDFile_RefusesForeignOwnedFile covers the read-path uid-ownership
// backstop: a regular, well-formed PID file whose owning uid is not ours
// (simulated via the pidFileOwner seam) must be rejected rather than
// returning its PID.
func TestReadPIDFile_RefusesForeignOwnedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cenci.pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}

	restore := pidFileOwner
	pidFileOwner = func() int { return os.Getuid() + 1 }
	defer func() { pidFileOwner = restore }()

	pid, err := ReadPIDFile(path)
	if err == nil {
		t.Fatalf("ReadPIDFile() error = nil, want error for a foreign-owned pid file (got pid %d)", pid)
	}
	if pid != 0 {
		t.Errorf("pid = %d, want 0 on rejection", pid)
	}
	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "not owned") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "not owned")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it to name the path %q", err.Error(), path)
	}
}

// TestReadPIDFile_AcceptsOwnedRegularFile is the happy-path guard against the
// new checks over-rejecting: an unstubbed pidFileOwner, a plain 0600 file
// owned by the current process's uid, must still round-trip the PID
// correctly.
func TestReadPIDFile_AcceptsOwnedRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cenci.pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}

	pid, err := ReadPIDFile(path)
	if err != nil {
		t.Fatalf("ReadPIDFile: unexpected error: %v", err)
	}
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want %d", pid, os.Getpid())
	}
}

// TestReadPIDFile_RefusesWorldWritableFile covers the #1147 LOW-finding fix:
// a regular, current-uid-owned PID file that is nonetheless group/other
// writable (0666) must be rejected — ownership alone is not enough, since any
// local user could still rewrite its contents.
func TestReadPIDFile_RefusesWorldWritableFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cenci.pid")
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o666); err != nil {
		t.Fatal(err)
	}
	// Chmod bypasses umask, ensuring the file is actually group/other
	// writable regardless of the process umask.
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}

	pid, err := ReadPIDFile(path)
	if err == nil {
		t.Fatalf("ReadPIDFile() error = nil, want error for a world-writable pid file (got pid %d)", pid)
	}
	if pid != 0 {
		t.Errorf("pid = %d, want 0 on rejection", pid)
	}
	lower := strings.ToLower(err.Error())
	if !strings.Contains(lower, "writable") && !strings.Contains(lower, "insecure permissions") {
		t.Errorf("error = %q, want it to mention %q or %q", err.Error(), "writable", "insecure permissions")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error = %q, want it to name the path %q", err.Error(), path)
	}
}

// TestStop_SymlinkedPIDFileNeverSignalsPlantedPID is the Stop-level
// regression: a symlinked PID file pointing (via its target's contents) at a
// live child process must never get that child signaled. The read fails, so
// Stop falls through to its pgrepDaemon scan (stubbed here to find nothing),
// and the live child and the untouched symlink are both asserted afterward.
func TestStop_SymlinkedPIDFileNeverSignalsPlantedPID(t *testing.T) {
	withShortStopTimeouts(t)
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "events.sock")
	pidPath := filepath.Join(dir, "cenci.pid")

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	defer func() {
		_ = cmd.Process.Kill()
		<-done
	}()

	target := filepath.Join(dir, "target.pid")
	if err := os.WriteFile(target, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, pidPath); err != nil {
		t.Fatalf("planting symlink %q -> %q: %v", pidPath, target, err)
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	restore := pgrepDaemon
	pgrepDaemon = func() (int, error) { return 0, nil }
	defer func() { pgrepDaemon = restore }()

	if _, err := Stop(socketPath, pidPath); err == nil {
		t.Fatal("expected Stop to error when the pid file is a symlink and pgrep finds nothing")
	}
	if !ProcessAlive(cmd.Process.Pid) {
		t.Error("expected the planted PID's process to remain alive: Stop must not have signaled it")
	}

	linkInfo, lerr := os.Lstat(pidPath)
	if lerr != nil {
		t.Fatalf("Lstat(%q): %v", pidPath, lerr)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Errorf("expected the planted symlink to remain untouched, got mode %v", linkInfo.Mode())
	}
}

// TestStop_UnusablePIDFileLogsReason is the #1147 observability regression:
// Stop against a PID file that ReadPIDFile rejects (here, a symlink) must log
// the specific rejection reason (not just fall through silently) so an
// operator can distinguish a genuine attack signature from routine
// staleness. Reuses arm_test.go's captureLog helper (same package).
func TestStop_UnusablePIDFileLogsReason(t *testing.T) {
	withShortStopTimeouts(t)
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "events.sock")
	pidPath := filepath.Join(dir, "cenci.pid")

	target := filepath.Join(dir, "target.pid")
	if err := os.WriteFile(target, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, pidPath); err != nil {
		t.Fatalf("planting symlink %q -> %q: %v", pidPath, target, err)
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	restore := pgrepDaemon
	pgrepDaemon = func() (int, error) { return 0, nil }
	defer func() { pgrepDaemon = restore }()

	buf := captureLog(t)

	if _, err := Stop(socketPath, pidPath); err == nil {
		t.Fatal("expected Stop to error when the pid file is a symlink and pgrep finds nothing")
	}

	logged := buf.String()
	if !strings.Contains(logged, pidPath) {
		t.Errorf("log output = %q, want it to name the pid file path %q", logged, pidPath)
	}
	if !strings.Contains(strings.ToLower(logged), "unusable") {
		t.Errorf("log output = %q, want it to mention the pid file is unusable", logged)
	}
}

// TestStop_MissingPIDFileLogsNoWarning covers #1147 Fix 3: a plain missing
// PID file (the common case -- no daemon has ever run, or a prior clean stop
// already removed it) must NOT produce the "pid file unusable" warning,
// since it would otherwise fire on every idle-host stop/restart and drown
// out genuine attack signatures (symlink, FIFO, foreign-owned,
// insecure-permissions), which TestStop_UnusablePIDFileLogsReason above
// confirms still log.
func TestStop_MissingPIDFileLogsNoWarning(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "events.sock")
	pidPath := filepath.Join(dir, "cenci.pid") // never written: missing

	buf := captureLog(t)

	outcome, err := Stop(socketPath, pidPath)
	if err != nil {
		t.Fatalf("Stop: unexpected error: %v", err)
	}
	if outcome.WasRunning {
		t.Error("expected WasRunning=false when nothing is running")
	}

	logged := buf.String()
	if strings.Contains(logged, "unusable") {
		t.Errorf("log output = %q, want no \"unusable\" warning for a plain missing pid file", logged)
	}
}

// TestStop_ForeignOwnedPIDFileNeverSignalsPlantedPID is the Stop-level
// regression for the ownership guard specifically (not the symlink guard): a
// regular, own-uid-*written* PID file holding a live child's PID, but read
// back under a stubbed foreign pidFileOwner, must never get that child
// signaled — Stop must fall through to pgrepDaemon (stubbed to find
// nothing) and leave the child alive.
func TestStop_ForeignOwnedPIDFileNeverSignalsPlantedPID(t *testing.T) {
	withShortStopTimeouts(t)
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "events.sock")
	pidPath := filepath.Join(dir, "cenci.pid")

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	defer func() {
		_ = cmd.Process.Kill()
		<-done
	}()

	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatal(err)
	}

	restoreOwner := pidFileOwner
	pidFileOwner = func() int { return os.Getuid() + 1 }
	defer func() { pidFileOwner = restoreOwner }()

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	restorePgrep := pgrepDaemon
	pgrepDaemon = func() (int, error) { return 0, nil }
	defer func() { pgrepDaemon = restorePgrep }()

	if _, err := Stop(socketPath, pidPath); err == nil {
		t.Fatal("expected Stop to error when the pid file is foreign-owned and pgrep finds nothing")
	}
	if !ProcessAlive(cmd.Process.Pid) {
		t.Error("expected the planted PID's process to remain alive: Stop must not have signaled it")
	}
}
