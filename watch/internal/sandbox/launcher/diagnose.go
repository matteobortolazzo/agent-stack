package launcher

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/matteobortolazzo/cenci/watch/v2/internal/daemon"
	"github.com/matteobortolazzo/cenci/watch/v2/internal/errcode"
	"github.com/matteobortolazzo/cenci/watch/v2/internal/ipc"
)

// Severity classifies how urgently a diagnose finding needs attention.
// String values are deliberately lowercase — they're printed verbatim in the
// human-readable report.
type Severity string

const (
	// SeverityFatal marks a finding that means the session cannot work at
	// all: the container exited/dead/not-found, or an agent-CLI/entrypoint
	// startup failure (START-001/START-002).
	SeverityFatal Severity = "fatal"
	// SeverityDegraded marks a finding that means the session still works but
	// with reduced functionality: a readiness-poll timeout, or the daemon/
	// event-socket being unreachable (sessions won't report to host status
	// bars).
	SeverityDegraded Severity = "degraded"
	// SeverityWarning marks a purely informational finding: an image or
	// plugin-manifest version that couldn't be determined.
	SeverityWarning Severity = "warning"
)

// finding is one reportable diagnose observation: a human-readable message,
// optionally annotated with a registered errcode.Code (empty for findings —
// like an unknown version read — that have no registered code to attach) and
// its severity tier.
type finding struct {
	Message  string
	Code     errcode.Code
	Severity Severity
}

// severityForCode maps a registered errcode.Code to its severity tier per
// the ticket's taxonomy. An unregistered or empty code (attached to no
// specific failure class — e.g. a version-read finding) defaults to the
// lowest tier, warning, rather than panicking or silently escalating.
func severityForCode(code errcode.Code) Severity {
	switch code {
	case errcode.SandboxStartAgentCLIMissing, errcode.SandboxStartGenericEntrypoint, errcode.SandboxSessionNotFound,
		errcode.SandboxDindRuntimeCreateFailed:
		return SeverityFatal
	case errcode.SandboxStartReadinessTimeout, errcode.DaemonConnUnreachable, errcode.DaemonSocketMissing, errcode.SandboxDindStartupFailure,
		errcode.SandboxSocketUnwired:
		return SeverityDegraded
	default:
		return SeverityWarning
	}
}

// indentContinuationLines indents every line after the first in s with a
// distinct "  | " gutter (#1163 security review). renderFinding's Message
// can come from container-controlled multi-line content (e.g. a boot log
// via startupFailureDetail) — printed at column 0, lines 2..N of that
// content would otherwise be visually indistinguishable inside the findings
// list from a genuine, separate "[severity] CENCI-...: ..." finding line,
// letting a malicious container forge fake findings by shaping its content.
// The gutter deliberately does NOT reuse the hint-rendering code's own
// "  - " prefix a few lines below: a plain two-space indent would let a
// continuation line starting with "- " render identically to a genuine
// recovery hint instead, just moving the forgery target rather than closing
// it. A single-line s is returned unchanged.
func indentContinuationLines(s string) string {
	return strings.ReplaceAll(s, "\n", "\n  | ")
}

// renderFinding renders f as a report block: a "[severity] message" header,
// followed by the code (if any) and its registered recovery hints — reused
// verbatim from errcode.Lookup(f.Code).Hints so the diagnose output and the
// errcode registry never drift apart. A finding with no Code (Code == "")
// prints only the severity and message; it never fabricates a code or hints
// that were never attached. A multi-line f.Message has its continuation
// lines indented (indentContinuationLines) so they read as part of this one
// finding, not as standalone report lines.
func renderFinding(f finding) string {
	var b strings.Builder
	message := indentContinuationLines(f.Message)
	if f.Code != "" {
		fmt.Fprintf(&b, "[%s] %s: %s\n", f.Severity, f.Code, message)
		if entry, ok := errcode.Lookup(f.Code); ok {
			for _, hint := range entry.Hints {
				fmt.Fprintf(&b, "  - %s\n", hint)
			}
		}
	} else {
		fmt.Fprintf(&b, "[%s] %s\n", f.Severity, message)
	}
	return b.String()
}

// versionOrUnknown is the shared "unknown" fallback a best-effort (value, ok
// bool) version read funnels through — today just imageBaseVersion (the
// plugin-manifest version has its own three-state display helper,
// pluginManifestVersionDisplay, since #1163 needs to distinguish a genuine
// read failure from a legitimately absent manifest, which a plain ok bool
// cannot represent): a failed read (ok == false) or a successful-but-empty
// read (ok == true, content == "") both surface as "unknown" rather than a
// blank string. Whitespace in a real, non-empty content value is preserved
// verbatim — the caller is responsible for trimming, not this fallback.
func versionOrUnknown(content string, ok bool) string {
	if !ok || content == "" {
		return "unknown"
	}
	return content
}

// scopeAgent recovers the agent slug ("claude"/"codex"/"opencode") from the
// scope's ContainerName prefix ("<agent>-cenci-..."). Scope carries no
// direct Agent field to read; ComputeScope builds both ContainerName and
// VolumeName from the same agent value, so parsing the prefix back out stays
// consistent with how the scope was constructed rather than requiring a
// second, easily out-of-sync argument.
func scopeAgent(scope Scope) string {
	if idx := strings.Index(scope.ContainerName, "-cenci-"); idx > 0 {
		return scope.ContainerName[:idx]
	}
	return "claude"
}

// marketplaceManifestPath returns the in-container path to the given agent's
// provisioned cenci marketplace manifest, mirroring the client-specific
// checkout locations sandbox/lib/migrate-settings.sh provisions into. Only
// claude and codex are provisioned via a marketplace.json manifest;
// opencode has no marketplace.json at all — it's provisioned via a git
// clone into a cenci-src directory (see
// sandbox/lib/migrate-settings.sh's provision_opencode_plugins) — so it
// returns "" rather than falling back to the claude path, which would
// silently read the wrong (and possibly absent) file.
func marketplaceManifestPath(agent string) string {
	switch agent {
	case "codex":
		return "/home/dev/.codex/plugins/marketplaces/cenci/.claude-plugin/marketplace.json"
	case "claude":
		return "/home/dev/.claude/plugins/marketplaces/cenci/.claude-plugin/marketplace.json"
	default:
		return ""
	}
}

// pluginManifestVersion best-effort reads agent's provisioned marketplace
// manifest from scope's home volume via the same short-lived-container
// pattern as startupFailureDetail's home-volume reads (the workload container
// may already be gone). An agent with no manifest path (opencode) reports a
// clean homeVolumeReadAbsent — there is nothing to read, so "the read
// failed" would be a wrong claim — rather than attempting a read with an
// empty path argument.
//
// readHomeVolumeFile's chokepoint sanitizes with the multi-line/newline-
// preserving variant (needed by other consumers of that chokepoint, e.g.
// startupFailureDetail's boot-log rendering), but manifest.json's content is
// rendered into Diagnose's single-line "Plugin manifest version: %s" field —
// a container-writable file containing embedded "\n"s could otherwise forge
// extra, legitimate-looking report lines. A successful read is re-sanitized
// with the single-line/head-keeping variant before it's returned, so no "\n"
// byte ever reaches that one-line field (#1163 security review).
func (e *Engine) pluginManifestVersion(scope Scope, agent string) (string, homeVolumeReadStatus) {
	path := marketplaceManifestPath(agent)
	if path == "" {
		return "", homeVolumeReadAbsent
	}
	content, status := e.readHomeVolumeFile(scope, path)
	if status == homeVolumeReadOK {
		content = sanitizeEventDeliveryMessage(content)
	}
	return content, status
}

// pluginManifestVersionDisplay renders pluginManifestVersion's three-state
// result the way Diagnose's "Plugin manifest version:" line needs (#1163):
// a successful read's content verbatim, a legitimately absent manifest (or
// opencode's clean short-circuit) as the plain "unknown", and a genuine read
// failure as the distinct "unknown (read failed)" — so an operator isn't
// told "we looked and found nothing" when the truth is "we could not look".
func pluginManifestVersionDisplay(content string, status homeVolumeReadStatus) string {
	switch status {
	case homeVolumeReadOK:
		if content == "" {
			return "unknown"
		}
		return content
	case homeVolumeReadFailed:
		return "unknown (read failed)"
	default:
		return "unknown"
	}
}

// imageBaseVersion best-effort reads image's baked cenci.base-version label.
// It reuses imageCurrent's combined agent-cli|base-version format string so a
// single `image inspect` invocation serves both call sites identically. The
// label's value comes from a per-repo .cenci/Dockerfile — the same
// untrusted-repo-content boundary as the other sanitized fields — and is
// rendered into Diagnose's single-line "Image base version: %s" field, so it
// is sanitized with the single-line/head-keeping sanitizer before it's
// returned (#1163 security review). Emptiness is decided on the raw,
// unsanitized value first, mirroring readHomeVolumeFile's own ordering, so a
// genuinely present-but-all-control-byte label can't misreport as absent.
func (e *Engine) imageBaseVersion(image string) (string, bool) {
	out, err := exec.Command(e.Runtime, "image", "inspect", "--format",
		`{{ index .Config.Labels "`+imageAgentLifecycleLabel+`" }}|{{ index .Config.Labels "`+imageBaseVersionLabel+`" }}`, image).Output()
	if err != nil {
		return "", false
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|", 2)
	if len(parts) < 2 {
		return "", false
	}
	v := parts[1]
	if v == "" {
		return "", false
	}
	return sanitizeEventDeliveryMessage(v), true
}

// containerLogsTail best-effort reads the container's last n log lines,
// sanitized with the multi-line sanitizer (#1163) — the container fully
// controls this content, and it can be many lines, so it goes through the
// same newline-preserving, tail-keeping sanitizer as the home-volume
// chokepoint rather than the single-line one.
func (e *Engine) containerLogsTail(name string, n int) (string, bool) {
	out, err := exec.Command(e.Runtime, "logs", "--tail", strconv.Itoa(n), name).CombinedOutput()
	if err != nil {
		return "", false
	}
	trimmed := sanitizeHomeVolumeContent(strings.TrimSpace(string(out)))
	return trimmed, trimmed != ""
}

// inspectMounts best-effort reads the container's mount list as
// "<host source> -> <container destination>" lines.
func (e *Engine) inspectMounts(name string) (string, bool) {
	out, err := exec.Command(e.Runtime, "inspect", "--format",
		`{{range .Mounts}}{{.Source}} -> {{.Destination}}{{"\n"}}{{end}}`, name).Output()
	if err != nil {
		return "", false
	}
	trimmed := strings.TrimSpace(string(out))
	return trimmed, trimmed != ""
}

// dindSessionState is the tri-state result of probing whether scope's dind
// storage volume was ever created (isDindSession, #1163). It replaces an
// earlier boolean probe built on `volume inspect`: podman exits 125 for
// `volume inspect` on a legitimately-absent volume — indistinguishable from
// a genuine runtime failure — which would have misclassified every non-dind
// podman session as "runtime failed" (podman is the first-detected
// runtime). `volume ls` instead returns a plain name list that this probe
// matches in Go, so a `volume ls` failure is unambiguously the runtime's own
// fault, never confused with "volume legitimately absent".
type dindSessionState int

const (
	// dindSessionAbsent means the dind volume was never created: this
	// session was never launched with --dind (or scope.DindVolumeName is
	// empty — legacy/ScopeForContainer scopes never carry one, so there is
	// nothing to probe at all).
	dindSessionAbsent dindSessionState = iota
	// dindSessionPresent means `volume ls` listed scope.DindVolumeName: a
	// genuine dind session.
	dindSessionPresent
	// dindSessionIndeterminate means the `volume ls` probe itself failed
	// (e.g. the runtime daemon is unreachable) — whether this is a dind
	// session could not be determined at all.
	dindSessionIndeterminate
)

// isDindSession probes whether scope's dind storage volume was ever created
// — the same signal a genuine --dind launch would have left behind — via the
// existing volumeExists helper (`volume ls --format {{.Name}}` + name match,
// engine.go). An empty scope.DindVolumeName short-circuits to
// dindSessionAbsent with no probe at all.
func (e *Engine) isDindSession(scope Scope) dindSessionState {
	if scope.DindVolumeName == "" {
		return dindSessionAbsent
	}
	exists, err := e.volumeExists(scope.DindVolumeName)
	if err != nil {
		return dindSessionIndeterminate
	}
	if exists {
		return dindSessionPresent
	}
	return dindSessionAbsent
}

// nestedDockerFinding implements #630's Q3: Diagnose must always print a
// "Nested Docker:" line (the package's #572 failure-visibility convention —
// never partial silence). It probes dind-session-ness via isDindSession,
// then — only for a genuine dind session — reads the dockerd-startup-error
// marker and returns the display line plus an optional finding: Degraded
// when the marker is present, or an explicit Warning (#1163) when either the
// dind-session probe itself or the marker read itself failed, so "we could
// not look" is never silently reported as "no failure recorded". A
// successful marker read is re-sanitized with the single-line/head-keeping
// sanitizer (on top of the chokepoint's multi-line one) before it's
// rendered: both the returned line and finding.Message are genuinely
// single-line report fields, and the marker is container-writable — an
// embedded "\n" could otherwise forge extra, legitimate-looking report
// lines (#1163 security review).
func (e *Engine) nestedDockerFinding(scope Scope) (line string, f *finding) {
	switch e.isDindSession(scope) {
	case dindSessionAbsent:
		return "not a dind session", nil
	case dindSessionIndeterminate:
		return "could not determine (the dind-volume probe failed)", &finding{
			Message:  "could not determine whether this is a dind session: the dind-volume probe (`volume ls`) failed",
			Severity: SeverityWarning,
		}
	}

	content, status := e.readHomeVolumeFile(scope, dockerdFailureMarkerPath)
	switch status {
	case homeVolumeReadOK:
		sanitized := sanitizeEventDeliveryMessage(content)
		return sanitized, &finding{
			Message:  sanitized,
			Code:     errcode.SandboxDindStartupFailure,
			Severity: severityForCode(errcode.SandboxDindStartupFailure),
		}
	case homeVolumeReadFailed:
		return "could not be read (the dockerd startup marker read failed)", &finding{
			Message:  "the dockerd startup marker could not be read",
			Severity: SeverityWarning,
		}
	default: // homeVolumeReadAbsent
		return "no failure recorded", nil
	}
}

// maxEventDeliveryMessageLen bounds the length of an undelivered-events
// marker's message before it is rendered — the marker crosses the
// container->host seam (an in-sandbox `cenci notify` writes it, a
// short-lived container reads it back on the host), so every field is
// validated rather than trusted verbatim (#1094).
const maxEventDeliveryMessageLen = 2048

// maxHomeVolumeContentLen bounds the length of home-volume-read content
// (startup markers, boot logs, the dockerd/events markers) before it is
// rendered (#1163) — much larger than maxEventDeliveryMessageLen since this
// content is legitimately multi-line (e.g. a 50-line boot log), unlike the
// single-line event-delivery message field.
const maxHomeVolumeContentLen = 8192

// homeVolumeTruncationMarker is prepended (followed by a newline) to
// sanitizeHomeVolumeContent's output when truncation occurs — visible so an
// operator reading the report knows the content was cut, distinct from
// content that never had more to show.
const homeVolumeTruncationMarker = "[... truncated, showing most recent content ...]\n"

// sanitizeText is the shared core sanitizeEventDeliveryMessage and
// sanitizeHomeVolumeContent both build on (#1163). It strips non-printable
// runes — unicode.IsPrint excludes not just ASCII C0 control bytes and DEL,
// but also the C1 range (U+0080-U+009F, notably U+009B CSI and U+009D OSC,
// which some terminals still act on) and Unicode bidi/format characters
// (e.g. U+202E RLO, U+200B, U+2066-U+2069) that could otherwise spoof or
// manipulate terminal output when a compromised/tampered container writes a
// crafted field — then bounds the result to maxLen bytes on a rune-safe
// boundary.
//
// allowNewlines preserves "\n"/"\t" verbatim instead of stripping them along
// with every other non-printable rune — multi-line content like a boot log
// must stay multi-line. keepTail selects both the truncation direction and
// marker placement:
//
//   - false (sanitizeEventDeliveryMessage's shape): keeps the HEAD — cut at
//     maxLen-len(marker), walk BACK via utf8.RuneStart, marker appended at
//     the end. Called with an empty marker, so behavior stays byte-identical
//     to before #1163.
//   - true (sanitizeHomeVolumeContent's shape): keeps the TAIL — cut at
//     len(out)-(maxLen-len(marker)), walk FORWARD to the next
//     utf8.RuneStart, marker PREPENDED followed by a newline. Head-keeping
//     would be wrong here: startupFailureDetail calls lastLines(content, 50)
//     AFTER this chokepoint, so head-keeping would silently return the head
//     of an already-truncated window instead of the true tail.
//
// marker's own bytes come out of the length budget in both directions, so
// the returned string's length is always <= maxLen.
func sanitizeText(s string, allowNewlines bool, maxLen int, keepTail bool, marker string) string {
	var b strings.Builder
	for _, r := range s {
		if allowNewlines && (r == '\n' || r == '\t') {
			b.WriteRune(r)
			continue
		}
		if !unicode.IsPrint(r) {
			continue
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) <= maxLen {
		return out
	}

	// budget clamps to [0, maxLen]; combined with the len(out) > maxLen
	// guard above, cut always lands within [0, len(out)] in both branches
	// below, so no further bounds clamping is needed.
	budget := maxLen - len(marker)
	if budget < 0 {
		budget = 0
	}
	if keepTail {
		cut := len(out) - budget
		for cut < len(out) && !utf8.RuneStart(out[cut]) {
			cut++
		}
		return marker + out[cut:]
	}

	cut := budget
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return out[:cut] + marker
}

// sanitizeEventDeliveryMessage strips non-printable runes and bounds the
// length of an undelivered-events marker's message (or code) field before it
// is rendered, since the marker crosses the container->host seam (#1094).
// Head-keeping, no truncation marker — unchanged behavior from before #1163
// generalized this into sanitizeText's shared core.
func sanitizeEventDeliveryMessage(s string) string {
	return sanitizeText(s, false, maxEventDeliveryMessageLen, false, "")
}

// sanitizeHomeVolumeContent sanitizes content read from a home-volume marker
// or log tail before it is rendered (#1163): preserves "\n"/"\t" (this
// content is legitimately multi-line), tail-keeping truncation with a
// visible marker at the front — see sanitizeText's doc comment for the full
// rationale.
func sanitizeHomeVolumeContent(s string) string {
	return sanitizeText(s, true, maxHomeVolumeContentLen, true, homeVolumeTruncationMarker)
}

// eventDeliveryFinding implements #1122's always-on "Event delivery:" line,
// mirroring nestedDockerFinding's shape: it reads the undelivered-events
// marker an in-sandbox `cenci notify` may have written after a hook event
// failed to reach the host daemon. Both the marker's code and message
// fields cross the container->host seam, so neither is trusted verbatim
// (#1094): an unparseable marker, or one naming a code that doesn't resolve
// via errcode.Lookup, is surfaced as an explicit SeverityWarning finding
// (never silently omitted, #572) rather than a code/severity that was never
// actually attached.
func (e *Engine) eventDeliveryFinding(scope Scope) (line string, f *finding) {
	content, status := e.readHomeVolumeFile(scope, daemon.UndeliveredEventsMarkerPath)
	switch status {
	case homeVolumeReadFailed:
		return "could not be read (the undelivered-events marker read failed)", &finding{
			Message:  "the undelivered-events marker could not be read",
			Severity: SeverityWarning,
		}
	case homeVolumeReadAbsent:
		return "no failure recorded", nil
	}
	m, err := daemon.ParseUndeliveredEventsMarker([]byte(content))
	if err != nil {
		return "an undelivered-events marker was found but is unparseable", &finding{
			Message:  fmt.Sprintf("the undelivered-events marker is present but unparseable: %v", err),
			Severity: SeverityWarning,
		}
	}
	if _, ok := errcode.Lookup(m.Code); !ok {
		code := sanitizeEventDeliveryMessage(string(m.Code))
		return "an undelivered-events marker was found but names an unrecognized code", &finding{
			Message:  fmt.Sprintf("the undelivered-events marker names an unrecognized code %q", code),
			Severity: SeverityWarning,
		}
	}
	message := sanitizeEventDeliveryMessage(m.Message)
	return message, &finding{
		Message:  message,
		Code:     m.Code,
		Severity: severityForCode(m.Code),
	}
}

// daemonDialTimeout bounds diagnose's read-only reachability probe.
const daemonDialTimeout = 200 * time.Millisecond

// daemonStatusFinding is diagnose's read-only daemon-reachability probe. It
// deliberately never calls daemon.EnsureRunning() (diagnose must not start a
// daemon as a side effect), and distinguishes two distinct failure classes:
// the event socket not existing at all (DaemonSocketMissing) versus existing
// but not answering a dial (DaemonConnUnreachable). Returns nil when the
// daemon is reachable — no finding to report.
func (e *Engine) daemonStatusFinding() *finding {
	socketPath := ipc.DefaultEventSocketPath()
	if !isSocket(socketPath) {
		return &finding{
			Message:  "the cenci daemon's event socket does not exist at " + socketPath,
			Code:     errcode.DaemonSocketMissing,
			Severity: severityForCode(errcode.DaemonSocketMissing),
		}
	}
	conn, err := net.DialTimeout("unix", socketPath, daemonDialTimeout)
	if err != nil {
		return &finding{
			Message:  "the cenci daemon is not answering at its event socket " + socketPath,
			Code:     errcode.DaemonConnUnreachable,
			Severity: severityForCode(errcode.DaemonConnUnreachable),
		}
	}
	_ = conn.Close()
	return nil
}

// containerExistenceFinding re-runs containerStartupState — the same probe
// Diagnose's not-found branch uses — and reports SandboxSessionNotFound only
// when the runtime conclusively answers "no such container" (an
// *exec.ExitError). Any other runtime-invocation failure (missing runtime
// binary, dial/timeout) is inconclusive for container existence
// specifically: a runtime problem, not evidence the container is missing or
// present. ok is false in that inconclusive case; f carries a message
// describing why, so Verify can still report the check as skipped instead of
// misreporting it as a pass/fail or attaching the wrong code — mirroring the
// classification-reuse pattern this package already applies in Diagnose's
// own switch (#572), and matching Diagnose's own "runtime unreachable"
// reporting for this exact inconclusive case rather than silently omitting
// it (#574).
func (e *Engine) containerExistenceFinding(scope Scope) (f *finding, ok bool) {
	_, _, err := e.containerStartupState(scope.ContainerName)
	var exitErr *exec.ExitError
	switch {
	case err != nil && errors.As(err, &exitErr):
		return &finding{
			Message:  fmt.Sprintf("container %q was not found", scope.ContainerName),
			Code:     errcode.SandboxSessionNotFound,
			Severity: severityForCode(errcode.SandboxSessionNotFound),
		}, true
	case err != nil:
		return &finding{Message: fmt.Sprintf("runtime unreachable (%s)", err)}, false
	default:
		return nil, true
	}
}

// verifyCheck is one re-runnable diagnostic probe --verify re-runs, reusing
// Diagnose's own probe helpers (daemonStatusFinding, containerStartupState)
// rather than a new framework. A nil *finding with ok true is a pass; ok
// false means the probe was inconclusive (e.g. the runtime binary itself
// could not run) — Verify still prints an explicit "[skip]" line for it
// (using f.Message, when set, to say why) rather than silently omitting the
// check.
type verifyCheck struct {
	Label string
	Run   func(e *Engine, scope Scope) (f *finding, ok bool)
}

// verifyChecks covers the recovery commands `cenci diagnose` already
// surfaces (#572): daemon reachability (DaemonSocketMissing/
// DaemonConnUnreachable) and container existence (SandboxSessionNotFound).
// The version/logs/mounts warnings carry no registered code and are out of
// verify scope (see the plan's Architectural Context).
var verifyChecks = []verifyCheck{
	{
		Label: "daemon reachability",
		Run: func(e *Engine, _ Scope) (*finding, bool) {
			return e.daemonStatusFinding(), true
		},
	},
	{
		Label: "container existence",
		Run:   (*Engine).containerExistenceFinding,
	},
}

// Verify re-runs the read-only diagnostic probes behind the recovery
// commands Diagnose surfaces and prints a "[pass]"/"[fail]"/"[skip]" line
// per check, so an operator can confirm a suggested recovery command
// actually worked. A check that came back inconclusive (ok == false, e.g.
// the runtime binary itself could not be invoked) still prints a "[skip]"
// line rather than being silently omitted — Verify must not have a weaker
// failure-visibility contract than Diagnose's own parallel switch, which
// reports this exact case as a finding (#572, #574). Like Diagnose, Verify
// is entirely read-only: it never launches, attaches, or executes a
// recovery command itself, only re-runs the same dial/inspect probes
// Diagnose already calls.
func (e *Engine) Verify(scope Scope) error {
	_, _ = fmt.Fprintf(e.Stdout, "cenci diagnose --verify: %s\n", scope.ContainerName)
	for _, check := range verifyChecks {
		f, ok := check.Run(e, scope)
		if !ok {
			reason := "inconclusive"
			if f != nil && f.Message != "" {
				reason = f.Message
			}
			_, _ = fmt.Fprintf(e.Stdout, "[skip] %s: %s\n", check.Label, reason)
			continue
		}
		if f == nil {
			_, _ = fmt.Fprintf(e.Stdout, "[pass] %s\n", check.Label)
			continue
		}
		_, _ = fmt.Fprintf(e.Stdout, "[fail] %s: %s: %s\n", check.Label, f.Code, f.Message)
	}
	return nil
}

// Diagnose writes a read-only, human-readable report for scope's sandbox
// session to e.Stdout: container status/exit, the timestamped startup
// marker (surfaced verbatim via startupFailureDetail), recent logs, mounted
// volumes, daemon/event-socket reachability, and plugin + image versions.
// Every failure is annotated with a registered errcode.Code and its
// fatal/degraded/warning severity via renderFinding. Diagnose is a report,
// not a gate: it always returns nil on a successful render, even when it
// finds fatal issues — the caller (diagnose_cmd.go) exits 0 in that case,
// reserving a non-nil error for when diagnose itself cannot run (never
// expected in normal operation, since every collection step here is
// best-effort).
func (e *Engine) Diagnose(scope Scope) error {
	agent := scopeAgent(scope)
	var findings []finding

	_, _ = fmt.Fprintf(e.Stdout, "cenci diagnose: %s (agent=%s)\n", scope.ContainerName, agent)
	_, _ = fmt.Fprintln(e.Stdout, "Note: recent logs and mount paths below may contain sensitive data (secrets, credentials, host paths) — review before sharing this output.")
	_, _ = fmt.Fprintf(e.Stdout, "Container: %s\n", scope.ContainerName)

	status, exitCode, stateErr := e.containerStartupState(scope.ContainerName)
	var stateExitErr *exec.ExitError
	switch {
	case stateErr != nil && errors.As(stateErr, &stateExitErr):
		// The runtime ran fine and reported a non-zero exit for `inspect
		// <name>` — the container genuinely does not exist (never launched,
		// launched under a different scope, or already auto-removed by
		// --rm). Still read the home-volume markers via
		// startupFailureDetail: that short-lived-container read doesn't
		// depend on the (missing) workload container.
		_, _ = fmt.Fprintln(e.Stdout, "Status: not found")
		content, _ := e.startupFailureDetail(scope)
		detail := strings.TrimSpace(string(stateExitErr.Stderr))
		if detail == "" {
			detail = stateErr.Error()
		}
		findings = append(findings, finding{
			Message:  fmt.Sprintf("container %q was not found: %s (%s)", scope.ContainerName, content, detail),
			Code:     errcode.SandboxSessionNotFound,
			Severity: severityForCode(errcode.SandboxSessionNotFound),
		})
	case stateErr != nil:
		// Some other error type (the runtime binary itself failed to run,
		// e.g. missing or a dial/timeout failure) — this is a
		// runtime/daemon-reachability problem, not evidence the container is
		// missing, so it must not be reported as SandboxSessionNotFound with
		// its "relaunch the session" recovery hints.
		_, _ = fmt.Fprintln(e.Stdout, "Status: unknown (runtime unreachable)")
		content, _ := e.startupFailureDetail(scope)
		findings = append(findings, finding{
			Message:  fmt.Sprintf("could not query the container runtime for %q: %s (%s)", scope.ContainerName, stateErr, content),
			Code:     errcode.DaemonConnUnreachable,
			Severity: severityForCode(errcode.DaemonConnUnreachable),
		})
	case status == "exited" || status == "dead":
		_, _ = fmt.Fprintf(e.Stdout, "Status: %s (exit %s)\n", status, exitCode)
		content, code := e.startupFailureDetail(scope)
		findings = append(findings, finding{
			Message:  content,
			Code:     code,
			Severity: severityForCode(code),
		})
	default:
		_, _ = fmt.Fprintf(e.Stdout, "Status: %s\n", status)
	}

	logs, logsOK := e.containerLogsTail(scope.ContainerName, 20)
	_, _ = fmt.Fprintf(e.Stdout, "Recent logs (tail):\n%s\n", versionOrUnknown(logs, logsOK))
	if !logsOK {
		findings = append(findings, finding{
			Message:  "recent container logs could not be read",
			Severity: SeverityWarning,
		})
	}

	mounts, mountsOK := e.inspectMounts(scope.ContainerName)
	_, _ = fmt.Fprintf(e.Stdout, "Mounts:\n%s\n", versionOrUnknown(mounts, mountsOK))
	if !mountsOK {
		findings = append(findings, finding{
			Message:  "container mounts could not be read",
			Severity: SeverityWarning,
		})
	}

	if f := e.daemonStatusFinding(); f != nil {
		findings = append(findings, *f)
	} else {
		_, _ = fmt.Fprintln(e.Stdout, "Daemon: reachable")
	}

	baseVersion, baseOK := e.imageBaseVersion(scope.Image)
	baseDisplay := versionOrUnknown(baseVersion, baseOK)
	_, _ = fmt.Fprintf(e.Stdout, "Image base version: %s\n", baseDisplay)
	if baseDisplay == "unknown" {
		findings = append(findings, finding{
			Message:  "image base version could not be determined",
			Severity: SeverityWarning,
		})
	}

	pluginVersion, pluginStatus := e.pluginManifestVersion(scope, agent)
	pluginDisplay := pluginManifestVersionDisplay(pluginVersion, pluginStatus)
	_, _ = fmt.Fprintf(e.Stdout, "Plugin manifest version: %s\n", pluginDisplay)
	switch pluginStatus {
	case homeVolumeReadFailed:
		findings = append(findings, finding{
			Message:  "plugin manifest version could not be read (the read failed)",
			Severity: SeverityWarning,
		})
	case homeVolumeReadAbsent:
		findings = append(findings, finding{
			Message:  "plugin manifest version could not be determined",
			Severity: SeverityWarning,
		})
	}

	nestedDockerLine, nestedDockerFnd := e.nestedDockerFinding(scope)
	_, _ = fmt.Fprintf(e.Stdout, "Nested Docker: %s\n", nestedDockerLine)
	if nestedDockerFnd != nil {
		findings = append(findings, *nestedDockerFnd)
	}

	eventDeliveryLine, eventDeliveryFnd := e.eventDeliveryFinding(scope)
	_, _ = fmt.Fprintf(e.Stdout, "Event delivery: %s\n", eventDeliveryLine)
	if eventDeliveryFnd != nil {
		findings = append(findings, *eventDeliveryFnd)
	}

	if len(findings) == 0 {
		_, _ = fmt.Fprintln(e.Stdout, "No issues found.")
		return nil
	}

	_, _ = fmt.Fprintln(e.Stdout, "\nFindings:")
	for _, f := range findings {
		_, _ = fmt.Fprint(e.Stdout, renderFinding(f))
	}
	return nil
}
