package babysit

// Ticket #975: resolve and persist babysit's launch target at arm time, gate
// every launch() call on the recorded session actually existing, and wire
// the detached supervisor's stdout/stderr into a 0600 per-repo/PR append
// log. These tests pin the Test Strategy table's named behaviors; the
// production symbols they exercise beyond State.LaunchSession/LaunchDir and
// Options.Session/Dir (the currentTmuxSession/tmuxHasSession/startSupervisor
// seams) are declared in tmux.go/babysit.go as unimplemented stubs -- test
// infrastructure only, per the ticket's red phase -- so every test here is
// expected to fail on its behavioral assertions, not to fail to compile.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// expectedLogPath mirrors statePath's own hashing formula
// (crypto/sha256(repo)[:6] hex, joined with "-<pr>") but for the new
// per-repo/PR supervisor log file (auto-adopted answer #6): a self-contained
// helper so these tests do not depend on a not-yet-existing production
// logPath symbol.
func expectedLogPath(dir, repo, pr string) string {
	sum := sha256.Sum256([]byte(repo))
	return filepath.Join(dir, hex.EncodeToString(sum[:6])+"-"+pr+".log")
}

// launchCallArgs returns the recorded command() call (as captured by
// withCommands' calls slice) that self-execs `cenci run <workflow> ...`,
// alongside whether one was found at all.
func launchCallArgs(calls [][]string, workflow string) ([]string, bool) {
	for _, c := range calls {
		if len(c) > 2 && c[1] == "run" && c[2] == workflow {
			return c, true
		}
	}
	return nil, false
}

// assertFlagValue asserts args carries flag immediately followed by want.
func assertFlagValue(t *testing.T, args []string, flag, want string) {
	t.Helper()
	for i, a := range args {
		if a == flag {
			if i+1 >= len(args) {
				t.Errorf("launch argv %v: %s has no following value", args, flag)
				return
			}
			if args[i+1] != want {
				t.Errorf("launch argv %v: %s = %q, want %q", args, flag, args[i+1], want)
			}
			return
		}
	}
	t.Errorf("launch argv %v: %s not found", args, flag)
}

// installHarmlessSelfExecShim overwrites os.Args[0] (restored via
// t.Cleanup) with a tiny script that exits immediately. Run's detach/arm
// branch still calls cmd.Start() directly against os.Args[0] today (the
// #975 startSupervisor seam these tests stub is not yet wired into Run --
// that is Phase 4's job), and under `go test` os.Args[0] is this very test
// binary -- spawning it unmodified would recursively re-run the whole
// suite. Mirrors internal/dispatch/chainfake_test.go's
// installFakeCenciProcess pattern for the identical os.Args[0]-self-exec
// hazard, scoped to this package's own arm-mode tests. Returns the shim
// path, for building the expected argv.
func installHarmlessSelfExecShim(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "harmless-exec")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := os.Args[0]
	os.Args[0] = path
	t.Cleanup(func() { os.Args[0] = orig })
	return path
}

// -- AC 2: arm-time resolution persisted before the first poll --------------

// TestArmResolvesAndPersistsLaunchTargetBeforeFirstTick extends
// TestRunWritesStateBeforeFirstTick's pattern (babysit_test.go:311): reads
// the state file from inside the stubbed first `gh pr view` call, and
// asserts LaunchSession/LaunchDir were resolved and persisted before tick
// ever ran -- not merely eventually, on some later save.
func TestArmResolvesAndPersistsLaunchTargetBeforeFirstTick(t *testing.T) {
	dir := t.TempDir()
	var atFirstTick *State
	originalCommand := command
	command = func(name string, args ...string) ([]byte, error) {
		if name == "git" {
			return []byte("/repo/root\n"), nil
		}
		return []byte(""), nil
	}
	originalCurrentTmuxSession := currentTmuxSession
	currentTmuxSession = func() (string, error) { return "host-pane-session", nil }
	originalExecGh := execGh
	execGh = func(args ...string) (string, string, error) {
		switch {
		case len(args) > 0 && args[0] == "repo":
			return "o/r\n", "", nil
		case len(args) > 1 && args[0] == "pr" && args[1] == "view":
			s := load(statePath(dir, "o/r", "42"))
			atFirstTick = &s
			return "", "", errors.New("exit status 1")
		}
		return "", "", nil
	}
	t.Cleanup(func() {
		command = originalCommand
		execGh = originalExecGh
		currentTmuxSession = originalCurrentTmuxSession
	})

	if err := Run(Options{PR: "42", Agent: "claude", StateDir: dir, Interval: time.Minute, Once: true}); err == nil {
		t.Fatal("Run: want the stubbed gh failure to surface")
	}
	if atFirstTick == nil {
		t.Fatal("the first tick never ran")
	}
	if atFirstTick.LaunchSession != "host-pane-session" {
		t.Errorf("state at first tick has LaunchSession %q, want %q", atFirstTick.LaunchSession, "host-pane-session")
	}
	if atFirstTick.LaunchDir != "/repo/root" {
		t.Errorf("state at first tick has LaunchDir %q, want %q", atFirstTick.LaunchDir, "/repo/root")
	}
}

// -- AC 3: every launch() call site passes the recorded target --------------

// TestLaunchPassesRecordedSessionAndDir drives tick() through all three
// launch() call sites (ci-repair, babysit-attention, address-review) with a
// recorded LaunchSession/LaunchDir on State, and asserts the exact
// --session/--dir values reach the launched `cenci run` argv at every site.
func TestLaunchPassesRecordedSessionAndDir(t *testing.T) {
	for _, tc := range []struct {
		name           string
		state          State
		responses      []string
		workflow       string
		wantNeedsInput bool
	}{
		{
			name: "ci-repair",
			state: State{
				PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 300, CurrentDelaySeconds: 900,
				LaunchSession: "work", LaunchDir: "/repo/root",
			},
			responses: []string{openPR(), `[{"bucket":"fail","name":"test","state":"FAILURE"}]`, `[]`, `[]`},
			workflow:  "ci-repair",
		},
		{
			name: "babysit-attention",
			state: State{
				PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 300, CurrentDelaySeconds: 900,
				FixAttempts:   fixCap,
				LaunchSession: "work", LaunchDir: "/repo/root",
			},
			responses:      []string{openPR(), `[{"bucket":"fail","name":"test","state":"FAILURE"}]`},
			workflow:       "babysit-attention",
			wantNeedsInput: true,
		},
		{
			name: "address-review",
			state: State{
				PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 300, CurrentDelaySeconds: 900,
				LaunchSession: "work", LaunchDir: "/repo/root",
			},
			responses: []string{openPR(), `[]`, `[{"id":7,"updated_at":"2026-01-02T00:00:00Z","user":{"login":"reviewer"}}]`, `[]`, unresolvedThreadFor(7)},
			workflow:  "address-review",
		},
		{
			// #995: the conflict-escalation launch site, alongside the
			// existing FixAttempts: fixCap attention case above -- unlike
			// that case, the conflict path never returns errNeedsInput
			// (wantNeedsInput stays false, the ticket's Decision).
			name: "babysit-attention-conflict",
			state: State{
				PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 300, CurrentDelaySeconds: 900,
				LastHeadSHA:   "abc",
				LaunchSession: "work", LaunchDir: "/repo/root",
			},
			responses: []string{conflictingOpenPR("abc"), `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`, `[]`, `[]`},
			workflow:  "babysit-attention",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls [][]string
			withCommands(t, tc.responses, &calls)
			s := tc.state
			_, _, err := tick(&s)
			switch {
			case tc.wantNeedsInput:
				if !errors.Is(err, errNeedsInput) {
					t.Fatalf("tick err = %v, want errors.Is(err, errNeedsInput)", err)
				}
			case err != nil:
				t.Fatalf("tick: %v", err)
			}
			args, found := launchCallArgs(calls, tc.workflow)
			if !found {
				t.Fatalf("%s: no launch call recorded for workflow %q: %#v", tc.name, tc.workflow, calls)
			}
			assertFlagValue(t, args, "--session", "work")
			assertFlagValue(t, args, "--dir", "/repo/root")
		})
	}
}

// -- AC 4: a missing/absent recorded session fails loudly --------------------

// TestLaunchFailsWhenRecordedSessionGone covers the probe-false branch: the
// recorded session no longer exists at launch time. The launch must fail
// with an error naming that session, issue zero `cenci run` calls, and
// persist AutomergeReason == reasonWorkflowLaunchFailed through the
// existing recordUpstreamReadFailure retry path.
func TestLaunchFailsWhenRecordedSessionGone(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	withCommands(t, []string{openPR(), `[{"bucket":"fail","name":"test","state":"FAILURE"}]`}, &calls)
	originalTmuxHasSession := tmuxHasSession
	tmuxHasSession = func(session string) (bool, error) { return false, nil }
	t.Cleanup(func() { tmuxHasSession = originalTmuxHasSession })

	s := State{
		PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 300, CurrentDelaySeconds: 900,
		// FixAttempts stays below fixCap, so this drives the ci-repair call
		// site: a direct, unwrapped tick error, not the babysit-attention
		// site's errNeedsInput sentinel -- cleaner for the content-specific
		// error-text assertion below.
		LaunchSession: "gone-session", LaunchDir: "/repo/dir",
	}
	_, _, err := tick(&s)
	if err == nil {
		t.Fatal("tick: err = nil, want the missing recorded session to fail the launch")
	}
	if !strings.Contains(err.Error(), "gone-session") {
		t.Fatalf("tick err = %q, want it to name the recorded session %q", err.Error(), "gone-session")
	}
	for _, c := range calls {
		if len(c) > 1 && c[1] == "run" {
			t.Fatalf("no cenci run call must be made when the recorded session is gone: %#v", calls)
		}
	}
	if s.AutomergeReason != reasonWorkflowLaunchFailed {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonWorkflowLaunchFailed)
	}
}

// TestLaunchFailsForConflictWhenRecordedSessionGone is #995's conflict-path
// analogue of TestLaunchFailsWhenRecordedSessionGone: a DIRTY PR whose
// recorded tmux session no longer exists must fail the babysit-attention
// launch loudly, issue zero `cenci run` calls, leave ConflictNotifiedSHA
// unset (so the next tick retries), and persist
// AutomergeReason == reasonWorkflowLaunchFailed through the existing
// recordUpstreamReadFailure retry path.
func TestLaunchFailsForConflictWhenRecordedSessionGone(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	withCommands(t, []string{conflictingOpenPR("abc"), `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`}, &calls)
	originalTmuxHasSession := tmuxHasSession
	tmuxHasSession = func(session string) (bool, error) { return false, nil }
	t.Cleanup(func() { tmuxHasSession = originalTmuxHasSession })

	s := State{
		PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 300, CurrentDelaySeconds: 900,
		LastHeadSHA:   "abc",
		LaunchSession: "gone-session", LaunchDir: "/repo/dir",
	}
	_, _, err := tick(&s)
	if err == nil {
		t.Fatal("tick: err = nil, want the missing recorded session to fail the conflict-path launch")
	}
	if !strings.Contains(err.Error(), "gone-session") {
		t.Fatalf("tick err = %q, want it to name the recorded session %q", err.Error(), "gone-session")
	}
	for _, c := range calls {
		if len(c) > 1 && c[1] == "run" {
			t.Fatalf("no cenci run call must be made when the recorded session is gone: %#v", calls)
		}
	}
	if s.AutomergeReason != reasonWorkflowLaunchFailed {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonWorkflowLaunchFailed)
	}
	if s.ConflictNotifiedSHA != "" {
		t.Fatalf("ConflictNotifiedSHA = %q, want empty: a failed launch must never record the dedup marker", s.ConflictNotifiedSHA)
	}
}

// TestLaunchFailsWhenNoSessionRecorded covers the "never recorded at all"
// branch (armed outside tmux, AC 6): the probe must never even be called,
// and no `cenci run` call may be made.
func TestLaunchFailsWhenNoSessionRecorded(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	withCommands(t, []string{openPR(), `[{"bucket":"fail","name":"test","state":"FAILURE"}]`}, &calls)
	probeCalled := false
	originalTmuxHasSession := tmuxHasSession
	tmuxHasSession = func(session string) (bool, error) { probeCalled = true; return true, nil }
	t.Cleanup(func() { tmuxHasSession = originalTmuxHasSession })

	s := State{
		PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 300, CurrentDelaySeconds: 900,
		// FixAttempts stays below fixCap (ci-repair site); LaunchSession is
		// deliberately left empty: never recorded.
	}
	_, _, err := tick(&s)
	if err == nil {
		t.Fatal("tick: err = nil, want an empty recorded session to fail the launch")
	}
	if probeCalled {
		t.Fatal("tmuxHasSession must not be called when no session was ever recorded")
	}
	for _, c := range calls {
		if len(c) > 1 && c[1] == "run" {
			t.Fatalf("no cenci run call must be made when no session was recorded: %#v", calls)
		}
	}
}

// -- AC 5: supervisor log wiring + AC 2's flags on the detached child -------

// TestArmDetachWiresSupervisorLogAndLaunchFlags covers the detached
// supervisor spawn: the startSupervisor seam must capture a *exec.Cmd whose
// Stdout/Stderr are both the per-repo/PR log file, whose env carries
// CENCI_BABYSIT_SUPERVISOR=1, whose SysProcAttr detaches (Setsid), and whose
// argv carries the resolved --session/--dir.
func TestArmDetachWiresSupervisorLogAndLaunchFlags(t *testing.T) {
	installHarmlessSelfExecShim(t)
	dir := t.TempDir()
	t.Setenv("CENCI_BABYSIT_SUPERVISOR", "")
	// #1094 isolation fix (watch/docs/test-isolation.md #202): CENCI_SANDBOX=1
	// is ambient in this dev container, and #1094 adds a CENCI_SANDBOX=1
	// forward branch to Run that would otherwise silently divert this
	// non-Once arming path into the container-forward path instead of the
	// detached-supervisor path this test actually exercises.
	t.Setenv("CENCI_SANDBOX", "")

	var captured *exec.Cmd
	originalStartSupervisor := startSupervisor
	startSupervisor = func(cmd *exec.Cmd) error {
		captured = cmd
		return nil
	}
	originalCommand := command
	command = func(name string, args ...string) ([]byte, error) {
		if name == "git" {
			return []byte("/repo/root\n"), nil
		}
		return []byte(""), nil
	}
	originalExecGh := execGh
	execGh = func(args ...string) (string, string, error) {
		if len(args) > 0 && args[0] == "repo" {
			return "o/r\n", "", nil
		}
		return "", "", nil
	}
	originalCurrentTmuxSession := currentTmuxSession
	currentTmuxSession = func() (string, error) { return "host-session", nil }
	t.Cleanup(func() {
		startSupervisor = originalStartSupervisor
		command = originalCommand
		execGh = originalExecGh
		currentTmuxSession = originalCurrentTmuxSession
	})

	if err := Run(Options{PR: "42", Agent: "claude", StateDir: dir, Interval: time.Minute}); err != nil {
		t.Fatalf("Run (arm): %v", err)
	}

	if captured == nil {
		t.Fatal("startSupervisor was never invoked; want the arming path to hand off the supervisor child through the seam")
	}
	wantLog := expectedLogPath(dir, "o/r", "42")
	stdout, ok := captured.Stdout.(*os.File)
	if !ok || stdout.Name() != wantLog {
		t.Errorf("captured.Stdout = %#v, want an *os.File for %q", captured.Stdout, wantLog)
	}
	stderr, ok := captured.Stderr.(*os.File)
	if !ok || stderr.Name() != wantLog {
		t.Errorf("captured.Stderr = %#v, want an *os.File for %q", captured.Stderr, wantLog)
	}
	foundEnv := false
	for _, e := range captured.Env {
		if e == "CENCI_BABYSIT_SUPERVISOR=1" {
			foundEnv = true
		}
	}
	if !foundEnv {
		t.Errorf("captured.Env = %v, want CENCI_BABYSIT_SUPERVISOR=1", captured.Env)
	}
	if captured.SysProcAttr == nil || !captured.SysProcAttr.Setsid {
		t.Errorf("captured.SysProcAttr = %#v, want Setsid true", captured.SysProcAttr)
	}
	assertFlagValue(t, captured.Args, "--session", "host-session")
	assertFlagValue(t, captured.Args, "--dir", "/repo/root")
}

// TestSupervisorLogFileIs0600Append covers AC 5's file-mode/append
// contract: the log file must end up mode 0600 even when it pre-existed at
// a looser mode, and a second arm must append rather than truncate.
func TestSupervisorLogFileIs0600Append(t *testing.T) {
	installHarmlessSelfExecShim(t)
	dir := t.TempDir()
	t.Setenv("CENCI_BABYSIT_SUPERVISOR", "")
	// #1094 isolation fix (watch/docs/test-isolation.md #202): see the
	// identical comment in TestArmDetachWiresSupervisorLogAndLaunchFlags.
	t.Setenv("CENCI_SANDBOX", "")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	wantLog := expectedLogPath(dir, "o/r", "42")
	if err := os.WriteFile(wantLog, []byte("pre-existing\n"), 0644); err != nil {
		t.Fatal(err)
	}

	var captured []*exec.Cmd
	originalStartSupervisor := startSupervisor
	startSupervisor = func(cmd *exec.Cmd) error {
		captured = append(captured, cmd)
		if f, ok := cmd.Stdout.(*os.File); ok {
			_, _ = f.WriteString("run-output\n")
		}
		return nil
	}
	originalCommand := command
	command = func(name string, args ...string) ([]byte, error) {
		if name == "git" {
			return []byte("/repo/root\n"), nil
		}
		return []byte(""), nil
	}
	originalExecGh := execGh
	execGh = func(args ...string) (string, string, error) {
		if len(args) > 0 && args[0] == "repo" {
			return "o/r\n", "", nil
		}
		return "", "", nil
	}
	originalCurrentTmuxSession := currentTmuxSession
	currentTmuxSession = func() (string, error) { return "host-session", nil }
	t.Cleanup(func() {
		startSupervisor = originalStartSupervisor
		command = originalCommand
		execGh = originalExecGh
		currentTmuxSession = originalCurrentTmuxSession
	})

	if err := Run(Options{PR: "42", Agent: "claude", StateDir: dir, Interval: time.Minute}); err != nil {
		t.Fatalf("Run (arm 1): %v", err)
	}
	if err := Run(Options{PR: "42", Agent: "claude", StateDir: dir, Interval: time.Minute}); err != nil {
		t.Fatalf("Run (arm 2): %v", err)
	}
	if len(captured) != 2 {
		t.Fatalf("startSupervisor invocations = %d, want 2", len(captured))
	}

	info, err := os.Stat(wantLog)
	if err != nil {
		t.Fatalf("stat log file: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("log file mode = %o, want 0600 even though it pre-existed at 0644", info.Mode().Perm())
	}
	got, err := os.ReadFile(wantLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "pre-existing") {
		t.Fatalf("log file content = %q, want the pre-existing content preserved (append, not truncate)", got)
	}
	if !strings.Contains(string(got), "run-output") {
		t.Fatalf("log file content = %q, want the new supervisor output appended", got)
	}
}

// -- AC 6: outside-tmux arming still succeeds, warns, records "" -----------

// TestArmOutsideTmuxWarnsAndRecordsEmptySession covers AC 6 through the
// --once/foreground path (extending TestRunWritesStateBeforeFirstTick's
// pattern): a session-resolution failure must still let arming succeed,
// must print an explicit warning to stderr, and must persist an empty
// LaunchSession.
func TestArmOutsideTmuxWarnsAndRecordsEmptySession(t *testing.T) {
	dir := t.TempDir()
	var atFirstTick *State
	originalCommand := command
	command = func(name string, args ...string) ([]byte, error) {
		if name == "git" {
			return []byte("/repo/root\n"), nil
		}
		return []byte(""), nil
	}
	originalExecGh := execGh
	execGh = func(args ...string) (string, string, error) {
		switch {
		case len(args) > 0 && args[0] == "repo":
			return "o/r\n", "", nil
		case len(args) > 1 && args[0] == "pr" && args[1] == "view":
			s := load(statePath(dir, "o/r", "42"))
			atFirstTick = &s
			return "", "", errors.New("exit status 1")
		}
		return "", "", nil
	}
	originalCurrentTmuxSession := currentTmuxSession
	currentTmuxSession = func() (string, error) { return "", errors.New("TMUX_PANE is not set; not running inside a tmux pane") }
	t.Cleanup(func() {
		command = originalCommand
		execGh = originalExecGh
		currentTmuxSession = originalCurrentTmuxSession
	})

	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStderr := os.Stderr
	os.Stderr = stderrW

	runErr := Run(Options{PR: "42", Agent: "claude", StateDir: dir, Interval: time.Minute, Once: true})

	os.Stderr = originalStderr
	_ = stderrW.Close()
	var buf strings.Builder
	stderrBytes := make([]byte, 4096)
	n, _ := stderrR.Read(stderrBytes)
	buf.Write(stderrBytes[:n])
	_ = stderrR.Close()

	if runErr == nil {
		t.Fatal("Run: want the stubbed gh failure to surface (unrelated to the tmux-outside warning)")
	}
	if atFirstTick == nil {
		t.Fatal("the first tick never ran")
	}
	if atFirstTick.LaunchSession != "" {
		t.Errorf("LaunchSession = %q, want empty when armed outside tmux", atFirstTick.LaunchSession)
	}
	warning := buf.String()
	if !strings.Contains(warning, "no repair window") {
		t.Errorf("stderr = %q, want an explicit warning that no repair window can be opened", warning)
	}
}

// -- AC 7: inside-tmux supervision is otherwise unchanged --------------------

// TestArmInsideTmuxSupervisionOtherwiseUnchanged asserts the pre-#975
// detached-spawn invariants (argv shape, env base, Setsid, Stdin) still
// hold exactly, with only the two new --session/--dir flags appended --
// "the only differences are the persisted launch target and the log file"
// (ticket Decision).
func TestArmInsideTmuxSupervisionOtherwiseUnchanged(t *testing.T) {
	shimPath := installHarmlessSelfExecShim(t)
	dir := t.TempDir()
	t.Setenv("CENCI_BABYSIT_SUPERVISOR", "")
	// #1094 isolation fix (watch/docs/test-isolation.md #202): see the
	// identical comment in TestArmDetachWiresSupervisorLogAndLaunchFlags.
	t.Setenv("CENCI_SANDBOX", "")

	var captured *exec.Cmd
	originalStartSupervisor := startSupervisor
	startSupervisor = func(cmd *exec.Cmd) error {
		captured = cmd
		return nil
	}
	originalCommand := command
	command = func(name string, args ...string) ([]byte, error) {
		if name == "git" {
			return []byte("/repo/root\n"), nil
		}
		return []byte(""), nil
	}
	originalExecGh := execGh
	execGh = func(args ...string) (string, string, error) {
		if len(args) > 0 && args[0] == "repo" {
			return "o/r\n", "", nil
		}
		return "", "", nil
	}
	originalCurrentTmuxSession := currentTmuxSession
	currentTmuxSession = func() (string, error) { return "host-session", nil }
	t.Cleanup(func() {
		startSupervisor = originalStartSupervisor
		command = originalCommand
		execGh = originalExecGh
		currentTmuxSession = originalCurrentTmuxSession
	})

	if err := Run(Options{PR: "42", Agent: "claude", StateDir: dir, Interval: time.Minute}); err != nil {
		t.Fatalf("Run (arm): %v", err)
	}
	if captured == nil {
		t.Fatal("startSupervisor was never invoked")
	}

	wantArgs := []string{
		shimPath, "babysit", "42", "--agent", "claude", "--interval", "1m0s", "--state-dir", dir,
		"--session", "host-session", "--dir", "/repo/root",
	}
	if !reflect.DeepEqual(captured.Args, wantArgs) {
		t.Errorf("captured.Args = %v, want %v", captured.Args, wantArgs)
	}
	if captured.Stdin != nil {
		t.Errorf("captured.Stdin = %v, want nil", captured.Stdin)
	}
	if captured.SysProcAttr == nil || !captured.SysProcAttr.Setsid {
		t.Errorf("captured.SysProcAttr = %#v, want Setsid true", captured.SysProcAttr)
	}
	foundEnv := false
	for _, e := range captured.Env {
		if e == "CENCI_BABYSIT_SUPERVISOR=1" {
			foundEnv = true
		}
	}
	if !foundEnv {
		t.Errorf("captured.Env = %v, want CENCI_BABYSIT_SUPERVISOR=1", captured.Env)
	}
	if len(captured.Env) < 2 {
		t.Errorf("captured.Env = %v, want the inherited os.Environ() base plus CENCI_BABYSIT_SUPERVISOR=1", captured.Env)
	}
}
