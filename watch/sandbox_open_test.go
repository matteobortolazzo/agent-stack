package main_test

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/matteobortolazzo/cenci/watch/v2/internal/exectest"
	"github.com/matteobortolazzo/cenci/watch/v2/internal/sandbox/launcher"
)

// -- fakes -------------------------------------------------------------
//
// These black-box tests exercise the real built `cenci` binary as a
// subprocess (binaryPath, built once in TestMain in main_test.go) with PATH
// overridden to a temp dir containing fake `docker`/`podman` (and, for the
// open path, `claude`) scripts. Fakes are plain POSIX `/bin/sh` scripts (not
// `#!/usr/bin/env ...`) so they resolve without depending on the overridden
// PATH containing a shell.

// writeFakeDocker writes a fake `docker` (or `podman`) to dir that appends
// each invocation's argv (space-joined) as a line to callLog, and — when
// invoked as `<name> ps ...` — answers psOutput (a tab-separated
// name/status/image row per line, ListContainers' `ps -a --format ...`
// shape): `ps -a` prints psOutput verbatim, while a plain `ps` (the
// RunningSandboxContainers(All) `--format {{.Names}}` shape `sandbox stop`
// relies on) prints only the first (name) field of each line — so a caller
// asserting on the exact stopped-container name (not just a call-log
// substring) gets a clean name, never the whole tab-joined row (#629).
func writeFakeDocker(t *testing.T, dir, name, callLog, psOutput string) {
	t.Helper()
	body := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + exectest.ShellQuote(callLog) + "\n" +
		"psout=" + exectest.ShellQuote(psOutput) + "\n" +
		`if [ "$1" = "ps" ]; then
  if [ "$2" = "-a" ]; then
    printf '%s' "$psout"
  else
    tab=$(printf '\t')
    IFS='
'
    for line in $psout; do
      fname=${line%%"$tab"*}
      [ -n "$fname" ] && printf '%s\n' "$fname"
    done
  fi
fi
exit 0
`
	exectest.WriteExecutable(t, filepath.Join(dir, name), body)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

// readCapturedArgv reads a fake binary's capture file into a []string, one
// element per line (trailing blank line from the final \n dropped). Shared
// with doctor_update_test.go's fake cenci-installer.
func readCapturedArgv(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read capture %s: %v", path, err)
	}
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func joinArgv(argv []string) string {
	return strings.Join(argv, " ")
}

// -- sandbox <batch verb> ------------------------------------------------
//
// build/build-base/prune/update-agent/update-plugins run natively against docker/podman
// via internal/sandbox/launcher, so these tests assert what the fake
// *runtime* received. Both docker and podman fakes are always written so the
// podman-first runtime detection can never escape to a real runtime on
// machines (or CI runners) that have one. reap-orphans is pinned by the
// relocated bash contract suite (tests/reap-orphans.test.sh); reseed-creds
// is an alias for `cenci open --reseed-creds` and is covered with the open
// tests below.

// writeScriptedRuntime writes a fake docker/podman to dir that appends each
// invocation's argv (space-joined) as a line to callLog and answers scripted
// responses from env vars:
//
//	FAKE_INSPECT_EXIT    — image listing exit code (default 0 = image exists)
//	FAKE_IMAGE_AGENT_LIFECYCLE — shared-agent image label (default shared-v2)
//	FAKE_IMAGE_BASE_VERSION — baked cenci.base-version image label (default
//	                        set per-test by batchEnv/openTestEnv to the
//	                        fixture's current BaseTag, so pre-existing
//	                        "image current -> skip build" tests keep passing
//	                        without every caller having to know the tag)
//	FAKE_BUILD_EXIT      — `build` exit code (default 0)
//	FAKE_IMAGES          — `images ...` stdout
//	FAKE_PS              — `ps ...` stdout
//	FAKE_PS_EXIT         — `ps ...` exit code (default 0); set nonzero to
//	                        simulate a container-listing failure (e.g.
//	                        support-bundle's ListContainers call)
//	FAKE_VOLUMES         — `volume ls` stdout
//	FAKE_INFO_RUNTIMES   — `info --format {{json .Runtimes}}` stdout (default
//	                        "{}"); the dind preflight's sysbox-registration
//	                        probe reads this, e.g.
//	                        `{"sysbox-runc":{},"runc":{}}` to simulate a
//	                        registered sysbox runtime.
//	FAKE_INFO_EXIT       — `info ...` exit code (default 0); set nonzero to
//	                        simulate a `docker info` failure (daemon down),
//	                        distinct from FAKE_INFO_RUNTIMES's stdout-shape
//	                        control — note the asymmetric naming: `fv
//	                        INFO_RUNTIMES` controls stdout, `fe INFO` (→
//	                        FAKE_INFO_EXIT, not FAKE_INFO_RUNTIMES_EXIT)
//	                        controls the exit code. Must stay byte-parallel
//	                        with internal/sandbox/launcher/faketest_test.go's
//	                        writeFakeRuntime (#493 keep-in-sync note).
//	FAKE_INSPECT_LABEL   — container `inspect` stdout for label lookups
//	FAKE_INSPECT_MOUNTS  — container `inspect` stdout for warnIfUnwired's
//	                        mount probe. One "<source>::<destination>" line
//	                        per mount — the same shape as FAKE_REUSE_POSTURE's
//	                        mount lines below — so warnIfUnwired can compare a
//	                        reused container's cenci socket-mount source
//	                        against the currently-resolved host socket dir, not
//	                        just its destination (ticket #1143). Must stay
//	                        byte-parallel with
//	                        internal/sandbox/launcher/faketest_test.go's
//	                        writeFakeRuntime FAKE_INSPECT_MOUNTS doc comment
//	                        (#493 keep-in-sync note).
//	FAKE_REUSE_POSTURE   — container `inspect` stdout for the combined
//	                        reuse-posture probe (ticket #628's
//	                        inspectReusePosture: the `cenci-sand.dind` label,
//	                        HostConfig.Runtime, and mount source/destination
//	                        pairs), told apart by the `cenci-sand.dind`
//	                        format-string token. Line 1 is
//	                        "<label>|<runtime>|<dindenv 0-or-1>"; remaining
//	                        lines are "<source>::<destination>" per mount,
//	                        followed by one trailing blank line — real
//	                        `docker inspect --format` appends its own
//	                        trailing newline on top of the template's last
//	                        mount's own "\n", and parseReusePosture trims
//	                        exactly that trailing blank line rather than
//	                        rejecting it (ticket #684); omitting it here would
//	                        let these fakes drift from the real runtime shape
//	                        again. Defaults to an empty label, non-sysbox
//	                        runtime, no dind env, and a workspace-only mount
//	                        (derives dindOff, no host socket) so existing
//	                        reuse tests that don't care about posture keep
//	                        passing; watch/docs/test-strategy.md #493
//	                        keep-in-sync note applies — this must stay
//	                        byte-parallel with
//	                        internal/sandbox/launcher/faketest_test.go's
//	                        writeFakeRuntime.
//	FAKE_INSPECT_STATE   — container startup state (default "running 0")
//	FAKE_CONTAINER_INSPECT_EXIT — container `inspect` exit code, for all three
//	                        format-string lookups (State.Status, .RW mounts,
//	                        Mounts) — default 0; set nonzero to simulate
//	                        inspect erroring (e.g. the container was already
//	                        removed by `--rm`)
//	FAKE_READY_EXIT      — readiness probe exit code (default 0)
//	FAKE_STARTUP_ERROR   — persistent agent-CLI-missing marker text, read
//	                        from /home/dev/.cenci-agent-startup-error
//	FAKE_BOOT_LOG        — entrypoint boot-log content, read from
//	                        /home/dev/.cenci-boot.log
//	FAKE_STARTUP_MARKER  — generic entrypoint-failure trap marker text, read
//	                        from /home/dev/.cenci-startup-failed
//	FAKE_AGENT_CHECK_EXIT — the `agent-cli.sh status <agent>` populated-check
//	                        probe's exit code (default 0 = the shared volume
//	                        is populated)
//	FAKE_AGENT_STATUS    — the `agent-cli.sh status <agent>` probe's stdout:
//	                        the five populated/version/pin/last_success/
//	                        last_attempt key=value lines (ticket #710's
//	                        agent-cli.sh swap). Defaults to a freshly
//	                        succeeded, unpinned status
//	                        (last_success=$(date +%s), evaluated at fake-
//	                        invocation time, not fixture-write time) so
//	                        pre-existing open tests that don't care about TTL
//	                        staleness never trigger the launch-time refresh.
//	                        Must stay byte-parallel with engine_test.go's
//	                        volumeCheckEngine/volumeCheckEngineStatus and
//	                        dual_runtime_test.go's writeAgentRuntimeStub
//	                        (watch/docs/test-strategy.md #493).
//	FAKE_AGENT_UNPIN_EXIT — 'agent-cli.sh unpin <agent>' exit code (default 0);
//	                        `update-agent --unpin`'s unpin-then-update
//	                        sequence for a given owner runs the plain
//	                        'agent-cli.sh update' only when the unpin call
//	                        itself succeeds (#709).
//	FAKE_ATTACH_EXIT     — exit code of the final `exec -it ...` attach
//	FAKE_PLUGIN_MANIFEST — `cenci diagnose`'s best-effort plugin-manifest
//	                        read, from either agent's marketplace.json
//	                        (/home/dev/.claude/plugins/marketplaces/cenci/
//	                        .claude-plugin/marketplace.json or the Codex
//	                        equivalent under /home/dev/.codex/...) via the
//	                        same short-lived `run --entrypoint /bin/cat`
//	                        home-volume read pattern as the three
//	                        startupFailureDetail vars above; unset/empty
//	                        simulates a read failure (diagnose falls back to
//	                        "unknown")
//	FAKE_DOCKERD_MARKER  — #630's persistent dockerd-startup-failure marker
//	                        text, read from /home/dev/.cenci-dockerd-startup-
//	                        error via the same short-lived `run --entrypoint
//	                        /bin/cat` home-volume read pattern; consumed by
//	                        both the launcher's before-attach warning
//	                        (warnDockerdStartupFailure) and `cenci diagnose`'s
//	                        "Nested Docker:" section. Unset/empty simulates no
//	                        recorded failure. FAKE_DOCKERD_MARKER_EXIT
//	                        (default 0) is that same read's exit code — set
//	                        it to 125/126/127 (#1163) to script
//	                        readHomeVolumeFile's read-failed classification;
//	                        any other non-zero exit (e.g. 1) scripts the
//	                        legitimately-absent classification instead.
//	FAKE_EVENTS_MARKER   — content returned by the short-lived
//	                        `run --entrypoint /bin/cat ... .cenci-events-
//	                        undelivered` home-volume read (#1122's
//	                        dropped-event marker); unset/empty simulates "no
//	                        failure recorded". FAKE_EVENTS_MARKER_EXIT
//	                        (default 0) is that same read's exit code, same
//	                        125/126/127 scripting convention as
//	                        FAKE_DOCKERD_MARKER_EXIT above (#1163).
//	FAKE_IMAGE_ID        — `image inspect --format '{{.Id}}' <image>` stdout
//	                        (ticket #947's printStaleContainerNotice: the
//	                        freshly built image's ID), told apart by the
//	                        distinctive `{{.Id}}` format-string token, checked
//	                        before the plain agent-cli/base-version label
//	                        answer above (which would otherwise answer any
//	                        format string, including this one). Defaults to
//	                        "sha256:fresh".
//	FAKE_IMAGE_ID_EXIT   — that probe's exit code (default 0); nonzero
//	                        simulates the image-ID probe itself failing.
//	FAKE_CONTAINER_IMAGES — space-separated `name=<ref>|<id>` pairs answering
//	                        the combined per-container probe `inspect
//	                        --format '{{.Config.Image}}|{{.Image}}' <name>`
//	                        (ticket #947), told apart by the distinctive
//	                        `{{.Image}}` format-string token. The fake looks
//	                        up the requested container's name and emits
//	                        `${pair#*=}` verbatim as the probe's stdout; a
//	                        container absent from the list defaults to
//	                        "cenci-sandbox:latest|<FAKE_IMAGE_ID default>".
//	                        Must stay byte-parallel with
//	                        internal/sandbox/launcher/engine_test.go's
//	                        buildEngine and faketest_test.go's
//	                        writeFakeRuntime (#493 keep-in-sync note).
//	FAKE_INSPECT_IMAGE_EXIT — the per-container combined probe's exit code
//	                        (default 0); nonzero simulates that probe
//	                        failing.
//	FAKE_CONTAINER_ENV   — (#1002) the attach-path CENCI_SANDBOX_PLUGINS
//	                        drift probe's `inspect --format
//	                        '{{range .Config.Env}}{{if eq (index (split . "=")
//	                        0) "CENCI_SANDBOX_PLUGINS"}}{{.}}{{end}}{{end}}'`
//	                        stdout — the template filters server-side, so the
//	                        real probe's stdout is either the single raw
//	                        "CENCI_SANDBOX_PLUGINS=VALUE" line or empty (never
//	                        the container's whole env — #1002 security
//	                        review), told apart by the distinctive
//	                        `.Config.Env` format-string token. Defaults to
//	                        empty (no CENCI_SANDBOX_PLUGINS line at all — the
//	                        legacy-container "unset" signal, compared against
//	                        the default pair, so pre-existing attach tests
//	                        that never set this stay drift-free). Its exit
//	                        code is the SEPARATE FAKE_CONTAINER_ENV_EXIT (not
//	                        FAKE_CONTAINER_INSPECT_EXIT, which every other
//	                        inspect probe above shares) so a test can fail
//	                        just this best-effort probe without also failing
//	                        the compatibility/reuse-posture/readiness probes
//	                        the attach path depends on — proving the drift
//	                        probe never blocks the attach. This branch is
//	                        placed AFTER the existing `*'cenci-sand.dind'*`
//	                        branch on purpose: inspectReusePosture's own
//	                        format string also contains the literal
//	                        `.Config.Env` substring, so matching it first
//	                        would silently break every reuse-posture test
//	                        (plan Risks: fake-runtime case-order shadowing).
//
// The open path drives the extra verbs: `rm` (exit 0), `run` (prints a
// container id), container `inspect` (label vs mounts told apart by the
// format string), non-interactive `exec` (readiness probe, exit 0), and the
// final `exec -it` attach — which the binary syscall.Execs, so the fake's
// exit code IS the binary's exit code. `run --entrypoint /bin/cat ... <path>`
// (startupFailureDetail's short-lived home-volume reads, and diagnose's
// plugin-manifest read) is told apart by the requested path, one of the four
// FAKE_* vars above.
//
// Every FAKE_<VERB>[_EXIT] var above also has a FAKE_<VERB>_DOCKER/
// FAKE_<VERB>_PODMAN (or FAKE_<VERB>_EXIT_DOCKER/FAKE_<VERB>_EXIT_PODMAN)
// override, resolved from the invoking binary's own name and falling back to
// the plain var — see internal/sandbox/launcher/faketest_test.go's
// writeFakeRuntime doc comment for the full rationale (ticket #629's
// dual-runtime test-fake keying: dual-runtime tests put both a docker and a
// podman fake on PATH at once, one shared process environment, so a single
// FAKE_PS could never script them to answer differently in the same test).
// This fv/fe/fset trio must stay byte-parallel with that one
// (watch/docs/test-strategy.md's #493 keep-in-sync rule).
func writeScriptedRuntime(t *testing.T, dir, name, callLog string) {
	t.Helper()
	body := `#!/bin/sh
printf '%s\n' "$*" >> ` + exectest.ShellQuote(callLog) + `
rtname=${0##*/}
rt=""
case "$rtname" in
docker) rt=DOCKER ;;
podman) rt=PODMAN ;;
esac
fv() {
  n=$1; d=$2
  if [ -n "$rt" ]; then
    v="FAKE_${n}_${rt}"
    eval "s=\${${v}+x}"
    if [ "$s" = x ]; then eval "printf '%s' \"\${${v}}\""; return; fi
  fi
  eval "s=\${FAKE_${n}+x}"
  if [ "$s" = x ]; then eval "printf '%s' \"\${FAKE_${n}}\""; else printf '%s' "$d"; fi
}
fe() {
  n=$1
  if [ -n "$rt" ]; then
    v="FAKE_${n}_EXIT_${rt}"
    eval "s=\${${v}+x}"
    if [ "$s" = x ]; then eval "printf '%s' \"\${${v}}\""; return; fi
  fi
  eval "s=\${FAKE_${n}_EXIT+x}"
  if [ "$s" = x ]; then eval "printf '%s' \"\${FAKE_${n}_EXIT}\""; else printf '%s' 0; fi
}
fset() {
  n=$1
  if [ -n "$rt" ]; then
    v="FAKE_${n}_${rt}"
    eval "s=\${${v}+x}"
    if [ "$s" = x ]; then printf x; return; fi
  fi
  eval "s=\${FAKE_${n}+x}"
  [ "$s" = x ] && printf x
}
# fvb is fv's %b-format counterpart (interprets backslash escapes, e.g. a
# literal "\n" default). fv/fvb are called directly as statements (never
# wrapped in "$(...)") wherever the resolved value is the entire printed
# output: command substitution unconditionally strips every trailing
# newline, which would silently corrupt a verbatim multi-line value like
# FAKE_LOGS (must stay byte-parallel with faketest_test.go's writeFakeRuntime
# fv/fe/fvb — #629's dual-runtime keying commit).
fvb() {
  n=$1; d=$2
  if [ -n "$rt" ]; then
    v="FAKE_${n}_${rt}"
    eval "s=\${${v}+x}"
    if [ "$s" = x ]; then eval "printf '%b' \"\${${v}}\""; return; fi
  fi
  eval "s=\${FAKE_${n}+x}"
  if [ "$s" = x ]; then eval "printf '%b' \"\${FAKE_${n}}\""; else printf '%b' "$d"; fi
}
case "$1" in
image)
  if [ "$2" = inspect ]; then
    case "$*" in
    *'{{.Id}}'*) fv IMAGE_ID "sha256:fresh"; exit "$(fe IMAGE_ID)" ;;
    esac
    printf '%s|%s\n' "$(fv IMAGE_AGENT_LIFECYCLE "shared-v2")" "$(fv IMAGE_BASE_VERSION "")"
    exit "$(fe IMAGE_INSPECT)"
  fi
  ;;
build) exit "$(fe BUILD)" ;;
images)
  if [ -n "$(fset IMAGES)" ]; then
    fv IMAGES ""
  else
    for last do :; done
    printf '%s\n' "${last}"
  fi
  exit "$(fe INSPECT)"
  ;;
ps)
  case "$*" in
  *'{{.Status}}'*)
    # ListContainers/ListAllContainers/RuntimesWithContainer's tri-field
    # "ps -a --format {{.Names}}\t{{.Status}}\t{{.Image}}" shape (#629):
    # every other ps invocation in this codebase (RunningSandboxContainers,
    # containerRunning, prune's name-only "ps -a --format {{.Names}}") wants
    # bare names, so FAKE_PS is conventionally scripted as one plain name
    # per line; synthesize a status/image suffix per name here rather than
    # requiring every existing FAKE_PS value to carry tabs, while still
    # passing an already-tab-shaped FAKE_PS line through verbatim.
    tab=$(printf '\t')
    IFS='
'
    for n in $(fv PS ""); do
      [ -n "$n" ] || continue
      case "$n" in
      *"$tab"*) printf '%s\n' "$n" ;;
      *) printf '%s\tUp 1 hour\tcenci-sandbox:latest\n' "$n" ;;
      esac
    done
    ;;
  *) fv PS "" ;;
  esac
  exit "$(fe PS)"
  ;;
volume)
  case "$2" in
  ls) fv VOLUMES ""; exit "$(fe VOLUME_LS)" ;;
  esac
  ;;
info) fv INFO_RUNTIMES "{}"; exit "$(fe INFO)" ;;
rm) exit 0 ;;
run) case "$*" in
  *'/bin/cat'*)
    case "$*" in
    *'.cenci-agent-startup-error'*) fv STARTUP_ERROR ""; exit "$(fe STARTUP_ERROR)" ;;
    *'.cenci-boot.log'*) fv BOOT_LOG ""; exit "$(fe BOOT_LOG)" ;;
    *'.cenci-startup-failed'*) fv STARTUP_MARKER ""; exit "$(fe STARTUP_MARKER)" ;;
    *'.cenci-dockerd-startup-error'*) fv DOCKERD_MARKER ""; exit "$(fe DOCKERD_MARKER)" ;;
    *'.cenci-events-undelivered'*) fv EVENTS_MARKER ""; exit "$(fe EVENTS_MARKER)" ;;
    *'marketplace.json'*) fv PLUGIN_MANIFEST ""; exit "$(fe PLUGIN_MANIFEST)" ;;
    esac
    ;;
  *'agent-cli.sh unpin'*) exit "$(fe AGENT_UNPIN)" ;;
  *'agent-cli.sh update'*) rs="$(fv RUN_STDERR "")"; [ -z "$rs" ] || printf '%s\n' "$rs" >&2; exit "$(fe RUN)" ;;
  *'agent-cli.sh status'*)
    fvb AGENT_STATUS "populated=yes\nversion=1.2.3\npin=\nlast_success=$(date +%s)\nlast_attempt=\n"
    exit "$(fe AGENT_CHECK)"
    ;;
  esac
  printf '%s\n' fake-container-id ;;
inspect)
  case "$*" in
  *'{{.Image}}'*)
    name="$4"
    result=""
    found=0
    for pair in $FAKE_CONTAINER_IMAGES; do
      case "$pair" in
      "$name="*)
        result="${pair#*=}"
        found=1
        ;;
      esac
    done
    if [ "$found" = 0 ]; then
      result="cenci-sandbox:latest|$(fv IMAGE_ID "sha256:fresh")"
    fi
    printf '%s\n' "$result"
    exit "$(fe INSPECT_IMAGE)"
    ;;
  *'cenci-sand.dind'*) fvb REUSE_POSTURE "|runc|0\nworkspace-vol::/workspace\n\n"; exit "$(fe CONTAINER_INSPECT)" ;;
  *'.Config.Env'*) fvb CONTAINER_ENV ""; exit "$(fe CONTAINER_ENV)" ;;
  *State.Status*) printf '%s\n' "$(fv INSPECT_STATE "running 0")"; exit "$(fe CONTAINER_INSPECT)" ;;
  *Labels*) printf '%s\n' "$(fv INSPECT_LABEL "")" ;;
  *'.RW'*) fvb AGENT_MOUNTS "cenci-agent-cli-claude|/opt/cenci-agent|false\n"; exit "$(fe CONTAINER_INSPECT)" ;;
  *Mounts*) fvb INSPECT_MOUNTS ""; exit "$(fe CONTAINER_INSPECT)" ;;
  esac
  ;;
logs) fv LOGS "" ;;
exec) if [ "$2" = "-it" ]; then exit "$(fe ATTACH)"; fi; case "$*" in *'/tmp/cenci-ready'*) exit "$(fe READY)" ;; esac; exit 0 ;;
esac
exit 0
`
	exectest.WriteExecutable(t, filepath.Join(dir, name), body)
}

// writeScriptedRuntimes writes the same scripted fake as both docker and
// podman and returns the shared call log path.
func writeScriptedRuntimes(t *testing.T, dir string) string {
	t.Helper()
	callLog := filepath.Join(dir, "calls.txt")
	writeScriptedRuntime(t, dir, "docker", callLog)
	writeScriptedRuntime(t, dir, "podman", callLog)
	return callLog
}

// writeScriptedRuntimePair writes both a docker and a podman fake to dir,
// each with its OWN call log — unlike writeScriptedRuntimes' single shared
// log (fine when only one of the two ever actually runs, the pre-#629
// single-preferred-runtime world), a dual-runtime test needs to tell which
// binary actually ran a given argv apart, since the sandbox commands under
// test now genuinely invoke both docker and podman in the same run (#629).
func writeScriptedRuntimePair(t *testing.T, dir string) (dockerLog, podmanLog string) {
	t.Helper()
	dockerLog = filepath.Join(dir, "docker-calls.txt")
	podmanLog = filepath.Join(dir, "podman-calls.txt")
	writeScriptedRuntime(t, dir, "docker", dockerLog)
	writeScriptedRuntime(t, dir, "podman", podmanLog)
	return dockerLog, podmanLog
}

// writeDockerOnlyRuntime writes only a fake docker (no podman) to dir. Dind
// mode requires Docker as the outer runtime, and podman-first detection
// would otherwise win were both fakes present, so dind happy-path tests use
// this instead of writeScriptedRuntimes.
func writeDockerOnlyRuntime(t *testing.T, dir string) string {
	t.Helper()
	callLog := filepath.Join(dir, "calls.txt")
	writeScriptedRuntime(t, dir, "docker", callLog)
	return callLog
}

// writePodmanOnlyRuntime writes only a fake podman (no docker) to dir, for
// dind preflight tests that need a podman-only host (Docker absent from
// PATH).
func writePodmanOnlyRuntime(t *testing.T, dir string) string {
	t.Helper()
	callLog := filepath.Join(dir, "calls.txt")
	writeScriptedRuntime(t, dir, "podman", callLog)
	return callLog
}

// writeAssetFixture creates a minimal sandbox asset dir (Dockerfile.base,
// entrypoint.sh, lib/) for CENCI_SANDBOX_ASSETS.
func writeAssetFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile.base"), []byte("FROM scratch\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile.base: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM cenci-sandbox-base\n"), 0o644); err != nil {
		t.Fatalf("write Dockerfile: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "entrypoint.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatalf("write entrypoint.sh: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o755); err != nil {
		t.Fatalf("mkdir lib: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "seed.sh"), []byte("# lib\n"), 0o644); err != nil {
		t.Fatalf("write lib/seed.sh: %v", err)
	}
	return dir
}

// batchEnv is the black-box environment for native batch-verb runs: fake
// runtimes first on PATH (with git/sh still resolvable from the system
// dirs), an isolated HOME, and the asset fixture pinned via
// CENCI_SANDBOX_ASSETS. os/exec keeps the LAST duplicate env key, so these
// appends override the inherited values. FAKE_IMAGE_BASE_VERSION defaults to
// the fixture's own current BaseTag so pre-existing "image current -> skip
// build" tests keep passing now that the fake's `image inspect` branch
// carries a base-version field too; tests that want to exercise base-drift
// staleness override it with their own append.
func batchEnv(t *testing.T, fakeDir, assets string) []string {
	t.Helper()
	tag, err := launcher.BaseTag(assets)
	if err != nil {
		t.Fatalf("BaseTag: %v", err)
	}
	return append(os.Environ(),
		"PATH="+fakeDir+":/usr/bin:/bin",
		"HOME="+t.TempDir(),
		"CENCI_SANDBOX_ASSETS="+assets,
		"FAKE_VOLUMES=cenci-agent-cli-claude\\ncenci-agent-cli-codex\\n",
		"FAKE_IMAGE_BASE_VERSION="+tag,
	)
}

// callLogLines reads the shared runtime call log (missing file = no calls).
func callLogLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read call log: %v", err)
	}
	s := strings.TrimSuffix(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func findLineWithPrefix(lines []string, prefix string) (string, bool) {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return l, true
		}
	}
	return "", false
}

func anyLineContains(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestSandboxBuildBase_BuildsBaseImageNatively(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	tag, err := launcher.BaseTag(assets)
	if err != nil {
		t.Fatalf("BaseTag: %v", err)
	}

	cmd := exec.Command(binaryPath, "sandbox", "build-base")
	cmd.Env = batchEnv(t, fakeDir, assets)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox build-base: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	want := "build -f " + assets + "/Dockerfile.base -t cenci-sandbox-base:" + tag + " -t cenci-sandbox-base:latest " + assets
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("runtime calls = %v, want exactly [%s]", lines, want)
	}
	if !strings.Contains(string(output), "Building cenci-sandbox-base:"+tag) {
		t.Errorf("expected build progress message, got:\n%s", output)
	}
}

func TestSandboxBuild_MonolithBuildsBaseFirstWhenMissing(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	tag, err := launcher.BaseTag(assets)
	if err != nil {
		t.Fatalf("BaseTag: %v", err)
	}

	cmd := exec.Command(binaryPath, "sandbox", "build")
	cmd.Env = append(batchEnv(t, fakeDir, assets), "FAKE_IMAGES=")
	cmd.Dir = t.TempDir() // non-git cwd → monolith image
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox build: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	if _, ok := findLineWithPrefix(lines, "images --format {{.Repository}}:{{.Tag}} cenci-sandbox-base:"+tag); !ok {
		t.Errorf("expected a typed base-image listing, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if !anyLineContains(lines, "-t cenci-sandbox-base:"+tag) {
		t.Errorf("expected the missing base image to be built first, got calls:\n%s", strings.Join(lines, "\n"))
	}
	want := "build --build-arg BASE_VERSION=" + tag + " --label cenci.agent-cli=shared-v2 --label cenci.base-version=" + tag + " -t cenci-sandbox:latest -f " + assets + "/Dockerfile " + assets
	if !anyLineContains(lines, want) {
		t.Errorf("expected monolith build call %q, got calls:\n%s", want, strings.Join(lines, "\n"))
	}
	if anyLineContains(lines, "INSTALL_CLAUDE") || anyLineContains(lines, "INSTALL_CODEX") || anyLineContains(lines, "AGENTS_REFRESH") {
		t.Errorf("build still passed removed agent build arguments; calls:\n%s", strings.Join(lines, "\n"))
	}
}

func TestSandboxBuild_RepoImageFromRepoDockerfile(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	tag, err := launcher.BaseTag(assets)
	if err != nil {
		t.Fatalf("BaseTag: %v", err)
	}

	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".cenci"), 0o755); err != nil {
		t.Fatalf("mkdir .cenci: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".cenci", "Dockerfile"), []byte("FROM cenci-sandbox-base\n"), 0o644); err != nil {
		t.Fatalf("write repo Dockerfile: %v", err)
	}
	slug := launcher.Slugify(filepath.Base(repo))
	repoRoot, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatalf("resolve repo path: %v", err)
	}

	cmd := exec.Command(binaryPath, "sandbox", "build")
	cmd.Env = append(batchEnv(t, fakeDir, assets), "FAKE_IMAGES=")
	cmd.Dir = repo
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox build (repo): %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	want := "build --build-arg BASE_VERSION=" + tag + " --label cenci.agent-cli=shared-v2 --label cenci.base-version=" + tag + " -t cenci-sandbox-" + slug + ":latest -f " + repoRoot + "/.cenci/Dockerfile " + repoRoot + "/.cenci"
	if !anyLineContains(lines, want) {
		t.Errorf("expected repo-image build call %q, got calls:\n%s", want, strings.Join(lines, "\n"))
	}
	if anyLineContains(lines, "INSTALL_CLAUDE") || anyLineContains(lines, "INSTALL_CODEX") || anyLineContains(lines, "AGENTS_REFRESH") {
		t.Errorf("repo build still passed removed agent build arguments; calls:\n%s", strings.Join(lines, "\n"))
	}
}

func TestSandboxBuild_AgentsFlagIsGone(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "build", "--agents", "claude")
	cmd.Env = batchEnv(t, fakeDir, assets)
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("expected removed --agents flag to exit 2, got %T %v\n%s", err, err, output)
	}
	if lines := callLogLines(t, callLog); len(lines) > 0 {
		t.Errorf("expected no runtime calls for removed --agents flag, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestSandboxUpdateAgent_RebuildsLegacyImageBeforeMigration(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "update-agent")
	cmd.Env = append(batchEnv(t, fakeDir, assets), "FAKE_IMAGE_AGENT_LIFECYCLE=legacy")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox update-agent legacy image migration: %v\n%s", err, output)
	}
	lines := callLogLines(t, callLog)
	if !anyLineContains(lines, "--label cenci.agent-cli=shared-v2 --label cenci.base-version=") {
		t.Errorf("expected legacy image to rebuild with both the shared-agent and base-version labels; calls:\n%s", strings.Join(lines, "\n"))
	}
}

func TestSandboxBuild_BuildFailureExits1(t *testing.T) {
	fakeDir := t.TempDir()
	writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "build")
	cmd.Env = append(batchEnv(t, fakeDir, assets), "FAKE_IMAGES=", "FAKE_BUILD_EXIT=3")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v\n%s", err, err, output)
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("exit code = %d, want 1\n%s", exitErr.ExitCode(), output)
	}
	if !strings.Contains(string(output), "cenci sandbox build:") {
		t.Errorf("expected a 'cenci sandbox build:' error, got:\n%s", output)
	}
}

func TestSandboxPrune_RemovesSupersededTagsAndStoppedContainers(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	tag, err := launcher.BaseTag(assets)
	if err != nil {
		t.Fatalf("BaseTag: %v", err)
	}

	cmd := exec.Command(binaryPath, "sandbox", "prune")
	cmd.Env = append(batchEnv(t, fakeDir, assets),
		"FAKE_IMAGES=cenci-sandbox-base:oldtag\ncenci-sandbox-base:latest\ncenci-sandbox-base:"+tag+"\n",
		"FAKE_PS=claude-cenci-old\nunrelated-container\n",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox prune: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	if !anyLineContains(lines, "rmi cenci-sandbox-base:oldtag") {
		t.Errorf("expected the superseded tag to be removed, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if anyLineContains(lines, "rmi cenci-sandbox-base:latest") || anyLineContains(lines, "rmi cenci-sandbox-base:"+tag) {
		t.Errorf("expected the current and :latest base tags to be kept, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if _, ok := findLineWithPrefix(lines, "rm claude-cenci-old"); !ok {
		t.Errorf("expected the stopped sandbox container to be removed, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if anyLineContains(lines, "rm unrelated-container") {
		t.Errorf("expected non-sandbox containers to be left alone, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if _, ok := findLineWithPrefix(lines, "image prune -f"); !ok {
		t.Errorf("expected dangling images to be pruned, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if anyLineContains(lines, "volume") {
		t.Errorf("expected no volume operations without --volumes, got calls:\n%s", strings.Join(lines, "\n"))
	}
}

func TestSandboxPruneVolumes_DefaultDeniesRemoval(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "prune", "--volumes")
	cmd.Env = append(batchEnv(t, fakeDir, assets),
		"FAKE_VOLUMES=claude-cenci-home-x\ncodex-cenci-home-y\n",
	)
	cmd.Dir = t.TempDir()
	cmd.Stdin = strings.NewReader("n\n")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox prune --volumes: %v\n%s", err, output)
	}

	if !strings.Contains(string(output), "Skipping volume removal.") {
		t.Errorf("expected the default-deny skip message, got:\n%s", output)
	}
	lines := callLogLines(t, callLog)
	if _, ok := findLineWithPrefix(lines, "volume ls"); !ok {
		t.Errorf("expected volumes to be listed, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if anyLineContains(lines, "volume rm") {
		t.Errorf("expected no volume removal on 'n', got calls:\n%s", strings.Join(lines, "\n"))
	}
}

func TestSandboxUpdateAgent_UsesIsolatedGlobalVolumeAndExactVersion(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "update-agent", "--agent", "codex", "--version", "1.2.3")
	cmd.Env = batchEnv(t, fakeDir, assets)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox update-agent: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	prefix := "run --rm --user root --cap-drop=ALL --security-opt=no-new-privileges --entrypoint /bin/bash -v cenci-agent-cli-codex:/opt/cenci-agent cenci-sandbox:latest /usr/local/bin/lib/agent-cli.sh update codex 1.2.3"
	line, ok := findLineWithPrefix(lines, prefix)
	if !ok {
		t.Fatalf("expected isolated global update [%s], got calls:\n%s", prefix, strings.Join(lines, "\n"))
	}
	for _, forbidden := range []string{"/home/dev", "/workspace", "HOST_UID", "HOST_GID", "OPENAI_API_KEY", "docker.sock", "/tmp/host-"} {
		if strings.Contains(line, forbidden) {
			t.Errorf("isolated updater argv contains %q: %s", forbidden, line)
		}
	}
	if !strings.Contains(string(output), "host-global") && !strings.Contains(string(output), "used by every sandbox on this host") {
		t.Errorf("expected a note that --version pins/downgrades host-globally, got:\n%s", output)
	}
}

// TestSandboxUpdateAgent_UsesMonolithImageEvenInsideRepoWithOwnImage pins the
// finding-1 security fix: the current directory's own repo image
// (<repo-root>/.cenci/Dockerfile) is a caller-controlled build input, and the
// updater runs as root with the host-global agent CLI volume mounted
// read-write. If the updater ever honored that repo image, a malicious repo
// image could gain root write access to a volume every sandbox on the host
// mounts read-only. The updater must always target the trusted, checked-in
// monolith image, never the repo's own image, even when running from inside
// such a repo.
func TestSandboxUpdateAgent_UsesMonolithImageEvenInsideRepoWithOwnImage(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".cenci"), 0o755); err != nil {
		t.Fatalf("mkdir .cenci: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".cenci", "Dockerfile"), []byte("FROM cenci-sandbox-base\n"), 0o644); err != nil {
		t.Fatalf("write repo Dockerfile: %v", err)
	}
	slug := launcher.Slugify(filepath.Base(repo))
	repoImage := "cenci-sandbox-" + slug + ":latest"

	cmd := exec.Command(binaryPath, "sandbox", "update-agent")
	cmd.Env = batchEnv(t, fakeDir, assets)
	cmd.Dir = repo // a repo that opts into its own image via .cenci/Dockerfile
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox update-agent (repo scope): %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	updateLine, ok := findLineWithPrefix(lines, "run --rm --user root --cap-drop=ALL --security-opt=no-new-privileges --entrypoint /bin/bash")
	if !ok {
		t.Fatalf("expected the isolated updater run, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(updateLine, "cenci-sandbox:latest") {
		t.Errorf("expected the updater to target the monolith image, got: %s", updateLine)
	}
	if strings.Contains(updateLine, repoImage) {
		t.Errorf("updater must never target the repo's own image (%s); got: %s", repoImage, updateLine)
	}
	if anyLineContains(lines, "-f "+repo+"/.cenci/Dockerfile") {
		t.Errorf("update-agent must never build the repo's own image; calls:\n%s", strings.Join(lines, "\n"))
	}
}

func TestSandboxUpdateAgent_IsGlobalEvenWhenRepositoryContainersAreRunning(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "update-agent")
	cmd.Env = append(batchEnv(t, fakeDir, assets), "FAKE_PS=claude-cenci-one\nclaude-cenci-two\n")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox update-agent running container: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	if !anyLineContains(lines, "-v cenci-agent-cli-claude:/opt/cenci-agent") || anyLineContains(lines, "exec -u dev") {
		t.Errorf("update should use only the global volume, never a running workload; calls:\n%s", strings.Join(lines, "\n"))
	}
}

func TestSandboxUpdateAgent_RemovedNameFlagIsUsageError(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	cmd := exec.Command(binaryPath, "sandbox", "update-agent", "--name", "old")
	cmd.Env = batchEnv(t, fakeDir, assets)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("expected removed flag usage error, got %T %v\n%s", err, err, output)
	}
	if lines := callLogLines(t, callLog); len(lines) != 0 {
		t.Errorf("removed --name must make no runtime calls: %v", lines)
	}
}

func TestSandboxUpdateAgent_BuildsMissingImageBeforeMaintenance(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "update-agent")
	cmd.Env = append(batchEnv(t, fakeDir, assets), "FAKE_IMAGES=")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox update-agent image creation: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	if !anyLineContains(lines, "-t cenci-sandbox:latest") || !anyLineContains(lines, "run --rm --user root") {
		t.Errorf("expected missing image to build before maintenance run; calls:\n%s", strings.Join(lines, "\n"))
	}
}

func TestSandboxUpdateAgent_UsageErrorsMakeNoRuntimeCalls(t *testing.T) {
	tests := [][]string{
		{"sandbox", "update-agent", "--agent", "gemini"},
		{"sandbox", "update-agent", "--version", "latest"},
		{"sandbox", "update-agent", "--version", "^1.2.3"},
		{"sandbox", "update-agent", "--name", "old"},
		{"sandbox", "update-agent", "--bogus"},
		{"sandbox", "update-agent", "extra"},
		// #709: --all sweeps every existing agent-CLI volume across every
		// runtime and cannot be combined with an explicitly-set --agent,
		// --version, or --unpin (mirrors update-plugins --all's guard).
		{"sandbox", "update-agent", "--all", "--agent", "codex"},
		{"sandbox", "update-agent", "--all", "--version", "1.2.3"},
		{"sandbox", "update-agent", "--all", "--unpin"},
		// #709: --unpin clears the pin and updates to latest, so a
		// simultaneous --version (which would instead re-pin to that
		// version) is a conflicting-input usage error.
		{"sandbox", "update-agent", "--unpin", "--version", "1.2.3"},
	}
	for _, args := range tests {
		t.Run(strings.Join(args[2:], "_"), func(t *testing.T) {
			fakeDir := t.TempDir()
			callLog := writeScriptedRuntimes(t, fakeDir)
			assets := writeAssetFixture(t)
			cmd := exec.Command(binaryPath, args...)
			cmd.Env = batchEnv(t, fakeDir, assets)
			output, err := cmd.CombinedOutput()
			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 2 {
				t.Fatalf("expected usage error exit 2, got %T %v\n%s", err, err, output)
			}
			if lines := callLogLines(t, callLog); len(lines) > 0 {
				t.Errorf("expected no runtime calls, got:\n%s", strings.Join(lines, "\n"))
			}
		})
	}
}

// TestSandboxUpdateAgent_PinnedRefusal_Exits2WithMessage pins #709's pin-gate
// propagation: a bare `update-agent` against a pinned shared volume must
// surface the isolated updater's exit-2 refusal as the CLI's own exit code
// (not the generic exit-1 "some error occurred" every other updater failure
// gets), with the refusal message naming both escape hatches.
func TestSandboxUpdateAgent_PinnedRefusal_Exits2WithMessage(t *testing.T) {
	fakeDir := t.TempDir()
	writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "update-agent")
	cmd.Env = append(batchEnv(t, fakeDir, assets), "FAKE_RUN_EXIT=2")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("expected the pin refusal to propagate as exit 2, got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), "--unpin") || !strings.Contains(string(output), "--version") {
		t.Errorf("expected the refusal message to name both --unpin and --version, got:\n%s", output)
	}
}

// TestSandboxUpdateAgent_OrdinaryUpdaterFailure_Exits1 is the sibling
// baseline to the pin-refusal case: a non-pin-related updater failure (exit
// 1 from agent-cli.sh, e.g. a network error) must still exit 1, proving the
// new refusal-aware exit classification doesn't broaden exit-2 to every
// updater failure.
func TestSandboxUpdateAgent_OrdinaryUpdaterFailure_Exits1(t *testing.T) {
	fakeDir := t.TempDir()
	writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "update-agent")
	cmd.Env = append(batchEnv(t, fakeDir, assets), "FAKE_RUN_EXIT=1")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected an ordinary updater failure to exit 1, got %T %v\n%s", err, err, output)
	}
}

// TestSandboxUpdateAgent_Unpin_RunsUnpinBeforeUpdate pins #709's --unpin
// sequence: `agent-cli.sh unpin <agent>` runs (hardened the same way as the
// updater, but network-isolated and with no version argument) strictly
// before the plain `agent-cli.sh update <agent>` (no version, no
// --skip-if-pinned — the pin is already cleared).
func TestSandboxUpdateAgent_Unpin_RunsUnpinBeforeUpdate(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "update-agent", "--agent", "claude", "--unpin")
	cmd.Env = batchEnv(t, fakeDir, assets)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox update-agent --unpin: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	unpinLine := "run --rm --user root --cap-drop=ALL --security-opt=no-new-privileges --network none --entrypoint /bin/bash -v cenci-agent-cli-claude:/opt/cenci-agent cenci-sandbox:latest /usr/local/bin/lib/agent-cli.sh unpin claude"
	updateLine := "run --rm --user root --cap-drop=ALL --security-opt=no-new-privileges --entrypoint /bin/bash -v cenci-agent-cli-claude:/opt/cenci-agent cenci-sandbox:latest /usr/local/bin/lib/agent-cli.sh update claude"
	unpinIdx, updateIdx := -1, -1
	for i, l := range lines {
		if l == unpinLine {
			unpinIdx = i
		}
		if l == updateLine {
			updateIdx = i
		}
	}
	if unpinIdx < 0 {
		t.Fatalf("expected the exact hardened unpin call [%s], got calls:\n%s", unpinLine, strings.Join(lines, "\n"))
	}
	if updateIdx < 0 {
		t.Fatalf("expected the exact no-version, no-flag update call [%s], got calls:\n%s", updateLine, strings.Join(lines, "\n"))
	}
	if unpinIdx >= updateIdx {
		t.Errorf("expected unpin to run before update, got calls:\n%s", strings.Join(lines, "\n"))
	}
}

// TestSandboxUpdateAgent_UnpinFailure_SkipsUpdateExits1 pins #709: when the
// unpin call itself fails for an owner, that owner's update must never run
// (an un-cleared pin would make the following plain `update` re-refuse), and
// the overall command exits 1.
func TestSandboxUpdateAgent_UnpinFailure_SkipsUpdateExits1(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "update-agent", "--agent", "claude", "--unpin")
	cmd.Env = append(batchEnv(t, fakeDir, assets), "FAKE_AGENT_UNPIN_EXIT=1")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected an unpin failure to exit 1, got %T %v\n%s", err, err, output)
	}
	lines := callLogLines(t, callLog)
	if !anyLineContains(lines, "agent-cli.sh unpin claude") {
		t.Errorf("expected the unpin call to have been attempted, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if anyLineContains(lines, "agent-cli.sh update claude") {
		t.Errorf("expected no update call after a failed unpin, got calls:\n%s", strings.Join(lines, "\n"))
	}
}

// TestSandboxUpdateAgent_All_NeverBootstraps_OnlyRefreshesExistingVolume pins
// #709: --all refreshes only agent-CLI volumes that already exist, passing
// --skip-if-pinned, and never bootstraps a volume for an agent that has none
// anywhere. writeScriptedRuntimes puts both a docker and a podman fake on
// PATH sharing one unscoped FAKE_VOLUMES, so both fakes genuinely report
// owning the claude volume here — this pins that --all refreshes every
// runtime that owns a volume (not just one), while codex/opencode (which
// neither fake reports owning) are still never bootstrapped.
func TestSandboxUpdateAgent_All_NeverBootstraps_OnlyRefreshesExistingVolume(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "update-agent", "--all")
	cmd.Env = append(batchEnv(t, fakeDir, assets), "FAKE_VOLUMES=cenci-agent-cli-claude\n")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox update-agent --all: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	count := 0
	for _, l := range lines {
		if strings.Contains(l, "agent-cli.sh update") {
			count++
			if !strings.HasSuffix(l, "agent-cli.sh update claude --skip-if-pinned") {
				t.Errorf("expected every updater call to target claude with --skip-if-pinned, got: %s", l)
			}
		}
	}
	if count != 2 {
		t.Fatalf("expected exactly two updater calls (claude in both docker and podman, which both own the volume; codex/opencode never bootstrapped), got %d; calls:\n%s", count, strings.Join(lines, "\n"))
	}
}

// TestSandboxUpdateAgent_All_NoVolumesAnywhere_ZeroUpdaterRunsExitZero pins
// #709: when no agent-CLI volume exists anywhere, --all is a no-op success
// (never bootstraps anything).
func TestSandboxUpdateAgent_All_NoVolumesAnywhere_ZeroUpdaterRunsExitZero(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "update-agent", "--all")
	cmd.Env = append(batchEnv(t, fakeDir, assets), "FAKE_VOLUMES=")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox update-agent --all (no volumes anywhere): %v\n%s", err, output)
	}
	if lines := callLogLines(t, callLog); anyLineContains(lines, "agent-cli.sh update") {
		t.Errorf("expected zero updater runs when no volume exists anywhere, got calls:\n%s", strings.Join(lines, "\n"))
	}
}

func TestSandboxUpdatePluginsCodex_RunsOneShotVolumeUpdate(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "update-plugins", "--agent", "codex")
	cmd.Env = batchEnv(t, fakeDir, assets) // image exists, no running container
	cmd.Dir = t.TempDir()                  // non-git cwd → legacy "default" scope
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox update-plugins --agent codex: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	prefix := "run --rm --user root -e HOST_UID=" + itoa(os.Getuid()) +
		" -e HOST_GID=" + itoa(os.Getgid()) +
		" -e CENCI_SANDBOX_AGENT=codex -e CENCI_SANDBOX_PLUGINS=cenci cenci-watch -e CENCI_AGENT_CLI=/opt/cenci-agent/current/node_modules/.bin/codex" +
		" -v codex-cenci-home-default:/home/dev -v cenci-agent-cli-codex:/opt/cenci-agent:ro cenci-sandbox:latest -c "
	line, ok := findLineWithPrefix(lines, prefix)
	if !ok {
		t.Fatalf("expected a one-shot volume update run [%s...], got calls:\n%s", prefix, strings.Join(lines, "\n"))
	}
	if !strings.Contains(line, "provision_codex_plugins") {
		t.Errorf("expected the codex provisioning command, got: %s", line)
	}
}

func TestSandboxUpdatePlugins_BadAgent_Exits2NoRuntimeCalls(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "update-plugins", "--agent", "bogus")
	cmd.Env = batchEnv(t, fakeDir, assets)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v\n%s", err, err, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2\n%s", exitErr.ExitCode(), output)
	}
	if lines := callLogLines(t, callLog); len(lines) > 0 {
		t.Errorf("expected no runtime calls on a bad --agent, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestSandboxUpdatePluginsAll_RefreshesEveryRunningContainer(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "update-plugins", "--all")
	cmd.Env = append(batchEnv(t, fakeDir, assets), "FAKE_PS=claude-cenci-one\nclaude-cenci-two\ncodex-cenci-three\n")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox update-plugins --all: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	for _, name := range []string{"claude-cenci-one", "claude-cenci-two", "codex-cenci-three"} {
		if !anyLineContains(lines, "exec -u dev "+name+" /bin/bash -c") {
			t.Errorf("expected a plugin refresh exec for %q, got calls:\n%s", name, strings.Join(lines, "\n"))
		}
	}
}

func TestSandboxUpdatePluginsAll_WithExplicitName_UsageError(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "update-plugins", "--all", "--name", "x")
	cmd.Env = batchEnv(t, fakeDir, assets)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("expected --all --name usage error exit 2, got %T %v\n%s", err, err, output)
	}
	if lines := callLogLines(t, callLog); len(lines) > 0 {
		t.Errorf("expected no runtime calls for --all --name usage error, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestSandboxUpdatePluginsAll_WithExplicitAgent_UsageError(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "update-plugins", "--all", "--agent", "codex")
	cmd.Env = batchEnv(t, fakeDir, assets)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("expected --all --agent usage error exit 2, got %T %v\n%s", err, err, output)
	}
	if lines := callLogLines(t, callLog); len(lines) > 0 {
		t.Errorf("expected no runtime calls for --all --agent usage error, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestSandboxReseedCreds_IsOpenReseedAlias(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "sandbox", "reseed-creds")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandbox reseed-creds: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	runLine, ok := findLineWithPrefix(lines, "run --name ")
	if !ok {
		t.Fatalf("expected a container run, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(runLine, "-e CENCI_SANDBOX_RESEED_CREDS=1") {
		t.Errorf("expected the reseed env flag in the run argv, got: %s", runLine)
	}
}

// TestSandboxReseedCreds_DindConfigRepo_BothRuntimesPresent_UsesDocker
// reproduces #585: `cenci sandbox reseed-creds` reaches Launch (via the
// open --reseed-creds alias path) just like `cenci open` does, so it must
// build its engine with NewForLaunch (leaving Runtime unresolved) rather than
// the eager, podman-first newEngine() every other batch verb uses. With both
// a docker and a podman fake registered (podman-first would normally win)
// and the repo's .cenci/config.json turning dind on, the eager podman-first
// resolution would lock the runtime to podman before Launch ever computes
// dind, and dindPreflight would then hard-fail with "requires Docker ...
// got \"podman\"" even though Docker is present. The fixed path must
// instead re-resolve docker-preferred once dind is known, succeeding and
// selecting the sysbox-runc runtime.
func TestSandboxReseedCreds_DindConfigRepo_BothRuntimesPresent_UsesDocker(t *testing.T) {
	repoRoot, slug := dindRepoEnv(t, true)

	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)
	env = append(env, `FAKE_INFO_RUNTIMES={"sysbox-runc":{},"runc":{}}`)

	cmd := exec.Command(binaryPath, "sandbox", "reseed-creds")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox reseed-creds (dind repo, both runtimes present): %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	createLine, ok := findLineWithPrefix(lines, "run --name ")
	if !ok {
		t.Fatalf("expected a container run, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(createLine, "--runtime=sysbox-runc") {
		t.Errorf("expected dind mode to resolve docker-preferred (--runtime=sysbox-runc), got: %s", createLine)
	}
	wantVolume := "claude-cenci-dind-" + slug + ":/var/lib/docker"
	if !strings.Contains(createLine, wantVolume) {
		t.Errorf("expected the dind storage volume mount %q, got: %s", wantVolume, createLine)
	}
	if !strings.Contains(createLine, "-e CENCI_SANDBOX_RESEED_CREDS=1") {
		t.Errorf("expected the reseed env flag in the run argv, got: %s", createLine)
	}
}

func TestSandboxUnknownFlag_Exits2NoRuntimeCalls(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "build", "--bogus")
	cmd.Env = batchEnv(t, fakeDir, assets)
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v\n%s", err, err, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2\n%s", exitErr.ExitCode(), output)
	}
	if lines := callLogLines(t, callLog); len(lines) > 0 {
		t.Errorf("expected the runtime to never be invoked for an unknown flag, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestSandboxUnknownFlag_OnPrune_Exits2NoRuntimeCalls(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "prune", "--bogus")
	cmd.Env = batchEnv(t, fakeDir, assets)
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v\n%s", err, err, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2\n%s", exitErr.ExitCode(), output)
	}
	if lines := callLogLines(t, callLog); len(lines) > 0 {
		t.Errorf("expected the runtime to never be invoked for an unknown flag, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestSandboxTrailingPositional_Exits2NoRuntimeCalls(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)

	cmd := exec.Command(binaryPath, "sandbox", "build", "extra")
	cmd.Env = batchEnv(t, fakeDir, assets)
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v\n%s", err, err, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2\n%s", exitErr.ExitCode(), output)
	}
	if lines := callLogLines(t, callLog); len(lines) > 0 {
		t.Errorf("expected the runtime to never be invoked for a trailing positional, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestSandboxUnknownSubcommand_Exits2(t *testing.T) {
	cmd := exec.Command(binaryPath, "sandbox", "bogus")
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v\n%s", err, err, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2\n%s", exitErr.ExitCode(), output)
	}
}

func TestSandboxNoSubcommand_Exits2(t *testing.T) {
	cmd := exec.Command(binaryPath, "sandbox")
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v\n%s", err, err, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2\n%s", exitErr.ExitCode(), output)
	}
}

// -- sandbox ls / stop (native Go against docker/podman) -----------------

const canonPSAllOutput = "claude-cenci-agentstack\tUp 2 hours\tcenci-sandbox:latest\n" +
	"codex-cenci-agentstack\tExited (0) 5 minutes ago\tcenci-sandbox:latest\n" +
	"unrelated-container\tUp 1 hour\tnginx:latest\n"

func TestSandboxLs_ListsMatchingContainersFromFakeDocker(t *testing.T) {
	dir := t.TempDir()
	callLog := filepath.Join(dir, "calls.txt")
	writeFakeDocker(t, dir, "docker", callLog, canonPSAllOutput)

	cmd := exec.Command(binaryPath, "sandbox", "ls")
	cmd.Env = append(os.Environ(), "PATH="+dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox ls: %v\n%s", err, output)
	}

	out := string(output)
	if !strings.Contains(out, "claude-cenci-agentstack") || !strings.Contains(out, "codex-cenci-agentstack") {
		t.Errorf("expected both sandbox containers listed, got:\n%s", out)
	}
	if strings.Contains(out, "unrelated-container") {
		t.Errorf("expected non-sandbox container to be filtered out, got:\n%s", out)
	}

	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	if !strings.Contains(string(calls), "ps -a") {
		t.Errorf("expected a 'ps -a' invocation, got call log:\n%s", calls)
	}
}

func TestSandboxStop_StopsMatchingContainers(t *testing.T) {
	dir := t.TempDir()
	callLog := filepath.Join(dir, "calls.txt")
	// Only running containers are relevant to `stop`.
	psRunning := "claude-cenci-agentstack\tUp 2 hours\tcenci-sandbox:latest\n" +
		"unrelated-container\tUp 1 hour\tnginx:latest\n"
	writeFakeDocker(t, dir, "docker", callLog, psRunning)

	cmd := exec.Command(binaryPath, "sandbox", "stop")
	cmd.Env = append(os.Environ(), "PATH="+dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox stop: %v\n%s", err, output)
	}

	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	callsStr := string(calls)
	if !strings.Contains(callsStr, "stop claude-cenci-agentstack") {
		t.Errorf("expected a 'stop claude-cenci-agentstack' invocation, got call log:\n%s", callsStr)
	}
	if strings.Contains(callsStr, "stop unrelated-container") {
		t.Errorf("expected non-sandbox container to never be stopped, got call log:\n%s", callsStr)
	}
}

func TestSandboxStop_WithFilterArg_OnlyStopsMatchingName(t *testing.T) {
	dir := t.TempDir()
	callLog := filepath.Join(dir, "calls.txt")
	psRunning := "claude-cenci-agentstack\tUp 2 hours\tcenci-sandbox:latest\n" +
		"codex-cenci-otherrepo\tUp 1 hour\tcenci-sandbox:latest\n"
	writeFakeDocker(t, dir, "docker", callLog, psRunning)

	cmd := exec.Command(binaryPath, "sandbox", "stop", "agentstack")
	cmd.Env = append(os.Environ(), "PATH="+dir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandbox stop agentstack: %v\n%s", err, output)
	}

	calls, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	callsStr := string(calls)
	if !strings.Contains(callsStr, "stop claude-cenci-agentstack") {
		t.Errorf("expected the matching container to be stopped, got call log:\n%s", callsStr)
	}
	if strings.Contains(callsStr, "stop codex-cenci-otherrepo") {
		t.Errorf("expected the non-matching container to be left alone, got call log:\n%s", callsStr)
	}
}

// -- open ------------------------------------------------------------------
//
// `cenci open` launches natively: the launcher engine creates/attaches the
// container itself and finally syscall.Execs the runtime CLI, so these tests
// assert the docker argv — including the entrypoint contract
// (launcher-lifecycle), the model shortcuts, the strict flag grammar
// (flag-parsing), and the #195 cenci wiring (cenci-wiring) that the retired
// bash suites used to pin against cenci-sand.

// openTestEnv builds the black-box environment for native `cenci open` runs:
// the scripted runtimes on PATH, an isolated HOME, the asset fixture,
// deterministic TERM/TMUX_PANE, scrubbed optional env passthroughs, and a live
// events socket under a private 0700 CENCI_SOCKET_DIR — pre-created here so
// the subprocess wires cenci without ever spawning a real daemon.
// CENCI_SOCKET_DIR's tier-1 override is verbatim (no appended "cenci/"
// segment, #1142), so socketDir (a fresh t.TempDir(), chmod'd to 0700) IS the
// resolved socket dir directly. No host `claude` is installed: agents live in
// the container's persistent home, so the launcher never resolves an agent
// binary from the host.
func openTestEnv(t *testing.T, fakeDir, assets string) (env []string, home, socketDir string) {
	t.Helper()
	home = t.TempDir()
	socketDir = t.TempDir()
	if err := os.Chmod(socketDir, 0700); err != nil {
		t.Fatalf("chmod socketDir: %v", err)
	}
	l, err := net.Listen("unix", filepath.Join(socketDir, "cenci-events.sock"))
	if err != nil {
		t.Fatalf("listen events socket: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	tag, err := launcher.BaseTag(assets)
	if err != nil {
		t.Fatalf("BaseTag: %v", err)
	}
	// os/exec keeps the LAST duplicate env key, so these appends override the
	// inherited values; the empty assignments scrub optional passthroughs
	// (and a possible ambient CENCI_SANDBOX) for deterministic argv.
	// FAKE_IMAGE_BASE_VERSION defaults to the fixture's own current BaseTag so
	// pre-existing "image current -> skip build" tests keep passing; tests
	// that want to exercise base-drift staleness override it with their own
	// append (see batchEnv, whose FAKE_IMAGE_BASE_VERSION convention this
	// mirrors).
	env = append(os.Environ(),
		"PATH="+fakeDir+":/usr/bin:/bin",
		"HOME="+home,
		"CENCI_SANDBOX_ASSETS="+assets,
		"CENCI_SOCKET_DIR="+socketDir,
		"TERM=xterm-256color",
		"TMUX_PANE=%7",
		// CENCI_TMUX_SOCKET (#1007) is derived from the first comma-separated
		// field of $TMUX; set explicitly here (rather than left ambient) so
		// the derived value is deterministic across test environments,
		// including ones not run inside tmux at all.
		"TMUX=/tmp/tmux-1000/cenci,12345,0",
		"COLORTERM=", "CONTEXT7_API_KEY=", "OPENAI_API_KEY=", "CENCI_SANDBOX=",
		"ANTHROPIC_API_KEY=",
		// #1087: the launcher resolves planning.attended from the fleet
		// config, whose path comes from XDG_CONFIG_HOME (falling back to
		// $HOME/.config). Pin it inside the fixture home so these black-box
		// runs can never read the developer's or CI runner's REAL fleet
		// config — an ambient `planning.attended: true` would otherwise flip
		// the forwarded CENCI_ATTENDED value and make the assertions below
		// machine-dependent. CENCI_ATTENDED itself is scrubbed to the empty
		// value for the same reason -- an ambient pin would override the
		// fixture config entirely. Empty is deliberately NOT a pin
		// (resolveAttendedFlag's "set and non-empty" gate), so the scrub
		// leaves the fixture home's own fleet config in charge: absent by
		// default, and whatever a test writes there when it exercises the
		// attended path.
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
		"CENCI_ATTENDED=",
		"FAKE_VOLUMES=cenci-agent-cli-claude\ncenci-agent-cli-codex\n",
		"FAKE_IMAGE_BASE_VERSION="+tag,
	)
	return env, home, socketDir
}

// attachLine returns the final `exec -it ...` attach argv line from the call
// log (the line the binary syscall.Exec'd the fake runtime with).
func attachLine(t *testing.T, lines []string) string {
	t.Helper()
	line, ok := findLineWithPrefix(lines, "exec -it ")
	if !ok {
		t.Fatalf("expected an interactive attach exec, got calls:\n%s", strings.Join(lines, "\n"))
	}
	return line
}

func TestOpenAllShortcuts_AttachTheirAgentAndModel(t *testing.T) {
	cases := []struct {
		token, agent, model, permFlag string
	}{
		{"ch", "claude", "haiku", "--dangerously-skip-permissions"},
		{"cs", "claude", "sonnet", "--dangerously-skip-permissions"},
		{"co", "claude", "opus", "--dangerously-skip-permissions"},
		{"cf", "claude", "fable", "--dangerously-skip-permissions"},
		// Codex additionally needs --dangerously-bypass-hook-trust: hook
		// trust lives in the user config layer and provisioning never seeds
		// it, so without the flag the cenci-watch hooks are silently skipped
		// as "pending review" and sandbox sessions never report (#426).
		{"xl", "codex", "gpt-5.6-luna", "--dangerously-bypass-approvals-and-sandbox --dangerously-bypass-hook-trust"},
		{"xt", "codex", "gpt-5.6-terra", "--dangerously-bypass-approvals-and-sandbox --dangerously-bypass-hook-trust"},
		{"xs", "codex", "gpt-5.6-sol", "--dangerously-bypass-approvals-and-sandbox --dangerously-bypass-hook-trust"},
	}
	for _, tc := range cases {
		t.Run(tc.token, func(t *testing.T) {
			fakeDir := t.TempDir()
			callLog := writeScriptedRuntimes(t, fakeDir)
			assets := writeAssetFixture(t)
			env, home, _ := openTestEnv(t, fakeDir, assets)
			if tc.agent == "codex" {
				// Codex refuses to launch without auth staged from the host.
				if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			cmd := exec.Command(binaryPath, "open", tc.token)
			cmd.Env = env
			cmd.Dir = t.TempDir() // non-git cwd → legacy "default" scope
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("open %s: %v\n%s", tc.token, err, output)
			}

			line := attachLine(t, callLogLines(t, callLog))
			wantTail := tc.agent + "-cenci-default /opt/cenci-agent/current/node_modules/.bin/" + tc.agent + " " + tc.permFlag + " --model " + tc.model
			if !strings.HasSuffix(line, wantTail) {
				t.Errorf("attach argv = %q, want suffix %q", line, wantTail)
			}
			if tc.agent == "claude" && !strings.Contains(line, "-e DISABLE_UPDATES=1") {
				t.Errorf("Claude agent exec did not suppress native updates: %s", line)
			}
			if tc.agent == "codex" && strings.Contains(line, "DISABLE_UPDATES") {
				t.Errorf("Claude-only update env leaked into Codex: %s", line)
			}
		})
	}
}

func TestOpenBare_DefaultsToClaudeSonnet(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open: %v\n%s", err, output)
	}

	line := attachLine(t, callLogLines(t, callLog))
	if !strings.HasSuffix(line, "/opt/cenci-agent/current/node_modules/.bin/claude --dangerously-skip-permissions --model sonnet") {
		t.Errorf("attach argv = %q, want the claude/sonnet defaults", line)
	}
}

func TestOpenModelFlag_OverridesShortcutModel(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch", "--model", "opus")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open ch --model opus: %v\n%s", err, output)
	}

	line := attachLine(t, callLogLines(t, callLog))
	if !strings.HasSuffix(line, "/opt/cenci-agent/current/node_modules/.bin/claude --dangerously-skip-permissions --model opus") {
		t.Errorf("attach argv = %q, want the explicit --model to win", line)
	}
}

func TestOpen_FreshCreate_PinsEntrypointContract(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, home, socketDir := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open ch: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	runLine, ok := findLineWithPrefix(lines, "run --name ")
	if !ok {
		t.Fatalf("expected a container run, got calls:\n%s", strings.Join(lines, "\n"))
	}

	// The entrypoint contract, byte-for-byte where it matters: privileged
	// create + detached lifecycle label + host identity env + the PID-1 idle
	// tail that owns readiness.
	for _, want := range []string{
		"--name claude-cenci-default",
		"--hostname sandbox-default",
		"--label cenci-sand.lifecycle=detached",
		"-d --init --rm",
		"--user root",
		"-e TERM=xterm-256color",
		"-e CENCI_SANDBOX=1",
		"-e CENCI_SANDBOX_AGENT=claude",
		"-e CENCI_SANDBOX_PLUGINS=cenci cenci-watch",
		"-e CENCI_AGENT_CLI=/opt/cenci-agent/current/node_modules/.bin/claude",
		fmt.Sprintf("-e HOST_UID=%d", os.Getuid()),
		fmt.Sprintf("-e HOST_GID=%d", os.Getgid()),
		"-e WORKSPACE_SCOPE=legacy",
		"-v " + home + "/Repos:/workspace",
		"-v claude-cenci-home-default:/home/dev",
		"-v cenci-agent-cli-claude:/opt/cenci-agent:ro",
		"-v " + socketDir + ":/run/user/1000/cenci:ro",
		":/usr/local/bin/cenci:ro",
		"-e XDG_RUNTIME_DIR=/run/user/1000",
		"-e CENCI_SOCKET_DIR=/run/user/1000/cenci",
	} {
		if !strings.Contains(runLine, want) {
			t.Errorf("run argv missing %q:\n%s", want, runLine)
		}
	}
	// Claude lives in the shared read-only volume, not the writable home or a
	// host bind mount.
	if strings.Contains(runLine, ":/usr/local/bin/claude:ro") {
		t.Errorf("run argv must not bind-mount the host claude binary:\n%s", runLine)
	}
	// TMUX_PANE must only travel per exec session (asserted on the attach
	// below): baked into the container-lifetime env it goes stale when the
	// creating pane closes, and reap-orphans would kill PID 1 (#356).
	if strings.Contains(runLine, "TMUX_PANE") {
		t.Errorf("run argv must not carry TMUX_PANE (#356):\n%s", runLine)
	}
	// CENCI_TMUX_SOCKET is pane-scoped identity exactly like TMUX_PANE (#1007):
	// baked into the container-lifetime env it would go stale the same way,
	// and reap-orphans' (socket, pane) matching would misclassify using a
	// stale socket.
	if strings.Contains(runLine, "CENCI_TMUX_SOCKET") {
		t.Errorf("run argv must not carry CENCI_TMUX_SOCKET (#1007):\n%s", runLine)
	}
	if strings.Contains(runLine, "DISABLE_UPDATES") {
		t.Errorf("DISABLE_UPDATES must be scoped to Claude agent execs, not plugin provisioning:\n%s", runLine)
	}
	if !strings.HasSuffix(runLine, "cenci-sandbox:latest -c touch /tmp/cenci-ready && exec sleep infinity") {
		t.Errorf("run argv must end with the image and PID-1 readiness tail, got:\n%s", runLine)
	}

	// Readiness was polled as dev before the attach.
	if !anyLineContains(lines, "exec -u dev claude-cenci-default test -e /tmp/cenci-ready") {
		t.Errorf("expected a readiness probe, got calls:\n%s", strings.Join(lines, "\n"))
	}

	// The attach itself runs as dev with the in-container marker set.
	line := attachLine(t, lines)
	for _, want := range []string{"-u dev", "-e CENCI_SANDBOX=1", "-e CENCI_SANDBOX_AGENT=claude", "-e TMUX_PANE=%7", "-e CENCI_TMUX_SOCKET=/tmp/tmux-1000/cenci", "-e DISABLE_UPDATES=1", "/opt/cenci-agent/current/node_modules/.bin/claude"} {
		if !strings.Contains(line, want) {
			t.Errorf("attach argv missing %q:\n%s", want, line)
		}
	}
}

func TestOpen_AbsentSharedVolumeBootstrapsBeforeWorkloadWithoutSecrets(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env, "FAKE_VOLUMES=", "CENCI_TEST_SENTINEL_SECRET=parent-only-secret")
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open bootstrap: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	updater, workload := -1, -1
	for i, line := range lines {
		if strings.Contains(line, "agent-cli.sh update claude") {
			updater = i
			for _, forbidden := range []string{"/home/dev", "/workspace", "OPENAI_API_KEY", "parent-only-secret", "docker.sock", "/tmp/host-"} {
				if strings.Contains(line, forbidden) {
					t.Errorf("bootstrap updater contains %q: %s", forbidden, line)
				}
			}
		}
		if strings.Contains(line, "--name claude-cenci-default") {
			workload = i
		}
	}
	if updater < 0 || workload < 0 || updater >= workload {
		t.Errorf("updater must complete before workload create; calls:\n%s", strings.Join(lines, "\n"))
	}
}

func TestOpen_ExistingSharedVolumePerformsNoUpdaterOrVersionCheck(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open existing volume: %v\n%s", err, output)
	}
	lines := callLogLines(t, callLog)
	if anyLineContains(lines, "agent-cli.sh update") {
		t.Errorf("existing shared volume triggered an updater/version check")
	}
	// The cheap populated-check (finding 3a) still runs against the existing
	// volume; it just never falls through to the updater when it passes.
	if !anyLineContains(lines, "agent-cli.sh status claude") {
		t.Errorf("expected the populated-check to run against the existing volume, got calls:\n%s", strings.Join(lines, "\n"))
	}
}

// TestOpen_ExistingButUnpopulatedVolume_FallsThroughToUpdater pins finding
// 3a: a volume that docker/podman auto-created via a concurrent first
// launch's `docker run -v` (or one left behind by a previously failed
// bootstrap) reports as "existing" but is actually empty/broken. The cheap
// populated-check must catch that and fall through to the updater instead of
// mounting a broken CLI into the workload.
func TestOpen_ExistingButUnpopulatedVolume_FallsThroughToUpdater(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env, "FAKE_AGENT_CHECK_EXIT=1")
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open (unpopulated existing volume): %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	updater, workload := -1, -1
	for i, line := range lines {
		if strings.Contains(line, "agent-cli.sh update claude") {
			updater = i
		}
		if strings.HasPrefix(line, "run --name claude-cenci-default") {
			workload = i
		}
	}
	if updater < 0 || workload < 0 || updater >= workload {
		t.Errorf("an existing-but-unpopulated volume must fall through to the updater before workload create; calls:\n%s", strings.Join(lines, "\n"))
	}
}

// agentStatusFakeLine builds a FAKE_AGENT_STATUS env assignment for
// writeScriptedRuntime's `agent-cli.sh status` branch: the five key=value
// lines (populated=yes, a fixed version, pin, last_success, last_attempt),
// with lastSuccess/lastAttempt resolved to unix-epoch-seconds at
// fixture-write time (zero time.Time renders as the empty "unset" fact) —
// ticket #710's staleness policy is exercised via these Go-computed relative
// timestamps rather than a production clock seam.
func agentStatusFakeLine(pin string, lastSuccess, lastAttempt time.Time) string {
	ls, la := "", ""
	if !lastSuccess.IsZero() {
		ls = fmt.Sprintf("%d", lastSuccess.Unix())
	}
	if !lastAttempt.IsZero() {
		la = fmt.Sprintf("%d", lastAttempt.Unix())
	}
	return fmt.Sprintf("FAKE_AGENT_STATUS=populated=yes\\nversion=1.2.3\\npin=%s\\nlast_success=%s\\nlast_attempt=%s\\n", pin, ls, la)
}

// TestOpen_StaleAgentVolume_StartsDetachedRefreshBeforeWorkloadCreate pins
// ticket #710's staleness policy under ticket #745's non-blocking refinement:
// a populated shared agent volume whose last_success is older than the
// (default 24h) TTL starts the updater *detached* (--detach, under the fixed
// cenci-agent-cli-refresh-<agent> name) before the workload container is
// created, tells the user the refresh happens in the background, and the
// launch still succeeds without waiting for the update's outcome.
func TestOpen_StaleAgentVolume_StartsDetachedRefreshBeforeWorkloadCreate(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	stale := time.Now().Add(-25 * time.Hour)
	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env, agentStatusFakeLine("", stale, time.Time{}))
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open (TTL-stale existing volume): %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "background") {
		t.Errorf("expected the stale-refresh notice to say the refresh happens in the background, got:\n%s", output)
	}

	lines := callLogLines(t, callLog)
	updater, workload := -1, -1
	for i, line := range lines {
		if strings.Contains(line, "agent-cli.sh update claude") {
			updater = i
			if !strings.Contains(line, "--detach") || !strings.Contains(line, "--name cenci-agent-cli-refresh-claude") {
				t.Errorf("expected the stale-refresh updater to start detached under the fixed refresh name, got: %s", line)
			}
		}
		if strings.HasPrefix(line, "run --name claude-cenci-default") {
			workload = i
		}
	}
	if updater < 0 || workload < 0 || updater >= workload {
		t.Errorf("a TTL-stale shared agent volume must start its detached refresh before workload create; calls:\n%s", strings.Join(lines, "\n"))
	}
	attachLine(t, lines)
}

func TestOpen_VolumeInspectionFailureIsNotMissingVolume(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env, "FAKE_VOLUME_LS_EXIT=17")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected volume infrastructure error, got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), "volume ls") || anyLineContains(callLogLines(t, callLog), "agent-cli.sh update") {
		t.Errorf("volume inspection failure was treated as an absent volume:\n%s", output)
	}
}

func TestOpen_FirstBootstrapFailureStreamsDiagnosticBeforeWorkloadCreate(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)
	env = append(env,
		"FAKE_VOLUMES=",
		"FAKE_RUN_STDERR=registry exploded: original diagnostic",
		"FAKE_RUN_EXIT=23",
	)

	cmd := exec.Command(binaryPath, "open")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected startup failure exit 1, got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), "registry exploded: original diagnostic") {
		t.Errorf("expected updater's original diagnostic, got:\n%s", output)
	}
	lines := callLogLines(t, callLog)
	if anyLineContains(lines, "--name claude-cenci-default") || anyLineContains(lines, "exec -it") {
		t.Errorf("workload must not be created after bootstrap failure; calls:\n%s", strings.Join(lines, "\n"))
	}
}

// TestOpen_ContainerExitsDuringStartup_ReportsStatusAndExitCode pins finding
// 4: a container that exits during startup is surfaced immediately with its
// status/exit code, not degraded into the generic 60-second readiness
// timeout.
func TestOpen_ContainerExitsDuringStartup_ReportsStatusAndExitCode(t *testing.T) {
	fakeDir := t.TempDir()
	writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env, "FAKE_READY_EXIT=1", "FAKE_INSPECT_STATE=exited 1")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected a startup failure exit 1, got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), "failed during startup (status exited, exit 1)") {
		t.Errorf("expected the status/exit code in the error, got:\n%s", output)
	}
	if strings.Contains(string(output), "did not become ready within 60 seconds") {
		t.Errorf("expected the immediate startup-failure error, not the generic timeout, got:\n%s", output)
	}
}

// TestOpen_StartupErrorMarkerSurfacedVerbatim pins finding 4: when
// sandbox/entrypoint.sh writes /home/dev/.cenci-agent-startup-error (agent
// CLI path missing/not executable) before exiting non-zero, the launch error
// surfaces that marker's content verbatim instead of falling back to
// container logs or the generic failure message.
func TestOpen_StartupErrorMarkerSurfacedVerbatim(t *testing.T) {
	fakeDir := t.TempDir()
	writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	const marker = "agent CLI missing at /opt/cenci-agent/current/node_modules/.bin/claude"
	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_READY_EXIT=1",
		"FAKE_INSPECT_STATE=exited 1",
		"FAKE_STARTUP_ERROR="+marker,
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected a startup failure exit 1, got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), marker) {
		t.Errorf("expected the startup-error marker content surfaced verbatim, got:\n%s", output)
	}
	if !strings.Contains(string(output), "CENCI-SANDBOX-START-001") {
		t.Errorf("expected the agent-CLI-missing error code CENCI-SANDBOX-START-001, got:\n%s", output)
	}
	if strings.Contains(string(output), "CENCI-SANDBOX-START-002") {
		t.Errorf("expected only the agent-CLI-missing code, not the generic-entrypoint code, got:\n%s", output)
	}
}

// TestOpen_WaitUntilReadyInspectFailure_NoBlankStatusExit pins ticket #473's
// first symptom: when the container is already gone (auto-removed by --rm)
// by the time waitUntilReady exhausts its inspect-error retry budget,
// containerStartupState returns blank status/exit strings alongside its
// error — the outer message must substitute a clear fallback instead of
// interpolating those blanks into "(status , exit )".
func TestOpen_WaitUntilReadyInspectFailure_NoBlankStatusExit(t *testing.T) {
	fakeDir := t.TempDir()
	writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_READY_EXIT=1",
		"FAKE_CONTAINER_INSPECT_EXIT=1",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected a startup failure exit 1, got %T %v\n%s", err, err, output)
	}
	if strings.Contains(string(output), "(status , exit )") {
		t.Errorf("expected a fallback in place of blank status/exit, got the literal blank parenthetical:\n%s", output)
	}
	if !strings.Contains(string(output), "(status unknown, exit unknown)") {
		t.Errorf("expected a clear status/exit fallback, got:\n%s", output)
	}
}

// TestOpen_GenericEntrypointFailureSurfacesBootLog pins ticket #473's second
// symptom: an entrypoint failure at a point other than the agent-CLI-missing
// check (so no /home/dev/.cenci-agent-startup-error marker exists) still
// surfaces real diagnostic content — sandbox/entrypoint.sh's teed boot log —
// instead of degrading into startupFailureDetail's fully generic fallback.
// Parametrized over both agents per the ticket's acceptance criteria.
func TestOpen_GenericEntrypointFailureSurfacesBootLog(t *testing.T) {
	cases := []struct {
		name, token string
	}{
		{"claude", "ch"},
		{"codex", "xt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeDir := t.TempDir()
			writeScriptedRuntimes(t, fakeDir)
			assets := writeAssetFixture(t)
			env, home, _ := openTestEnv(t, fakeDir, assets)
			if tc.name == "codex" {
				if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			const bootLog = "boot log: mkdir /workspace/.cenci: permission denied"
			cmd := exec.Command(binaryPath, "open", tc.token)
			cmd.Env = append(env,
				"FAKE_READY_EXIT=1",
				"FAKE_INSPECT_STATE=exited 1",
				"FAKE_BOOT_LOG="+bootLog,
			)
			cmd.Dir = t.TempDir()
			output, err := cmd.CombinedOutput()

			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 1 {
				t.Fatalf("expected a startup failure exit 1, got %T %v\n%s", err, err, output)
			}
			if !strings.Contains(string(output), bootLog) {
				t.Errorf("expected the boot log content surfaced, got:\n%s", output)
			}
			if strings.Contains(string(output), "entrypoint exited before initialization completed") {
				t.Errorf("expected the boot log, not the fully generic fallback, got:\n%s", output)
			}
			if !strings.Contains(string(output), "CENCI-SANDBOX-START-002") {
				t.Errorf("expected the generic-entrypoint error code CENCI-SANDBOX-START-002, got:\n%s", output)
			}
			if strings.Contains(string(output), "CENCI-SANDBOX-START-001") {
				t.Errorf("expected only the generic-entrypoint code, not the agent-CLI-missing code, got:\n%s", output)
			}
		})
	}
}

// TestOpen_GenericEntrypointFailureSurfacesStartupMarker pins the third
// startupFailureDetail precedence step: when no agent-CLI marker AND no boot
// log content exists, the generic EXIT-trap marker
// (/home/dev/.cenci-startup-failed) is still surfaced verbatim rather than
// falling through to the fully generic fallback string.
func TestOpen_GenericEntrypointFailureSurfacesStartupMarker(t *testing.T) {
	fakeDir := t.TempDir()
	writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	const marker = "generic startup failure: entrypoint trap fired at credential seeding"
	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_READY_EXIT=1",
		"FAKE_INSPECT_STATE=exited 1",
		"FAKE_STARTUP_MARKER="+marker,
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected a startup failure exit 1, got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), marker) {
		t.Errorf("expected the generic trap marker content surfaced verbatim, got:\n%s", output)
	}
	if strings.Contains(string(output), "entrypoint exited before initialization completed") {
		t.Errorf("expected the trap marker, not the fully generic fallback, got:\n%s", output)
	}
	if !strings.Contains(string(output), "CENCI-SANDBOX-START-002") {
		t.Errorf("expected the generic-entrypoint error code CENCI-SANDBOX-START-002, got:\n%s", output)
	}
	if strings.Contains(string(output), "CENCI-SANDBOX-START-001") {
		t.Errorf("expected only the generic-entrypoint code, not the agent-CLI-missing code, got:\n%s", output)
	}
}

// -- ticket #1163 Q1: startupFailureDetail's mixed read-failed fallback ----
//
// startupFailureDetail checks up to three home-volume markers plus a
// `logs --tail 50` fallback, in precedence order. Before #1163, every
// non-successful read (a legitimate "no such marker" AND a genuine runtime
// read failure) was silently the same "absent" outcome, so a
// daemon-unreachable failure on all three reads degraded into the generic
// "entrypoint exited before initialization completed" fallback — actively
// misleading, since nothing was actually learned about the entrypoint at
// all. Q1's rule: if ANY of the three reads comes back read-failed, emit a
// distinct read-failed fallback string instead; the generic fallback is
// reserved for the case where every read was DEFINITIVELY absent (and the
// logs fallback also yielded nothing).

// TestOpen_AllHomeVolumeReadsFailed_ReportsReadFailedNotGenericFallback pins
// Q1 case (a): every one of the three home-volume reads fails at the
// runtime level (exit 125) — the read-failed fallback string must be
// emitted, and the generic "entrypoint exited before initialization
// completed" string must never appear (it would misrepresent "we could not
// look" as "we looked and found nothing").
func TestOpen_AllHomeVolumeReadsFailed_ReportsReadFailedNotGenericFallback(t *testing.T) {
	fakeDir := t.TempDir()
	writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_READY_EXIT=1",
		"FAKE_INSPECT_STATE=exited 1",
		"FAKE_STARTUP_ERROR_EXIT=125",
		"FAKE_BOOT_LOG_EXIT=125",
		"FAKE_STARTUP_MARKER_EXIT=125",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected a startup failure exit 1, got %T %v\n%s", err, err, output)
	}
	if strings.Contains(string(output), "entrypoint exited before initialization completed") {
		t.Errorf("expected the read-failed fallback, not the generic \"nothing found\" fallback (every read failed at the runtime level, not absent), got:\n%s", output)
	}
	if !strings.Contains(string(output), "read failed") {
		t.Errorf("expected a distinct read-failed fallback string naming the read failure, got:\n%s", output)
	}
}

// TestOpen_MixedHomeVolumeReadFailure_ReportsReadFailedNotGenericFallback
// pins Q1 case (b): only the FIRST read (the agent-CLI-missing marker)
// fails at the runtime level; the other two reads and the logs fallback are
// legitimately absent/empty. Even one read-failed among the three must
// still emit the read-failed fallback — the outcome isn't "upgraded" to
// looking clean just because the other two reads came back absent.
func TestOpen_MixedHomeVolumeReadFailure_ReportsReadFailedNotGenericFallback(t *testing.T) {
	fakeDir := t.TempDir()
	writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_READY_EXIT=1",
		"FAKE_INSPECT_STATE=exited 1",
		"FAKE_STARTUP_ERROR_EXIT=125",
		"FAKE_BOOT_LOG_EXIT=1",
		"FAKE_STARTUP_MARKER_EXIT=1",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected a startup failure exit 1, got %T %v\n%s", err, err, output)
	}
	if strings.Contains(string(output), "entrypoint exited before initialization completed") {
		t.Errorf("expected the read-failed fallback even though only ONE of the three reads failed, got:\n%s", output)
	}
	if !strings.Contains(string(output), "read failed") {
		t.Errorf("expected a distinct read-failed fallback string naming the read failure, got:\n%s", output)
	}
}

// TestOpen_AllHomeVolumeReadsDefinitivelyAbsent_ReportsGenericFallback pins
// Q1 case (c), the non-regression guard: when all three reads are
// DEFINITIVELY absent (exit 1, "no such marker") and the logs fallback
// yields nothing, the existing generic fallback string must still be
// emitted verbatim — the #1163 change must not make every startup failure
// report "read failed".
func TestOpen_AllHomeVolumeReadsDefinitivelyAbsent_ReportsGenericFallback(t *testing.T) {
	fakeDir := t.TempDir()
	writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_READY_EXIT=1",
		"FAKE_INSPECT_STATE=exited 1",
		"FAKE_STARTUP_ERROR_EXIT=1",
		"FAKE_BOOT_LOG_EXIT=1",
		"FAKE_STARTUP_MARKER_EXIT=1",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected a startup failure exit 1, got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), "entrypoint exited before initialization completed") {
		t.Errorf("expected the generic fallback when every read is definitively absent and logs are empty, got:\n%s", output)
	}
	if strings.Contains(string(output), "read failed") {
		t.Errorf("did not expect the read-failed fallback when every read was definitively absent, not failed, got:\n%s", output)
	}
}

// TestOpen_AgentStartupErrorMarkerAllControlBytes_FallsThroughToBootLog pins
// a #1163 security re-review regression: fix #2 made readHomeVolumeFile
// return (homeVolumeReadOK, "") for a genuinely successful read whose
// content sanitizes down to empty (e.g. a marker containing only stripped
// control bytes), rather than misclassifying it as Absent. Before this fix,
// startupFailureDetail's first tier matched a plain `case homeVolumeReadOK`
// and returned immediately on that empty content, suppressing every
// remaining tier (boot log, startup-failed, the logs fallback) down to a
// bare error code with no detail — a container-controlled way to blind the
// very diagnostics this ticket exists to make reliable. An all-control-byte
// .cenci-agent-startup-error marker must fall through to the boot-log tier
// exactly like a legitimately absent marker does, not short-circuit.
func TestOpen_AgentStartupErrorMarkerAllControlBytes_FallsThroughToBootLog(t *testing.T) {
	fakeDir := t.TempDir()
	writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	const bootLog = "boot log: mkdir /workspace/.cenci: permission denied"
	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_READY_EXIT=1",
		"FAKE_INSPECT_STATE=exited 1",
		"FAKE_STARTUP_ERROR=\x1b\x1b\x1b",
		"FAKE_BOOT_LOG="+bootLog,
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected a startup failure exit 1, got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), bootLog) {
		t.Errorf("expected an all-control-byte agent-CLI-missing marker to fall through to the boot-log tier instead of suppressing it, got:\n%s", output)
	}
	if strings.Contains(string(output), "CENCI-SANDBOX-START-001") {
		t.Errorf("expected the OK-but-empty agent-CLI-missing marker to NOT short-circuit with its own code (no real detail was ever found there), got:\n%s", output)
	}
	if !strings.Contains(string(output), "CENCI-SANDBOX-START-002") {
		t.Errorf("expected the generic-entrypoint code (the boot-log tier that should have fired), got:\n%s", output)
	}
}

func TestOpen_NameFlag_ScopesContainerVolumeHostname(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "--name", "mybox")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open --name mybox: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	runLine, ok := findLineWithPrefix(lines, "run --name ")
	if !ok {
		t.Fatalf("expected a container run, got calls:\n%s", strings.Join(lines, "\n"))
	}
	for _, want := range []string{
		"--name claude-cenci-mybox",
		"--hostname sandbox-mybox",
		"-v claude-cenci-home-mybox:/home/dev",
	} {
		if !strings.Contains(runLine, want) {
			t.Errorf("run argv missing %q:\n%s", want, runLine)
		}
	}
}

func TestOpenShell_AttachesBash(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "--shell")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open --shell: %v\n%s", err, output)
	}

	line := attachLine(t, callLogLines(t, callLog))
	if !strings.HasSuffix(line, "claude-cenci-default /bin/bash") {
		t.Errorf("attach argv = %q, want a /bin/bash shell attach", line)
	}
	if !strings.Contains(string(output), "Attaching shell to running 'claude-cenci-default'") {
		t.Errorf("expected the shell-attach message, got:\n%s", output)
	}
}

func TestOpenPassthrough_ReachesAgentVerbatim(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch", "--", "--resume", "-p", "fix the bug")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open ch -- ...: %v\n%s", err, output)
	}

	line := attachLine(t, callLogLines(t, callLog))
	if !strings.HasSuffix(line, "/opt/cenci-agent/current/node_modules/.bin/claude --dangerously-skip-permissions --model haiku --resume -p fix the bug") {
		t.Errorf("attach argv = %q, want the passthrough tokens verbatim after the agent flags", line)
	}
}

func TestOpenReseedCreds_SetsReseedEnvOnCreate(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "--reseed-creds")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open --reseed-creds: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	runLine, ok := findLineWithPrefix(lines, "run --name ")
	if !ok {
		t.Fatalf("expected a container run, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(runLine, "-e CENCI_SANDBOX_RESEED_CREDS=1") {
		t.Errorf("expected the reseed env flag in the run argv, got:\n%s", runLine)
	}
}

func TestOpenHostNetwork_AddsNetworkHostWithWarning(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "--host-network")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open --host-network: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	runLine, _ := findLineWithPrefix(lines, "run --name ")
	if !strings.Contains(runLine, "--network host") {
		t.Errorf("expected --network host in the run argv, got:\n%s", runLine)
	}
	if !strings.Contains(string(output), "weakens the container's isolation boundary") {
		t.Errorf("expected the host-network warning, got:\n%s", output)
	}
}

// -- open --dind (sysbox nested-Docker mode, #585) -------------------------
//
// dind replaces the removed --docker DooD path: instead of bind-mounting the
// host's runtime socket (root-equivalent access to the host), dind runs the
// workload container itself under the sysbox-runc OCI runtime with its own
// isolated per-repo Docker storage volume, so a nested `docker` inside the
// sandbox never touches the host runtime at all.

// dindRepoEnv builds a git-initialized repo (dind is only ever on in repo
// scope) with an optional `.cenci/config.json` `sandbox.dind` value, and
// returns the repo's resolved root and slug.
func dindRepoEnv(t *testing.T, dindConfig bool) (repoRoot, slug string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if dindConfig {
		if err := os.MkdirAll(filepath.Join(repo, ".cenci"), 0o755); err != nil {
			t.Fatalf("mkdir .cenci: %v", err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".cenci", "config.json"), []byte(`{"sandbox":{"dind":true}}`), 0o644); err != nil {
			t.Fatalf("write .cenci/config.json: %v", err)
		}
	}
	repoRoot, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatalf("resolve repo path: %v", err)
	}
	return repoRoot, launcher.Slugify(filepath.Base(repoRoot))
}

func TestOpenDind_ViaRepoConfig_AddsSysboxRuntimeVolumeAndEnv(t *testing.T) {
	repoRoot, slug := dindRepoEnv(t, true)

	fakeDir := t.TempDir()
	// docker-only: dind's preflight requires Docker as the outer runtime, and
	// podman-first detection would otherwise win were both fakes present.
	callLog := writeDockerOnlyRuntime(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)
	env = append(env, `FAKE_INFO_RUNTIMES={"sysbox-runc":{},"runc":{}}`)

	cmd := exec.Command(binaryPath, "open")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open (dind via repo config): %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	createLine, ok := findLineWithPrefix(lines, "run --name ")
	if !ok {
		t.Fatalf("expected a container run, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(createLine, "--runtime=sysbox-runc") {
		t.Errorf("expected --runtime=sysbox-runc in the run argv, got: %s", createLine)
	}
	wantVolume := "claude-cenci-dind-" + slug + ":/var/lib/docker"
	if !strings.Contains(createLine, wantVolume) {
		t.Errorf("expected the dind storage volume mount %q, got: %s", wantVolume, createLine)
	}
	if !strings.Contains(createLine, "-e CENCI_SANDBOX_DIND=1") {
		t.Errorf("expected -e CENCI_SANDBOX_DIND=1 in the run argv, got: %s", createLine)
	}
}

func TestOpenNoDind_OverridesDindConfigRepo(t *testing.T) {
	repoRoot, _ := dindRepoEnv(t, true)

	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "--no-dind")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open --no-dind: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	createLine, ok := findLineWithPrefix(lines, "run --name ")
	if !ok {
		t.Fatalf("expected a container run, got calls:\n%s", strings.Join(lines, "\n"))
	}
	for _, marker := range []string{"--runtime=sysbox-runc", "-cenci-dind-", "CENCI_SANDBOX_DIND"} {
		if strings.Contains(createLine, marker) {
			t.Errorf("expected --no-dind to suppress dind despite the repo config, but found %q in: %s", marker, createLine)
		}
	}
}

// malformedDindRepoEnv builds a git-initialized repo with an unparsable
// `.cenci/config.json` (#632): RepoDindConfig must hard-fail on this input
// rather than silently resolving dind off, since a corrupt config must not
// silently change launch posture.
func malformedDindRepoEnv(t *testing.T) (repoRoot string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".cenci"), 0o755); err != nil {
		t.Fatalf("mkdir .cenci: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".cenci", "config.json"), []byte(`{not valid json`), 0o644); err != nil {
		t.Fatalf("write .cenci/config.json: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatalf("resolve repo path: %v", err)
	}
	return resolved
}

// configuredRepoEnv (#1002) git-inits a temp dir (skipping the test if git
// isn't available, mirroring malformedDindRepoEnv) and writes config
// verbatim as its `.cenci/config.json` — a fixture for exercising
// sandbox.plugins values (valid subsets, the empty array, or a malformed
// key), as opposed to malformedDindRepoEnv's fixed unparsable-JSON case.
func configuredRepoEnv(t *testing.T, config string) (repoRoot string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".cenci"), 0o755); err != nil {
		t.Fatalf("mkdir .cenci: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".cenci", "config.json"), []byte(config), 0o644); err != nil {
		t.Fatalf("write .cenci/config.json: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatalf("resolve repo path: %v", err)
	}
	return resolved
}

// TestOpen_MalformedDindConfig_Exits1 pins the #632 hard-fail: a corrupt
// .cenci/config.json must never silently launch with dind off — it must
// hard-fail the launch (exit 1) with a path-bearing, non-usage error.
func TestOpen_MalformedDindConfig_Exits1(t *testing.T) {
	repoRoot := malformedDindRepoEnv(t)

	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected malformed .cenci/config.json to exit 1 (hard fail, not a usage error), got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), "config.json") {
		t.Errorf("expected a path-bearing error naming config.json, got:\n%s", output)
	}
	if lines := callLogLines(t, callLog); len(lines) != 0 {
		t.Errorf("expected no runtime calls once config parsing hard-fails, got:\n%s", strings.Join(lines, "\n"))
	}
}

// TestOpenNoDind_SucceedsDespiteMalformedConfig pinned --no-dind as a
// config-free escape hatch pre-#1002 (#632): it kept working even with a
// corrupt repo .cenci/config.json, because ResolveDind returns before
// RepoDindConfig is ever reached. #1002 narrows that escape hatch (Q&A #1):
// resolveLaunchContext now ALSO resolves sandbox.plugins unconditionally
// (after resolveDindForHost, regardless of --no-dind -- there is no
// "--no-plugins" equivalent), so the same unparsable config.json that
// --no-dind used to route around is read again for plugins and hard-fails
// the launch. --no-dind still suppresses dind's own config read; it can no
// longer make a corrupt config file launchable at all.
func TestOpenNoDind_SucceedsDespiteMalformedConfig(t *testing.T) {
	repoRoot := malformedDindRepoEnv(t)

	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "--no-dind")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected --no-dind with a malformed .cenci/config.json to exit 1 (the plugins read still hard-fails on it, #1002), got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), "config.json") {
		t.Errorf("expected a path-bearing error naming config.json, got:\n%s", output)
	}
	if lines := callLogLines(t, callLog); len(lines) != 0 {
		t.Errorf("expected no runtime calls once the plugins config read hard-fails, got:\n%s", strings.Join(lines, "\n"))
	}
}

// -- ticket #1002: sandbox.plugins resolution ------------------------------

// TestOpen_MalformedSandboxPluginsConfig_Exits1 pins RepoSandboxPlugins'
// #632-mirroring hard-fail reaching `cenci open`: a well-formed JSON
// document whose "sandbox.plugins" value is outside the closed set must
// hard-fail the launch (exit 1) with a path-bearing, non-usage error naming
// the offending value — not silently fall back to the default pair.
func TestOpen_MalformedSandboxPluginsConfig_Exits1(t *testing.T) {
	repoRoot := configuredRepoEnv(t, `{"sandbox":{"plugins":["cenci","bogus-plugin"]}}`)

	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected an unrecognized sandbox.plugins value to exit 1 (hard fail, not a usage error), got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), "config.json") {
		t.Errorf("expected a path-bearing error naming config.json, got:\n%s", output)
	}
	if !strings.Contains(string(output), "bogus-plugin") {
		t.Errorf("expected the error to name the offending value \"bogus-plugin\", got:\n%s", output)
	}
	if lines := callLogLines(t, callLog); len(lines) != 0 {
		t.Errorf("expected no runtime calls once the plugins config read hard-fails, got:\n%s", strings.Join(lines, "\n"))
	}
}

// TestOpen_FreshCreate_DefaultPluginsSubset_PrintsNoInformationalLine pins
// the negative half of the AC: when the resolved list equals the default
// pair (no repo config at all, the common case), nothing is printed about
// plugin provisioning.
func TestOpen_FreshCreate_DefaultPluginsSubset_PrintsNoInformationalLine(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = env
	cmd.Dir = t.TempDir() // non-git cwd -> legacy scope, always the default pair
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open ch: %v\n%s", err, output)
	}

	if strings.Contains(string(output), "plugin") {
		t.Errorf("expected no plugin-provisioning note for the default plugin list, got:\n%s", output)
	}

	runLine, ok := findLineWithPrefix(callLogLines(t, callLog), "run --name ")
	if !ok {
		t.Fatal("expected a container run")
	}
	if !strings.Contains(runLine, "-e CENCI_SANDBOX_PLUGINS=cenci cenci-watch") {
		t.Errorf("expected the default resolved list in the create argv, got:\n%s", runLine)
	}
}

// TestOpen_FreshCreate_NarrowedPluginsSubset_PrintsInformationalLine pins
// the AC's informational line: when the repo config narrows sandbox.plugins
// away from the default pair, `cenci open` prints one line before the
// create/attach decision naming what will be provisioned, and the create
// argv's CENCI_SANDBOX_PLUGINS reflects exactly the narrowed subset.
func TestOpen_FreshCreate_NarrowedPluginsSubset_PrintsInformationalLine(t *testing.T) {
	repoRoot := configuredRepoEnv(t, `{"sandbox":{"plugins":["cenci"]}}`)

	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open (narrowed sandbox.plugins): %v\n%s", err, output)
	}

	out := string(output)
	if !strings.Contains(out, "plugin") {
		t.Errorf("expected an informational note about the narrowed plugin list, got:\n%s", out)
	}
	if strings.Contains(out, "cenci-watch") {
		t.Errorf("expected the informational note to name only the narrowed subset (not cenci-watch, which was dropped), got:\n%s", out)
	}

	runLine, ok := findLineWithPrefix(callLogLines(t, callLog), "run --name ")
	if !ok {
		t.Fatal("expected a container run")
	}
	if !strings.Contains(runLine, "-e CENCI_SANDBOX_PLUGINS=cenci") {
		t.Errorf("expected the narrowed resolved list in the create argv, got:\n%s", runLine)
	}
	if strings.Contains(runLine, "CENCI_SANDBOX_PLUGINS=cenci cenci-watch") {
		t.Errorf("expected the create argv to carry only the narrowed subset, not the default pair, got:\n%s", runLine)
	}
}

// TestOpen_FreshCreate_EmptyPluginsList_PrintsDistinctInformationalLine pins
// the AC's "distinct wording for the empty list" requirement: an explicitly
// empty sandbox.plugins array is a valid, non-default resolution and must
// still print an informational line, worded differently from the narrowed-
// but-non-empty case above (this test only pins that the line exists and
// the create argv's CENCI_SANDBOX_PLUGINS is empty; the exact wording is an
// implementation choice for the next, non-red phase).
func TestOpen_FreshCreate_EmptyPluginsList_PrintsDistinctInformationalLine(t *testing.T) {
	repoRoot := configuredRepoEnv(t, `{"sandbox":{"plugins":[]}}`)

	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open (empty sandbox.plugins): %v\n%s", err, output)
	}

	out := string(output)
	if !strings.Contains(out, "plugin") {
		t.Errorf("expected an informational note about the empty plugin list, got:\n%s", out)
	}

	runLine, ok := findLineWithPrefix(callLogLines(t, callLog), "run --name ")
	if !ok {
		t.Fatal("expected a container run")
	}
	if !strings.Contains(runLine, "-e CENCI_SANDBOX_PLUGINS=") {
		t.Errorf("expected CENCI_SANDBOX_PLUGINS to still be set (with an empty value) even for the empty list, got:\n%s", runLine)
	}
	if strings.Contains(runLine, "-e CENCI_SANDBOX_PLUGINS=cenci") {
		t.Errorf("expected an empty resolved list in the create argv, got:\n%s", runLine)
	}
}

// openAttachEnvFor (#1002) is the shared setup for the attach-path plugins
// drift tests below: a running, detached-labeled, compatibly-mounted
// container named containerName (mirroring
// TestOpen_AttachToRunning_SkipsCreate's fixture) scripted to answer the
// drift probe with containerEnv (verbatim FAKE_CONTAINER_ENV content — e.g.
// "CENCI_SANDBOX_PLUGINS=cenci\n", or "" to simulate an absent var / legacy
// container).
func openAttachEnvFor(t *testing.T, fakeDir, assets, containerName, containerEnv string) []string {
	t.Helper()
	env, _, socketDir := openTestEnv(t, fakeDir, assets)
	return append(env,
		"FAKE_PS="+containerName+"\n",
		"FAKE_INSPECT_LABEL=detached",
		"FAKE_INSPECT_MOUNTS=/workspace::/workspace\n/home/dev::/home/dev\n"+socketDir+"::/run/user/1000/cenci\n",
		"FAKE_CONTAINER_ENV="+containerEnv,
	)
}

// TestOpenAttach_PluginsDrift_WarnsWithStopRemedy pins the AC's attach-path
// drift warning: when the resolved list (from repo config) differs from the
// already-running container's actual CENCI_SANDBOX_PLUGINS (read back via
// the best-effort drift probe), the launcher prints exactly one drift line
// naming the `cenci sandbox stop <name>` remedy — and never auto-recreates
// or blocks the attach.
func TestOpenAttach_PluginsDrift_WarnsWithStopRemedy(t *testing.T) {
	repoRoot := configuredRepoEnv(t, `{"sandbox":{"plugins":["cenci"]}}`)
	containerName := "claude-cenci-" + launcher.Slugify(filepath.Base(repoRoot))

	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env := openAttachEnvFor(t, fakeDir, assets, containerName, "CENCI_SANDBOX_PLUGINS=cenci cenci-watch\n")

	cmd := exec.Command(binaryPath, "open")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open (attach, plugins drift): %v\n%s", err, output)
	}

	out := string(output)
	if !strings.Contains(out, "cenci sandbox stop "+containerName) {
		t.Errorf("expected the drift warning to name the 'cenci sandbox stop %s' remedy, got:\n%s", containerName, out)
	}
	if _, ok := findLineWithPrefix(callLogLines(t, callLog), "run --name "); ok {
		t.Errorf("expected drift to warn, never auto-recreate the container, got calls:\n%s", strings.Join(callLogLines(t, callLog), "\n"))
	}
	attachLine(t, callLogLines(t, callLog))
}

// TestOpenAttach_PluginsNoDrift_ResolvedMatchesRunning_NoWarning pins the
// no-drift baseline: when the resolved list (the default pair, no repo
// config) matches the running container's actual CENCI_SANDBOX_PLUGINS
// exactly, no drift line is printed.
func TestOpenAttach_PluginsNoDrift_ResolvedMatchesRunning_NoWarning(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env := openAttachEnvFor(t, fakeDir, assets, "claude-cenci-default", "CENCI_SANDBOX_PLUGINS=cenci cenci-watch\n")

	cmd := exec.Command(binaryPath, "open")
	cmd.Env = env
	cmd.Dir = t.TempDir() // legacy scope -> resolved default pair
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open (attach, no drift, default): %v\n%s", err, output)
	}
	if strings.Contains(string(output), "cenci sandbox stop") {
		t.Errorf("expected no drift warning when the resolved list matches the running container's value, got:\n%s", output)
	}
	attachLine(t, callLogLines(t, callLog))
}

// TestOpenAttach_PluginsNoDrift_LegacyUnsetRunning_ComparesAgainstDefault
// pins the legacy-container signal (Q&A #5): a running container that
// carries no CENCI_SANDBOX_PLUGINS at all (predates this ticket) must be
// treated as if it were created with the default pair — so a launch whose
// own resolved list IS the default pair sees no drift.
func TestOpenAttach_PluginsNoDrift_LegacyUnsetRunning_ComparesAgainstDefault(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env := openAttachEnvFor(t, fakeDir, assets, "claude-cenci-default", "") // no CENCI_SANDBOX_PLUGINS line at all

	cmd := exec.Command(binaryPath, "open")
	cmd.Env = env
	cmd.Dir = t.TempDir() // legacy scope -> resolved default pair
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open (attach, legacy unset running env): %v\n%s", err, output)
	}
	if strings.Contains(string(output), "cenci sandbox stop") {
		t.Errorf("expected no drift warning: an unset running CENCI_SANDBOX_PLUGINS (legacy container) must compare against the default pair, which is what this launch resolved to, got:\n%s", output)
	}
	attachLine(t, callLogLines(t, callLog))
}

// TestOpenAttach_PluginsDriftProbeFails_DoesNotBlockAttach pins the AC's
// "never blocks the attach when the probe or parse fails" requirement: the
// drift probe's own inspect call failing (a transient runtime error, an
// unparsable response) must never abort the attach — unlike
// inspectReusePosture, which deliberately fails closed. The attach must
// still succeed and no drift warning is owed (the probe couldn't determine
// anything).
func TestOpenAttach_PluginsDriftProbeFails_DoesNotBlockAttach(t *testing.T) {
	repoRoot := configuredRepoEnv(t, `{"sandbox":{"plugins":["cenci"]}}`)
	containerName := "claude-cenci-" + launcher.Slugify(filepath.Base(repoRoot))

	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env := append(openAttachEnvFor(t, fakeDir, assets, containerName, "CENCI_SANDBOX_PLUGINS=cenci cenci-watch\n"),
		"FAKE_CONTAINER_ENV_EXIT=1", // the drift probe itself fails
	)

	cmd := exec.Command(binaryPath, "open")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open (attach, drift probe failure must not block attach): %v\n%s", err, output)
	}
	if strings.Contains(string(output), "cenci sandbox stop") {
		t.Errorf("expected no drift warning when the drift probe itself fails (nothing could be determined), got:\n%s", output)
	}
	attachLine(t, callLogLines(t, callLog))
}

// symlinkGitOnlyDir symlinks the real git binary into dir (already holding a
// scripted single-runtime fake) and returns dir, so a caller can set PATH to
// dir alone: git resolves, but no host PATH directory can leak a real
// docker/podman into the runtime lookup a test deliberately made single.
func symlinkGitOnlyDir(t *testing.T, dir string) string {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	if err := os.Symlink(gitPath, filepath.Join(dir, "git")); err != nil {
		t.Fatalf("symlink git into %s: %v", dir, err)
	}
	return dir
}

func TestOpenDind_PodmanOnlyHost_Exits1RequiresDocker(t *testing.T) {
	repoRoot, _ := dindRepoEnv(t, false)

	fakeDir := t.TempDir()
	callLog := writePodmanOnlyRuntime(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)
	// openTestEnv's PATH appends /usr/bin:/bin so git resolves, but on a host
	// that also has a real docker installed there, that would leak a real
	// docker back onto PATH and defeat this test's "docker absent from PATH"
	// premise. Symlink git into fakeDir and use it alone, so no host
	// directory can supply a real docker/podman.
	env = append(env, "PATH="+symlinkGitOnlyDir(t, fakeDir))

	cmd := exec.Command(binaryPath, "open", "--dind")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected the podman-outer-runtime dind preflight failure to exit 1, got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), "requires Docker") {
		t.Errorf("expected an error naming the Docker-as-outer-runtime requirement, got:\n%s", output)
	}
	if lines := callLogLines(t, callLog); anyLineContains(lines, "run --name ") {
		t.Errorf("expected no container to be created when dind preflight fails, got calls:\n%s", strings.Join(lines, "\n"))
	}
}

func TestOpenDind_SysboxNotRegistered_Exits1WithInstallPointer(t *testing.T) {
	repoRoot, _ := dindRepoEnv(t, false)

	fakeDir := t.TempDir()
	callLog := writeDockerOnlyRuntime(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)
	env = append(env, `FAKE_INFO_RUNTIMES={"runc":{}}`) // no sysbox-runc registered

	cmd := exec.Command(binaryPath, "open", "--dind")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected the sysbox-not-registered preflight failure to exit 1, got %T %v\n%s", err, err, output)
	}
	lower := strings.ToLower(string(output))
	for _, want := range []string{"sysbox", "arch", "ubuntu", "nestybox"} {
		if !strings.Contains(lower, want) {
			t.Errorf("expected the sysbox install-pointer message to mention %q, got:\n%s", want, output)
		}
	}
	if lines := callLogLines(t, callLog); anyLineContains(lines, "run --name ") {
		t.Errorf("expected no container to be created when dind preflight fails, got calls:\n%s", strings.Join(lines, "\n"))
	}
}

// TestOpenDind_DockerInfoFails_Exits1 covers AC #7: dindPreflight's
// `docker info` call failing (nonzero exit — the daemon is down) must exit 1
// with the operator-facing wrapping-chain message ("checking sysbox-runc
// registration ... docker info: exit status 1"), distinct from the
// already-covered unregistered-sysbox install-pointer message (asserted
// absent below so this test can never accidentally pass by hitting that
// other branch), and must never create a container.
func TestOpenDind_DockerInfoFails_Exits1(t *testing.T) {
	repoRoot, _ := dindRepoEnv(t, false)

	fakeDir := t.TempDir()
	callLog := writeDockerOnlyRuntime(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)
	env = append(env, "FAKE_INFO_EXIT=1")

	cmd := exec.Command(binaryPath, "open", "--dind")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected the docker-info-fails dind preflight failure to exit 1, got %T %v\n%s", err, err, output)
	}
	out := string(output)
	if !strings.Contains(out, "checking sysbox-runc registration") {
		t.Errorf("expected the daemon-down wrapping message to mention checking sysbox-runc registration, got:\n%s", out)
	}
	if !strings.Contains(out, "info") {
		t.Errorf("expected the daemon-down wrapping message to mention the failing `info` call, got:\n%s", out)
	}
	if strings.Contains(out, "sysbox-ce") {
		t.Errorf("expected NOT to hit the already-covered unregistered-sysbox install-pointer message (mentions sysbox-ce), got:\n%s", out)
	}
	if lines := callLogLines(t, callLog); anyLineContains(lines, "run --name ") {
		t.Errorf("expected no container to be created when dind preflight fails, got calls:\n%s", strings.Join(lines, "\n"))
	}
}

// TestOpenDind_DockerdMarkerPresent_FreshCreate_WarnsBeforeAttachButStillAttaches
// pins ticket #630's Q1 (warn, still attach): a persistent dockerd
// startup-failure marker detected right before the first agent attach on a
// freshly-created container must print a prominent, non-fatal Warning naming
// CENCI-SANDBOX-DIND-001 and the captured diagnostic, but must NOT block the
// attach — the agent session still launches (matches #586's non-blocking
// dockerd-start mandate; the container and every other capability but nested
// Docker still work).
func TestOpenDind_DockerdMarkerPresent_FreshCreate_WarnsBeforeAttachButStillAttaches(t *testing.T) {
	repoRoot, _ := dindRepoEnv(t, true)

	fakeDir := t.TempDir()
	callLog := writeDockerOnlyRuntime(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)
	const marker = "2026-07-24T09:00:00Z dockerd exited with status 1: failed to start daemon: mkdir /var/lib/docker/overlay2: read-only file system"
	env = append(env,
		`FAKE_INFO_RUNTIMES={"sysbox-runc":{},"runc":{}}`,
		"FAKE_DOCKERD_MARKER="+marker,
	)

	cmd := exec.Command(binaryPath, "open")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open (dind, dockerd marker present): %v\n%s", err, output)
	}

	out := string(output)
	if !strings.Contains(out, "Warning:") {
		t.Errorf("expected a prominent Warning: for the dockerd startup-failure marker, got:\n%s", out)
	}
	if !strings.Contains(out, marker) {
		t.Errorf("expected the marker's captured diagnostic surfaced verbatim, got:\n%s", out)
	}
	if !strings.Contains(out, "CENCI-SANDBOX-DIND-001") {
		t.Errorf("expected the CENCI-SANDBOX-DIND-001 error code attached, got:\n%s", out)
	}

	// Non-fatal: the agent session still attached despite the warning.
	attachLine(t, callLogLines(t, callLog))
}

// TestOpenDind_NoDockerdMarker_NoWarning is the sibling of the test above:
// when no dockerd-startup-error marker was ever written, dind mode must
// launch silently on this front (no false-positive warning).
func TestOpenDind_NoDockerdMarker_NoWarning(t *testing.T) {
	repoRoot, _ := dindRepoEnv(t, true)

	fakeDir := t.TempDir()
	callLog := writeDockerOnlyRuntime(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)
	env = append(env, `FAKE_INFO_RUNTIMES={"sysbox-runc":{},"runc":{}}`)
	// FAKE_DOCKERD_MARKER deliberately left unset.

	cmd := exec.Command(binaryPath, "open")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open (dind, no dockerd marker): %v\n%s", err, output)
	}

	if strings.Contains(string(output), "CENCI-SANDBOX-DIND-001") {
		t.Errorf("did not expect the dind startup-failure warning when no marker was ever written, got:\n%s", output)
	}
	attachLine(t, callLogLines(t, callLog))
}

// TestOpen_NonDind_NeverProbesDockerdMarker asserts the dockerd-marker
// before-attach check is strictly gated on ctx.DindOn: a non-dind launch
// must never even attempt the short-lived-container home-volume read for
// .cenci-dockerd-startup-error (that path doesn't exist for a non-dind
// session's home volume in the first place).
func TestOpen_NonDind_NeverProbesDockerdMarker(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open ch (non-dind): %v\n%s", err, output)
	}

	if anyLineContains(callLogLines(t, callLog), ".cenci-dockerd-startup-error") {
		t.Errorf("non-dind launch must never probe the dockerd-startup-error marker, got calls:\n%s", strings.Join(callLogLines(t, callLog), "\n"))
	}
}

// TestOpenDind_DockerdMarkerPresent_AttachToRunning_WarnsBeforeAttach covers
// the launcher's second warnDockerdStartupFailure call site (before the
// "attach to an already-running container" runAgent, not just fresh
// create): the marker must still surface non-fatally before this attach too.
func TestOpenDind_DockerdMarkerPresent_AttachToRunning_WarnsBeforeAttach(t *testing.T) {
	repoRoot, slug := dindRepoEnv(t, true)

	fakeDir := t.TempDir()
	callLog := writeDockerOnlyRuntime(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, socketDir := openTestEnv(t, fakeDir, assets)
	const marker = "2026-07-24T09:05:00Z dockerd exited with status 137: killed"
	env = append(env,
		`FAKE_INFO_RUNTIMES={"sysbox-runc":{},"runc":{}}`,
		"FAKE_DOCKERD_MARKER="+marker,
		"FAKE_PS=claude-cenci-"+slug+"\n",
		"FAKE_INSPECT_LABEL=detached",
		"FAKE_INSPECT_MOUNTS=/workspace::/workspace\n/home/dev::/home/dev\n"+socketDir+"::/run/user/1000/cenci\n",
		"FAKE_REUSE_POSTURE=on|sysbox-runc|1\nworkspace-vol::/workspace\ndind-vol::/var/lib/docker\n\n",
	)

	cmd := exec.Command(binaryPath, "open")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open (dind, attach to running, dockerd marker present): %v\n%s", err, output)
	}

	out := string(output)
	if !strings.Contains(out, "CENCI-SANDBOX-DIND-001") {
		t.Errorf("expected the CENCI-SANDBOX-DIND-001 warning before attaching to the already-running container, got:\n%s", out)
	}
	if !strings.Contains(out, marker) {
		t.Errorf("expected the marker's captured diagnostic surfaced verbatim, got:\n%s", out)
	}
	attachLine(t, callLogLines(t, callLog))
}

func TestOpenDindAndNoDind_Together_Exits2(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "--dind", "--no-dind")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("expected --dind --no-dind together to exit 2, got %T %v\n%s", err, err, output)
	}
	// Distinguishes the intended usage-conflict error from --dind simply not
	// existing yet as a recognized flag (which also exits 2, coincidentally).
	if strings.Contains(string(output), "flag provided but not defined") {
		t.Errorf("expected --dind and --no-dind to be recognized flags rejected for conflicting together, not unrecognized flags; got:\n%s", output)
	}
	if lines := callLogLines(t, callLog); len(lines) > 0 {
		t.Errorf("expected no runtime calls for the --dind/--no-dind usage error, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestOpenDind_LegacyScope_Exits2(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "--dind")
	cmd.Env = env
	cmd.Dir = t.TempDir() // non-git cwd -> legacy "default" scope
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("expected --dind in legacy scope to be a usage error (exit 2), got %T %v\n%s", err, err, output)
	}
	// Distinguishes the intended scope-conflict usage error from --dind
	// simply not existing yet as a recognized flag (which also exits 2,
	// coincidentally).
	if strings.Contains(string(output), "flag provided but not defined") {
		t.Errorf("expected --dind to be a recognized flag rejected for legacy scope, not an unrecognized flag; got:\n%s", output)
	}
	if lines := callLogLines(t, callLog); len(lines) > 0 {
		t.Errorf("expected no runtime calls for the dind/legacy-scope usage error, got:\n%s", strings.Join(lines, "\n"))
	}
}

// TestOpenDockerFlag_IsRemoved_Exits2NoRunCall regression-tests the removal
// of the --docker DooD flag (replaced by --dind): it must now be rejected as
// an unrecognized flag, and critically must never reach container creation.
func TestOpenDockerFlag_IsRemoved_Exits2NoRunCall(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "--docker")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("expected the removed --docker flag to exit 2, got %T %v\n%s", err, err, output)
	}
	if lines := callLogLines(t, callLog); anyLineContains(lines, "run --name ") {
		t.Errorf("expected no 'run' subcommand for the removed --docker flag, got calls:\n%s", strings.Join(lines, "\n"))
	}
}

func TestOpenCodexWithoutAuth_Exits1(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets) // no ~/.codex/auth.json, OPENAI_API_KEY scrubbed

	cmd := exec.Command(binaryPath, "open", "xt")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v\n%s", err, err, output)
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("exit code = %d, want 1\n%s", exitErr.ExitCode(), output)
	}
	if !strings.Contains(string(output), "requires Codex auth") {
		t.Errorf("expected the codex auth error, got:\n%s", output)
	}
	// Credential validation happens late, while assembling the workload's own
	// create args, so it never races the shared agent volume's read-only
	// populated-check (EnsureAgentVolume runs unconditionally early in
	// Launch) -- only the workload container itself must never be created.
	if lines := callLogLines(t, callLog); anyLineContains(lines, "run --name ") {
		t.Errorf("expected no workload container run without codex auth, got:\n%s", strings.Join(lines, "\n"))
	}
}

// TestOpenCodexWithoutAuth_DoesNotRemoveStoppedContainer is a #620 follow-up
// regression test (code review, "Should Fix"): Launch's create branch used to
// run `rm <container-name>` unconditionally before building the create argv,
// so a stopped/absent same-named container was always deleted even when the
// launch itself then failed (e.g. codex credential validation). The #620
// refactor moved `rm` to run after planArgvs (which includes buildRunArgv's
// credential validation) succeeds, so a failed launch must now leave any
// stopped, same-named container untouched — nothing removes it before the
// hard error is returned. FAKE_PS is left empty (its default), which is the
// "not running" case containerRunning reads regardless of whether the
// container is stopped or absent.
func TestOpenCodexWithoutAuth_DoesNotRemoveStoppedContainer(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets) // no ~/.codex/auth.json, OPENAI_API_KEY scrubbed

	cmd := exec.Command(binaryPath, "open", "xt")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected codex auth failure to exit 1, got %T %v\n%s", err, err, output)
	}

	lines := callLogLines(t, callLog)
	if anyLineContains(lines, "rm codex-cenci-default") {
		t.Errorf("expected the stopped/absent same-named container to survive a failed credential validation (rm must run after planArgvs succeeds, not before), got calls:\n%s", strings.Join(lines, "\n"))
	}
}

// TestOpenCodex_RemovesStoppedContainerOnSuccessfulCreate is the success-path
// sibling of TestOpenCodexWithoutAuth_DoesNotRemoveStoppedContainer: once
// credential validation (and the rest of planArgvs) succeeds, Launch must
// still remove a stopped/absent same-named container before creating the new
// one, exactly as it always has.
func TestOpenCodex_RemovesStoppedContainerOnSuccessfulCreate(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "xt")
	cmd.Env = append(env, "OPENAI_API_KEY=sk-cenci-test-secret") // satisfies the codex auth gate
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open xt: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	rmIdx := -1
	runIdx := -1
	for i, l := range lines {
		if l == "rm codex-cenci-default" {
			rmIdx = i
		}
		if strings.HasPrefix(l, "run --name codex-cenci-default") {
			runIdx = i
		}
	}
	if rmIdx == -1 {
		t.Fatalf("expected 'rm codex-cenci-default' before creating the container, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if runIdx == -1 {
		t.Fatalf("expected a 'run --name codex-cenci-default' create call, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if rmIdx > runIdx {
		t.Errorf("expected rm (index %d) to run before create (index %d), got calls:\n%s", rmIdx, runIdx, strings.Join(lines, "\n"))
	}
}

// TestOpenCodex_ForwardsProviderKeyPerExecOnly pins Codex's OPENAI_API_KEY
// forwarding to the per-exec-only model OpenCode already uses (#490/#509):
// the env var alone must satisfy the codex auth gate (no ~/.codex/auth.json
// staged) and reach the exec-time attach, but it must never appear in the
// create-time `run --name` args — else it would sit in the container's PID-1
// environ for the container's whole lifetime, readable via `docker inspect`/
// `/proc/1/environ` (#510). The attach exec argv itself must carry the bare
// "-e OPENAI_API_KEY" token only, never the secret VALUE — the full argv
// stays live in `ps` for the whole interactive session via execAttach's
// syscall.Exec handoff (#759), so the runtime CLI reads the value from its
// own inherited environment instead.
func TestOpenCodex_ForwardsProviderKeyPerExecOnly(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets) // no ~/.codex/auth.json staged

	cmd := exec.Command(binaryPath, "open", "--agent", "codex")
	cmd.Env = append(env, "OPENAI_API_KEY=sk-cenci-test-secret")
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open --agent codex: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	runLine, ok := findLineWithPrefix(lines, "run --name ")
	if !ok {
		t.Fatalf("expected a container run, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if strings.Contains(runLine, "OPENAI_API_KEY") {
		t.Errorf("create-time run args must never bake the provider key into the PID-1 environ; got:\n%s", runLine)
	}

	line := attachLine(t, lines)
	if !strings.Contains(line, "-e OPENAI_API_KEY") {
		t.Errorf("attach argv missing per-exec OPENAI_API_KEY forwarding:\n%s", line)
	}
	if strings.Contains(line, "sk-cenci-test-secret") {
		t.Errorf("attach argv must never carry the OPENAI_API_KEY secret VALUE (readable via ps for the whole session, #759); got:\n%s", line)
	}
	if strings.Contains(line, "OPENAI_API_KEY=") {
		t.Errorf("attach argv must forward OPENAI_API_KEY value-less (bare -e NAME, letting the runtime CLI read its own inherited environment, #759); got:\n%s", line)
	}
}

// TestOpenOpencode_FreshCreate_PinsBareInvocationAndScopesProviderKeys pins
// #490's launch contract for OpenCode: a bare `opencode` invocation (no
// --dangerously-skip-permissions equivalent — permissions are config-driven
// via the seeded opencode.json), and ANTHROPIC_API_KEY/OPENAI_API_KEY
// forwarded only at exec time (per-session), never baked into the
// container-lifetime create-time env/PID-1 environ.
func TestOpenOpencode_FreshCreate_PinsBareInvocationAndScopesProviderKeys(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "--agent", "opencode")
	cmd.Env = append(env, "ANTHROPIC_API_KEY=sk-test-anthropic", "OPENAI_API_KEY=sk-test-openai")
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open --agent opencode: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	runLine, ok := findLineWithPrefix(lines, "run --name ")
	if !ok {
		t.Fatalf("expected a container run, got calls:\n%s", strings.Join(lines, "\n"))
	}
	for _, forbidden := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		if strings.Contains(runLine, forbidden) {
			t.Errorf("create-time run args must never bake provider keys into the PID-1 environ; got %q in:\n%s", forbidden, runLine)
		}
	}

	line := attachLine(t, lines)
	if !strings.HasSuffix(line, "/opt/cenci-agent/current/node_modules/.bin/opencode") {
		t.Errorf("attach argv = %q, want a bare opencode invocation with no permission-skip flag and no forced --model", line)
	}
	for _, name := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		if !strings.Contains(line, "-e "+name) {
			t.Errorf("attach argv missing bare -e %s (opencode-only per-exec provider key forwarding):\n%s", name, line)
		}
	}
	for _, secret := range []string{"sk-test-anthropic", "sk-test-openai"} {
		if strings.Contains(line, secret) {
			t.Errorf("attach argv must never carry a provider key secret VALUE (readable via ps for the whole session, #759); got:\n%s", line)
		}
	}
	for _, forbidden := range []string{"ANTHROPIC_API_KEY=", "OPENAI_API_KEY="} {
		if strings.Contains(line, forbidden) {
			t.Errorf("attach argv must forward provider keys value-less (bare -e NAME, #759); got %q in:\n%s", forbidden, line)
		}
	}
}

// TestOpenClaude_NeverForwardsProviderKeys pins the opencode-only scoping of
// ANTHROPIC_API_KEY/OPENAI_API_KEY (#490): a Claude launch must never receive
// either provider key, at create time or exec time, even when both are set
// on the host.
func TestOpenClaude_NeverForwardsProviderKeys(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env, "OPENAI_API_KEY=sk-test-openai", "ANTHROPIC_API_KEY=sk-test-anthropic")
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open ch: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	runLine, ok := findLineWithPrefix(lines, "run --name ")
	if !ok {
		t.Fatalf("expected a container run, got calls:\n%s", strings.Join(lines, "\n"))
	}
	line := attachLine(t, lines)
	for _, forbidden := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY"} {
		if strings.Contains(runLine, forbidden) {
			t.Errorf("claude create-time run args must never carry a provider key (opencode-only forwarding); got %q in:\n%s", forbidden, runLine)
		}
		if strings.Contains(line, forbidden) {
			t.Errorf("claude attach exec args must never carry a provider key (opencode-only forwarding); got %q in:\n%s", forbidden, line)
		}
	}
}

// TestOpenClaude_ForwardsContext7KeyValuelessAtCreateTime pins ticket #759's
// create-path fix: unlike the provider API keys (per-exec only),
// CONTEXT7_API_KEY is also forwarded at container-create time
// (Engine.assembleEnv), but its VALUE must never land in the create-time
// `run --name` argv — only the bare "-e CONTEXT7_API_KEY" token, letting the
// entrypoint's own inherited environment supply the value. openTestEnv
// scrubs CONTEXT7_API_KEY= from the inherited env, and os/exec keeps the
// last duplicate key, so the sentinel is appended explicitly here.
func TestOpenClaude_ForwardsContext7KeyValuelessAtCreateTime(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	const context7Secret = "sk-context7-test-secret"
	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env, "CONTEXT7_API_KEY="+context7Secret)
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open ch: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	runLine, ok := findLineWithPrefix(lines, "run --name ")
	if !ok {
		t.Fatalf("expected a container run, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(runLine, "-e CONTEXT7_API_KEY") {
		t.Errorf("create-time run args missing the bare -e CONTEXT7_API_KEY token; got:\n%s", runLine)
	}
	if strings.Contains(runLine, context7Secret) {
		t.Errorf("create-time run args must never carry the CONTEXT7_API_KEY secret VALUE (readable via ps/docker inspect for the container's whole lifetime, #759); got:\n%s", runLine)
	}
	if strings.Contains(runLine, "CONTEXT7_API_KEY=") {
		t.Errorf("create-time run args must forward CONTEXT7_API_KEY value-less (bare -e NAME, #759); got:\n%s", runLine)
	}
}

// TestOpenOpencodeWithoutAuth_Exits1 mirrors TestOpenCodexWithoutAuth_Exits1:
// OpenCode has a hard credential requirement (ANTHROPIC_API_KEY,
// OPENAI_API_KEY, or a staged ~/.local/share/opencode/auth.json), so a launch
// with none of those present must fail before ever creating the workload
// container.
func TestOpenOpencodeWithoutAuth_Exits1(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets) // no opencode auth.json, provider keys scrubbed

	cmd := exec.Command(binaryPath, "open", "--agent", "opencode")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v\n%s", err, err, output)
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("exit code = %d, want 1\n%s", exitErr.ExitCode(), output)
	}
	if !strings.Contains(string(output), "OpenCode") || !strings.Contains(string(output), "auth") {
		t.Errorf("expected an OpenCode auth error, got:\n%s", output)
	}
	if lines := callLogLines(t, callLog); anyLineContains(lines, "run --name ") {
		t.Errorf("expected no workload container run without opencode auth, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestOpen_AttachToRunning_SkipsCreate(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, socketDir := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_PS=claude-cenci-default\n",
		"FAKE_INSPECT_LABEL=detached",
		"FAKE_INSPECT_MOUNTS=/workspace::/workspace\n/home/dev::/home/dev\n"+socketDir+"::/run/user/1000/cenci\n",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open ch (running): %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	if _, ok := findLineWithPrefix(lines, "run --name "); ok {
		t.Errorf("expected no container create when already running, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if !anyLineContains(lines, "exec -u dev claude-cenci-default test -e /tmp/cenci-ready") {
		t.Errorf("expected the detached-label readiness gate, got calls:\n%s", strings.Join(lines, "\n"))
	}
	attachLine(t, lines)
	if strings.Contains(string(output), "created without cenci wiring") {
		t.Errorf("wired container must not warn, got:\n%s", output)
	}
	if strings.Contains(string(output), "stale cenci socket mount") {
		t.Errorf("a matching-source cenci socket mount must not trigger the stale-mount warning, got:\n%s", output)
	}
}

// TestOpen_RunningContainer_StaleAgentVolume_SkipsRefresh pins ticket #710's
// attach skip: EnsureAgentVolume's TTL staleness branch must be skipped
// entirely (no updater run) when the scoped container is already running —
// an attach must stay instant — even though the shared agent volume itself
// is well past the TTL. The populated/bootstrap check still runs (it's not
// container-scoped: the volume is host-global).
func TestOpen_RunningContainer_StaleAgentVolume_SkipsRefresh(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	stale := time.Now().Add(-25 * time.Hour)
	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_PS=claude-cenci-default\n",
		agentStatusFakeLine("", stale, time.Time{}),
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open ch (running, stale shared agent volume): %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	if !anyLineContains(lines, "agent-cli.sh status claude") {
		t.Errorf("expected the populated/bootstrap probe to still run on the attach path, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if anyLineContains(lines, "agent-cli.sh update") {
		t.Errorf("expected a running (attach) container to skip the TTL staleness refresh even with a stale shared agent volume; calls:\n%s", strings.Join(lines, "\n"))
	}
	attachLine(t, lines)
}

// TestOpen_ContainerRunningProbeFailure_AbortsBeforeAgentVolumeBootstrap pins
// the ticket's ordering regression (watch/docs/test-strategy.md #620):
// hoisting containerRunning into Launch (between EnsureImage and
// EnsureAgentVolume) means a `ps` failure now aborts BEFORE the agent-volume
// bootstrap/refresh probe ever runs — not after it, as it did pre-#710.
// EnsureAgentVolume's very first side effect is volumeExists (`docker
// volume ls`), so its absence from the call log proves EnsureAgentVolume
// was never entered.
func TestOpen_ContainerRunningProbeFailure_AbortsBeforeAgentVolumeBootstrap(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env, "FAKE_PS_EXIT=1")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected a ps-failure runtime error, got %T %v\n%s", err, err, output)
	}

	lines := callLogLines(t, callLog)
	if anyLineContains(lines, "volume ls") {
		t.Errorf("a containerRunning (ps) failure must abort before EnsureAgentVolume's volumeExists probe ever runs (#620 ordering); calls:\n%s", strings.Join(lines, "\n"))
	}
	if anyLineContains(lines, "agent-cli.sh") {
		t.Errorf("a ps failure must abort before any agent-volume bootstrap/refresh work; calls:\n%s", strings.Join(lines, "\n"))
	}
	if anyLineContains(lines, "exec -it") {
		t.Errorf("attach must not run after a ps failure")
	}
}

func TestOpen_RunningLegacyContainerRequiresStopAndRelaunch(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch", "--name", "old")
	cmd.Env = append(env,
		"FAKE_PS=claude-cenci-old\n",
		"FAKE_AGENT_MOUNTS=/workspace|/workspace|true\n",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected legacy rejection, got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), "cenci sandbox stop claude-cenci-old") || !strings.Contains(string(output), "then relaunch") {
		t.Errorf("missing precise migration instruction:\n%s", output)
	}
	if anyLineContains(callLogLines(t, callLog), "exec -it") {
		t.Errorf("legacy container must not receive an agent session")
	}
}

func TestOpen_RunningContainerInspectFailureIsInfrastructureError(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_PS=claude-cenci-default\n",
		"FAKE_CONTAINER_INSPECT_EXIT=17",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected runtime error, got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), "inspect claude-cenci-default mounts") || strings.Contains(string(output), "predates shared") {
		t.Errorf("runtime inspection failure was misclassified:\n%s", output)
	}
	if anyLineContains(callLogLines(t, callLog), "exec -it") {
		t.Errorf("attach must not run after inspection failure")
	}
}

// -- ticket #628: reject legacy socket mounts and DinD-mode mismatches ----
//
// Before attaching to a running same-name container, planArgvs must also
// inspect its mounts/runtime/DinD env/storage mount (inspectReusePosture)
// and reject: a host Docker/Podman socket exposure (independent of DinD
// posture), a requested/derived DinD mismatch, and an ambiguous
// (unrecognized) cenci-sand.dind label -- all with the same
// "cenci sandbox stop <name>", then relaunch instruction as the sibling
// "predates shared read-only agent CLIs" rejection, and all provably never
// reaching the interactive agent exec (anyLineContains(lines, "exec -it")
// is false), per watch/docs/error-handling.md #446 (content-specific error
// assertions distinguishing each rejection class).

func TestOpen_RunningContainerWithHostSocketMount_RejectsEvenWithCompatibleAgentMount(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_PS=claude-cenci-default\n",
		// FAKE_AGENT_MOUNTS left at its default (compatible shared agent-CLI
		// mount present) -- the socket rejection must fire even when the
		// agent-CLI mount is otherwise fine.
		"FAKE_REUSE_POSTURE=off|runc|0\n/var/run/docker.sock::/var/run/docker.sock\n\n",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected the host-socket rejection to exit 1, got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), "host Docker/Podman socket") {
		t.Errorf("expected the rejection to name the host Docker/Podman socket, got:\n%s", output)
	}
	if !strings.Contains(string(output), "cenci sandbox stop claude-cenci-default") || !strings.Contains(string(output), "then relaunch") {
		t.Errorf("missing precise migration instruction:\n%s", output)
	}
	if anyLineContains(callLogLines(t, callLog), "exec -it") {
		t.Errorf("a container exposing a host socket must not receive an agent session")
	}
}

func TestOpenDind_OntoDerivedNonDindContainer_Rejects(t *testing.T) {
	repoRoot, _ := dindRepoEnv(t, false)
	containerName := "claude-cenci-" + launcher.Slugify(filepath.Base(repoRoot))

	fakeDir := t.TempDir()
	// docker-only: dind's preflight requires Docker as the outer runtime.
	callLog := writeDockerOnlyRuntime(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)
	env = append(env,
		`FAKE_INFO_RUNTIMES={"sysbox-runc":{},"runc":{}}`,
		"FAKE_PS="+containerName+"\n",
		"FAKE_REUSE_POSTURE=off|runc|0\nworkspace-vol::/workspace\n\n",
	)

	cmd := exec.Command(binaryPath, "open", "--dind")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected the --dind/non-dind-container mismatch to exit 1, got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), "without --dind") {
		t.Errorf("expected the rejection to mention the container was created without --dind, got:\n%s", output)
	}
	if !strings.Contains(string(output), "cenci sandbox stop "+containerName) || !strings.Contains(string(output), "then relaunch") {
		t.Errorf("missing precise migration instruction:\n%s", output)
	}
	if anyLineContains(callLogLines(t, callLog), "exec -it") {
		t.Errorf("a DinD-mode-mismatched container must not receive an agent session")
	}
}

func TestOpen_DefaultOntoDerivedDindContainer_Rejects(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_PS=claude-cenci-default\n",
		"FAKE_REUSE_POSTURE=on|sysbox-runc|1\nworkspace-vol::/workspace\ndind-vol::/var/lib/docker\n\n",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected the default(--no-dind)/dind-container mismatch to exit 1, got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), "with --dind") {
		t.Errorf("expected the rejection to mention the container was created with --dind, got:\n%s", output)
	}
	if !strings.Contains(string(output), "cenci sandbox stop claude-cenci-default") || !strings.Contains(string(output), "then relaunch") {
		t.Errorf("missing precise migration instruction:\n%s", output)
	}
	if anyLineContains(callLogLines(t, callLog), "exec -it") {
		t.Errorf("a DinD-mode-mismatched container must not receive an agent session")
	}
}

func TestOpen_AmbiguousDindLabel_Rejects(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_PS=claude-cenci-default\n",
		"FAKE_REUSE_POSTURE=maybe|runc|0\nworkspace-vol::/workspace\n\n",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected the ambiguous-label rejection to exit 1, got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), "ambiguous") {
		t.Errorf("expected the rejection to call the posture ambiguous (watch/docs/go-gotchas.md #598: never collapse an unrecognized enum value into the safest-looking case), got:\n%s", output)
	}
	if !strings.Contains(string(output), "cenci sandbox stop claude-cenci-default") || !strings.Contains(string(output), "then relaunch") {
		t.Errorf("missing precise migration instruction:\n%s", output)
	}
	if anyLineContains(callLogLines(t, callLog), "exec -it") {
		t.Errorf("a container with ambiguous DinD posture must not receive an agent session")
	}
}

// TestOpen_MalformedReusePostureHeader_Rejects covers the Critical
// silent-failure finding on ticket #628's parseReusePosture: a scripted
// docker fake answering the reuse-posture inspect with a header line that
// exits 0 but doesn't match the expected "<label>|<runtime>|<dindenv>"
// shape (fewer than 3 "|"-delimited fields) must reject the reuse attempt
// -- fail closed -- rather than silently defaulting to the zero-value
// reusePosture{}, which derives to the fully permissive "no host socket,
// dindOff" outcome (indistinguishable from a legitimately compatible
// legacy container).
func TestOpen_MalformedReusePostureHeader_Rejects(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_PS=claude-cenci-default\n",
		"FAKE_REUSE_POSTURE=garbage-no-pipes\n\n",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected the malformed reuse-posture header to exit 1, got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), "reuse posture") {
		t.Errorf("expected the rejection to mention the reuse posture parse failure, got:\n%s", output)
	}
	if anyLineContains(callLogLines(t, callLog), "exec -it") {
		t.Errorf("a container with malformed reuse-posture inspect output must not receive an agent session")
	}
}

// TestOpen_MalformedReusePostureMountLine_Rejects covers the second gap
// this finding calls out: a mount line that fails to split on "::" must be
// treated as a parse failure (fail closed), not silently dropped -- the
// dropped line could be exactly the host-socket bind or DinD storage mount
// the security checks depend on. The malformed line is deliberately followed
// by the trailing blank line real `docker inspect` always appends (ticket
// #684), pinning that the trailing-blank-line tolerance only trims a
// genuinely-trailing empty line and never masks a malformed non-trailing one.
func TestOpen_MalformedReusePostureMountLine_Rejects(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_PS=claude-cenci-default\n",
		"FAKE_REUSE_POSTURE=off|runc|0\nno-delimiter-here\n\n",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("expected the malformed reuse-posture mount line to exit 1, got %T %v\n%s", err, err, output)
	}
	if !strings.Contains(string(output), "reuse posture") {
		t.Errorf("expected the rejection to mention the reuse posture parse failure, got:\n%s", output)
	}
	if anyLineContains(callLogLines(t, callLog), "exec -it") {
		t.Errorf("a container with an unparseable mount line must not receive an agent session")
	}
}

func TestOpen_CompatibleNormalContainer_ReusesSuccessfully(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_PS=claude-cenci-default\n",
		"FAKE_REUSE_POSTURE=off|runc|0\nworkspace-vol::/workspace\n\n",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open ch (compatible normal reuse): %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	if _, ok := findLineWithPrefix(lines, "run --name "); ok {
		t.Errorf("expected no container create when reusing a compatible normal container, got calls:\n%s", strings.Join(lines, "\n"))
	}
	attachLine(t, lines)
}

func TestOpenDind_CompatibleDindContainer_ReusesSuccessfully(t *testing.T) {
	repoRoot, _ := dindRepoEnv(t, false)
	containerName := "claude-cenci-" + launcher.Slugify(filepath.Base(repoRoot))

	fakeDir := t.TempDir()
	// docker-only: dind's preflight requires Docker as the outer runtime.
	callLog := writeDockerOnlyRuntime(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)
	env = append(env,
		`FAKE_INFO_RUNTIMES={"sysbox-runc":{},"runc":{}}`,
		"FAKE_PS="+containerName+"\n",
		"FAKE_REUSE_POSTURE=on|sysbox-runc|1\nworkspace-vol::/workspace\ndind-vol::/var/lib/docker\n\n",
	)

	cmd := exec.Command(binaryPath, "open", "--dind")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open --dind (compatible dind reuse): %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	if _, ok := findLineWithPrefix(lines, "run --name "); ok {
		t.Errorf("expected no container create when reusing a compatible DinD container, got calls:\n%s", strings.Join(lines, "\n"))
	}
	attachLine(t, lines)
}

// -- ticket #628: new containers stamp an authoritative DinD-mode label ----

func TestOpen_FreshCreate_StampsDindLabelOff(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open ch: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	runLine, ok := findLineWithPrefix(lines, "run --name ")
	if !ok {
		t.Fatalf("expected a container run, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(runLine, "--label cenci-sand.dind=off") {
		t.Errorf("expected the new container to be stamped --label cenci-sand.dind=off, got:\n%s", runLine)
	}
}

func TestOpenDind_FreshCreate_StampsDindLabelOn(t *testing.T) {
	repoRoot, _ := dindRepoEnv(t, true)

	fakeDir := t.TempDir()
	// docker-only: dind's preflight requires Docker as the outer runtime.
	callLog := writeDockerOnlyRuntime(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)
	env = append(env, `FAKE_INFO_RUNTIMES={"sysbox-runc":{},"runc":{}}`)

	cmd := exec.Command(binaryPath, "open")
	cmd.Env = env
	cmd.Dir = repoRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open (dind via repo config): %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	runLine, ok := findLineWithPrefix(lines, "run --name ")
	if !ok {
		t.Fatalf("expected a container run, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if !strings.Contains(runLine, "--label cenci-sand.dind=on") {
		t.Errorf("expected the new dind container to be stamped --label cenci-sand.dind=on, got:\n%s", runLine)
	}
}

func TestOpen_AttachToUnwiredRunning_Warns(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_PS=claude-cenci-default\n",
		"FAKE_INSPECT_LABEL=detached",
		"FAKE_INSPECT_MOUNTS=/workspace::/workspace\n/home/dev::/home/dev\n", // no cenci socket mount
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open ch (unwired): %v\n%s", err, output)
	}

	if !strings.Contains(string(output), "was created without cenci wiring") {
		t.Errorf("expected the #195 unwired warning, got:\n%s", output)
	}
	attachLine(t, callLogLines(t, callLog))
}

// TestOpen_AttachToRunning_StaleSocketMount_Warns pins the new (#1143)
// stale-mount check: a reused container whose cenci socket mount's
// DESTINATION matches (so the existing #195 "created without cenci wiring"
// check sees it as wired) but whose SOURCE is the old host socket path (the
// pre-#1146 default, coincidentally identical to the in-container
// destination) — while CENCI_SOCKET_DIR now resolves the host socket dir to
// a different path (openTestEnv's fresh t.TempDir()) — must still warn, but
// with wording distinct from the "created without cenci wiring" line (that
// literal is asserted for the missing-mount case above and must not appear
// here), naming stop-and-relaunch as the remedy.
func TestOpen_AttachToRunning_StaleSocketMount_Warns(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_PS=claude-cenci-default\n",
		"FAKE_INSPECT_LABEL=detached",
		// Source is the pre-#1146 default host socket path, which
		// coincidentally is the same string as the in-container destination;
		// the currently-resolved host socket dir (CENCI_SOCKET_DIR, set by
		// openTestEnv to a fresh t.TempDir()) is guaranteed to differ.
		"FAKE_INSPECT_MOUNTS=/workspace::/workspace\n/home/dev::/home/dev\n/run/user/1000/cenci::/run/user/1000/cenci\n",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open ch (stale socket mount): %v\n%s", err, output)
	}

	out := string(output)
	if strings.Contains(out, "was created without cenci wiring") {
		t.Errorf("a stale-source mismatch must use wording distinct from the missing-mount warning, got:\n%s", out)
	}
	if !strings.Contains(out, "stale cenci socket mount") {
		t.Errorf("expected a stale cenci socket mount warning, got:\n%s", out)
	}
	if !strings.Contains(out, "stop claude-cenci-default") {
		t.Errorf("expected the warning to name the 'stop' remedy, got:\n%s", out)
	}
	if !strings.Contains(out, "relaunch") {
		t.Errorf("expected the warning to name relaunching as part of the remedy, got:\n%s", out)
	}
	attachLine(t, callLogLines(t, callLog))
}

func TestOpen_NoEventsSocket_LaunchesUnwiredWithWarning(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	// Point the socket dir at a fresh dir with no live socket. CENCI_SANDBOX=1
	// keeps daemon.EnsureRunning inert (the launcher reads it nowhere else),
	// so the subprocess never spawns a real daemon; the 3s socket poll then
	// expires.
	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"CENCI_SOCKET_DIR="+t.TempDir(),
		"CENCI_SANDBOX=1",
	)
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("open ch (no socket): %v\n%s", err, output)
	}

	if !strings.Contains(string(output), "events socket is unavailable") {
		t.Errorf("expected the unavailable-socket warning, got:\n%s", output)
	}
	lines := callLogLines(t, callLog)
	runLine, ok := findLineWithPrefix(lines, "run --name ")
	if !ok {
		t.Fatalf("expected the launch to proceed unwired, got calls:\n%s", strings.Join(lines, "\n"))
	}
	if strings.Contains(runLine, ":/usr/local/bin/cenci:ro") || strings.Contains(runLine, ":/run/user/1000/cenci:ro") {
		t.Errorf("expected no cenci mounts without a live events socket, got:\n%s", runLine)
	}
}

func TestOpen_AttachExitCodePropagates(t *testing.T) {
	fakeDir := t.TempDir()
	writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env, "FAKE_ATTACH_EXIT=7")
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v\n%s", err, err, output)
	}
	if exitErr.ExitCode() != 7 {
		t.Errorf("exit code = %d, want the attach's own 7 (syscall.Exec propagation)\n%s", exitErr.ExitCode(), output)
	}
}

func TestOpenShortcutConflictsWithAgentFlag_Exits2NoRuntimeCalls(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "ch", "--agent", "codex")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v\n%s", err, err, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2\n%s", exitErr.ExitCode(), output)
	}
	if lines := callLogLines(t, callLog); len(lines) > 0 {
		t.Errorf("expected no runtime calls on a shortcut/--agent conflict, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestOpenUnknownFlag_Exits2NoRuntimeCalls(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "--bogus")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v\n%s", err, err, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2\n%s", exitErr.ExitCode(), output)
	}
	if lines := callLogLines(t, callLog); len(lines) > 0 {
		t.Errorf("expected no runtime calls for an unknown flag, got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestOpenUnrecognizedLeadingPositional_Exits2NoRuntimeCalls(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(binaryPath, "open", "not-a-shortcut")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v\n%s", err, err, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2\n%s", exitErr.ExitCode(), output)
	}
	if lines := callLogLines(t, callLog); len(lines) > 0 {
		t.Errorf("expected no runtime calls for an unrecognized positional, got:\n%s", strings.Join(lines, "\n"))
	}
}

// -- argv[0] == "cn" dispatch ------------------------------------------

// buildArgv0Alias copies the built cenci binary to <dir>/<name> so
// filepath.Base(os.Args[0]) == name inside the copy ("cn" routes to open,
// "cenci-sand" hits the tombstone).
func buildArgv0Alias(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}
	aliasPath := filepath.Join(dir, name)
	if err := os.WriteFile(aliasPath, data, 0o755); err != nil {
		t.Fatalf("write %s alias: %v", name, err)
	}
	return aliasPath
}

func TestCnArgv0_RoutesToOpen(t *testing.T) {
	binDir := t.TempDir()
	cnPath := buildArgv0Alias(t, binDir, "cn")

	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, home, _ := openTestEnv(t, fakeDir, assets)
	if err := os.MkdirAll(filepath.Join(home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(cnPath, "xs")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cn xs: %v\n%s", err, output)
	}

	line := attachLine(t, callLogLines(t, callLog))
	if !strings.HasSuffix(line, "/opt/cenci-agent/current/node_modules/.bin/codex --dangerously-bypass-approvals-and-sandbox --dangerously-bypass-hook-trust --model gpt-5.6-sol") {
		t.Errorf("attach argv = %q, want the codex/sol shortcut resolution", line)
	}
}

func TestCnArgv0_BareInvocationDoesNotErrorLikeCenci(t *testing.T) {
	// A bare `cenci` (no subcommand) exits 2. `cn` with no args is a
	// bare `open` with no shortcut/flags -- valid, not an error.
	binDir := t.TempDir()
	cnPath := buildArgv0Alias(t, binDir, "cn")

	fakeDir := t.TempDir()
	writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(cnPath)
	cmd.Env = env
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("cn (bare): %v\n%s", err, output)
	}
}

// -- argv[0] == "cenci-sand" tombstone ----------------------------------

func TestCenciSandArgv0_TombstoneExits2WithMigrationMap(t *testing.T) {
	// install.sh repoints a stale ~/.local/bin/cenci-sand symlink at the
	// cenci binary; invoking it must fail loudly with the migration map
	// instead of guessing at the removed bash launcher's grammar.
	binDir := t.TempDir()
	sandPath := buildArgv0Alias(t, binDir, "cenci-sand")

	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, _, _ := openTestEnv(t, fakeDir, assets)

	cmd := exec.Command(sandPath, "--build")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	output, err := cmd.CombinedOutput()

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected *exec.ExitError, got %T: %v\n%s", err, err, output)
	}
	if exitErr.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2\n%s", exitErr.ExitCode(), output)
	}
	out := string(output)
	if !strings.Contains(out, "cenci open") || !strings.Contains(out, "cenci sandbox") {
		t.Errorf("expected the migration map to mention cenci open and cenci sandbox, got:\n%s", out)
	}
	if lines := callLogLines(t, callLog); len(lines) > 0 {
		t.Errorf("expected no runtime calls from the tombstone, got: %v", lines)
	}
}

// -- ticket #1087: CENCI_ATTENDED reaches the container ---------------------

// TestOpenClaude_ForwardsResolvedAttendedFlag is the end-to-end half of
// #1087: the unit tests pin assembleExecEnv's resolution, this one proves the
// resolved value actually survives the whole `cenci open` path into the
// attach argv, reading the fleet config from its real resolved location
// rather than an injected path. Every host state is covered in one table so
// the "always explicit, never unset" contract is visible as a set: absent
// must stay meaningful as its own third state for the consuming skill, so an
// off, missing, or broken host flag has to say "0" rather than omit the
// variable — and a broken fleet config must never fail the launch.
func TestOpenClaude_ForwardsResolvedAttendedFlag(t *testing.T) {
	cases := []struct {
		name        string
		fleetConfig string
		want        string
	}{
		{name: "attended on", fleetConfig: `{"planning": {"attended": true}}`, want: "CENCI_ATTENDED=1"},
		{name: "attended off", fleetConfig: `{"planning": {"attended": false}}`, want: "CENCI_ATTENDED=0"},
		{name: "no fleet config", fleetConfig: "", want: "CENCI_ATTENDED=0"},
		{name: "malformed fleet config", fleetConfig: `{"planning": `, want: "CENCI_ATTENDED=0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeDir := t.TempDir()
			callLog := writeScriptedRuntimes(t, fakeDir)
			assets := writeAssetFixture(t)
			env, home, _ := openTestEnv(t, fakeDir, assets)

			if tc.fleetConfig != "" {
				writeFleetConfigFixture(t, home, tc.fleetConfig)
			}

			cmd := exec.Command(binaryPath, "open", "ch")
			cmd.Env = env
			cmd.Dir = t.TempDir()
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("open ch: %v\n%s", err, output)
			}

			line := attachLine(t, callLogLines(t, callLog))
			if !strings.Contains(line, "-e "+tc.want) {
				t.Errorf("attach exec args missing %q; got:\n%s", "-e "+tc.want, line)
			}
		})
	}
}

// TestOpenClaude_PinnedAttendedEnvOverridesFleetConfig proves the
// dispatch-pin precedence end to end: `cenci dispatch` spawns its window with
// CENCI_ATTENDED=0 already in the environment (run.Opts.Unattended), and that
// pin must survive `cenci open` even when the host fleet config says
// attended. A dispatched session that reached an interactive question inside
// a detached tmux window would wait there forever with its ticket on Working.
func TestOpenClaude_PinnedAttendedEnvOverridesFleetConfig(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, home, _ := openTestEnv(t, fakeDir, assets)
	writeFleetConfigFixture(t, home, `{"planning": {"attended": true}}`)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env, "CENCI_ATTENDED=0")
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open ch: %v\n%s", err, output)
	}

	line := attachLine(t, callLogLines(t, callLog))
	if !strings.Contains(line, "-e CENCI_ATTENDED=0") {
		t.Errorf("attach exec args must honor the pinned CENCI_ATTENDED=0 over the host fleet config; got:\n%s", line)
	}
	if strings.Contains(line, "CENCI_ATTENDED=1") {
		t.Errorf("attach exec args must never forward CENCI_ATTENDED=1 for a pinned-unattended launch; got:\n%s", line)
	}
}

// writeFleetConfigFixture writes body to the fleet config path openTestEnv's
// pinned XDG_CONFIG_HOME resolves to, so a black-box run reads it exactly the
// way the launcher resolves the real one.
func writeFleetConfigFixture(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "cenci")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir fleet config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write fleet config: %v", err)
	}
}

// TestOpenAttachToRunning_ForwardsCurrentAttendedFlag covers the AC that
// toggling planning.attended on the host and running `cenci open` again
// against an ALREADY-RUNNING container yields the new value with no container
// recreation. This is the whole reason the flag rides at exec time rather
// than container-create time: the sandbox container is long-lived and every
// later launch only execs into it, so a create-time forward would freeze the
// posture the container happened to be created with.
func TestOpenAttachToRunning_ForwardsCurrentAttendedFlag(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, home, socketDir := openTestEnv(t, fakeDir, assets)
	writeFleetConfigFixture(t, home, `{"planning": {"attended": true}}`)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = append(env,
		"FAKE_PS=claude-cenci-default\n",
		"FAKE_INSPECT_LABEL=detached",
		"FAKE_INSPECT_MOUNTS=/workspace::/workspace\n/home/dev::/home/dev\n"+socketDir+"::/run/user/1000/cenci\n",
	)
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open ch (running): %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	if _, ok := findLineWithPrefix(lines, "run --name "); ok {
		t.Fatalf("expected no container create when already running, got calls:\n%s", strings.Join(lines, "\n"))
	}
	line := attachLine(t, lines)
	if !strings.Contains(line, "-e CENCI_ATTENDED=1") {
		t.Errorf("attach into a running container must forward the CURRENT host attended flag; got:\n%s", line)
	}
}

// TestOpenClaude_AttendedFlagAddsNoFleetConfigMount covers the AC that the
// forward introduces no new bind mount of ~/.config/cenci or any file under
// it. The fleet config is read on the host and forwarded as a value precisely
// so the container never sees the file: a file-level bind would pin the
// pre-toggle inode (the config is written temp-file-then-rename), and a
// directory bind would expose the whole fleet config — repos list included —
// to every sandbox.
func TestOpenClaude_AttendedFlagAddsNoFleetConfigMount(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := writeScriptedRuntimes(t, fakeDir)
	assets := writeAssetFixture(t)
	env, home, _ := openTestEnv(t, fakeDir, assets)
	writeFleetConfigFixture(t, home, `{"planning": {"attended": true}}`)

	cmd := exec.Command(binaryPath, "open", "ch")
	cmd.Env = env
	cmd.Dir = t.TempDir()
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("open ch: %v\n%s", err, output)
	}

	lines := callLogLines(t, callLog)
	runLine, ok := findLineWithPrefix(lines, "run --name ")
	if !ok {
		t.Fatalf("expected a container run, got calls:\n%s", strings.Join(lines, "\n"))
	}
	for _, forbidden := range []string{
		filepath.Join(home, ".config", "cenci"),
		".config/cenci",
	} {
		if strings.Contains(runLine, forbidden) {
			t.Errorf("create-time run args must never mount the host fleet config (%q); got:\n%s", forbidden, runLine)
		}
	}
}
