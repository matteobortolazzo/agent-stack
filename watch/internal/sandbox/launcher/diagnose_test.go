package launcher

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/matteobortolazzo/cenci/watch/v2/internal/daemon"
	"github.com/matteobortolazzo/cenci/watch/v2/internal/errcode"
)

// TestSeverityForCode_MapsRegisteredCodesToTheirTier pins the severity
// taxonomy from the ticket: container exited/dead/not-found, START-001, and
// START-002 are fatal; the readiness-timeout START-003, daemon-unreachable,
// and event-socket-missing are degraded. Version-mismatch findings (which
// attach no registered code at all) are covered separately below.
func TestSeverityForCode_MapsRegisteredCodesToTheirTier(t *testing.T) {
	cases := []struct {
		name string
		code errcode.Code
		want Severity
	}{
		{"agent CLI missing", errcode.SandboxStartAgentCLIMissing, SeverityFatal},
		{"generic entrypoint failure", errcode.SandboxStartGenericEntrypoint, SeverityFatal},
		{"session/container not found", errcode.SandboxSessionNotFound, SeverityFatal},
		{"readiness timeout", errcode.SandboxStartReadinessTimeout, SeverityDegraded},
		{"daemon unreachable", errcode.DaemonConnUnreachable, SeverityDegraded},
		{"event socket missing", errcode.DaemonSocketMissing, SeverityDegraded},
		{"dind startup failure", errcode.SandboxDindStartupFailure, SeverityDegraded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := severityForCode(tc.code); got != tc.want {
				t.Errorf("severityForCode(%s) = %q, want %q", tc.code, got, tc.want)
			}
		})
	}
}

// TestSeverityForCode_UnregisteredOrEmptyCodeDefaultsWarning covers the
// unknown-plugin/image-version findings, which attach no registered code at
// all (Code is the zero value) — severityForCode must not panic and must
// classify them as the lowest tier (warning), never fatal or degraded.
func TestSeverityForCode_UnregisteredOrEmptyCodeDefaultsWarning(t *testing.T) {
	cases := []errcode.Code{
		errcode.Code(""),
		errcode.Code("CENCI-DOES-NOTEXIST-999"),
	}
	for _, code := range cases {
		if got := severityForCode(code); got != SeverityWarning {
			t.Errorf("severityForCode(%q) = %q, want %q", code, got, SeverityWarning)
		}
	}
}

// TestSeverityForCode_AllRegisteredCodesAreExplicitlyMapped is a regression
// guard for severityForCode's exhaustiveness: every code registered in
// errcode's registry (errcode.AllCodes()) must land in one of
// severityForCode's explicit fatal/degraded switch cases, not silently fall
// through to the default warning branch — that branch is reserved for
// codeless findings (e.g. an unknown image/plugin version), not for a
// registered-but-forgotten Code. warningByDesign lists any registered code
// that is intentionally warning-tier; it is empty today because every
// registered code (as of #571/#572) maps to fatal or degraded. A future code
// added to the registry without a matching severityForCode case will fail
// this test (silently under-classified as warning) instead of drifting
// undetected.
func TestSeverityForCode_AllRegisteredCodesAreExplicitlyMapped(t *testing.T) {
	// CENCI-SANDBOX-DIND-002 (#962) is intentionally warning-tier: it
	// records a host capability the sandbox never had (macOS cannot
	// register sysbox-runc), not a failure of this session. The session
	// itself launched and works — only nested Docker is absent — and no
	// recovery action on this host can change that, which is exactly what
	// separates it from DIND-001's degraded tier (a dockerd that should
	// have started and didn't).
	warningByDesign := map[errcode.Code]bool{
		errcode.SandboxDindPlatformUnsupported: true,
	}

	codes := errcode.AllCodes()
	if len(codes) == 0 {
		t.Fatal("errcode.AllCodes() returned no codes; test cannot exercise the exhaustiveness guard")
	}

	for _, code := range codes {
		got := severityForCode(code)
		if got == SeverityWarning && !warningByDesign[code] {
			t.Errorf("severityForCode(%s) = %q, the default/unmapped tier; add an explicit fatal/degraded case to severityForCode's switch, or add %s to warningByDesign if it is intentionally warning-tier", code, got, code)
		}
	}
}

// TestRenderFinding_ReusesRegisteredHintsVerbatim asserts renderFinding pulls
// its recovery-command lines straight from errcode.Lookup(code).Hints rather
// than re-deriving or paraphrasing them, so the diagnose output and the
// errcode registry never drift apart.
func TestRenderFinding_ReusesRegisteredHintsVerbatim(t *testing.T) {
	entry, ok := errcode.Lookup(errcode.SandboxStartAgentCLIMissing)
	if !ok {
		t.Fatalf("errcode.Lookup(%s) = _, false; want a registered entry", errcode.SandboxStartAgentCLIMissing)
	}
	if len(entry.Hints) == 0 {
		t.Fatalf("errcode.Lookup(%s).Hints is empty; test needs at least one hint to assert verbatim reuse", errcode.SandboxStartAgentCLIMissing)
	}

	f := finding{
		Message:  "the agent CLI path is missing or not executable",
		Code:     errcode.SandboxStartAgentCLIMissing,
		Severity: SeverityFatal,
	}
	rendered := renderFinding(f)

	for _, hint := range entry.Hints {
		if !strings.Contains(rendered, hint) {
			t.Errorf("renderFinding output missing recovery hint %q verbatim; got:\n%s", hint, rendered)
		}
	}
	if !strings.Contains(rendered, f.Message) {
		t.Errorf("renderFinding output missing the finding message %q; got:\n%s", f.Message, rendered)
	}
	if !strings.Contains(rendered, string(errcode.SandboxStartAgentCLIMissing)) {
		t.Errorf("renderFinding output missing the error code %q; got:\n%s", errcode.SandboxStartAgentCLIMissing, rendered)
	}
}

// TestRenderFinding_NoCodeOmitsHintsButKeepsMessage covers a finding with no
// registered code (e.g. an "unknown" version-reporting warning): renderFinding
// must still surface the message and severity without fabricating hints or
// an error code that was never attached.
func TestRenderFinding_NoCodeOmitsHintsButKeepsMessage(t *testing.T) {
	f := finding{
		Message:  "plugin manifest version could not be determined",
		Severity: SeverityWarning,
	}
	rendered := renderFinding(f)

	if !strings.Contains(rendered, f.Message) {
		t.Errorf("renderFinding output missing the finding message %q; got:\n%s", f.Message, rendered)
	}
	if strings.Contains(rendered, "CENCI-") {
		t.Errorf("renderFinding output unexpectedly contains an error code for a finding with no Code; got:\n%s", rendered)
	}
}

// TestRenderFinding_MultiLineMessage_IndentsContinuationLines pins #1163's
// security re-review Fix 2 (and its own follow-up fix): a multi-line
// finding.Message (e.g. a boot log via startupFailureDetail) must have its
// continuation lines (2..N) indented with a distinct "  | " gutter so they
// read as part of this one finding, not as standalone column-0 report lines
// a malicious container could shape to look like a separate, legitimate
// "[severity] CENCI-...: ..." finding — and, since the gutter must NOT
// collide with the hint-rendering code's own "  - " prefix, a continuation
// line starting with "- forged hint" must not render as "  - forged hint"
// (which would be indistinguishable from a genuine recovery hint).
func TestRenderFinding_MultiLineMessage_IndentsContinuationLines(t *testing.T) {
	f := finding{
		Message:  "first line\n[fatal] CENCI-FORGED-999: forged finding line\n- forged hint",
		Code:     errcode.SandboxStartGenericEntrypoint,
		Severity: SeverityFatal,
	}
	rendered := renderFinding(f)

	for _, line := range strings.Split(rendered, "\n") {
		if line == "[fatal] CENCI-FORGED-999: forged finding line" {
			t.Errorf("a continuation line of a multi-line finding.Message must not land at column 0 as a standalone line; got:\n%s", rendered)
		}
		if line == "  - forged hint" {
			t.Errorf("a continuation line starting with \"- \" must not render identically to a genuine \"  - \" recovery hint; got:\n%s", rendered)
		}
	}
	if !strings.Contains(rendered, "\n  | [fatal] CENCI-FORGED-999: forged finding line") {
		t.Errorf("expected the second line indented with the \"  | \" gutter; got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "\n  | - forged hint") {
		t.Errorf("expected the third line indented with the \"  | \" gutter; got:\n%s", rendered)
	}
}

// TestVersionOrUnknown_FallsBackWhenReadFails pins the shared "unknown"
// fallback every best-effort version read (image base-version,
// pluginManifestVersion) funnels through: a failed or empty read must never
// surface as a blank string, always as the literal "unknown".
func TestVersionOrUnknown_FallsBackWhenReadFails(t *testing.T) {
	cases := []struct {
		name    string
		content string
		ok      bool
		want    string
	}{
		{"successful read", "1.2.3", true, "1.2.3"},
		{"read failed", "", false, "unknown"},
		{"read reported ok but content is empty", "", true, "unknown"},
		{"content has surrounding whitespace preserved by caller", "  1.2.3  ", true, "  1.2.3  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionOrUnknown(tc.content, tc.ok); got != tc.want {
				t.Errorf("versionOrUnknown(%q, %v) = %q, want %q", tc.content, tc.ok, got, tc.want)
			}
		})
	}
}

// TestDiagnose_RuntimeUnreachable_ReportsDaemonUnreachableNotSessionNotFound
// pins the containerStartupState error-type split: when the runtime binary
// itself cannot be run (an error type other than *exec.ExitError — e.g. the
// binary is missing), that is a runtime/daemon-reachability problem, not
// evidence the container is missing, so it must be reported as
// DaemonConnUnreachable/degraded, never as the misleading
// SandboxSessionNotFound/fatal ("relaunch the session") finding.
func TestDiagnose_RuntimeUnreachable_ReportsDaemonUnreachableNotSessionNotFound(t *testing.T) {
	var stdout, stderr bytes.Buffer
	e := &Engine{
		Runtime: "cenci-test-runtime-does-not-exist-572",
		Stdout:  &stdout,
		Stderr:  &stderr,
	}
	scope := Scope{
		ContainerName: "claude-cenci-test",
		VolumeName:    "claude-cenci-home-test",
		Image:         "cenci-sandbox:test",
	}

	if err := e.Diagnose(scope); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, string(errcode.DaemonConnUnreachable)) {
		t.Errorf("expected %s (runtime unreachable) in the report, got:\n%s", errcode.DaemonConnUnreachable, out)
	}
	if !strings.Contains(out, string(SeverityDegraded)) {
		t.Errorf("expected degraded severity for the runtime-unreachable finding, got:\n%s", out)
	}
	if strings.Contains(out, string(errcode.SandboxSessionNotFound)) {
		t.Errorf("did not expect %s (session not found) when the runtime itself is unreachable, got:\n%s", errcode.SandboxSessionNotFound, out)
	}
}

// TestDiagnose_RecentLogsAndMountsReadFailure_ReportUnknownAndWarn pins Fix
// 2: when the best-effort logs/mounts reads fail (here, because the runtime
// binary cannot be run at all), the report still prints the "Recent logs"
// and "Mounts" section headers with "unknown" bodies — never silently
// dropping the section — and appends a SeverityWarning finding for each so
// the failure is visible in the findings summary too.
func TestDiagnose_RecentLogsAndMountsReadFailure_ReportUnknownAndWarn(t *testing.T) {
	var stdout, stderr bytes.Buffer
	e := &Engine{
		Runtime: "cenci-test-runtime-does-not-exist-572",
		Stdout:  &stdout,
		Stderr:  &stderr,
	}
	scope := Scope{
		ContainerName: "claude-cenci-test",
		VolumeName:    "claude-cenci-home-test",
		Image:         "cenci-sandbox:test",
	}

	if err := e.Diagnose(scope); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Recent logs (tail):\nunknown") {
		t.Errorf("expected the Recent logs section header with an \"unknown\" body, got:\n%s", out)
	}
	if !strings.Contains(out, "Mounts:\nunknown") {
		t.Errorf("expected the Mounts section header with an \"unknown\" body, got:\n%s", out)
	}
	if !strings.Contains(out, "recent container logs could not be read") {
		t.Errorf("expected a warning finding for the failed logs read, got:\n%s", out)
	}
	if !strings.Contains(out, "container mounts could not be read") {
		t.Errorf("expected a warning finding for the failed mounts read, got:\n%s", out)
	}
}

// TestDiagnose_IncludesSecretsCaveat pins Fix 6: the report always prints a
// one-line caveat warning that recent logs and mount paths may contain
// sensitive data, so an operator doesn't paste raw diagnose output somewhere
// public without reviewing it first.
func TestDiagnose_IncludesSecretsCaveat(t *testing.T) {
	var stdout, stderr bytes.Buffer
	e := &Engine{
		Runtime: "cenci-test-runtime-does-not-exist-572",
		Stdout:  &stdout,
		Stderr:  &stderr,
	}
	scope := Scope{
		ContainerName: "claude-cenci-test",
		VolumeName:    "claude-cenci-home-test",
		Image:         "cenci-sandbox:test",
	}

	if err := e.Diagnose(scope); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "may contain sensitive data") {
		t.Errorf("expected a secrets caveat in the report, got:\n%s", out)
	}
}

// TestMarketplaceManifestPath_OpencodeReturnsEmptyNotClaudePath pins Fix 5:
// opencode has no marketplace.json (it's provisioned via a git clone, not a
// marketplace checkout), so it must not fall back to the claude path.
func TestMarketplaceManifestPath_OpencodeReturnsEmptyNotClaudePath(t *testing.T) {
	cases := []struct {
		agent string
		want  string
	}{
		{"claude", "/home/dev/.claude/plugins/marketplaces/cenci/.claude-plugin/marketplace.json"},
		{"codex", "/home/dev/.codex/plugins/marketplaces/cenci/.claude-plugin/marketplace.json"},
		{"opencode", ""},
	}
	for _, tc := range cases {
		if got := marketplaceManifestPath(tc.agent); got != tc.want {
			t.Errorf("marketplaceManifestPath(%q) = %q, want %q", tc.agent, got, tc.want)
		}
	}
}

// TestPluginManifestVersion_OpencodeReportsCleanFailureWithoutReadingClaudePath
// asserts pluginManifestVersion short-circuits for opencode instead of
// attempting a read with an empty path (or worse, silently reading claude's
// manifest). Runtime is set to a binary that does not exist so the test
// would fail loudly (a non-"" Runtime invocation attempt surfaces as an
// unrelated error) if pluginManifestVersion regressed to actually invoking
// the runtime for opencode.
//
// #1163 Q4: opencode has no manifest path at all (there's nothing to read),
// so the short-circuit must classify as homeVolumeReadAbsent, never
// homeVolumeReadFailed — "read failed" would be a wrong claim when no read
// was ever attempted.
func TestPluginManifestVersion_OpencodeReportsCleanFailureWithoutReadingClaudePath(t *testing.T) {
	e := &Engine{Runtime: "cenci-test-runtime-does-not-exist-572"}
	scope := Scope{
		ContainerName: "opencode-cenci-test",
		VolumeName:    "opencode-cenci-home-test",
		Image:         "cenci-sandbox:test",
	}

	version, status := e.pluginManifestVersion(scope, "opencode")
	if status != homeVolumeReadAbsent || version != "" {
		t.Errorf("pluginManifestVersion(opencode) = %q, %v; want \"\", homeVolumeReadAbsent", version, status)
	}
}

// -- Nested Docker (#630): always-on 3-state "Nested Docker:" section -------
//
// Per the ticket's Q3, Diagnose must always print a "Nested Docker:" line
// (never partial silence, per the package's #572 failure-visibility
// convention): "not a dind session" when scope.DindVolumeName was never
// created (a `volume ls` miss — #1163 retired the earlier `volume inspect`
// probe, since podman exits 125 for `volume inspect` on a
// legitimately-absent volume, indistinguishable from a genuine runtime
// failure), "no failure recorded" when the session is dind but the
// dockerd-startup-error marker is absent, or the marker's diagnostic content
// plus a Degraded CENCI-SANDBOX-DIND-001 finding when the marker is present.
// These three tests use writeFakeRuntime (a real, scriptable fake docker on
// PATH), not the "runtime does not exist" pattern the tests above use, since
// exercising all three states needs a working `volume ls` + home-volume
// marker read.

// dindDiagnoseScope returns a Scope with a realistic DindVolumeName set, so
// Diagnose's isDindSession probe (`volume ls --format {{.Name}}`, #1163) has
// a name to look for — the fake's FAKE_VOLUMES var must include this exact
// name for a test to script "dind volume present".
func dindDiagnoseScope() Scope {
	return Scope{
		ContainerName:  "claude-cenci-test",
		VolumeName:     "claude-cenci-home-test",
		DindVolumeName: "claude-cenci-dind-test",
		Image:          "cenci-sandbox:test",
	}
}

func TestDiagnose_NestedDocker_NotADindSession(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	e := &Engine{Runtime: "docker", Stdout: &stdout, Stderr: &stderr}
	// FAKE_VOLUMES unset (default empty): `volume ls` never lists
	// scope.DindVolumeName — this session was never launched with --dind.

	if err := e.Diagnose(dindDiagnoseScope()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Nested Docker:") {
		t.Errorf("expected an always-on \"Nested Docker:\" line (Q3), got:\n%s", out)
	}
	if !strings.Contains(out, "not a dind session") {
		t.Errorf("expected the not-a-dind-session state, got:\n%s", out)
	}
	if strings.Contains(out, string(errcode.SandboxDindStartupFailure)) {
		t.Errorf("did not expect %s when the session isn't dind at all, got:\n%s", errcode.SandboxDindStartupFailure, out)
	}
}

func TestDiagnose_NestedDocker_NoFailureRecorded(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	e := &Engine{Runtime: "docker", Stdout: &stdout, Stderr: &stderr}
	// FAKE_VOLUMES lists scope.DindVolumeName: `volume ls` reports the dind
	// volume present, so this is a genuine dind session. FAKE_DOCKERD_MARKER
	// unset: no persistent dockerd-startup-error marker was ever written.
	t.Setenv("FAKE_VOLUMES", "claude-cenci-dind-test\n")

	if err := e.Diagnose(dindDiagnoseScope()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Nested Docker:") {
		t.Errorf("expected an always-on \"Nested Docker:\" line (Q3), got:\n%s", out)
	}
	if !strings.Contains(out, "no failure recorded") {
		t.Errorf("expected the no-failure-recorded state for a healthy dind session, got:\n%s", out)
	}
	if strings.Contains(out, string(errcode.SandboxDindStartupFailure)) {
		t.Errorf("did not expect %s when no dockerd marker was ever written, got:\n%s", errcode.SandboxDindStartupFailure, out)
	}
}

func TestDiagnose_NestedDocker_MarkerPresent_AttachesDegradedFinding(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	e := &Engine{Runtime: "docker", Stdout: &stdout, Stderr: &stderr}
	t.Setenv("FAKE_VOLUMES", "claude-cenci-dind-test\n")
	const marker = "2026-07-24T09:00:00Z dockerd exited with status 1: failed to start daemon: mkdir /var/lib/docker/overlay2: read-only file system"
	t.Setenv("FAKE_DOCKERD_MARKER", marker)

	if err := e.Diagnose(dindDiagnoseScope()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Nested Docker:") {
		t.Errorf("expected an always-on \"Nested Docker:\" line (Q3), got:\n%s", out)
	}
	if !strings.Contains(out, marker) {
		t.Errorf("expected the dockerd-startup-error marker's diagnostic surfaced verbatim, got:\n%s", out)
	}
	if !strings.Contains(out, string(errcode.SandboxDindStartupFailure)) {
		t.Errorf("expected %s attached for the recorded dockerd startup failure, got:\n%s", errcode.SandboxDindStartupFailure, out)
	}
	if !strings.Contains(out, string(SeverityDegraded)) {
		t.Errorf("expected degraded severity for the dind startup-failure finding, got:\n%s", out)
	}
}

// -- Event delivery (#1122): always-on "Event delivery:" section -----------
//
// Mirrors the Nested Docker section's always-on-line convention: "no failure
// recorded" when the .cenci-events-undelivered home-volume marker is absent,
// the marker's message plus a severity-mapped finding when present, and an
// explicit [warning] finding (never silent omission) when the marker exists
// but fails to parse.

func TestDiagnose_EventDelivery_NoFailureRecorded(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	e := &Engine{Runtime: "docker", Stdout: &stdout, Stderr: &stderr}
	// FAKE_EVENTS_MARKER unset: no undelivered-events marker was ever written.

	if err := e.Diagnose(dindDiagnoseScope()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Event delivery: no failure recorded") {
		t.Errorf("expected an always-on \"Event delivery:\" line reporting no failure, got:\n%s", out)
	}
}

func TestDiagnose_EventDelivery_ValidMarker_AttachesDegradedFinding(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	e := &Engine{Runtime: "docker", Stdout: &stdout, Stderr: &stderr}
	const message = "the sandbox socket directory's bind mount is dangling inside this container; recreate the container to restore it"
	marker := `{"session_id":"sess-1","code":"` + string(errcode.SandboxSocketUnwired) + `","message":"` + message + `","timestamp":"2026-09-07T00:00:00Z"}`
	t.Setenv("FAKE_EVENTS_MARKER", marker)

	if err := e.Diagnose(dindDiagnoseScope()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Event delivery:") {
		t.Errorf("expected an always-on \"Event delivery:\" line, got:\n%s", out)
	}
	if !strings.Contains(out, message) {
		t.Errorf("expected the marker message surfaced verbatim, got:\n%s", out)
	}
	if !strings.Contains(out, string(errcode.SandboxSocketUnwired)) {
		t.Errorf("expected %s attached for the recorded delivery failure, got:\n%s", errcode.SandboxSocketUnwired, out)
	}
	if !strings.Contains(out, string(SeverityDegraded)) {
		t.Errorf("expected degraded severity for the event-delivery finding, got:\n%s", out)
	}
}

func TestDiagnose_EventDelivery_UnparseableMarker_ReportsWarningNotOmitted(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	e := &Engine{Runtime: "docker", Stdout: &stdout, Stderr: &stderr}
	t.Setenv("FAKE_EVENTS_MARKER", "not valid json")

	if err := e.Diagnose(dindDiagnoseScope()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Event delivery:") {
		t.Errorf("expected an always-on \"Event delivery:\" line, got:\n%s", out)
	}
	if !strings.Contains(out, "["+string(SeverityWarning)+"]") {
		t.Errorf("expected an explicit [warning] finding for the unparseable marker rather than silent omission, got:\n%s", out)
	}
	if !strings.Contains(out, "unparseable") {
		t.Errorf("expected the warning finding to name the unparseable marker, got:\n%s", out)
	}
}

// -- sanitizeEventDeliveryMessage (#1122 fix cycle: sanitizer hardening) ----

// TestSanitizeEventDeliveryMessage_StripsControlAndBidiCharacters pins fix 1:
// the sanitizer must strip not just ASCII C0 control bytes and DEL, but also
// C1 control characters and Unicode bidi-override characters that could
// otherwise spoof or manipulate terminal output.
func TestSanitizeEventDeliveryMessage_StripsControlAndBidiCharacters(t *testing.T) {
	input := "safe\x1btext\u009bmore\rhere\x7fend\u202etail"
	got := sanitizeEventDeliveryMessage(input)

	for _, bad := range []string{"\x1b", "\u009b", "\r", "\x7f", "\u202e"} {
		if strings.Contains(got, bad) {
			t.Errorf("sanitizeEventDeliveryMessage(%q) = %q; must not contain stripped char %q", input, got, bad)
		}
	}
	want := "safetextmorehereendtail"
	if got != want {
		t.Errorf("sanitizeEventDeliveryMessage(%q) = %q, want %q", input, got, want)
	}
}

// TestSanitizeEventDeliveryMessage_TruncationProducesValidUTF8 pins fix 2:
// truncating over maxEventDeliveryMessageLen must never split a multi-byte
// UTF-8 rune. The input is built so a naive byte-length cut at exactly
// maxEventDeliveryMessageLen would land in the middle of a 3-byte rune.
func TestSanitizeEventDeliveryMessage_TruncationProducesValidUTF8(t *testing.T) {
	prefix := strings.Repeat("a", maxEventDeliveryMessageLen-1)
	input := prefix + "€€€€€€€€€€" // "€" (U+20AC) is 3 bytes; starts exactly one byte before the cutoff

	got := sanitizeEventDeliveryMessage(input)

	if !utf8.ValidString(got) {
		t.Fatalf("sanitizeEventDeliveryMessage truncated output is not valid UTF-8: %q", got)
	}
	if len(got) > maxEventDeliveryMessageLen {
		t.Errorf("sanitizeEventDeliveryMessage output length = %d, want <= %d", len(got), maxEventDeliveryMessageLen)
	}
	if strings.Contains(got, "�") {
		t.Errorf("sanitizeEventDeliveryMessage output contains a replacement character (partial rune): %q", got)
	}
}

// TestDiagnose_EventDelivery_MessageWithEscapeSequence_StripsRawEscapeByte is
// an end-to-end proof (fix 1/2) that the sanitizer's fix actually reaches the
// printed diagnose line, not just the helper in isolation: a marker message
// containing a raw ANSI escape sequence must not leave a raw ESC byte in
// stdout.
func TestDiagnose_EventDelivery_MessageWithEscapeSequence_StripsRawEscapeByte(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	e := &Engine{Runtime: "docker", Stdout: &stdout, Stderr: &stderr}
	// Built via json.Marshal (rather than a hand-written JSON literal) so the
	// raw ESC byte (0x1b) in Message is correctly JSON-escaped by the encoder,
	// then decodes back into an actual ESC rune in m.Message when
	// eventDeliveryFinding parses it.
	esc := string(rune(0x1b))
	b, err := json.Marshal(daemon.UndeliveredEventsMarker{
		SessionID: "sess-1",
		Code:      errcode.SandboxSocketUnwired,
		Message:   "clear screen " + esc + "[2J attack",
		Timestamp: "2026-09-07T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("json.Marshal marker: %v", err)
	}
	t.Setenv("FAKE_EVENTS_MARKER", string(b))

	if err := e.Diagnose(dindDiagnoseScope()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	if strings.ContainsRune(out, rune(0x1b)) {
		t.Errorf("expected no raw ESC byte in diagnose output, got:\n%q", out)
	}
}

// TestDiagnose_EventDelivery_UnregisteredCode_ReportsWarningNamingTheCode
// pins fixes 3 and 4, and the silent-failure-hunter's missing-coverage
// finding: valid JSON whose code field is well-formed but does not resolve
// via errcode.Lookup must surface as an explicit [warning] finding naming
// the unrecognized code, distinct in wording from the "unparseable JSON"
// case (it must not say "unparseable" — the JSON parsed fine).
func TestDiagnose_EventDelivery_UnregisteredCode_ReportsWarningNamingTheCode(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	e := &Engine{Runtime: "docker", Stdout: &stdout, Stderr: &stderr}
	const bogusCode = "CENCI-BOGUS-999"
	marker := `{"session_id":"s1","code":"` + bogusCode + `","message":"x","timestamp":"2024-01-01T00:00:00Z"}`
	t.Setenv("FAKE_EVENTS_MARKER", marker)

	if err := e.Diagnose(dindDiagnoseScope()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "["+string(SeverityWarning)+"]") {
		t.Errorf("expected an explicit [warning] finding for the unrecognized code rather than silent omission, got:\n%s", out)
	}
	if !strings.Contains(out, bogusCode) {
		t.Errorf("expected the finding to name the unrecognized code %q, got:\n%s", bogusCode, out)
	}
	if strings.Contains(out, "unparseable") {
		t.Errorf("expected wording distinct from the unparseable-JSON case (the JSON parsed fine; only the code was unrecognized), got:\n%s", out)
	}
	if !strings.Contains(out, "unrecognized code") {
		t.Errorf("expected wording naming an \"unrecognized code\", got:\n%s", out)
	}
}

// TestVerify_DoesNotGainAnEventDeliveryCheck pins AC #14: --verify output
// must not change — no new entry in verifyChecks for the event-delivery
// marker.
func TestVerify_DoesNotGainAnEventDeliveryCheck(t *testing.T) {
	labels := make(map[string]bool, len(verifyChecks))
	for _, c := range verifyChecks {
		labels[c.Label] = true
	}
	if labels["event delivery"] {
		t.Error("verifyChecks must not gain an \"event delivery\" check (AC: --verify output is unchanged)")
	}
	if len(verifyChecks) != 2 {
		t.Errorf("verifyChecks has %d entries, want 2 (daemon reachability, container existence) — --verify output must not change", len(verifyChecks))
	}
}

// -- ticket #1163: home-volume-read three-state classification reaches ------
// -- Diagnose's "Nested Docker:"/"Event delivery:" sections -----------------
//
// NOTE (red phase): homeVolumeReadStatus/homeVolumeReadOK/homeVolumeReadAbsent/
// homeVolumeReadFailed, the isDindSession tri-state
// (dindSessionAbsent/dindSessionPresent/dindSessionIndeterminate), and
// sanitizeHomeVolumeContent/maxHomeVolumeContentLen/
// pluginManifestVersionDisplay do not exist yet in launch.go/diagnose.go —
// they land in Phase 4. Every reference below is a compile error until then;
// that is the intended red-phase state (mirrors launch_test.go's #628
// reuse-posture red-phase note), not a bug to fix by stubbing this file.

// TestDiagnose_EventDelivery_MarkerReadFailed_ReportsWarningNotNoFailureRecorded
// pins the #1163 fix: a docker/podman daemon failure while reading the
// undelivered-events marker (exit 125) must be distinguished from a
// legitimately absent marker — "no failure recorded" must never be printed,
// and an explicit [warning] finding must surface the read failure instead of
// silently reporting a false "all clear".
func TestDiagnose_EventDelivery_MarkerReadFailed_ReportsWarningNotNoFailureRecorded(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_EVENTS_MARKER_EXIT", "125")

	var stdout, stderr bytes.Buffer
	e := &Engine{Runtime: "docker", Stdout: &stdout, Stderr: &stderr}

	if err := e.Diagnose(dindDiagnoseScope()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Event delivery:") {
		t.Errorf("expected an always-on \"Event delivery:\" line, got:\n%s", out)
	}
	if strings.Contains(out, "no failure recorded") {
		t.Errorf("a runtime read failure (exit 125) must not be reported as \"no failure recorded\" (that claim is reserved for a legitimately absent marker), got:\n%s", out)
	}
	if !strings.Contains(out, "["+string(SeverityWarning)+"]") {
		t.Errorf("expected an explicit [warning] finding for the read failure rather than a silent false all-clear, got:\n%s", out)
	}
}

// TestDiagnose_NestedDocker_MarkerReadFailed_ReportsWarningNotNoFailureRecorded
// is the "Nested Docker:" sibling of the test above: a genuine dind session
// (FAKE_VOLUMES lists the dind volume) whose dockerd-marker read fails (exit
// 125) must not be reported as "no failure recorded".
func TestDiagnose_NestedDocker_MarkerReadFailed_ReportsWarningNotNoFailureRecorded(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_VOLUMES", "claude-cenci-dind-test\n")
	t.Setenv("FAKE_DOCKERD_MARKER_EXIT", "125")

	var stdout, stderr bytes.Buffer
	e := &Engine{Runtime: "docker", Stdout: &stdout, Stderr: &stderr}

	if err := e.Diagnose(dindDiagnoseScope()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Nested Docker:") {
		t.Errorf("expected an always-on \"Nested Docker:\" line (Q3), got:\n%s", out)
	}
	// Scoped to the "Nested Docker:" line specifically -- the unrelated
	// "Event delivery: no failure recorded" line is expected to be present
	// in this same report (FAKE_EVENTS_MARKER is unset), so a whole-output
	// substring check would false-positive on that line instead of proving
	// anything about the Nested Docker section.
	if strings.Contains(out, "Nested Docker: no failure recorded") {
		t.Errorf("a runtime read failure (exit 125) must not be reported as \"no failure recorded\" (that claim is reserved for a legitimately absent marker), got:\n%s", out)
	}
	if !strings.Contains(out, "["+string(SeverityWarning)+"]") {
		t.Errorf("expected an explicit [warning] finding for the read failure rather than a silent false all-clear, got:\n%s", out)
	}
}

// TestDiagnose_NestedDocker_VolumeLsFails_ReportsIndeterminateNotNotADindSession
// pins the isDindSession tri-state conversion (#1163): podman returns 125 for
// `volume inspect` on a legitimately-absent volume, so the probe was changed
// to `volume ls` — but that call can itself fail (daemon unreachable), which
// must surface as an explicit indeterminate state ("could not determine"),
// never as the confident (and here false) "not a dind session" claim.
func TestDiagnose_NestedDocker_VolumeLsFails_ReportsIndeterminateNotNotADindSession(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_VOLUME_LS_EXIT", "1")

	var stdout, stderr bytes.Buffer
	e := &Engine{Runtime: "docker", Stdout: &stdout, Stderr: &stderr}

	if err := e.Diagnose(dindDiagnoseScope()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	if !strings.Contains(out, "Nested Docker:") {
		t.Errorf("expected an always-on \"Nested Docker:\" line (Q3), got:\n%s", out)
	}
	if strings.Contains(out, "not a dind session") {
		t.Errorf("a `volume ls` failure must never be reported as the confident \"not a dind session\" claim, got:\n%s", out)
	}
	if !strings.Contains(out, "could not determine") {
		t.Errorf("expected wording naming the indeterminate state (\"could not determine\"), got:\n%s", out)
	}
	if !strings.Contains(out, "["+string(SeverityWarning)+"]") {
		t.Errorf("expected an explicit [warning] finding for the indeterminate dind-session probe, got:\n%s", out)
	}
}

// TestIsDindSession_TriState is a direct unit test of the isDindSession
// tri-state conversion pinned by #1163: an empty scope.DindVolumeName
// short-circuits to absent with no probe at all (legacy/ScopeForContainer
// scopes never carry a dind volume name); a present name is looked up via
// `volume ls` (never `volume inspect`, which podman exits 125 for on a
// legitimately-absent volume); a `volume ls` failure is indeterminate, not
// absent.
func TestIsDindSession_TriState(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))

	e := &Engine{Runtime: "docker", Stderr: io.Discard}

	t.Run("empty DindVolumeName short-circuits to absent, no probe", func(t *testing.T) {
		if got := e.isDindSession(Scope{}); got != dindSessionAbsent {
			t.Errorf("isDindSession(empty DindVolumeName) = %v, want dindSessionAbsent", got)
		}
		if containsLineWithAll(readCallLog(t, callLog), "volume") {
			t.Errorf("isDindSession must never probe `volume ls` when scope.DindVolumeName is empty; calls:\n%s", strings.Join(readCallLog(t, callLog), "\n"))
		}
	})

	t.Run("volume ls lists the name -> present", func(t *testing.T) {
		t.Setenv("FAKE_VOLUMES", "claude-cenci-dind-test\n")
		if got := e.isDindSession(Scope{DindVolumeName: "claude-cenci-dind-test"}); got != dindSessionPresent {
			t.Errorf("isDindSession(present) = %v, want dindSessionPresent", got)
		}
	})

	t.Run("volume ls omits the name -> absent", func(t *testing.T) {
		t.Setenv("FAKE_VOLUMES", "some-other-volume\n")
		if got := e.isDindSession(Scope{DindVolumeName: "claude-cenci-dind-test"}); got != dindSessionAbsent {
			t.Errorf("isDindSession(absent) = %v, want dindSessionAbsent", got)
		}
	})

	t.Run("volume ls fails -> indeterminate, not absent", func(t *testing.T) {
		t.Setenv("FAKE_VOLUME_LS_EXIT", "1")
		if got := e.isDindSession(Scope{DindVolumeName: "claude-cenci-dind-test"}); got != dindSessionIndeterminate {
			t.Errorf("isDindSession(volume ls failure) = %v, want dindSessionIndeterminate", got)
		}
	})
}

// -- ticket #1163: readHomeVolumeFile's three-state classification ---------

// TestReadHomeVolumeFile_ClassifiesExitCodes pins the exact classification
// rules the ticket pins: exit 125/126/127 (the runtime itself rejected or
// couldn't run the request) is a read failure; any other non-zero exit
// (e.g. `cat`'s ordinary "file not found" exit 1) is a legitimately absent
// marker; exit 0 with empty content is also absent (nothing was written);
// exit 0 with content is a successful read. Each case gets its own
// assertion (watch/docs/error-handling.md #446) rather than a collapsed
// ok/not-ok check, so a regression that merges two classes back together is
// caught.
func TestReadHomeVolumeFile_ClassifiesExitCodes(t *testing.T) {
	cases := []struct {
		name       string
		exitCode   string // FAKE_DOCKERD_MARKER_EXIT; "" leaves it unset (default 0)
		content    string
		wantStatus homeVolumeReadStatus
	}{
		{"exit 125 (runtime rejected the request) is a read failure", "125", "", homeVolumeReadFailed},
		{"exit 126 (cannot execute) is a read failure", "126", "", homeVolumeReadFailed},
		{"exit 127 (not found inside the runtime) is a read failure", "127", "", homeVolumeReadFailed},
		{"exit 1 (cat: no such file) is a legitimately absent marker", "1", "", homeVolumeReadAbsent},
		{"exit 0 with empty content is a legitimately absent marker", "0", "", homeVolumeReadAbsent},
		{"exit 0 with content is a successful read", "0", "hello from the marker file", homeVolumeReadOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeDir := t.TempDir()
			callLog := filepath.Join(fakeDir, "calls.txt")
			writeFakeRuntime(t, fakeDir, "docker", callLog)
			t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))
			if tc.exitCode != "" {
				t.Setenv("FAKE_DOCKERD_MARKER_EXIT", tc.exitCode)
			}
			if tc.content != "" {
				t.Setenv("FAKE_DOCKERD_MARKER", tc.content)
			}
			var stderr bytes.Buffer
			e := &Engine{Runtime: "docker", Stderr: &stderr}
			scope := Scope{VolumeName: "claude-cenci-home-test", Image: "cenci-sandbox:test"}

			content, status := e.readHomeVolumeFile(scope, dockerdFailureMarkerPath)
			if status != tc.wantStatus {
				t.Errorf("readHomeVolumeFile status = %v, want %v", status, tc.wantStatus)
			}
			if tc.wantStatus == homeVolumeReadOK && content != tc.content {
				t.Errorf("readHomeVolumeFile content = %q, want %q", content, tc.content)
			}
		})
	}
}

// TestReadHomeVolumeFile_RuntimeBinaryMissing_IsReadFailed pins the
// non-*exec.ExitError branch: a runtime binary that can't even be invoked
// (missing from PATH) is a read failure, not a legitimately absent marker —
// distinguishing "we could not look" from "we looked and it wasn't there".
func TestReadHomeVolumeFile_RuntimeBinaryMissing_IsReadFailed(t *testing.T) {
	var stderr bytes.Buffer
	e := &Engine{Runtime: "cenci-test-runtime-does-not-exist-1163", Stderr: &stderr}
	scope := Scope{VolumeName: "claude-cenci-home-test", Image: "cenci-sandbox:test"}

	_, status := e.readHomeVolumeFile(scope, dockerdFailureMarkerPath)
	if status != homeVolumeReadFailed {
		t.Errorf("readHomeVolumeFile status = %v, want homeVolumeReadFailed for a runtime binary that cannot be invoked", status)
	}
}

// TestReadHomeVolumeFile_AllControlByteContent_ClassifiesOKNotAbsent pins
// the #1163 security-review fix: the OK-vs-absent decision must be made on
// the RAW read bytes, before sanitization. A read that genuinely succeeds
// (exit 0) with non-empty raw content that happens to sanitize down to ""
// (here, entirely C0/C1 control bytes) is a real "we looked and got
// something" outcome, not "we looked and found nothing" — it must classify
// homeVolumeReadOK (with empty sanitized content), never
// homeVolumeReadAbsent. Deciding emptiness AFTER sanitizing would silently
// misclassify this case across all six readHomeVolumeFile call sites at
// once.
func TestReadHomeVolumeFile_AllControlByteContent_ClassifiesOKNotAbsent(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))
	// Every byte here is stripped by sanitizeHomeVolumeContent (C0 ESC, C1
	// U+009B, DEL, bidi U+202E) — the raw read still succeeded and returned
	// real, non-empty bytes.
	t.Setenv("FAKE_DOCKERD_MARKER", "\x1b\u009b\x7f\u202e")

	var stderr bytes.Buffer
	e := &Engine{Runtime: "docker", Stderr: &stderr}
	scope := Scope{VolumeName: "claude-cenci-home-test", Image: "cenci-sandbox:test"}

	content, status := e.readHomeVolumeFile(scope, dockerdFailureMarkerPath)
	if status != homeVolumeReadOK {
		t.Errorf("readHomeVolumeFile status = %v, want homeVolumeReadOK for a genuinely successful read whose content sanitizes to empty", status)
	}
	if content != "" {
		t.Errorf("readHomeVolumeFile content = %q, want \"\" (every byte was stripped by the sanitizer)", content)
	}
}

// -- ticket #1163: multi-line-safe sanitizer (sanitizeHomeVolumeContent) ---
//
// Mirrors sanitizeEventDeliveryMessage's control/bidi-character stripping,
// but preserves "\n"/"\t" (multi-line content like a 50-line boot log must
// stay multi-line) and truncates from the FRONT (tail-keeping: startupFailureDetail
// calls lastLines(content, 50) AFTER this chokepoint, so head-keeping
// truncation would silently return the head of an already-truncated window
// instead of the true tail, per Q2).

// TestSanitizeHomeVolumeContent_PreservesNewlinesAndTabs pins the
// multi-line/head-vs-tail-keeping distinction from sanitizeEventDeliveryMessage:
// unlike the single-line wrapper (where \n is stripped, since unicode.IsPrint
// excludes it), the multi-line wrapper must keep every "\n" and "\t" verbatim.
func TestSanitizeHomeVolumeContent_PreservesNewlinesAndTabs(t *testing.T) {
	input := "line one\tindented\nline two\nline three"
	got := sanitizeHomeVolumeContent(input)
	if got != input {
		t.Errorf("sanitizeHomeVolumeContent(%q) = %q, want newlines/tabs preserved verbatim: %q", input, got, input)
	}
}

// TestSanitizeHomeVolumeContent_StripsControlAndBidiCharacters pins the same
// C0/DEL/C1/bidi-format stripping sanitizeEventDeliveryMessage already
// applies, while still preserving "\n"/"\t" alongside the stripped bytes.
func TestSanitizeHomeVolumeContent_StripsControlAndBidiCharacters(t *testing.T) {
	input := "safe\x1btext\u009bmore\x7fend\u202etail\nkeep\tme"
	got := sanitizeHomeVolumeContent(input)

	for _, bad := range []string{"\x1b", "\u009b", "\x7f", "\u202e"} {
		if strings.Contains(got, bad) {
			t.Errorf("sanitizeHomeVolumeContent(%q) = %q; must not contain stripped char %q", input, got, bad)
		}
	}
	for _, good := range []string{"\n", "\t"} {
		if !strings.Contains(got, good) {
			t.Errorf("sanitizeHomeVolumeContent(%q) = %q; must preserve %q", input, got, good)
		}
	}
}

// TestSanitizeHomeVolumeContent_TailKeepingTruncation_ValidUTF8MarkerAtFrontWithinCap
// pins Q2's truncation shape: tail-keeping (the END of the content survives,
// not the start), a visible truncation marker at the FRONT of the retained
// text, the marker's own bytes come out of the length budget (total output
// length always <= maxHomeVolumeContentLen), and the cut is rune-boundary
// safe (never splits a multi-byte UTF-8 character).
func TestSanitizeHomeVolumeContent_TailKeepingTruncation_ValidUTF8MarkerAtFrontWithinCap(t *testing.T) {
	var b strings.Builder
	b.WriteString("HEAD-SENTINEL-MUST-BE-DROPPED\n")
	for i := 0; i < 2000; i++ {
		b.WriteString("filler-line-padding-padding-padding-padding\n")
	}
	b.WriteString("€€€TAIL-SENTINEL-MUST-SURVIVE") // leading multi-byte runes near the retained window's start
	input := b.String()

	got := sanitizeHomeVolumeContent(input)

	if !utf8.ValidString(got) {
		t.Fatalf("sanitizeHomeVolumeContent truncated output is not valid UTF-8: %q", got)
	}
	if len(got) > maxHomeVolumeContentLen {
		t.Errorf("sanitizeHomeVolumeContent output length = %d, want <= %d", len(got), maxHomeVolumeContentLen)
	}
	if strings.Contains(got, "HEAD-SENTINEL-MUST-BE-DROPPED") {
		t.Errorf("expected the head to be dropped (tail-keeping truncation), got a prefix of:\n%.200s", got)
	}
	if !strings.Contains(got, "TAIL-SENTINEL-MUST-SURVIVE") {
		t.Errorf("expected the tail to survive (tail-keeping truncation), got a suffix of:\n%s", got[max(0, len(got)-200):])
	}
	if !strings.Contains(got[:min(200, len(got))], "truncat") {
		t.Errorf("expected a visible truncation marker at the FRONT of the retained text, got:\n%.200s", got)
	}
}

// TestSanitizeHomeVolumeContent_SmallInputNeverTruncated is the sibling
// happy-path: content under the cap must pass through unchanged (byte for
// byte, aside from the control/bidi stripping already covered above), never
// gaining a spurious truncation marker.
func TestSanitizeHomeVolumeContent_SmallInputNeverTruncated(t *testing.T) {
	input := "line one\nline two\nline three"
	got := sanitizeHomeVolumeContent(input)
	if got != input {
		t.Errorf("sanitizeHomeVolumeContent(%q) = %q, want unchanged (under the cap, nothing to truncate)", input, got)
	}
}

// TestDiagnose_RecentLogs_OversizedContent_TailKept is the end-to-end proof
// (item 6/7) that the chokepoint's tail-keeping truncation actually reaches
// a genuinely multi-line Diagnose report field: a sentinel on the log tail's
// FINAL line must survive, a sentinel on its FIRST line must not, and the
// rendered report must retain more than one line (proving the content
// stayed multi-line rather than collapsing to a single line). The "Recent
// logs (tail):" section (containerLogsTail) is used rather than "Nested
// Docker:" because #1163's security review (Fix 2) added a SECOND,
// single-line resanitize pass on top of the chokepoint's multi-line one for
// nestedDockerFinding specifically (that field is genuinely one line) — so
// "Nested Docker:" no longer tail-keeps oversized content end-to-end, while
// "Recent logs (tail):" legitimately stays multi-line and still does.
func TestDiagnose_RecentLogs_OversizedContent_TailKept(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))

	var b strings.Builder
	b.WriteString("HEAD-SENTINEL-SHOULD-BE-DROPPED\n")
	for i := 0; i < 2000; i++ {
		b.WriteString("filler-line-padding-padding-padding-padding\n")
	}
	b.WriteString("TAIL-SENTINEL-SHOULD-SURVIVE")
	t.Setenv("FAKE_LOGS", b.String())

	var stdout, stderr bytes.Buffer
	e := &Engine{Runtime: "docker", Stdout: &stdout, Stderr: &stderr}

	if err := e.Diagnose(dindDiagnoseScope()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	if strings.Contains(out, "HEAD-SENTINEL-SHOULD-BE-DROPPED") {
		t.Errorf("expected the oversized log tail's head to be dropped (tail-keeping truncation), got:\n%.500s", out)
	}
	if !strings.Contains(out, "TAIL-SENTINEL-SHOULD-SURVIVE") {
		t.Errorf("expected the oversized log tail's tail to survive (tail-keeping truncation), got:\n%s", out)
	}
	if strings.Count(out, "\n") < 2 {
		t.Errorf("expected the rendered log tail to stay multi-line (more than one line), got:\n%s", out)
	}
}

// TestDiagnose_NestedDocker_MarkerWithEmbeddedNewline_RenderedSingleLine
// pins #1163's security-review Fix 2: nestedDockerFinding's "Nested Docker:"
// line and finding.Message are genuinely single-line report fields, but the
// dockerd-startup-error marker is container-writable — a compromised
// container could otherwise embed a "\n" to forge a second, legitimate-
// looking report line (e.g. a fake "Nested Docker: no failure recorded" or
// a fake "[fatal] CENCI-..." finding). The marker's content is re-sanitized
// with the single-line/head-keeping sanitizer on top of the chokepoint's
// multi-line one specifically to close this: no "\n" byte may reach the
// rendered line, so the forged second line must never appear.
func TestDiagnose_NestedDocker_MarkerWithEmbeddedNewline_RenderedSingleLine(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_VOLUMES", "claude-cenci-dind-test\n")

	const forgedLine = "Nested Docker: no failure recorded"
	marker := "dockerd exited with status 1: read-only file system\n" + forgedLine
	t.Setenv("FAKE_DOCKERD_MARKER", marker)

	var stdout, stderr bytes.Buffer
	e := &Engine{Runtime: "docker", Stdout: &stdout, Stderr: &stderr}

	if err := e.Diagnose(dindDiagnoseScope()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	nestedDockerLines := 0
	for _, line := range strings.Split(out, "\n") {
		if line == forgedLine {
			t.Errorf("the marker's embedded \"\\n\" forged a standalone %q report line; got full output:\n%s", forgedLine, out)
		}
		if strings.HasPrefix(line, "Nested Docker:") {
			nestedDockerLines++
		}
	}
	// Exactly one PHYSICAL LINE may start with "Nested Docker:" -- the
	// forged text is expected to still appear, glued onto that same line
	// with no separator (the sanitizer strips the "\n", it doesn't drop
	// content), so a plain substring count of "Nested Docker:" isn't the
	// right assertion here (it would also match the forged text itself).
	if nestedDockerLines != 1 {
		t.Errorf("expected exactly one line starting with \"Nested Docker:\" even with an embedded newline in the marker, got %d in:\n%s", nestedDockerLines, out)
	}
}

// TestDiagnose_NestedDocker_MarkerControlAndBidiBytes_SanitizedInStdout and
// TestDiagnose_RecentLogs_ControlAndBidiBytes_SanitizedInStdout pin item 9:
// control/bidi bytes crossing the container->host seam via the
// dockerd-startup-error marker or the `logs --tail` fallback must never
// reach the terminal raw.

func TestDiagnose_NestedDocker_MarkerControlAndBidiBytes_SanitizedInStdout(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))
	t.Setenv("FAKE_VOLUMES", "claude-cenci-dind-test\n")
	payload := "boom \x1b[2J \x7f \u202e attack"
	t.Setenv("FAKE_DOCKERD_MARKER", payload)

	var stdout, stderr bytes.Buffer
	e := &Engine{Runtime: "docker", Stdout: &stdout, Stderr: &stderr}

	if err := e.Diagnose(dindDiagnoseScope()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	for _, bad := range []string{"\x1b", "\x7f", "\u202e"} {
		if strings.Contains(out, bad) {
			t.Errorf("expected no raw control/bidi byte %q in Diagnose stdout, got:\n%q", bad, out)
		}
	}
}

func TestDiagnose_RecentLogs_ControlAndBidiBytes_SanitizedInStdout(t *testing.T) {
	fakeDir := t.TempDir()
	callLog := filepath.Join(fakeDir, "calls.txt")
	writeFakeRuntime(t, fakeDir, "docker", callLog)
	t.Setenv("PATH", fakeDir+":"+os.Getenv("PATH"))
	payload := "boot ok \x1b[2J \x7f \u202e done"
	t.Setenv("FAKE_LOGS", payload)

	var stdout, stderr bytes.Buffer
	e := &Engine{Runtime: "docker", Stdout: &stdout, Stderr: &stderr}

	if err := e.Diagnose(dindDiagnoseScope()); err != nil {
		t.Fatalf("Diagnose: %v", err)
	}

	out := stdout.String()
	for _, bad := range []string{"\x1b", "\x7f", "\u202e"} {
		if strings.Contains(out, bad) {
			t.Errorf("expected no raw control/bidi byte %q in Diagnose stdout (Recent logs section), got:\n%q", bad, out)
		}
	}
}

// -- ticket #1163 Q4: pluginManifestVersion's three-state display ----------

// TestPluginManifestVersionDisplay pins the new three-state display helper
// pluginManifestVersion's caller uses: a successful read surfaces its
// content verbatim, a legitimately absent manifest (or opencode's clean
// short-circuit) surfaces the plain "unknown", and a read failure surfaces
// the distinct "unknown (read failed)" — the exact literal text Q4 pins.
func TestPluginManifestVersionDisplay(t *testing.T) {
	cases := []struct {
		name    string
		content string
		status  homeVolumeReadStatus
		want    string
	}{
		{"successful read", "1.2.3", homeVolumeReadOK, "1.2.3"},
		{"legitimately absent (no manifest, or opencode's clean short-circuit)", "", homeVolumeReadAbsent, "unknown"},
		{"read failed", "", homeVolumeReadFailed, "unknown (read failed)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pluginManifestVersionDisplay(tc.content, tc.status); got != tc.want {
				t.Errorf("pluginManifestVersionDisplay(%q, %v) = %q, want %q", tc.content, tc.status, got, tc.want)
			}
		})
	}
}

// TestDiagnose_PluginManifestReadFailed_UnitContract drives
// pluginManifestVersion/pluginManifestVersionDisplay directly (the unit-level
// contract): writeFakeRuntime carries no marketplace.json arm at all, so
// Diagnose's end-to-end "Plugin manifest version:" read-failed case is
// covered by the subprocess suite in watch/diagnose_test.go instead
// (TestDiagnose_PluginManifest_ReadFailed).
func TestDiagnose_PluginManifestReadFailed_UnitContract(t *testing.T) {
	e := &Engine{Runtime: "cenci-test-runtime-does-not-exist-1163", Stderr: io.Discard}
	scope := Scope{
		ContainerName: "claude-cenci-test",
		VolumeName:    "claude-cenci-home-test",
		Image:         "cenci-sandbox:test",
	}

	_, status := e.pluginManifestVersion(scope, "claude")
	if status != homeVolumeReadFailed {
		t.Errorf("pluginManifestVersion(claude) status = %v, want homeVolumeReadFailed when the runtime binary cannot be invoked", status)
	}
	if got := pluginManifestVersionDisplay("", status); got != "unknown (read failed)" {
		t.Errorf("pluginManifestVersionDisplay(\"\", homeVolumeReadFailed) = %q, want %q", got, "unknown (read failed)")
	}
}
