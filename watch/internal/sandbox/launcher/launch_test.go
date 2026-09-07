package launcher

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestValidateAgent pins the launcher's single agent gate: claude, codex, and
// opencode (#490) are the only accepted values; anything else is a usage
// error.
func TestValidateAgent(t *testing.T) {
	cases := []struct {
		agent   string
		wantErr bool
	}{
		{"claude", false},
		{"codex", false},
		{"opencode", false},
		{"gemini", true},
		{"", true},
	}
	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			err := ValidateAgent(tc.agent)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateAgent(%q) = nil, want a usage error", tc.agent)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateAgent(%q) = %v, want nil", tc.agent, err)
			}
		})
	}
}

// TestDefaultModel pins the per-agent model default applied when neither a
// shortcut nor an explicit --model chose one. OpenCode has no cenci-side
// model default: config-driven permissions with no --model equivalent to
// force, so DefaultModel("opencode") must return "" and --model is only
// forwarded when the user explicitly passes one (#490).
func TestDefaultModel(t *testing.T) {
	cases := []struct {
		agent string
		want  string
	}{
		{"claude", "sonnet"},
		{"codex", "gpt-5.6-terra"},
		{"opencode", ""},
	}
	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			if got := DefaultModel(tc.agent); got != tc.want {
				t.Errorf("DefaultModel(%q) = %q, want %q", tc.agent, got, tc.want)
			}
		})
	}
}

// Creation-time env must never carry TMUX_PANE: `docker run -e TMUX_PANE=...`
// lands in /proc/1/environ of the long-lived shared container and goes stale
// as soon as the creating pane closes, at which point `cenci sandbox
// reap-orphans` classified PID 1 as an orphan, killed it, and tore down the
// container with every attached agent session (#356). Pane identity is
// injected per exec session instead (Launch's execEnvArgs).
func TestAssembleRunArgs_NoCreationTimeTmuxPane(t *testing.T) {
	t.Setenv("TMUX_PANE", "%20")
	home := t.TempDir()
	e := &Engine{Runtime: "docker", Stderr: &bytes.Buffer{}}
	scope := Scope{
		ContainerName:     "claude-cenci-repo",
		VolumeName:        "claude-cenci-repo-home",
		Hostname:          "cenci-repo",
		Image:             "claude-cenci-repo:latest",
		WorkspaceBindHost: t.TempDir(),
		Workdir:           "/workspace",
		WorkspaceScope:    "repo",
	}

	args, err := e.assembleRunArgs("claude", "/usr/local/bin/cenci", "/run/user/1000/cenci", true, scope, Options{Agent: "claude"}, home, false, false, DefaultSandboxPlugins())
	if err != nil {
		t.Fatalf("assembleRunArgs: %v", err)
	}

	for _, a := range args {
		if strings.Contains(a, "TMUX_PANE") {
			t.Errorf("container-creation args must not carry TMUX_PANE (goes stale on the shared container, #356); got %q in:\n%s", a, strings.Join(args, " "))
		}
	}
}

// -- ticket #759: provider API key VALUES must never appear in argv ---------
//
// `docker exec -e NAME=value ...`/`docker run ... -e NAME=value ...` argv is
// readable by any host user via `ps` for the entire session (execAttach's
// syscall.Exec handoff never returns). Both call sites that ever forward a
// provider API key -- assembleExecEnv (per-exec attach path) and
// Engine.assembleEnv (container-create path's CONTEXT7_API_KEY forward) --
// must emit only the bare "-e NAME" token, letting the runtime CLI read the
// value from its own inherited environment instead. Assertions below use
// slices.Contains for an exact-token match against a bare "-e NAME" entry,
// as opposed to strings.Contains against the joined argv, which would also
// match the value-bearing "NAME=value" form.

// providerSecretEnvNames are the three host env vars whose VALUES must never
// appear in any assembled argv token (ticket #759's AC list).
var providerSecretEnvNames = []string{"CONTEXT7_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"}

// TestAssembleExecEnvAndAssembleEnv_NeverEmitProviderSecretValues is the
// core AC test: with distinct sentinel values set for all three provider
// secret names, neither assembleExecEnv (every agent) nor Engine.assembleEnv
// (the create-time CONTEXT7_API_KEY forward) may ever emit a sentinel value
// anywhere in their argv, or a "NAME=" prefixed token for any of the three
// names -- only the bare "-e NAME" form. Per-agent scoping (#490) is
// verified against the actual assembleExecEnv switch: opencode forwards
// ANTHROPIC_API_KEY+OPENAI_API_KEY, codex forwards OPENAI_API_KEY only,
// claude forwards neither -- CONTEXT7_API_KEY is unscoped (forwarded for
// every agent when set).
func TestAssembleExecEnvAndAssembleEnv_NeverEmitProviderSecretValues(t *testing.T) {
	sentinels := map[string]string{
		"CONTEXT7_API_KEY":  "sentinel-secret-value-context7",
		"ANTHROPIC_API_KEY": "sentinel-secret-value-anthropic",
		"OPENAI_API_KEY":    "sentinel-secret-value-openai",
	}
	for _, name := range providerSecretEnvNames {
		t.Setenv(name, sentinels[name])
	}

	cases := []struct {
		agent          string
		wantExecNames  []string // bare -e NAME tokens assembleExecEnv must emit
		wantExecAbsent []string // names assembleExecEnv must never emit at all
	}{
		{
			agent:          "claude",
			wantExecNames:  []string{"CONTEXT7_API_KEY"},
			wantExecAbsent: []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"},
		},
		{
			agent:          "codex",
			wantExecNames:  []string{"CONTEXT7_API_KEY", "OPENAI_API_KEY"},
			wantExecAbsent: []string{"ANTHROPIC_API_KEY"},
		},
		{
			agent:         "opencode",
			wantExecNames: []string{"CONTEXT7_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.agent, func(t *testing.T) {
			execArgs := assembleExecEnv(tc.agent)
			e := &Engine{Runtime: "docker", Stderr: &bytes.Buffer{}}
			createArgs := e.assembleEnv(tc.agent, Scope{WorkspaceScope: "s"}, Options{}, false, DefaultSandboxPlugins())

			execJoined := strings.Join(execArgs, " ")
			createJoined := strings.Join(createArgs, " ")

			for _, name := range providerSecretEnvNames {
				secret := sentinels[name]
				if strings.Contains(execJoined, secret) {
					t.Errorf("assembleExecEnv(%q) leaks the %s sentinel value; got: %s", tc.agent, name, execJoined)
				}
				if strings.Contains(createJoined, secret) {
					t.Errorf("assembleEnv(%q) leaks the %s sentinel value; got: %s", tc.agent, name, createJoined)
				}
			}

			for _, name := range tc.wantExecNames {
				if !slices.Contains(execArgs, name) {
					t.Errorf("assembleExecEnv(%q) missing bare -e %s token; got: %v", tc.agent, name, execArgs)
				}
			}
			for _, name := range tc.wantExecAbsent {
				if strings.Contains(execJoined, name) {
					t.Errorf("assembleExecEnv(%q) must never forward %s (per-agent scoping, #490); got: %s", tc.agent, name, execJoined)
				}
			}

			// The create-time path only ever forwards CONTEXT7_API_KEY,
			// unconditionally on agent (assembleEnv's own CONTEXT7_API_KEY
			// passthrough has no agent gate).
			if !slices.Contains(createArgs, "CONTEXT7_API_KEY") {
				t.Errorf("assembleEnv(%q) missing bare -e CONTEXT7_API_KEY token; got: %v", tc.agent, createArgs)
			}

			for _, args := range [][]string{execArgs, createArgs} {
				for _, name := range providerSecretEnvNames {
					for _, tok := range args {
						if strings.HasPrefix(tok, name+"=") {
							t.Errorf("found a %q= prefixed token (value-bearing form) in %v; want the bare -e %s token only", name, args, name)
						}
					}
				}
			}
		})
	}
}

// TestAssembleExecEnvAndAssembleEnv_EmptyProviderKeys_OmitEntirely pins the
// non-empty gate (AC2): an explicitly empty host var must never emit even
// the bare -e NAME token -- both assembleExecEnv and assembleEnv only
// forward a name when its host value is set and non-empty.
func TestAssembleExecEnvAndAssembleEnv_EmptyProviderKeys_OmitEntirely(t *testing.T) {
	for _, name := range providerSecretEnvNames {
		t.Setenv(name, "")
	}

	for _, agent := range []string{"claude", "codex", "opencode"} {
		execArgs := assembleExecEnv(agent)
		e := &Engine{Runtime: "docker", Stderr: &bytes.Buffer{}}
		createArgs := e.assembleEnv(agent, Scope{WorkspaceScope: "s"}, Options{}, false, DefaultSandboxPlugins())

		for _, name := range providerSecretEnvNames {
			if slices.Contains(execArgs, name) {
				t.Errorf("assembleExecEnv(%q) emitted %s with an empty host value, want entirely absent; got: %v", agent, name, execArgs)
			}
			if slices.Contains(createArgs, name) {
				t.Errorf("assembleEnv(%q) emitted %s with an empty host value, want entirely absent; got: %v", agent, name, createArgs)
			}
		}
	}
}

// -- ticket #628: reuse-posture derivation/socket-matcher unit tests --------
//
// NOTE (red phase): deriveDindPosture, mountExposesHostSocket, reusePosture,
// reuseMount, reuseDindPosture, and the dindOn/dindOff/dindUnknown constants
// do not exist yet -- they land in launch.go in the next, non-red phase
// (ticket #628's Implementation Order step 2). Every reference to them below
// is therefore a compile error until that lands; that is the intended
// red-phase state (mirroring dryrun_test.go's historical #589 red-phase
// note), not a bug to fix by stubbing the implementation in this file.
//
// The tri-state type is named reuseDindPosture (not dindPosture) because
// audit.go already defines a function named dindPosture -- a distinct name
// avoids colliding with that existing identifier while keeping the plan's
// dindOn/dindOff/dindUnknown value names exactly as specified.

// TestDeriveDindPosture pins the tri-state derivation rules: an explicit
// cenci-sand.dind label is authoritative over every other signal (even a
// conflicting one), a legacy unlabeled container derives its posture from any
// one of the three independent create-time DinD signals, and an unrecognized
// label value is ambiguous -- never collapsed into the safest-looking case
// (watch/docs/go-gotchas.md #598).
func TestDeriveDindPosture(t *testing.T) {
	cases := []struct {
		name string
		p    reusePosture
		want reuseDindPosture
	}{
		{
			name: "label on is authoritative even with no other dind signals",
			p:    reusePosture{DindLabel: "on"},
			want: dindOn,
		},
		{
			name: "label off is authoritative even with a conflicting sysbox-runc runtime signal",
			p:    reusePosture{DindLabel: "off", Runtime: "sysbox-runc"},
			want: dindOff,
		},
		{
			name: "legacy unlabeled container derives on from the sysbox-runc runtime signal alone",
			p:    reusePosture{Runtime: "sysbox-runc"},
			want: dindOn,
		},
		{
			name: "legacy unlabeled container derives on from the CENCI_SANDBOX_DIND env signal alone",
			p:    reusePosture{DindEnv: true},
			want: dindOn,
		},
		{
			name: "legacy unlabeled container derives on from a /var/lib/docker storage mount alone",
			p:    reusePosture{Mounts: []reuseMount{{Source: "claude-cenci-dind-repo", Destination: "/var/lib/docker"}}},
			want: dindOn,
		},
		{
			name: "legacy unlabeled container with none of the three signals derives off",
			p:    reusePosture{Runtime: "runc", Mounts: []reuseMount{{Source: "workspace-vol", Destination: "/workspace"}}},
			want: dindOff,
		},
		{
			name: "unrecognized label value is ambiguous, not the safest-looking case (#598)",
			p:    reusePosture{DindLabel: "maybe"},
			want: dindUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deriveDindPosture(tc.p); got != tc.want {
				t.Errorf("deriveDindPosture(%+v) = %v, want %v", tc.p, got, tc.want)
			}
		})
	}
}

// TestMountExposesHostSocket pins the host-socket matcher: it flags a mount
// whose source OR destination basename is docker.sock/podman.sock, and it
// must not over-match the cenci socket-dir mount (a directory, not a
// *.sock file) or ordinary workspace/home mounts (plan Risks: over-broad
// socket match).
func TestMountExposesHostSocket(t *testing.T) {
	cases := []struct {
		name   string
		mounts []reuseMount
		want   bool
	}{
		{
			name:   "docker.sock source basename is flagged",
			mounts: []reuseMount{{Source: "/var/run/docker.sock", Destination: "/var/run/docker.sock"}},
			want:   true,
		},
		{
			name:   "podman.sock destination basename is flagged",
			mounts: []reuseMount{{Source: "/run/user/1000/podman/podman.sock", Destination: "/run/podman/podman.sock"}},
			want:   true,
		},
		{
			name:   "the cenci socket directory mount is not flagged",
			mounts: []reuseMount{{Source: "/run/user/1000/cenci", Destination: cenciSocketMountDest}},
			want:   false,
		},
		{
			name:   "the workspace bind mount is not flagged",
			mounts: []reuseMount{{Source: "/home/user/repo", Destination: "/workspace"}},
			want:   false,
		},
		{
			name:   "the home volume mount is not flagged",
			mounts: []reuseMount{{Source: "claude-cenci-home-default", Destination: "/home/dev"}},
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mountExposesHostSocket(tc.mounts); got != tc.want {
				t.Errorf("mountExposesHostSocket(%+v) = %v, want %v", tc.mounts, got, tc.want)
			}
		})
	}
}

// TestParseReusePosture_FailsClosedOnMalformedOrEmptyOutput is a direct unit
// test of parseReusePosture's boundary shapes -- justified despite this
// project's integration-first default because the exact "genuinely empty
// string" case this Critical finding calls out cannot be produced through
// the fake-runtime shell scripts' `${FAKE_REUSE_POSTURE:-default}`
// substitution (an empty env var value falls back to the default under
// that substitution form, same as unset), so an integration test cannot
// reach it. It pins the finding's core requirement: a truncated/garbled
// `inspect` response that still exits 0 must never silently collapse to
// the zero-value reusePosture{} (which derives to the fully permissive
// "no host socket, dindOff" outcome) -- it must return an error instead,
// exactly like containerStartupState's existing "unexpected container
// state" shape check (watch/docs/error-handling.md #572,
// watch/docs/go-gotchas.md #598).
func TestParseReusePosture_FailsClosedOnMalformedOrEmptyOutput(t *testing.T) {
	cases := []struct {
		name       string
		out        string
		wantErrSub string
	}{
		{
			name:       "genuinely empty output (exit 0, zero bytes) is rejected, not defaulted to the zero-value posture",
			out:        "",
			wantErrSub: "empty",
		},
		{
			name:       "a header line missing the expected pipe-delimited fields is rejected",
			out:        "garbage-no-pipes\n",
			wantErrSub: "malformed reuse posture header",
		},
		{
			name:       "a header line with only one pipe (missing the dind-env field) is rejected",
			out:        "off|runc\n",
			wantErrSub: "malformed reuse posture header",
		},
		{
			name:       "a mount line that fails to split on \"::\" is rejected rather than silently dropped",
			out:        "off|runc|0\nno-delimiter-here\n",
			wantErrSub: "malformed reuse posture mount line",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseReusePosture(tc.out)
			if err == nil {
				t.Fatalf("parseReusePosture(%q) = nil error, want a fail-closed rejection", tc.out)
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Errorf("parseReusePosture(%q) error = %q, want it to contain %q", tc.out, err.Error(), tc.wantErrSub)
			}
		})
	}
}

// TestParseReusePosture_ValidShapeStillParses is the happy-path companion to
// the fail-closed test above: a well-formed inspect response must still
// parse into the expected reusePosture fields, so the new validation doesn't
// reject legitimate output.
func TestParseReusePosture_ValidShapeStillParses(t *testing.T) {
	p, err := parseReusePosture("on|sysbox-runc|1\nworkspace-vol::/workspace\ndind-vol::/var/lib/docker\n")
	if err != nil {
		t.Fatalf("parseReusePosture: %v", err)
	}
	if p.DindLabel != "on" || p.Runtime != "sysbox-runc" || !p.DindEnv {
		t.Errorf("parseReusePosture header fields = %+v, want DindLabel=on Runtime=sysbox-runc DindEnv=true", p)
	}
	want := []reuseMount{
		{Source: "workspace-vol", Destination: "/workspace"},
		{Source: "dind-vol", Destination: "/var/lib/docker"},
	}
	if len(p.Mounts) != len(want) || p.Mounts[0] != want[0] || p.Mounts[1] != want[1] {
		t.Errorf("parseReusePosture mounts = %+v, want %+v", p.Mounts, want)
	}
}

// TestParseReusePosture_TrailingBlankLineFromRealDockerInspect_IsTolerated
// pins ticket #684's fix: real `docker inspect --format` appends its own
// trailing newline on top of the template's own per-mount "\n", so the
// captured stdout always ends with one blank line after the last mount (or
// right after the header when there are no mounts at all). Before this fix,
// parseReusePosture treated that blank line as a malformed mount line and
// rejected every reuse of a real, already-running container. A non-trailing
// malformed line must still fail closed exactly as before.
func TestParseReusePosture_TrailingBlankLineFromRealDockerInspect_IsTolerated(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want reusePosture
	}{
		{
			name: "trailing blank line after mounts is trimmed, not rejected",
			out:  "off|runc|0\nworkspace-vol::/workspace\n\n",
			want: reusePosture{
				DindLabel: "off",
				Runtime:   "runc",
				Mounts:    []reuseMount{{Source: "workspace-vol", Destination: "/workspace"}},
			},
		},
		{
			name: "trailing blank line right after the header (no mounts) is trimmed, not rejected",
			out:  "off|runc|0\n\n",
			want: reusePosture{DindLabel: "off", Runtime: "runc"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parseReusePosture(tc.out)
			if err != nil {
				t.Fatalf("parseReusePosture(%q): %v", tc.out, err)
			}
			if p.DindLabel != tc.want.DindLabel || p.Runtime != tc.want.Runtime || p.DindEnv != tc.want.DindEnv {
				t.Errorf("parseReusePosture(%q) header fields = %+v, want %+v", tc.out, p, tc.want)
			}
			if len(p.Mounts) != len(tc.want.Mounts) {
				t.Errorf("parseReusePosture(%q) Mounts = %+v, want %+v", tc.out, p.Mounts, tc.want.Mounts)
			}
			for i := range tc.want.Mounts {
				if i < len(p.Mounts) && p.Mounts[i] != tc.want.Mounts[i] {
					t.Errorf("parseReusePosture(%q) Mounts[%d] = %+v, want %+v", tc.out, i, p.Mounts[i], tc.want.Mounts[i])
				}
			}
		})
	}

	// A non-trailing malformed line must still fail closed even though the
	// output also carries the real trailing blank line.
	_, err := parseReusePosture("off|runc|0\nno-delimiter-here\n\n")
	if err == nil {
		t.Fatal("parseReusePosture with a non-trailing malformed mount line = nil error, want a fail-closed rejection even with the trailing blank line present")
	}
	if !strings.Contains(err.Error(), "malformed reuse posture mount line") {
		t.Errorf("parseReusePosture error = %q, want it to mention the malformed mount line", err.Error())
	}
}

// dindShutdownSentinelPath is the shutdown-sentinel file the dind-only
// keepalive TERM/INT trap must write (#630's Assumptions: hardcoded under
// /home/dev, mirroring the marker's own hardcoded root-phase path — never
// $HOME, since this runs as PID 1 inside the container where dev's home is
// always /home/dev).
const dindShutdownSentinelPath = "/home/dev/.cenci-dockerd-shutdown"

// buildRunArgvTrailingCommand builds a minimal argv via buildRunArgv and
// returns its trailing PID-1 command string (the argument right after
// "-c"), failing the test if the argv doesn't end in the expected
// "<image> -c <command>" shape.
func buildRunArgvTrailingCommand(t *testing.T, dindOn bool) string {
	t.Helper()
	home := t.TempDir()
	e := &Engine{Runtime: "docker", Stderr: &bytes.Buffer{}}
	scope := Scope{
		ContainerName:     "claude-cenci-repo",
		VolumeName:        "claude-cenci-repo-home",
		DindVolumeName:    "claude-cenci-dind-repo",
		Hostname:          "cenci-repo",
		Image:             "claude-cenci-repo:latest",
		WorkspaceBindHost: t.TempDir(),
		Workdir:           "/workspace",
		WorkspaceScope:    "repo",
	}

	argv, err := e.buildRunArgv("claude", "/usr/local/bin/cenci", "/run/user/1000/cenci", true, scope, Options{Agent: "claude"}, home, dindOn, false, DefaultSandboxPlugins())
	if err != nil {
		t.Fatalf("buildRunArgv(dindOn=%v): %v", dindOn, err)
	}
	if len(argv) < 3 || argv[len(argv)-2] != "-c" {
		t.Fatalf("buildRunArgv(dindOn=%v) argv does not end in \"-c\" <command>; got: %v", dindOn, argv)
	}
	return argv[len(argv)-1]
}

// TestBuildRunArgv_NonDind_KeepsExecSleepInfinityVerbatim pins the
// non-regression half of #630's keepalive change: a non-dind launch's
// trailing PID-1 command must stay byte-identical to today's
// "touch /tmp/cenci-ready && exec sleep infinity" — `exec` replaces bash
// entirely, which is fine (and cheaper) when there's no dind-only shutdown
// signal to trap.
func TestBuildRunArgv_NonDind_KeepsExecSleepInfinityVerbatim(t *testing.T) {
	got := buildRunArgvTrailingCommand(t, false)
	want := "touch /tmp/cenci-ready && exec sleep infinity"
	if got != want {
		t.Errorf("non-dind trailing command = %q, want %q verbatim", got, want)
	}
}

// TestBuildRunArgv_Dind_KeepaliveTrapsTermAndWritesSentinel pins #630's
// dind-only keepalive: it must still touch the readiness marker, but must
// NOT `exec sleep infinity` (exec would replace bash and make it unable to
// trap/handle TERM), must install a trap on TERM and INT that touches the
// shutdown-sentinel file dind.sh's sentinel-based classification reads
// (dindShutdownSentinelPath), and must still block indefinitely via some
// `sleep infinity` backgrounded and `wait`ed on (the standard trap-then-wait
// container keepalive shape), so a normal container lifetime doesn't exit
// early absent a signal.
func TestBuildRunArgv_Dind_KeepaliveTrapsTermAndWritesSentinel(t *testing.T) {
	got := buildRunArgvTrailingCommand(t, true)

	if !strings.Contains(got, "touch /tmp/cenci-ready") {
		t.Errorf("dind trailing command missing the readiness marker touch, got: %q", got)
	}
	if strings.Contains(got, "exec sleep infinity") {
		t.Errorf("dind trailing command must not `exec sleep infinity` (exec would replace bash, so it could never trap TERM to write the shutdown sentinel), got: %q", got)
	}
	if !strings.Contains(got, "trap") {
		t.Errorf("dind trailing command missing a `trap` installation, got: %q", got)
	}
	if !strings.Contains(got, "TERM") {
		t.Errorf("dind trailing command's trap must cover TERM (docker/podman stop's signal), got: %q", got)
	}
	if !strings.Contains(got, dindShutdownSentinelPath) {
		t.Errorf("dind trailing command's trap must write the shutdown sentinel at %q, got: %q", dindShutdownSentinelPath, got)
	}
	if !strings.Contains(got, "sleep infinity") {
		t.Errorf("dind trailing command must still block indefinitely (sleep infinity) absent a signal, got: %q", got)
	}
	if !strings.Contains(got, "wait") {
		t.Errorf("dind trailing command must `wait` on the backgrounded sleep so the trap fires while the shell blocks, got: %q", got)
	}
}

// TestBuildRunArgv_Dind_DoesNotLeakIntoNonDindLaunch is a sibling-instance
// regression: dind's keepalive changes must be strictly gated on dindOn —
// asserting both trailing commands in the same test guards against a future
// edit accidentally making the dind trap unconditional.
func TestBuildRunArgv_Dind_DoesNotLeakIntoNonDindLaunch(t *testing.T) {
	nonDind := buildRunArgvTrailingCommand(t, false)
	dind := buildRunArgvTrailingCommand(t, true)
	if nonDind == dind {
		t.Fatalf("non-dind and dind trailing commands must differ; both were: %q", nonDind)
	}
	if strings.Contains(nonDind, "trap") || strings.Contains(nonDind, dindShutdownSentinelPath) {
		t.Errorf("non-dind trailing command must not carry the dind-only TERM trap/sentinel, got: %q", nonDind)
	}
}

// -- ticket #1163 Q3: warnDockerdStartupFailure's read-failed warning ------
//
// NOTE (red phase): homeVolumeReadFailed and readHomeVolumeFile's new
// (string, homeVolumeReadStatus) signature do not exist yet — see
// diagnose_test.go's #1163 red-phase note. Every reference below is a
// compile error until Phase 4 lands them.

// warnDockerdStartupFailureEngine returns an Engine wired to a fake docker
// (writeFakeRuntime) via an absolute Runtime path — mirrors this file's own
// auditEngineWithFakeRuntime (faketest_test.go), but captures Stderr into a
// buffer instead of io.Discard since these tests assert on the printed
// warning text.
func warnDockerdStartupFailureEngine(t *testing.T) (*Engine, *bytes.Buffer) {
	t.Helper()
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	var stderr bytes.Buffer
	return &Engine{Runtime: filepath.Join(fakeDir, "docker"), Stderr: &stderr}, &stderr
}

// TestWarnDockerdStartupFailure_ReadFailed_PrintsDistinctWarning pins Q3: a
// docker/podman daemon failure while reading the dockerd-startup-error
// marker (exit 125) must print a distinct, non-fatal warning naming the read
// failure — separate wording from the marker-PRESENT warning ("failed to
// start"), and it must never block the (already-succeeded) attach.
func TestWarnDockerdStartupFailure_ReadFailed_PrintsDistinctWarning(t *testing.T) {
	e, stderr := warnDockerdStartupFailureEngine(t)
	t.Setenv("FAKE_DOCKERD_MARKER_EXIT", "125")

	ctx := launchCtx{DindOn: true, Scope: Scope{VolumeName: "claude-cenci-home-test", Image: "cenci-sandbox:test"}}
	e.warnDockerdStartupFailure(ctx)

	out := stderr.String()
	if !strings.Contains(out, "read failed") {
		t.Errorf("expected a distinct read-failure warning naming the read failure, got:\n%s", out)
	}
	if strings.Contains(out, "failed to start") {
		t.Errorf("expected wording distinct from the marker-PRESENT warning (\"failed to start\"), got:\n%s", out)
	}
}

// TestWarnDockerdStartupFailure_LegitimatelyAbsent_NoWarning is the sibling
// non-regression: a legitimately absent marker (exit 1, as `cat` returns for
// a missing file) must print nothing at all, exactly like today.
func TestWarnDockerdStartupFailure_LegitimatelyAbsent_NoWarning(t *testing.T) {
	e, stderr := warnDockerdStartupFailureEngine(t)
	t.Setenv("FAKE_DOCKERD_MARKER_EXIT", "1")

	ctx := launchCtx{DindOn: true, Scope: Scope{VolumeName: "claude-cenci-home-test", Image: "cenci-sandbox:test"}}
	e.warnDockerdStartupFailure(ctx)

	if out := stderr.String(); out != "" {
		t.Errorf("expected no warning for a legitimately absent marker, got:\n%s", out)
	}
}

// TestWarnDockerdStartupFailure_ControlAndBidiBytesInMarker_NotInStderr pins
// item 9/11: a control/bidi payload in the marker-PRESENT case must never
// reach Stderr raw — the same multi-line sanitizer applied at the
// readHomeVolumeFile chokepoint must cover this call site too.
func TestWarnDockerdStartupFailure_ControlAndBidiBytesInMarker_NotInStderr(t *testing.T) {
	e, stderr := warnDockerdStartupFailureEngine(t)
	payload := "boom \x1b[2J \x7f \u202e attack"
	t.Setenv("FAKE_DOCKERD_MARKER", payload)

	ctx := launchCtx{DindOn: true, Scope: Scope{VolumeName: "claude-cenci-home-test", Image: "cenci-sandbox:test"}}
	e.warnDockerdStartupFailure(ctx)

	out := stderr.String()
	for _, bad := range []string{"\x1b", "\x7f", "\u202e"} {
		if strings.Contains(out, bad) {
			t.Errorf("expected no raw control/bidi byte %q in warnDockerdStartupFailure stderr, got:\n%q", bad, out)
		}
	}
}

// TestWarnDockerdStartupFailure_EmbeddedNewlineInMarker_RenderedSingleLine
// pins #1163's security-review Fix 2: this warning is a genuinely
// single-line stderr message, but the dockerd-startup-error marker is
// container-writable — a compromised container could otherwise embed a "\n"
// to forge a second, standalone line of terminal output that looks like a
// separate, legitimate warning. No "\n" byte may survive into the printed
// warning — content that followed the embedded newline in the marker is
// still expected to appear (the sanitizer strips the "\n", it doesn't drop
// content), just glued onto the same line with no separator.
func TestWarnDockerdStartupFailure_EmbeddedNewlineInMarker_RenderedSingleLine(t *testing.T) {
	e, stderr := warnDockerdStartupFailureEngine(t)
	t.Setenv("FAKE_DOCKERD_MARKER", "dockerd exited with status 1\nWarning: forged second line")

	ctx := launchCtx{DindOn: true, Scope: Scope{VolumeName: "claude-cenci-home-test", Image: "cenci-sandbox:test"}}
	e.warnDockerdStartupFailure(ctx)

	out := stderr.String()
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("expected the warning to render as exactly one physical line despite the marker's embedded newline, got %d lines:\n%q", len(lines), out)
	}
	if len(lines) == 1 && lines[0] == "Warning: forged second line" {
		t.Errorf("the marker's embedded \"\\n\" forged a standalone warning line; got:\n%q", out)
	}
}
