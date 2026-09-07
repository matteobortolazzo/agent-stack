package launcher

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matteobortolazzo/cenci/watch/v2/internal/exectest"
)

// writeFakeRuntime writes a fake docker/podman to dir that appends each
// invocation's argv (space-joined) as a line to callLog and answers the
// read-only listing verbs from env vars, so tests script responses without a
// real container runtime:
//
//	FAKE_IMAGES        → `images ...` stdout, optionally filtered by a
//	                     trailing positional repository argument (e.g.
//	                     `images --format {{.Repository}}:{{.Tag}}
//	                     cenci-sandbox-base` returns only the FAKE_IMAGES
//	                     lines whose repository is "cenci-sandbox-base");
//	                     with no positional, every FAKE_IMAGES line is
//	                     returned unfiltered.
//	FAKE_PS            → `ps ...` stdout (any form)
//	FAKE_PS_EXIT       → `ps ...` exit code (default 0); set nonzero to
//	                     simulate one runtime's container-listing failure
//	                     (the dual-runtime one-runtime-fails cases, #629) or a
//	                     daemon-unreachable/runtime-invocation failure for
//	                     containerRunning (ticket #627's Audit dispatch must
//	                     treat this as inconclusive — inspectWarning +
//	                     basis:"planned" — never as "not running").
//	FAKE_VOLUMES       → `volume ls ...` stdout
//	FAKE_VOLUME_LS_EXIT → `volume ls ...` exit code (default 0); set
//	                     nonzero to simulate one runtime's volume-listing
//	                     failure (#629)
//	FAKE_INFO_RUNTIMES → `info --format ...` stdout (the sysbox-runc
//	                     registration probe, sandbox.SysboxRegistered);
//	                     defaults to "{}" (no runtimes registered), mirroring
//	                     sandbox_open_test.go's writeScriptedRuntime.
//	FAKE_INFO_EXIT     → `info ...` exit code (default 0); set nonzero to
//	                     simulate a `docker info` failure (daemon down),
//	                     distinct from FAKE_INFO_RUNTIMES's stdout-shape
//	                     control — note the asymmetric naming: `fv
//	                     INFO_RUNTIMES` controls stdout, `fe INFO` (→
//	                     FAKE_INFO_EXIT, not FAKE_INFO_RUNTIMES_EXIT) controls
//	                     the exit code. Must stay byte-parallel with
//	                     sandbox_open_test.go's writeScriptedRuntime (#493
//	                     keep-in-sync note). writeAuditFakeRuntime
//	                     (audit_security_faketest_test.go) deliberately does
//	                     NOT get this hook: it has no `info` arm at all, and
//	                     `Audit` never reaches `dindPreflight`.
//	FAKE_INSPECT_MOUNTS → `inspect --format ...` stdout for the ".RW" mounts
//	                     format containerHasSharedAgentMount uses (ticket
//	                     #620's read-only running-container disposition
//	                     probe); defaults to "" (no mounts -> incompatible).
//	                     Named to mirror sandbox_open_test.go's
//	                     writeScriptedRuntime FAKE_INSPECT_MOUNTS var (#493
//	                     keep-in-sync note), though that fake's var answers a
//	                     different inspect format (the "<source>::<destination>"
//	                     per-mount shape warnIfUnwired uses to compare a reused
//	                     container's cenci socket-mount source against the
//	                     currently-resolved host socket dir, ticket #1143)
//	                     since this package's DryRun only ever needs the ".RW"
//	                     shared-agent-mount check.
//	FAKE_REUSE_POSTURE → `inspect --format ...` stdout for the combined
//	                     reuse-posture probe (ticket #628's
//	                     inspectReusePosture: the `cenci-sand.dind` label,
//	                     HostConfig.Runtime, and mount source/destination
//	                     pairs), told apart by the `cenci-sand.dind`
//	                     format-string token. Line 1 is
//	                     "<label>|<runtime>|<dindenv 0-or-1>"; remaining
//	                     lines are "<source>::<destination>" per mount.
//	                     Defaults to an empty label, non-sysbox runtime, no
//	                     dind env, and a workspace-only mount (derives
//	                     dindOff, no host socket) so existing reuse tests
//	                     that don't care about posture keep passing; must
//	                     stay byte-parallel with sandbox_open_test.go's
//	                     writeScriptedRuntime FAKE_REUSE_POSTURE var (#493
//	                     keep-in-sync note).
//	FAKE_INSPECT_STATE  → `inspect --format ...` stdout for the
//	                     "{{.State.Status}} {{.State.ExitCode}}" format
//	                     containerStartupState uses (Diagnose's
//	                     container-status probe, #630); defaults to
//	                     "running 0", mirroring sandbox_open_test.go's
//	                     writeScriptedRuntime default.
//	FAKE_VOLUME_INSPECT_EXIT → `volume inspect <name>` exit code (default 0 =
//	                     the volume exists); Diagnose's dind-session probe
//	                     (#630) treats a non-zero exit as "not a dind
//	                     session" (scope.DindVolumeName was never created).
//	FAKE_DOCKERD_MARKER → content returned by the short-lived
//	                     `run --entrypoint /bin/cat ... .cenci-dockerd-
//	                     startup-error` home-volume read (#630's dind
//	                     failure marker); unset/empty simulates "no failure
//	                     recorded". FAKE_DOCKERD_MARKER_EXIT (default 0) is
//	                     that same read's exit code.
//	FAKE_EVENTS_MARKER  → content returned by the short-lived
//	                     `run --entrypoint /bin/cat ... .cenci-events-
//	                     undelivered` home-volume read (#1122's dropped-
//	                     event marker, written in-sandbox by
//	                     internal/daemon and surfaced by diagnose.go's
//	                     "Event delivery:" section); unset/empty simulates
//	                     "no failure recorded". FAKE_EVENTS_MARKER_EXIT
//	                     (default 0) is that same read's exit code.
//	FAKE_OBSERVED_POSTURE → `inspect --format ...` stdout for ticket #627's
//	                     combined observed-inspect probe (Audit's
//	                     running-container derivation: image reference,
//	                     network mode, OCI runtime, the `cenci-sand.dind`
//	                     label, dind env, and every mount's source/
//	                     destination/read-only state), told apart by the
//	                     distinctive `.HostConfig.NetworkMode` format-string
//	                     token (checked before the `cenci-sand.dind` and
//	                     `.RW` arms above, whose format strings do not
//	                     contain this token). Line 1 is
//	                     "<image>|<networkMode>|<runtime>|<dindLabel>|
//	                     <dindenv 0-or-1>"; remaining lines are
//	                     "<source>::<destination>::<rw true-or-false>" per
//	                     mount. Defaults to an unweakened bridge-mode,
//	                     no-mounts, dind-off header so existing tests that
//	                     don't care about observed content still parse
//	                     cleanly. Per the plan's #493 keep-in-sync note:
//	                     sandbox_open_test.go's writeScriptedRuntime does
//	                     NOT need this format — it never exercises Audit's
//	                     observed path (only the launch/dry-run paths,
//	                     which reuse FAKE_REUSE_POSTURE/FAKE_INSPECT_MOUNTS
//	                     instead). watch/audit_security_faketest_test.go
//	                     (package main_test) carries an independent,
//	                     byte-parallel copy of this same token/format for
//	                     the black-box `cenci audit`/`security explain`
//	                     subprocess tests, since that package cannot import
//	                     this unexported test helper.
//	FAKE_OBSERVED_POSTURE_EXIT → combined observed-inspect probe exit code
//	                     (default 0); nonzero simulates an inspect failure
//	                     on an otherwise-running container.
//	FAKE_IMAGE_ID       — `image inspect --format '{{.Id}}' <image>` stdout
//	                     (ticket #947's printStaleContainerNotice: the
//	                     freshly built image's ID), told apart by the
//	                     distinctive `{{.Id}}` format-string token. Defaults
//	                     to "sha256:fresh".
//	FAKE_IMAGE_ID_EXIT  — that probe's exit code (default 0); nonzero
//	                     simulates the image-ID probe itself failing.
//	FAKE_CONTAINER_IMAGES — space-separated `name=<ref>|<id>` pairs
//	                     answering the combined per-container probe
//	                     `inspect --format '{{.Config.Image}}|{{.Image}}'
//	                     <name>` (ticket #947), told apart by the
//	                     distinctive `{{.Image}}` format-string token
//	                     (checked alongside the `cenci-sand.dind` and `.RW`
//	                     arms below; the format string also contains
//	                     `{{.Config.Image}}`, which does not collide with
//	                     any existing token). The fake looks up the
//	                     requested container's name and emits `${pair#*=}`
//	                     verbatim as the probe's stdout; a container absent
//	                     from the list defaults to
//	                     "cenci-sandbox:latest|<FAKE_IMAGE_ID default>".
//	                     Must stay byte-parallel with engine_test.go's
//	                     buildEngine and sandbox_open_test.go's
//	                     writeScriptedRuntime (#493 keep-in-sync note).
//	FAKE_INSPECT_IMAGE_EXIT — the per-container combined probe's exit code
//	                     (default 0); nonzero simulates that probe failing.
//
// Every FAKE_<VERB>[_EXIT] var above also has a FAKE_<VERB>_DOCKER/
// FAKE_<VERB>_PODMAN (or FAKE_<VERB>_EXIT_DOCKER/FAKE_<VERB>_EXIT_PODMAN)
// override, resolved from the invoking binary's own name (argv[0]'s
// basename) and falling back to the plain FAKE_<VERB>[_EXIT] var when the
// runtime-suffixed one isn't set. This is needed because dual-runtime tests
// put both a docker fake and a podman fake on PATH simultaneously — one
// shared process environment — so a single FAKE_PS could never script
// docker and podman to report different containers in the same test run
// (ticket #629's dual-runtime host-wide command coverage). A var "set" to
// the empty string counts as set (distinct from unset), so a test can
// deliberately script one runtime's listing as empty while the other
// runtime's plain (non-suffixed) fallback still answers something.
//
// Plain /bin/sh (not env) so it resolves under a minimal overridden PATH.
func writeFakeRuntime(t *testing.T, dir, name, callLog string) {
	t.Helper()
	// PATH is overridden to only the fake-runtime dir for these tests, so
	// the images filter below (and the runtime-name lookup, which
	// deliberately uses the shell builtin "${0##*/}" rather than the
	// external `basename` command) must stay shell-builtin-only (case/for,
	// no external commands) or it silently fails with "command not found"
	// once the real PATH is gone.
	body := `#!/bin/sh
printf '%s\n' "$*" >> ` + exectest.ShellQuote(callLog) + `
rtname=${0##*/}
rt=""
case "$rtname" in
docker) rt=DOCKER ;;
podman) rt=PODMAN ;;
esac
# fv NAME DEFAULT prints FAKE_<NAME>_<rt> when that exact var is set (even to
# an empty string), else FAKE_<NAME> when set, else DEFAULT — see the doc
# comment above for why (#629 dual-runtime test-fake keying). Must stay
# byte-parallel with sandbox_open_test.go's writeScriptedRuntime fv/fe/fset.
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
# fe NAME is fv's exit-code counterpart: FAKE_<NAME>_EXIT_<rt> falling back
# to FAKE_<NAME>_EXIT, defaulting to 0.
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
# fvb is fv's %b-format counterpart (interprets backslash escapes in the
# resolved value, e.g. a literal "\n" default). fv/fvb are called directly as
# statements (never wrapped in "$(...)") wherever the resolved value is the
# entire printed output: command substitution unconditionally strips every
# trailing newline, which would silently corrupt a verbatim multi-line value
# (see #629's dual-runtime keying commit — this bit an early draft of this
# fake on FAKE_PS/FAKE_VOLUMES/FAKE_IMAGES output).
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
images)
  if [ -n "$4" ]; then
    result=""
    IFS='
'
    for line in $(fv IMAGES ""); do
      case "$line" in
        "$4":*) result="${result}${line}
" ;;
      esac
    done
    printf '%s' "$result"
  else
    fv IMAGES ""
  fi
  ;;
image)
  if [ "$2" = inspect ]; then
    case "$*" in
    *'{{.Id}}'*) fv IMAGE_ID "sha256:fresh"; exit "$(fe IMAGE_ID)" ;;
    esac
  fi
  ;;
ps) fv PS ""; exit "$(fe PS)" ;;
volume)
  case "$2" in
  ls) fv VOLUMES ""; exit "$(fe VOLUME_LS)" ;;
  inspect) exit "$(fe VOLUME_INSPECT)" ;;
  esac
  ;;
info) fv INFO_RUNTIMES "{}"; exit "$(fe INFO)" ;;
run)
  case "$*" in
  *'/bin/cat'*)
    case "$*" in
    *'.cenci-dockerd-startup-error'*) fv DOCKERD_MARKER ""; exit "$(fe DOCKERD_MARKER)" ;;
    *'.cenci-events-undelivered'*) fv EVENTS_MARKER ""; exit "$(fe EVENTS_MARKER)" ;;
    esac
    ;;
  esac
  ;;
inspect)
  case "$*" in
  *'.HostConfig.NetworkMode'*) fvb OBSERVED_POSTURE "cenci-sandbox:latest|bridge|runc||\n\n"; exit "$(fe OBSERVED_POSTURE)" ;;
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
  *'cenci-sand.dind'*) fvb REUSE_POSTURE "|runc|0\nworkspace-vol::/workspace\n\n" ;;
  *State.Status*) fv INSPECT_STATE "running 0"; printf '\n' ;;
  *'.RW'*) fvb INSPECT_MOUNTS "" ;;
  esac
  ;;
esac
exit 0
`
	exectest.WriteExecutable(t, filepath.Join(dir, name), body)
}

// setFakeDockerNotRunning puts a fake "docker" (FAKE_PS left unset, so
// containerRunning reports nothing running) first on PATH, so tests that
// don't care about container disposition can call DryRun (which now always
// performs the read-only containerRunning probe via planArgvs, ticket #620)
// without depending on a real container runtime being reachable on the host
// running the test suite.
func setFakeDockerNotRunning(t *testing.T) {
	t.Helper()
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))
}

// auditEngineWithFakeRuntime returns an Engine with Runtime set to an
// absolute path pointing at a fake docker (writeFakeRuntime) — for ticket
// #627's Audit observed-mode dispatch (e.Runtime != ""), so tests can script
// ps/inspect responses via t.Setenv(FAKE_PS/FAKE_OBSERVED_POSTURE/...)
// without a real container runtime and without perturbing the test
// process's PATH (the absolute Runtime path bypasses PATH lookup entirely,
// unlike setFakeDockerNotRunning's PATH-prepend approach). Returns the
// Engine and the fake's call-log path for no-mutation assertions.
func auditEngineWithFakeRuntime(t *testing.T) (*Engine, string) {
	t.Helper()
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	return &Engine{Runtime: filepath.Join(fakeDir, "docker"), Stderr: io.Discard}, callLog
}

// readCallLog returns the fake runtime's call log lines.
func readCallLog(t *testing.T, path string) []string {
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

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func containsPrefix(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// containsLineWithAll reports whether some line contains every one of subs.
func containsLineWithAll(lines []string, subs ...string) bool {
	for _, l := range lines {
		all := true
		for _, s := range subs {
			if !strings.Contains(l, s) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}
