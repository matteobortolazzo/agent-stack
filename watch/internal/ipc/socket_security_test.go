package ipc

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matteobortolazzo/cenci/watch/v2/pkg/watch"
)

// TestSafeListen_RefusesEmptyPath covers #1147 Fix 1: net.Listen("unix", "")
// does NOT fail closed on Linux -- the kernel autobinds into the abstract
// namespace, producing a listener with no filesystem permissions at all,
// connectable by any local process in the same network namespace. safeListen
// must refuse an empty path outright rather than falling through its
// symlink/probe logic (os.Lstat("") returns ENOENT, which os.IsNotExist would
// otherwise treat as "nothing there yet") straight into net.Listen.
func TestSafeListen_RefusesEmptyPath(t *testing.T) {
	ln, err := safeListen("")
	if err == nil {
		_ = ln.Close()
		t.Fatal("safeListen(\"\") error = nil, want error")
	}
	if ln != nil {
		t.Errorf("safeListen(\"\") listener = %v, want nil", ln)
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "empty")
	}
}

func TestSafeListen_RejectsSymlink(t *testing.T) {
	dir := tempSocketDir(t)
	socketPath := filepath.Join(dir, "s.sock")
	target := filepath.Join(dir, "evil-target")

	// Create a symlink at the socket path.
	if err := os.Symlink(target, socketPath); err != nil {
		t.Fatal(err)
	}

	ln, err := safeListen(socketPath)
	if err == nil {
		_ = ln.Close()
		t.Fatal("expected error for symlink, got nil")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("expected error mentioning symlink, got: %v", err)
	}
}

func TestSafeListen_WorksWithCleanPath(t *testing.T) {
	dir := tempSocketDir(t)
	socketPath := filepath.Join(dir, "s.sock")

	ln, err := safeListen(socketPath)
	if err != nil {
		t.Fatalf("safeListen() error: %v", err)
	}
	defer func() { _ = ln.Close() }()

	// Verify we can connect to the listener.
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to connect to socket: %v", err)
	}
	_ = conn.Close()
}

func TestSafeListen_RemovesStaleSocket(t *testing.T) {
	dir := tempSocketDir(t)
	socketPath := filepath.Join(dir, "s.sock")

	// Simulate a stale non-symlink file left behind (e.g., from a crash).
	if err := os.WriteFile(socketPath, nil, 0600); err != nil {
		t.Fatal(err)
	}

	// Verify the stale file exists.
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("stale socket file should exist: %v", err)
	}

	// safeListen should remove the stale file and create a new listener.
	ln, err := safeListen(socketPath)
	if err != nil {
		t.Fatalf("safeListen() error: %v", err)
	}
	defer func() { _ = ln.Close() }()

	// Verify the new listener works.
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("failed to connect to new socket: %v", err)
	}
	_ = conn.Close()
}

func TestDefaultEventSocketPath_UsesSecureDir(t *testing.T) {
	socketDir := t.TempDir()
	t.Setenv("CENCI_SOCKET_DIR", socketDir)

	got := DefaultEventSocketPath()
	secureDir, err := watch.SocketDir()
	if err != nil {
		t.Fatalf("watch.SocketDir() error: %v", err)
	}

	if !strings.HasPrefix(got, secureDir) {
		t.Errorf("DefaultEventSocketPath() = %q, want prefix %q", got, secureDir)
	}
}

// TestDefaultEventSocketPath_PoisonedBase_ReturnsEmpty covers #1147's
// security-relevant classification in DefaultEventSocketPath: when
// watch.SocketDir() fails because the tmp tier's base was proven hostile
// (0777, group/other writable), DefaultEventSocketPath must return ""
// rather than falling back to the unhardened flat
// /tmp/cenci-events-<uid>.sock path, mirroring
// TestDefaultSocketPath_PoisonedBase_ReturnsEmpty and
// TestDefaultPIDPath_PoisonedBase_ReturnsEmpty in pkg/watch/socket_test.go.
func TestDefaultEventSocketPath_PoisonedBase_ReturnsEmpty(t *testing.T) {
	t.Setenv("CENCI_SOCKET_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)

	base := filepath.Join(tmpRoot, fmt.Sprintf("cenci-%d", os.Getuid()))
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatalf("pre-creating base %q: %v", base, err)
	}
	// Chmod bypasses umask, ensuring the base is actually group/other
	// writable regardless of the process umask -- a proven-hostile base per
	// hardenDir, so watch.SocketDir() must return an error classified via
	// watch.IsInsecureDirError.
	if err := os.Chmod(base, 0777); err != nil {
		t.Fatalf("chmod %q: %v", base, err)
	}

	got := DefaultEventSocketPath()
	if got != "" {
		t.Errorf("DefaultEventSocketPath() = %q, want \"\" (must not fall back to an unhardened /tmp path when the base is proven hostile)", got)
	}
}
