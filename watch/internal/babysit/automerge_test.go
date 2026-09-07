package babysit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// -- test helpers -------------------------------------------------------------

// intp returns a pointer to n, for policyBlock's pointer-typed cap fields.
func intp(n int) *int { return &n }

// greenAutomergeInputs returns an automergeInputs value that satisfies every
// stage of the condition chain -- the starting point for the condition-matrix
// table below, which mutates exactly one field per case to isolate a single
// reason. A fresh struct (with its own *effectivePolicy) is returned on every
// call so mutating a subtest's copy never leaks into another subtest (#822 --
// a shared pointer would let one case's mutation silently corrupt another's).
func greenAutomergeInputs() automergeInputs {
	return automergeInputs{
		Enabled:          true,
		ClosingIssues:    []int{9},
		IssueLabels:      map[int][]string{9: {"automerge:ok"}},
		Checks:           []check{{Bucket: "pass", Name: "test"}},
		RepairPending:    false,
		PendingKeys:      nil,
		CommentsComplete: true,
		ReviewsComplete:  true,
		IsDraft:          false,
		Mergeable:        "MERGEABLE",
		HeadRefOID:       "abc",
		ChangedFiles:     2,
		Additions:        10,
		Deletions:        5,
		Files: []string{
			"watch/internal/babysit/automerge.go",
			"watch/internal/babysit/automerge_test.go",
		},
		Policy: &effectivePolicy{
			ProtectedPaths:  []string{"*secret*"},
			MaxChangedFiles: 10,
			MaxDiffLines:    500,
			MergeMethod:     "squash",
		},
		MergeMethod:    "squash",
		AllowedMethods: map[string]bool{"squash": true, "merge": false, "rebase": true},
	}
}

// withScriptedCommands replaces the package's execGh seam (for every `gh`
// invocation) and its command seam (for the package's remaining non-`gh`
// calls: `git rev-parse` and the `cenci run` self-exec) for the test's
// duration. script serves each `gh` call in order, each entry providing
// both an output body and an error so a rejected `gh pr merge` can be
// modeled -- unlike babysit_test.go's withCommands, which always returns a
// nil error. An error entry delivers its body on stderr (never stdout), so
// existing Detail assertions built on a captured `gh pr merge` failure
// still hold once execGh's separated-stream contract replaces the old
// CombinedOutput-shaped `command` seam. Every invocation, gh or not, is
// recorded into calls in call order.
func withScriptedCommands(t *testing.T, script []scriptedCall, calls *[][]string) {
	t.Helper()
	originalCommand := command
	originalExecGh := execGh
	i := 0
	command = func(name string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string{name}, args...))
		return []byte(""), nil
	}
	execGh = func(args ...string) (string, string, error) {
		*calls = append(*calls, append([]string{"gh"}, args...))
		if i >= len(script) {
			return "", "", fmt.Errorf("unexpected command: %s", strings.Join(args, " "))
		}
		c := script[i]
		i++
		// #923: a non-empty stderr always serves out on stdout and stderr on
		// stderr, regardless of err -- the only way to script "nonzero exit
		// with valid JSON on stdout AND diagnostic text on stderr" at once,
		// which every one of gh pr checks' documented exit-8/exit-1 shapes
		// needs and the err-only convention below cannot express (an error
		// entry's body always landed on stderr, never stdout). Leaving
		// stderr empty (the zero value) preserves today's exact behavior for
		// every pre-existing call site.
		if c.stderr != "" {
			return c.out, c.stderr, c.err
		}
		if c.err != nil {
			return "", c.out, c.err
		}
		return c.out, "", nil
	}
	t.Cleanup(func() {
		command = originalCommand
		execGh = originalExecGh
	})
}

type scriptedCall struct {
	out string
	err error
	// stderr is additive (#923): see withScriptedCommands' routing rule
	// above. Every pre-#923 call site leaves it unset ("") and is
	// unaffected.
	stderr string
}

// withFleetAutomergeEnabled points the fleetConfigPath seam at a temp fleet
// config file with automerge.enabled set to enabled, restoring the previous
// seam value (the package TestMain's "" pin) on cleanup.
func withFleetAutomergeEnabled(t *testing.T, enabled bool) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	body := fmt.Sprintf(`{"automerge":{"enabled":%v}}`, enabled)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	original := fleetConfigPath
	fleetConfigPath = func() string { return path }
	t.Cleanup(func() { fleetConfigPath = original })
}

// withFleetAutomergeSequence installs a fleetConfigPath seam that returns a
// distinct temp path on each call, serving bodies in order and repeating the
// last body once exhausted (#886) -- so a test can script the fleet kill
// switch's first-pass read and its separate final pre-merge recheck read
// (recheckAutomergeInputs' loadFleetSwitch() call) with different content.
// Two sentinel bodies get special handling instead of being written as
// literal file content: "missing" points fleetConfigPath at a path that was
// never created (os.ReadFile -> fs.ErrNotExist), and "unreadable" points it
// at a directory rather than a file (os.ReadFile on a directory always fails
// with EISDIR -- root-proof, unlike chmod 000).
func withFleetAutomergeSequence(t *testing.T, bodies ...string) {
	t.Helper()
	dir := t.TempDir()
	call := 0
	original := fleetConfigPath
	fleetConfigPath = func() string {
		idx := call
		if idx >= len(bodies) {
			idx = len(bodies) - 1
		}
		call++
		switch bodies[idx] {
		case "missing":
			return filepath.Join(dir, fmt.Sprintf("missing-%d.json", idx))
		case "unreadable":
			sub := filepath.Join(dir, fmt.Sprintf("unreadable-%d", idx))
			if err := os.MkdirAll(sub, 0700); err != nil {
				t.Fatal(err)
			}
			return sub
		default:
			path := filepath.Join(dir, fmt.Sprintf("config-%d.json", idx))
			if err := os.WriteFile(path, []byte(bodies[idx]), 0600); err != nil {
				t.Fatal(err)
			}
			return path
		}
	}
	t.Cleanup(func() { fleetConfigPath = original })
}

// automergeEligiblePR is a `pr view` fixture with every field the automerge
// gate chain needs already at its green value: not a draft, MERGEABLE, one
// changed file matching changedFiles (no truncation), and a base ref distinct
// from the head ref -- so a test asserting the policy fetch used baseRefName
// (never headRefName) has something to actually distinguish.
func automergeEligiblePR() string {
	return `{"number":42,"title":"Change","state":"OPEN","headRefName":"feature","headRefOid":"abc","baseRefName":"main","mergeable":"MERGEABLE","isDraft":false,"changedFiles":1,"additions":5,"deletions":2,"files":[{"path":"watch/internal/babysit/x.go"}],"url":"https://example/pr/42","closingIssuesReferences":[{"number":9}]}`
}

// queueProbeResponse renders the merge-queue GraphQL probe's response body
// (merge.go's fetchMergeQueueState): inQueue/enabled feed automergeQueueHold,
// and headRefOID doubles as runAutomerge's pre-merge head-SHA re-check
// (Decision 9) -- the same lazy probe serves both purposes at no extra `gh`
// call cost.
func queueProbeResponse(inQueue, enabled bool, headRefOID string) string {
	return fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"isInMergeQueue":%v,"isMergeQueueEnabled":%v,"headRefOid":%q}}}}`, inQueue, enabled, headRefOID)
}

// automergeFirstPassScript renders the first, genuine full-evaluation
// pass's script (Decisions 1-4): pr view, checks, comments, reviews,
// closing-issue labels, base-ref policy, repo allowed-methods, merge-queue
// probe -- every field green, head commit pinned to automergeEligiblePR's
// "abc" throughout -- the common prefix every merge-eligible tick test
// shares before #854's pre-merge re-evaluation (recheckAutomergeInputs)
// fetches everything a second time.
func automergeFirstPassScript() []scriptedCall {
	return []scriptedCall{
		{out: automergeEligiblePR()},
		{out: `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`},
		{out: `[]`},
		{out: `[]`},
		{out: `{"labels":[{"name":"automerge:ok"}]}`},
		{out: `{"automerge":{"maxChangedFiles":10,"maxDiffLines":500,"mergeMethod":"squash"}}`},
		{out: `{"squash":true,"merge":false,"rebase":true}`},
		{out: queueProbeResponse(false, false, "abc")},
	}
}

// automergeGreenRecheckScript renders the pre-merge re-evaluation's own
// full re-fetch (Decision 9, #854, merge.go's recheckAutomergeInputs): pr
// view, checks, comments, reviews, closing-issue labels, base-ref policy,
// merge-queue probe -- no allowed-methods re-fetch, since that carries over
// unchanged from the first pass. Every field green by default (matching
// automergeFirstPassScript's evidence), for tests that only care about the
// merge/verification path beyond the re-check.
func automergeGreenRecheckScript() []scriptedCall {
	return automergeRecheckScript(
		automergeEligiblePR(),
		`[{"bucket":"pass","name":"test","state":"SUCCESS"}]`,
		`[]`,
		`[]`,
		`{"labels":[{"name":"automerge:ok"}]}`,
		`{"automerge":{"maxChangedFiles":10,"maxDiffLines":500,"mergeMethod":"squash"}}`,
		queueProbeResponse(false, false, "abc"),
	)
}

// automergeRecheckScript renders the pre-merge re-evaluation's full re-
// fetch script from explicit bodies, so a test can mutate exactly one call
// to isolate the recheck holding on that single piece of newly-changed
// evidence.
func automergeRecheckScript(pr, checks, comments, reviews, labels, policy, queue string) []scriptedCall {
	return []scriptedCall{
		{out: pr},
		{out: checks},
		{out: comments},
		{out: reviews},
		{out: labels},
		{out: policy},
		{out: queue},
	}
}

// hasArg reports whether call contains arg anywhere after the command name.
func hasArg(call []string, arg string) bool {
	for _, a := range call {
		if a == arg {
			return true
		}
	}
	return false
}

// argValue returns the value immediately following the first occurrence of
// flag in call, and whether flag was found at all -- for asserting an exact
// flag=value pair (e.g. "--match-head-commit abc") rather than just the
// flag's bare presence.
func argValue(call []string, flag string) (string, bool) {
	for i, a := range call {
		if a == flag && i+1 < len(call) {
			return call[i+1], true
		}
	}
	return "", false
}

// -- evaluateAutomerge: condition matrix (#824) ------------------------------
//
// One case per reason constant in the condition chain, each starting from an
// all-green automergeInputs and mutating exactly one field so the case
// isolates that single reason -- per watch/docs/error-handling.md #446, the
// assertion is the exact reason string, never a bare "denied" check, so a
// regression collapsing two failure classes into the same reason is caught.
func TestEvaluateAutomergeConditionMatrix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*automergeInputs)
		wantReason string
	}{
		{"fleet automerge disabled", func(in *automergeInputs) { in.Enabled = false }, reasonAutomergeDisabled},
		{"no closing issue", func(in *automergeInputs) { in.ClosingIssues = nil }, reasonNoClosingIssue},
		{"closing issue labels unreadable", func(in *automergeInputs) { in.LabelsErr = errors.New("boom") }, reasonLabelUnreadable},
		{"closing issue lacks automerge:ok", func(in *automergeInputs) { in.IssueLabels = map[int][]string{9: {"bug"}} }, reasonLabelMissing},
		{"no CI checks at all", func(in *automergeInputs) { in.Checks = nil }, reasonNoChecks},
		{"CI not green (pending)", func(in *automergeInputs) { in.Checks = []check{{Bucket: "pending"}} }, reasonCICheckPending},
		{"CI repair pending", func(in *automergeInputs) { in.RepairPending = true }, reasonRepairPending},
		{"comments detection read truncated", func(in *automergeInputs) { in.CommentsComplete = false }, reasonCommentsReadTruncated},
		{"reviews detection read truncated", func(in *automergeInputs) { in.ReviewsComplete = false }, reasonReviewsReadTruncated},
		{"review feedback pending", func(in *automergeInputs) { in.PendingKeys = []string{"comment:1"} }, reasonReviewPending},
		// The four new #850 feedback-hold reasons: FeedbackHold is checked
		// after RepairPending and before len(PendingKeys), so any of these
		// four never masquerades as the ordinary reasonReviewPending hold.
		{"review feedback state unreadable (API error)", func(in *automergeInputs) { in.FeedbackHold = reasonFeedbackUnreadable }, reasonFeedbackUnreadable},
		{"review feedback state truncated (incomplete pagination)", func(in *automergeInputs) { in.FeedbackHold = reasonFeedbackTruncated }, reasonFeedbackTruncated},
		{"review feedback state unknown (absent thread/review or unknown review state)", func(in *automergeInputs) { in.FeedbackHold = reasonReviewStateUnknown }, reasonReviewStateUnknown},
		{"unsupported review feedback type", func(in *automergeInputs) { in.FeedbackHold = reasonFeedbackUnsupported }, reasonFeedbackUnsupported},
		{"draft PR", func(in *automergeInputs) { in.IsDraft = true }, reasonDraft},
		{"mergeable state UNKNOWN", func(in *automergeInputs) { in.Mergeable = "UNKNOWN" }, reasonMergeableUnknown},
		{"mergeable state CONFLICTING", func(in *automergeInputs) { in.Mergeable = "CONFLICTING" }, reasonNotMergeable},
		{"head commit SHA unknown", func(in *automergeInputs) { in.HeadRefOID = "" }, reasonHeadSHAUnknown},
		{"no changed files", func(in *automergeInputs) { in.ChangedFiles = 0 }, reasonNoChanges},
		{"diff file list truncated", func(in *automergeInputs) { in.Files = in.Files[:1] }, reasonDiffTruncated},
		{"policy unreadable", func(in *automergeInputs) { in.PolicyErr = errors.New("boom") }, reasonPolicyUnreadable},
		{"policy absent", func(in *automergeInputs) { in.Policy = nil; in.PolicyReason = reasonPolicyAbsent }, reasonPolicyAbsent},
		{"policy malformed", func(in *automergeInputs) { in.Policy = nil; in.PolicyReason = reasonPolicyMalformed }, reasonPolicyMalformed},
		{"too many changed files", func(in *automergeInputs) { in.Policy.MaxChangedFiles = 1 }, reasonTooManyFiles},
		{"too many changed lines", func(in *automergeInputs) { in.Policy.MaxDiffLines = 1 }, reasonTooManyLines},
		{"touches a protected path", func(in *automergeInputs) {
			in.Policy.ProtectedPaths = []string{"watch/internal/babysit/*"}
		}, reasonProtectedPath},
		{"merge method is not squash", func(in *automergeInputs) { in.MergeMethod = "merge" }, reasonMergeMethodNotSquash},
		{"merge method disallowed by repo", func(in *automergeInputs) {
			in.AllowedMethods = map[string]bool{"squash": false, "merge": true, "rebase": true}
		}, reasonMergeMethodDisallowed},
		{"repo allowed merge methods unknown", func(in *automergeInputs) {
			in.AllowedMethodsErr = errors.New("boom")
		}, reasonMergeMethodUnknown},
		{"already in the merge queue", func(in *automergeInputs) {
			t := true
			f := false
			in.QueueInMergeQueue = &t
			in.QueueEnabled = &f
		}, reasonMergeQueueRequired},
		{"merge queue state unknown (probe error)", func(in *automergeInputs) { in.QueueErr = errors.New("boom") }, reasonMergeQueueUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := greenAutomergeInputs()
			tc.mutate(&in)
			got := evaluateAutomerge(in)
			if got.Merge {
				t.Fatalf("evaluateAutomerge(%s).Merge = true, want false", tc.name)
			}
			if got.Reason != tc.wantReason {
				t.Fatalf("evaluateAutomerge(%s).Reason = %q, want %q", tc.name, got.Reason, tc.wantReason)
			}
		})
	}
}

// TestEvaluateAutomergeRequiresEveryClosingIssueLabeled pins the "all" in
// "all closing issues carry automerge:ok" -- a PR closing two issues where
// only one carries the grant must still be denied, not merged on a
// first-match basis.
func TestEvaluateAutomergeRequiresEveryClosingIssueLabeled(t *testing.T) {
	in := greenAutomergeInputs()
	in.ClosingIssues = []int{9, 10}
	in.IssueLabels = map[int][]string{9: {"automerge:ok"}, 10: {"bug"}}
	got := evaluateAutomerge(in)
	if got.Merge {
		t.Fatal("Merge = true, want false: issue #10 lacks automerge:ok")
	}
	if got.Reason != reasonLabelMissing {
		t.Fatalf("Reason = %q, want %q", got.Reason, reasonLabelMissing)
	}
}

// TestEvaluateAutomergeAllGreenMerges is the matrix's counterpart all-pass
// case: Merge is true and every stage's condition reports Reached && Pass, so
// the full-verdict Conditions slice used by logLine's [k=v ...] rendering is
// actually populated end to end, not just up to the first (nonexistent)
// failure.
func TestEvaluateAutomergeAllGreenMerges(t *testing.T) {
	got := evaluateAutomerge(greenAutomergeInputs())
	if !got.Merge {
		t.Fatalf("Merge = false (reason %q), want true", got.Reason)
	}
	wantStages := []string{"enabled", "label", "ci", "review", "mergeable", "headsha", "files", "policy", "filecap", "lines", "protected", "method", "queue"}
	seen := map[string]bool{}
	for _, c := range got.Conditions {
		if !c.Reached || !c.Pass {
			t.Errorf("condition %q = %+v, want Reached=true Pass=true on the all-green path", c.Key, c)
		}
		seen[c.Key] = true
	}
	for _, k := range wantStages {
		if !seen[k] {
			t.Errorf("Conditions never recorded stage %q: %#v", k, got.Conditions)
		}
	}
}

// TestEvaluateAutomergeTooManyFilesRendersDistinctFilecapKey pins the fix for
// a log-rendering bug: stage 8's file-count cap check used to reuse stage 6's
// "files" key, so a hold on reasonTooManyFiles still rendered "files=yes" in
// the log line (stage 6's earlier passing entry won conditionSymbol's
// first-match lookup). The cap check must render under its own "filecap" key
// so the log line accurately reflects which check actually failed.
func TestEvaluateAutomergeTooManyFilesRendersDistinctFilecapKey(t *testing.T) {
	in := greenAutomergeInputs()
	in.Policy.MaxChangedFiles = 1
	got := evaluateAutomerge(in)
	if got.Reason != reasonTooManyFiles {
		t.Fatalf("Reason = %q, want %q", got.Reason, reasonTooManyFiles)
	}
	line := got.logLine()
	if !strings.Contains(line, "filecap=no") {
		t.Errorf("logLine() = %q, want it to contain %q", line, "filecap=no")
	}
	if !strings.Contains(line, "files=yes") {
		t.Errorf("logLine() = %q, want it to still contain %q (stage 6's changedFiles>0/no-truncation check passed)", line, "files=yes")
	}
}

// TestEvaluateAutomergeSurfacesFetchErrorDetail pins that the three wrapped
// fetch errors (LabelsErr/PolicyErr/AllowedMethodsErr) -- which already wrap
// the underlying `gh` output via %w -- have their .Error() text surfaced onto
// the decision's Detail field, not silently collapsed into just the generic
// reason constant.
func TestEvaluateAutomergeSurfacesFetchErrorDetail(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*automergeInputs)
		wantReason string
		detail     func(automergeInputs) string
	}{
		{
			"closing issue labels unreadable surfaces detail",
			func(in *automergeInputs) { in.LabelsErr = errors.New("gh issue view: exit status 1: rate limited") },
			reasonLabelUnreadable,
			func(in automergeInputs) string { return in.LabelsErr.Error() },
		},
		{
			"policy unreadable surfaces detail",
			func(in *automergeInputs) { in.PolicyErr = errors.New("automerge policy unreadable: 404 Not Found") },
			reasonPolicyUnreadable,
			func(in automergeInputs) string { return in.PolicyErr.Error() },
		},
		{
			"allowed merge methods unreadable surfaces detail",
			func(in *automergeInputs) {
				in.AllowedMethodsErr = errors.New("gh api repos/o/r: exit status 1: auth failure")
			},
			reasonMergeMethodUnknown,
			func(in automergeInputs) string { return in.AllowedMethodsErr.Error() },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := greenAutomergeInputs()
			tc.mutate(&in)
			got := evaluateAutomerge(in)
			if got.Reason != tc.wantReason {
				t.Fatalf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			wantDetail := tc.detail(in)
			if got.Detail != wantDetail {
				t.Fatalf("Detail = %q, want %q", got.Detail, wantDetail)
			}
		})
	}
}

// TestEvaluateAutomergeSanitizesMultilineAndLongDetail pins that a raw `gh`
// failure message -- with embedded newlines, or exceeding the bounded length
// -- is sanitized before landing on Detail (newlines collapsed to spaces,
// truncated with a "..." suffix), and that the resulting log line stays a
// single line: Detail is raw, unbounded `gh` output, but it feeds logLine's
// single-line-per-tick format that downstream tooling substring-classifies.
func TestEvaluateAutomergeSanitizesMultilineAndLongDetail(t *testing.T) {
	in := greenAutomergeInputs()
	longTail := strings.Repeat("x", 250)
	in.LabelsErr = fmt.Errorf("boom:\nsecond line\nthird line: %s", longTail)
	got := evaluateAutomerge(in)
	if got.Reason != reasonLabelUnreadable {
		t.Fatalf("Reason = %q, want %q", got.Reason, reasonLabelUnreadable)
	}
	if strings.Contains(got.Detail, "\n") {
		t.Fatalf("Detail = %q, must not contain an embedded newline", got.Detail)
	}
	if len(got.Detail) > detailMaxLen+len("...") {
		t.Fatalf("Detail length = %d, want capped at roughly %d", len(got.Detail), detailMaxLen)
	}
	if !strings.HasSuffix(got.Detail, "...") {
		t.Fatalf("Detail = %q, want a truncated value to end with \"...\"", got.Detail)
	}
	line := got.logLine()
	if strings.Contains(line, "\n") {
		t.Fatalf("logLine() = %q, must render as a single line", line)
	}
}

// TestSanitizeDetailStripsAllControlCharactersNotJustNewline pins #854's
// item 5 fix: sanitizeDetail previously stripped only "\n", leaving "\r"
// and other C0 control bytes (form feed, vertical tab, ...) and DEL free to
// break the single-line logLine format or corrupt the persisted state
// file. Every rune below 0x20, plus DEL (0x7f), must be replaced.
func TestSanitizeDetailStripsAllControlCharactersNotJustNewline(t *testing.T) {
	raw := "line one\rline two\x0bline three\x0cline four\x7fend"
	got := sanitizeDetail(raw)
	for _, r := range got {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("sanitizeDetail(%q) = %q, still contains control byte %q", raw, got, r)
		}
	}
	if !strings.Contains(got, "line one") || !strings.Contains(got, "end") {
		t.Fatalf("sanitizeDetail(%q) = %q, want the surrounding text preserved", raw, got)
	}
}

// TestSanitizeDetailTruncatesOnRuneBoundary pins the polish fix to #854's
// item 2: a multi-byte UTF-8 character straddling detailMaxLen's byte
// cutoff must never be split mid-encoding. The fixture places 199 ASCII
// bytes before a run of 2-byte "é" (U+00E9) characters, so the naive
// sanitized[:detailMaxLen] byte slice (detailMaxLen==200) would cut after
// only the first byte of the first "é", leaving invalid UTF-8 in the
// persisted state file / log line.
func TestSanitizeDetailTruncatesOnRuneBoundary(t *testing.T) {
	raw := strings.Repeat("x", 199) + strings.Repeat("é", 10)
	got := sanitizeDetail(raw)
	if !utf8.ValidString(got) {
		t.Fatalf("sanitizeDetail(%q) = %q, want valid UTF-8, got invalid encoding from a mid-rune split", raw, got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("sanitizeDetail(%q) = %q, want a truncated value to end with \"...\"", raw, got)
	}
	if len(got) > detailMaxLen+len("...") {
		t.Fatalf("sanitizeDetail(%q) length = %d, want capped at roughly %d", raw, len(got), detailMaxLen)
	}
}

// -- automergeCIHold / automergeQueueHold: pure classifiers (#854) -----------
//
// Unit tests, per the plan's Test Strategy ("unit-level only for pure
// classifiers and the pagination helper") -- these two functions mirror
// evaluateAutomerge's pure, I/O-free discipline (automerge.go's automergeCIHold/
// automergeQueueHold doc comments), so the bucket/queue matrices are testable
// without any `gh` scripting.

// TestAutomergeCIHoldBucketMatrix pins the strict pass-only CI classifier
// (Decisions 1/2): every bucket combination holds under its own distinct
// reason, per watch/docs/error-handling.md #446 -- the assertion is the
// exact reason string, never a bare pass/fail check, so a regression
// collapsing two failure classes into the same reason is caught.
func TestAutomergeCIHoldBucketMatrix(t *testing.T) {
	for _, tc := range []struct {
		name   string
		checks []check
		want   string
	}{
		{"zero checks at all", nil, reasonNoChecks},
		{"all pass merges", []check{{Bucket: "pass"}, {Bucket: "pass"}}, ""},
		{"single pass merges", []check{{Bucket: "pass"}}, ""},
		{"any fail holds", []check{{Bucket: "pass"}, {Bucket: "fail"}}, reasonCICheckFailed},
		{"any pending holds", []check{{Bucket: "pass"}, {Bucket: "pending"}}, reasonCICheckPending},
		{"any cancel holds", []check{{Bucket: "pass"}, {Bucket: "cancel"}}, reasonCICheckCancelled},
		// #1129 AC 1: at least one pass plus any number of skipping is green
		// -- paths-filtered monorepo checks must not hold automerge forever.
		{"pass plus skipping is green (#1129 AC 1)", []check{{Bucket: "pass"}, {Bucket: "skipping"}}, ""},
		{"multiple pass plus multiple skipping is green (#1129 AC 1)", []check{{Bucket: "pass"}, {Bucket: "pass"}, {Bucket: "skipping"}, {Bucket: "skipping"}, {Bucket: "skipping"}}, ""},
		// #1129 AC 2: zero pass, only skipping, holds under its own distinct
		// reason -- separate from reasonNoChecks (zero checks at all).
		{"all skipping with zero pass holds under its own reason (#1129 AC 2)", []check{{Bucket: "skipping"}, {Bucket: "skipping"}}, reasonCIAllChecksSkipped},
		{"single skipping with zero pass holds under its own reason (#1129 AC 2)", []check{{Bucket: "skipping"}}, reasonCIAllChecksSkipped},
		{"empty-string bucket holds", []check{{Bucket: ""}}, reasonCIBucketEmpty},
		{"unknown future bucket holds", []check{{Bucket: "neutral"}}, reasonCIBucketUnknown},
		{"mixed: fail found before pending in appearance order", []check{{Bucket: "fail"}, {Bucket: "pending"}}, reasonCICheckFailed},
		{"mixed: pending found before cancel in appearance order", []check{{Bucket: "pending"}, {Bucket: "cancel"}}, reasonCICheckPending},
		// #1129 AC 4: skipping no longer masks a genuinely non-pass bucket --
		// this case flips from reasonCICheckSkipped (retired) to
		// reasonCIBucketUnknown, since skipping is now treated like pass in
		// the appearance-order loop and the unknown bucket is the first
		// genuinely-holding one found.
		{"skipping no longer masks an unknown bucket found after it (#1129 AC 4)", []check{{Bucket: "skipping"}, {Bucket: "neutral"}}, reasonCIBucketUnknown},
		// #1129 AC 4: skipping-masks-nothing, one case per other distinct
		// non-pass bucket, confirming skipping is transparent to every
		// existing hold reason regardless of which side of it appears.
		{"skipping does not mask a fail found after it (#1129 AC 4)", []check{{Bucket: "skipping"}, {Bucket: "fail"}}, reasonCICheckFailed},
		{"skipping does not mask a pending found after it (#1129 AC 4)", []check{{Bucket: "skipping"}, {Bucket: "pending"}}, reasonCICheckPending},
		{"skipping does not mask a cancel found after it (#1129 AC 4)", []check{{Bucket: "skipping"}, {Bucket: "cancel"}}, reasonCICheckCancelled},
		{"skipping does not mask an empty bucket found after it (#1129 AC 4)", []check{{Bucket: "skipping"}, {Bucket: ""}}, reasonCIBucketEmpty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := automergeCIHold(tc.checks)
			if got != tc.want {
				t.Fatalf("automergeCIHold(%v) = %q, want %q", tc.checks, got, tc.want)
			}
		})
	}
}

// TestAutomergeDecisionConditionBracketSkippedSuffix pins the #1129 AC 11
// diagnostic suffix: conditionBracket renders "(skipped=N)" on the "ci" key
// only when the "ci" stage was actually reached (rendered symbol "yes" or
// "no") AND SkippedChecks > 0. A zero SkippedChecks renders plain, and an
// unreached "ci" stage (rendered "-") never grows the suffix regardless of
// SkippedChecks -- byte-exact regression pin for the "-" case, since
// automerge_test.go:618 (TestAutomergeDecisionLogLine), docs/autonomous-
// loop.md, and watch/README.md's example brackets all depend on it staying
// plain.
func TestAutomergeDecisionConditionBracketSkippedSuffix(t *testing.T) {
	for _, tc := range []struct {
		name          string
		conditions    []conditionResult
		skippedChecks int
		wantCI        string
	}{
		{
			"non-zero skipped renders on a passing ci stage (ci=yes)",
			[]conditionResult{{Key: "ci", Reached: true, Pass: true}},
			6,
			"ci=yes(skipped=6)",
		},
		{
			"non-zero skipped renders on a holding ci stage (ci=no)",
			[]conditionResult{{Key: "ci", Reached: true, Pass: false}},
			9,
			"ci=no(skipped=9)",
		},
		{
			"zero skipped renders plain on a passing ci stage",
			[]conditionResult{{Key: "ci", Reached: true, Pass: true}},
			0,
			"ci=yes",
		},
		{
			"zero skipped renders plain on a holding ci stage",
			[]conditionResult{{Key: "ci", Reached: true, Pass: false}},
			0,
			"ci=no",
		},
		{
			// Regression pin: an unreached "ci" stage must render plain "-"
			// even when SkippedChecks is non-zero (e.g. computed once at the
			// top of evaluateAutomerge before an earlier stage, like
			// "label", ever reaches the ci stage at all).
			"unreached ci stage renders plain ci=- even with a non-zero skipped count",
			[]conditionResult{{Key: "label", Reached: true, Pass: false}},
			6,
			"ci=-",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := automergeDecision{Conditions: tc.conditions, SkippedChecks: tc.skippedChecks}
			bracket := d.conditionBracket()
			// Exact-token check: a space-delimited field in the bracket must
			// match wantCI exactly -- not merely strings.Contains, which
			// would also (wrongly) pass if the suffix were rendered as a
			// sibling stage rather than appended onto "ci".
			parts := strings.Fields(strings.TrimSuffix(strings.TrimPrefix(bracket, "["), "]"))
			found := false
			for _, p := range parts {
				if p == tc.wantCI {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("conditionBracket() = %q, want exactly one space-delimited token %q", bracket, tc.wantCI)
			}
		})
	}
}

// TestAutomergeQueueHoldMatrix pins the merge-queue gate (Decision 4, Q3):
// an explicit isInMergeQueue or isMergeQueueEnabled true holds under
// reasonMergeQueueRequired (never enqueue as a side effect); a probe error
// or either field left absent (nil, not merely false) holds under
// reasonMergeQueueUnknown, since absence must never be read as "not
// required". probed disambiguates the "not yet probed" lazy-fetch trial
// sentinel (both fields nil, probed == false) from a probe that genuinely
// ran and came back with nothing (both fields nil, probed == true) -- a
// real GraphQL response shaped like {"pullRequest":null} decodes to exactly
// that same both-nil shape, and previously the two were indistinguishable
// (#854 fix, item 1).
func TestAutomergeQueueHoldMatrix(t *testing.T) {
	boolp := func(b bool) *bool { return &b }
	for _, tc := range []struct {
		name    string
		inQueue *bool
		enabled *bool
		err     error
		probed  bool
		want    string
	}{
		{"already in the merge queue", boolp(true), boolp(false), nil, true, reasonMergeQueueRequired},
		{"queue required/enabled on the base branch", boolp(false), boolp(true), nil, true, reasonMergeQueueRequired},
		{"both true", boolp(true), boolp(true), nil, true, reasonMergeQueueRequired},
		{"neither required nor enqueued merges normally", boolp(false), boolp(false), nil, true, ""},
		{"GraphQL probe error", nil, nil, errors.New("boom"), true, reasonMergeQueueUnknown},
		{"isInMergeQueue field absent (nil, not false)", nil, boolp(false), nil, true, reasonMergeQueueUnknown},
		{"isMergeQueueEnabled field absent (nil, not false)", boolp(false), nil, nil, true, reasonMergeQueueUnknown},
		{"not yet probed: the lazy-fetch trial sentinel passes through", nil, nil, nil, false, ""},
		{"probed but pullRequest came back null: both fields genuinely absent after the probe ran", nil, nil, nil, true, reasonMergeQueueUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := automergeQueueHold(tc.inQueue, tc.enabled, tc.err, tc.probed)
			if got != tc.want {
				t.Fatalf("automergeQueueHold(inQueue=%v, enabled=%v, err=%v, probed=%v) = %q, want %q", tc.inQueue, tc.enabled, tc.err, tc.probed, got, tc.want)
			}
		})
	}
}

// -- automergeDecision.logLine (#824) ----------------------------------------

// TestAutomergeDecisionLogLine pins the exact one-line log format from the
// plan, including the lazyboards-safe substrings check (mainsync.go:217-219's
// rule applied to this new log line).
func TestAutomergeDecisionLogLine(t *testing.T) {
	d := automergeDecision{
		PR:     "42",
		Merge:  false,
		Reason: reasonLabelMissing,
		Conditions: []conditionResult{
			{Key: "enabled", Reached: true, Pass: true},
			{Key: "label", Reached: true, Pass: false},
		},
	}
	want := `babysit: automerge PR #42 held: ticket lacks automerge:ok [enabled=yes label=no ci=- review=- mergeable=- headsha=- policy=- files=- filecap=- lines=- protected=- method=- queue=-]`
	got := d.logLine()
	if got != want {
		t.Fatalf("logLine() = %q, want %q", got, want)
	}
	if strings.Contains(got, " skip:") || strings.Contains(got, " dispatch ") {
		t.Errorf("logLine() must avoid lazyboards' classification substrings: %q", got)
	}
}

// TestAutomergeDecisionLogLineIncludesDetail pins that a non-empty Detail
// (e.g. a rejected merge's captured `gh` output, or a wrapped fetch error's
// message) is surfaced in the rendered log line alongside the stable Reason
// constant, rather than discarded.
func TestAutomergeDecisionLogLineIncludesDetail(t *testing.T) {
	d := automergeDecision{
		PR:     "42",
		Merge:  false,
		Reason: reasonMergeFailed,
		Detail: "branch protection required review",
		Conditions: []conditionResult{
			{Key: "enabled", Reached: true, Pass: true},
		},
	}
	got := d.logLine()
	if !strings.Contains(got, reasonMergeFailed) || !strings.Contains(got, "branch protection required review") {
		t.Fatalf("logLine() = %q, want it to contain both the reason and the detail", got)
	}
}

// -- matchesProtected (#824) --------------------------------------------------
//
// Unit tests: an integration test through tick cannot economically cover
// these glob edge cases (Test Strategy table).
func TestMatchesProtected(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pattern string
		path    string
		want    bool
		wantErr bool
	}{
		{"exact literal match", "watch/internal/sandbox/", "watch/internal/sandbox/", true, false},
		{"star spans the path separator", "watch/internal/sandbox/*", "watch/internal/sandbox/deep/nested/file.go", true, false},
		{"case-insensitive", "*SECRET*", "path/to/secret.txt", true, false},
		{"metacharacters are escaped: literal dot matches", "config.json", "config.json", true, false},
		{"metacharacters are escaped: dot never matches any char", "config.json", "configXjson", false, false},
		{"no match", "*credential*", "watch/internal/sandbox/launch.go", false, false},
		{"empty pattern is malformed", "", "any/path.go", false, true},
		// A bare directory-prefix entry (trailing "/", no "*") must match
		// everything under it, not just the exact literal string -- the
		// shipped .cenci/config.json ships entries in exactly this shape
		// ("watch/internal/sandbox/", "watch/plugin/"), and without this fix
		// they can only ever match that one literal path.
		{"directory-prefix pattern matches a nested file", "watch/internal/sandbox/", "watch/internal/sandbox/launch.go", true, false},
		{"directory-prefix pattern still matches its own literal path", "watch/plugin/", "watch/plugin/", true, false},
		{"directory-prefix pattern does not match a sibling directory", "watch/plugin/", "watch/plugins/x.go", false, false},
		// A "*"-wildcard pattern must span an embedded newline in the path --
		// without the "s" (dot-matches-\n) regex flag, an attacker-
		// controlled changed-file path containing "\n" could dodge every
		// "*"-wildcard protected-path pattern.
		{"star wildcard spans an embedded newline", "watch/internal/sandbox/*", "watch/internal/sandbox/deep\nnested/file.go", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := matchesProtected(tc.pattern, tc.path)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("matchesProtected(%q, %q) err = nil, want an error (malformed pattern)", tc.pattern, tc.path)
				}
				return
			}
			if err != nil {
				t.Fatalf("matchesProtected(%q, %q) unexpected err: %v", tc.pattern, tc.path, err)
			}
			if got != tc.want {
				t.Errorf("matchesProtected(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
			}
		})
	}
}

// -- resolvePolicy (#824) -----------------------------------------------------
//
// Unit tests: monorepo prefix mapping, most-restrictive merge, mergeMethod
// conflict, malformed-cap detection, and absent-block-denies (Q2) are
// combinatorial and unreachable one-at-a-time through tick (Test Strategy
// table).
func TestResolvePolicyProjectPrefixMapping(t *testing.T) {
	cfg := repoConfigFile{
		Automerge: &policyBlock{MaxChangedFiles: intp(100), MaxDiffLines: intp(1000), MergeMethod: "squash"},
		Projects: []projectPolicy{
			{Path: "watch", Automerge: &policyBlock{MaxChangedFiles: intp(50), MaxDiffLines: intp(500), MergeMethod: "squash"}},
			{Path: "watch/internal/sandbox", Automerge: &policyBlock{MaxChangedFiles: intp(1), MaxDiffLines: intp(10), MergeMethod: "squash"}},
		},
	}

	t.Run("longest project prefix wins over a shorter one", func(t *testing.T) {
		got, reason := resolvePolicy(cfg, []string{"watch/internal/sandbox/launch.go"})
		if reason != "" {
			t.Fatalf("resolvePolicy reason = %q, want none", reason)
		}
		if got.MaxChangedFiles != 1 {
			t.Errorf("MaxChangedFiles = %d, want 1 (the longest-prefix project block)", got.MaxChangedFiles)
		}
	})

	t.Run("a project-owned file uses its own block over the top-level fallback", func(t *testing.T) {
		got, reason := resolvePolicy(cfg, []string{"watch/daemon_cmd.go"})
		if reason != "" {
			t.Fatalf("resolvePolicy reason = %q, want none", reason)
		}
		if got.MaxChangedFiles != 50 {
			t.Errorf("MaxChangedFiles = %d, want 50 (the watch project block)", got.MaxChangedFiles)
		}
	})

	t.Run("a file owned by no project falls back to the top-level block", func(t *testing.T) {
		got, reason := resolvePolicy(cfg, []string{"README.md"})
		if reason != "" {
			t.Fatalf("resolvePolicy reason = %q, want none", reason)
		}
		if got.MaxChangedFiles != 100 {
			t.Errorf("MaxChangedFiles = %d, want 100 (the top-level block)", got.MaxChangedFiles)
		}
	})
}

func TestResolvePolicyMostRestrictiveMerge(t *testing.T) {
	cfg := repoConfigFile{
		Projects: []projectPolicy{
			{Path: "a", Automerge: &policyBlock{ProtectedPaths: []string{"*secretA*"}, MaxChangedFiles: intp(10), MaxDiffLines: intp(100), MergeMethod: "squash"}},
			{Path: "b", Automerge: &policyBlock{ProtectedPaths: []string{"*secretB*"}, MaxChangedFiles: intp(5), MaxDiffLines: intp(500), MergeMethod: "squash"}},
		},
	}
	got, reason := resolvePolicy(cfg, []string{"a/x.go", "b/y.go"})
	if reason != "" {
		t.Fatalf("resolvePolicy reason = %q, want none", reason)
	}
	if got.MaxChangedFiles != 5 {
		t.Errorf("MaxChangedFiles = %d, want the min across applicable blocks (5)", got.MaxChangedFiles)
	}
	if got.MaxDiffLines != 100 {
		t.Errorf("MaxDiffLines = %d, want the min across applicable blocks (100)", got.MaxDiffLines)
	}
	wantPaths := map[string]bool{"*secretA*": true, "*secretB*": true}
	if len(got.ProtectedPaths) != len(wantPaths) {
		t.Fatalf("ProtectedPaths = %v, want the union of both blocks (%v)", got.ProtectedPaths, wantPaths)
	}
	for _, p := range got.ProtectedPaths {
		if !wantPaths[p] {
			t.Errorf("ProtectedPaths contains unexpected pattern %q", p)
		}
	}
}

func TestResolvePolicyConflictingMergeMethodIsMalformed(t *testing.T) {
	cfg := repoConfigFile{
		Projects: []projectPolicy{
			{Path: "a", Automerge: &policyBlock{MaxChangedFiles: intp(10), MaxDiffLines: intp(100), MergeMethod: "squash"}},
			{Path: "b", Automerge: &policyBlock{MaxChangedFiles: intp(10), MaxDiffLines: intp(100), MergeMethod: "rebase"}},
		},
	}
	_, reason := resolvePolicy(cfg, []string{"a/x.go", "b/y.go"})
	if reason != reasonPolicyMalformed {
		t.Fatalf("resolvePolicy reason = %q, want %q", reason, reasonPolicyMalformed)
	}
}

func TestResolvePolicyMalformedCapDetection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block policyBlock
	}{
		{"maxChangedFiles absent", policyBlock{MaxDiffLines: intp(100)}},
		{"maxChangedFiles zero", policyBlock{MaxChangedFiles: intp(0), MaxDiffLines: intp(100)}},
		{"maxChangedFiles negative", policyBlock{MaxChangedFiles: intp(-1), MaxDiffLines: intp(100)}},
		{"maxDiffLines absent", policyBlock{MaxChangedFiles: intp(10)}},
		{"maxDiffLines zero", policyBlock{MaxChangedFiles: intp(10), MaxDiffLines: intp(0)}},
		{"empty protected-path pattern", policyBlock{MaxChangedFiles: intp(10), MaxDiffLines: intp(100), ProtectedPaths: []string{""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := repoConfigFile{Automerge: &tc.block}
			_, reason := resolvePolicy(cfg, []string{"any/file.go"})
			if reason != reasonPolicyMalformed {
				t.Fatalf("resolvePolicy reason = %q, want %q", reason, reasonPolicyMalformed)
			}
		})
	}
}

func TestResolvePolicyAbsentBlockDenies(t *testing.T) {
	t.Run("no automerge key anywhere (Q2: no fallback to built-in defaults)", func(t *testing.T) {
		_, reason := resolvePolicy(repoConfigFile{}, []string{"any/file.go"})
		if reason != reasonPolicyAbsent {
			t.Fatalf("resolvePolicy reason = %q, want %q", reason, reasonPolicyAbsent)
		}
	})
	t.Run("touched project has no automerge block and there is no top-level fallback", func(t *testing.T) {
		cfg := repoConfigFile{Projects: []projectPolicy{{Path: "watch"}}}
		_, reason := resolvePolicy(cfg, []string{"watch/x.go"})
		if reason != reasonPolicyAbsent {
			t.Fatalf("resolvePolicy reason = %q, want %q", reason, reasonPolicyAbsent)
		}
	})
}

// -- fetchPolicy (#824) -------------------------------------------------------

// TestFetchPolicyUsesBaseRefNotHeadRef pins the exact invocation shape (Q1):
// the base ref, never the head ref -- a PR must never be able to widen its
// own policy to self-approve by editing .cenci/config.json on its own branch.
func TestFetchPolicyUsesBaseRefNotHeadRef(t *testing.T) {
	var calls [][]string
	original := execGh
	execGh = func(args ...string) (string, string, error) {
		calls = append(calls, append([]string{"gh"}, args...))
		return `{"automerge":{"maxChangedFiles":5,"maxDiffLines":200,"mergeMethod":"squash"}}`, "", nil
	}
	t.Cleanup(func() { execGh = original })

	if _, err := fetchPolicy("o/r", "main"); err != nil {
		t.Fatalf("fetchPolicy: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %#v, want exactly 1", calls)
	}
	want := []string{"gh", "api", "-H", "Accept: application/vnd.github.raw", "repos/o/r/contents/.cenci/config.json?ref=main"}
	if !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("fetchPolicy invocation = %#v, want %#v", calls[0], want)
	}
}

// TestFetchPolicyEscapesBaseRef pins that a baseRef containing characters
// that are legal in a git ref but reserved in a URL query string ("#", "&")
// is query-escaped before interpolation -- raw string concatenation would
// otherwise corrupt the query string (e.g. truncate it at "#" or append a
// bogus second query parameter at "&").
func TestFetchPolicyEscapesBaseRef(t *testing.T) {
	var calls [][]string
	original := execGh
	execGh = func(args ...string) (string, string, error) {
		calls = append(calls, append([]string{"gh"}, args...))
		return `{"automerge":{"maxChangedFiles":5,"maxDiffLines":200,"mergeMethod":"squash"}}`, "", nil
	}
	t.Cleanup(func() { execGh = original })

	if _, err := fetchPolicy("o/r", "feature#1&x"); err != nil {
		t.Fatalf("fetchPolicy: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %#v, want exactly 1", calls)
	}
	want := []string{"gh", "api", "-H", "Accept: application/vnd.github.raw", "repos/o/r/contents/.cenci/config.json?ref=feature%231%26x"}
	if !reflect.DeepEqual(calls[0], want) {
		t.Fatalf("fetchPolicy invocation = %#v, want %#v", calls[0], want)
	}
}

func TestFetchPolicyNonJSONBodyIsUnreadable(t *testing.T) {
	original := execGh
	execGh = func(...string) (string, string, error) { return "not json", "", nil }
	t.Cleanup(func() { execGh = original })
	_, err := fetchPolicy("o/r", "main")
	if err == nil || !strings.Contains(err.Error(), reasonPolicyUnreadable) {
		t.Fatalf("fetchPolicy non-JSON body: err = %v, want an error containing %q", err, reasonPolicyUnreadable)
	}
}

// -- tick automerge wiring (#824) ---------------------------------------------

// TestTickIssuesAutomergeWhenGreenLabeledAndWithinPolicy is test-strategy
// case (a): green + label + policy ⇒ gh pr merge --squash issued, actionable
// is set (the backoff delay resets to the interval), and the untouched
// MERGED-branch relabel does not fire inline (it fires on the *next* tick,
// once `pr view` itself reports MERGED).
func TestTickIssuesAutomergeWhenGreenLabeledAndWithinPolicy(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	script := automergeFirstPassScript()                      // the first, genuine full-evaluation pass
	script = append(script, automergeGreenRecheckScript()...) // #854 Decision 9: the pre-merge re-evaluation's own full re-fetch, still all-green
	script = append(script,
		scriptedCall{out: "Merged pull request #42 (o/r)"}, // gh pr merge, zero exit
		scriptedCall{out: `{"number":42,"title":"Change","state":"MERGED","headRefName":"feature","headRefOid":"abc","baseRefName":"main","mergeable":"MERGEABLE","isDraft":false,"changedFiles":1,"additions":5,"deletions":2,"files":[{"path":"watch/internal/babysit/x.go"}],"url":"https://example/pr/42","closingIssuesReferences":[{"number":9}]}`}, // post-merge verification refetch
		scriptedCall{out: "https://example/pr/42#issuecomment-1"}, // #1049: the post-merge attribution comment
	)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	terminal, delay, err := tick(&s)
	if err != nil {
		t.Fatal(err)
	}
	if terminal {
		t.Fatal("tick must not be terminal on the merge-issuing tick itself")
	}
	if delay != 60*time.Second || s.CurrentDelaySeconds != 60 {
		t.Fatalf("a successful merge must reset the backoff delay to the interval: delay=%v state=%d", delay, s.CurrentDelaySeconds)
	}

	var mergeCall, relabelCall []string
	for _, c := range calls {
		if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
			mergeCall = c
		}
		if len(c) > 3 && c[1] == "issue" && c[2] == "edit" && c[3] == "9" {
			relabelCall = c
		}
	}
	if mergeCall == nil {
		t.Fatalf("gh pr merge was not issued: %#v", calls)
	}
	if !hasArg(mergeCall, "--squash") {
		t.Errorf("merge call = %#v, want --squash", mergeCall)
	}
	if hasArg(mergeCall, "--delete-branch") {
		t.Errorf("merge call = %#v, must never pass --delete-branch", mergeCall)
	}
	// TOCTOU pin: the merge must be pinned to the exact head commit
	// evaluateAutomerge actually evaluated (automergeEligiblePR's
	// headRefOid "abc"), so a push landing between evaluation and merge can
	// never get content merged that was never checked.
	if v, ok := argValue(mergeCall, "--match-head-commit"); !ok || v != "abc" {
		t.Errorf("merge call = %#v, want --match-head-commit abc", mergeCall)
	}
	if relabelCall != nil {
		t.Fatalf("the In Review -> Implemented relabel must not run inline on the merge tick: %#v", calls)
	}
	for _, c := range calls {
		if len(c) > 4 && c[1] == "api" && strings.Contains(c[4], "ref=feature") {
			t.Fatalf("policy fetch used the head ref, want the base ref: %#v", c)
		}
	}
	if s.AutomergeDecision != "merge" {
		t.Errorf("AutomergeDecision = %q, want %q", s.AutomergeDecision, "merge")
	}
	if s.AutomergeReason != "" {
		t.Errorf("AutomergeReason = %q, want empty on a successful merge", s.AutomergeReason)
	}
	if s.AutomergeCheckedAt.IsZero() {
		t.Error("AutomergeCheckedAt was not stamped")
	}
	// #886: recordDecision must assign AutomergeFailureClass unconditionally,
	// so a clean, successful merge tick never carries a stale class forward.
	if s.AutomergeFailureClass != "" {
		t.Errorf("AutomergeFailureClass = %q, want empty on a clean successful merge", s.AutomergeFailureClass)
	}
}

// TestTickAutomergeSquashOnlyHoldsOnNonSquashPolicy inverts the old
// TestTickAutomergeUsesResolvedMergeMethod (#854 squash-only, Decision 3):
// mergeMethod stays readable for configuration compatibility, but only
// "squash" is ever executed -- a resolved policy mergeMethod of "rebase"
// (or "merge") must hold under its own distinct reason and issue no merge
// at all, never fall through to --rebase/--merge. The method-not-squash
// stage is evaluated before any allowed-merge-methods fetch, so a
// non-squash policy also costs zero extra gh calls beyond the policy fetch
// itself.
func TestTickAutomergeSquashOnlyHoldsOnNonSquashPolicy(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	withScriptedCommands(t, []scriptedCall{
		{out: automergeEligiblePR()},
		{out: `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`},
		{out: `[]`},
		{out: `[]`},
		{out: `{"labels":[{"name":"automerge:ok"}]}`},
		{out: `{"automerge":{"maxChangedFiles":10,"maxDiffLines":500,"mergeMethod":"rebase"}}`},
	}, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeReason != reasonMergeMethodNotSquash {
		t.Fatalf("AutomergeReason = %q, want %q: a rebase policy must hold, never merge", s.AutomergeReason, reasonMergeMethodNotSquash)
	}
	for _, c := range calls {
		if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
			t.Fatalf("gh pr merge must not be issued for a non-squash policy: %#v", calls)
		}
		if len(c) > 1 && c[1] == "api" && strings.Contains(strings.Join(c, " "), "allow_squash_merge") {
			t.Fatalf("the repo allowed-methods probe must not be fetched: the method-not-squash stage is evaluated before it, so a non-squash policy costs zero extra gh calls: %#v", calls)
		}
	}
}

// TestTickAutomergeSquashDisallowedByRepoStillHoldsDistinctReason pins the
// other half of squash-only: when the policy itself already specifies
// "squash" but the repo's own settings disallow it, the pre-existing
// reasonMergeMethodDisallowed reason still applies (distinct from
// reasonMergeMethodNotSquash, which never even reaches the allowed-methods
// fetch).
func TestTickAutomergeSquashDisallowedByRepoStillHoldsDistinctReason(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	withScriptedCommands(t, []scriptedCall{
		{out: automergeEligiblePR()},
		{out: `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`},
		{out: `[]`},
		{out: `[]`},
		{out: `{"labels":[{"name":"automerge:ok"}]}`},
		{out: `{"automerge":{"maxChangedFiles":10,"maxDiffLines":500,"mergeMethod":"squash"}}`},
		{out: `{"squash":false,"merge":true,"rebase":true}`},
	}, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeReason != reasonMergeMethodDisallowed {
		t.Fatalf("AutomergeReason = %q, want %q: squash is the policy's mergeMethod, but the repo itself disallows it", s.AutomergeReason, reasonMergeMethodDisallowed)
	}
	for _, c := range calls {
		if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
			t.Fatalf("gh pr merge must not be issued when the repo disallows squash: %#v", calls)
		}
	}
}

// TestTickAutomergeHoldsOnMergeQueueProbeNullPullRequest is the unanimous
// critical fix all three #854 reviewers flagged (item 1): a real GraphQL
// queue-probe response shaped like {"data":{"repository":{"pullRequest":
// null}}} -- zero exit, no top-level errors[], a deleted/renumbered/
// inaccessible PR or transient null-propagation -- decodes to the exact
// same (nil, nil, nil) shape as the "queue stage not yet reached" lazy-
// fetch trial sentinel. It must hold under reasonMergeQueueUnknown (the
// probe genuinely ran and came back with nothing), never silently pass as
// if the probe had never run at all.
func TestTickAutomergeHoldsOnMergeQueueProbeNullPullRequest(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	withScriptedCommands(t, []scriptedCall{
		{out: automergeEligiblePR()},
		{out: `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`},
		{out: `[]`},
		{out: `[]`},
		{out: `{"labels":[{"name":"automerge:ok"}]}`},
		{out: `{"automerge":{"maxChangedFiles":10,"maxDiffLines":500,"mergeMethod":"squash"}}`},
		{out: `{"squash":true,"merge":false,"rebase":true}`},
		{out: `{"data":{"repository":{"pullRequest":null}}}`}, // genuinely probed: zero exit, no errors[], but pullRequest is null
	}, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeReason != reasonMergeQueueUnknown {
		t.Fatalf("AutomergeReason = %q, want %q: a genuinely-probed GraphQL response with pullRequest:null must hold, not pass through as if the probe never ran", s.AutomergeReason, reasonMergeQueueUnknown)
	}
	for _, c := range calls {
		if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
			t.Fatalf("gh pr merge must not be issued when the merge-queue probe returns a null pullRequest: %#v", calls)
		}
	}
}

// TestTickAutomergeDisabledMakesNoExtraGHCalls is test-strategy case (b):
// automerge.enabled unset ⇒ zero extra gh calls and identical pre-existing
// behavior -- the existing babysit_test.go fixtures script exactly 4 gh
// responses, and this must keep passing unchanged (Alternatives Considered).
// Relies on the package TestMain pinning fleetConfigPath to "" (disabled).
func TestTickAutomergeDisabledMakesNoExtraGHCalls(t *testing.T) {
	var calls [][]string
	withCommands(t, []string{
		automergeEligiblePR(),
		`[{"bucket":"pass","name":"test","state":"SUCCESS"}]`,
		`[]`,
		`[]`,
	}, &calls)
	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	ghCalls := 0
	for _, c := range calls {
		if len(c) > 0 && c[0] == "gh" {
			ghCalls++
		}
	}
	if ghCalls != 4 {
		t.Fatalf("gh calls = %d, want exactly 4 (the pre-existing fixture; automerge disabled must add none)", ghCalls)
	}
	if s.AutomergeReason != reasonAutomergeDisabled {
		t.Errorf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonAutomergeDisabled)
	}
}

// TestTickAutomergeHeldOnRepairOrReviewPending is test-strategy case (c):
// RepairPending or a non-empty PendingKeys holds automerge, without ever
// reaching the policy fetch or merge call.
func TestTickAutomergeHeldOnRepairOrReviewPending(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*State)
		// extraScript is the lazy GraphQL thread fetch (#850): reconcile
		// runs in tick() via reconcileFeedback unconditionally, right
		// before runAutomerge is even called -- i.e. before runAutomerge's
		// own labels fetch -- but only the "review feedback pending"
		// subtest's "comment:1" PendingKeys entry actually triggers it.
		extraScript []scriptedCall
		wantReason  string
	}{
		// LastHeadSHA is pinned to automergeEligiblePR's "abc" headRefOid: the
		// pre-existing (out-of-scope) CI-repair head-advance detection treats
		// a HeadRefOID/LastHeadSHA mismatch as "the head moved on since this
		// was recorded, so it's now stale" and resets RepairPending to false
		// -- well before automerge's insertion point after the
		// review-feedback block. Without pinning it, the "CI repair pending"
		// subtest's RepairPending would already be wiped by the time
		// automerge runs and would spuriously fall through to the policy
		// stage. PendingHeadSHA is no longer compared for clearing purposes
		// at all (#850 deletes that rule) -- it survives only as
		// repair-attempt/dedup metadata, so the "review feedback pending"
		// subtest sets it purely for fixture realism, not because anything
		// still reads it to decide resolution.
		{"CI repair pending", func(s *State) { s.RepairPending = true; s.LastHeadSHA = "abc" }, nil, reasonRepairPending},
		{"review feedback pending", func(s *State) { s.PendingKeys = []string{"comment:1"}; s.PendingHeadSHA = "abc" }, []scriptedCall{
			// Left unresolved so PendingKeys stays non-empty and the hold
			// still reaches reasonReviewPending (an ordinary hold, not one
			// of the four new feedback-hold reasons -- see
			// TestTickFeedbackHoldsAutomerge for those).
			{out: `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[{"isResolved":false,"comments":{"totalCount":1,"nodes":[{"databaseId":1}]}}]}}}}}`},
		}, reasonReviewPending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFleetAutomergeEnabled(t, true)
			var calls [][]string
			script := []scriptedCall{
				{out: automergeEligiblePR()},
				{out: `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`},
				{out: `[]`},
				{out: `[]`},
			}
			script = append(script, tc.extraScript...)
			script = append(script, scriptedCall{out: `{"labels":[{"name":"automerge:ok"}]}`})
			withScriptedCommands(t, script, &calls)
			// #975: the "review feedback pending" subtest's unresolved
			// comment:1 key is still un-launched, so tick's address-review
			// launch trigger fires before runAutomerge ever runs -- a
			// recorded session (and a stubbed tmuxHasSession, since
			// withScriptedCommands doesn't default-stub it like withCommands
			// does) is required for that launch to succeed and reach the
			// reasonReviewPending assertion below, rather than short-
			// circuiting into reasonWorkflowLaunchFailed.
			originalTmuxHasSession := tmuxHasSession
			tmuxHasSession = func(string) (bool, error) { return true, nil }
			t.Cleanup(func() { tmuxHasSession = originalTmuxHasSession })
			s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 60, LaunchSession: "work"}
			tc.setup(&s)
			if _, _, err := tick(&s); err != nil {
				t.Fatal(err)
			}
			if s.AutomergeReason != tc.wantReason {
				t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, tc.wantReason)
			}
			for _, c := range calls {
				if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
					t.Fatalf("gh pr merge must not be issued while held: %#v", calls)
				}
			}
		})
	}
}

// TestTickAutomergeHeldWhenMergeRejected is test-strategy case (d): a
// rejected merge (e.g. branch protection) is a logged hold, not a tick
// error -- no backoff escalation, retried next tick.
func TestTickAutomergeHeldWhenMergeRejected(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	script := automergeFirstPassScript()
	script = append(script, automergeGreenRecheckScript()...)
	script = append(script,
		scriptedCall{out: "branch protection required review", err: errors.New("exit status 1")},
		// #886: executeMerge now performs the post-merge refetch
		// unconditionally, even after a nonzero exit -- still OPEN confirms
		// the rejection was real, matching matrix case (c): merge nonzero
		// exit + refetch readable non-MERGED => reasonMergeFailed.
		scriptedCall{out: automergeEligiblePR()},
	)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 60}
	terminal, _, err := tick(&s)
	if err != nil {
		t.Fatalf("tick returned an error on a rejected merge, want nil (retried next tick, no backoff escalation): %v", err)
	}
	if terminal {
		t.Fatal("tick must not treat a rejected merge as terminal")
	}
	if s.AutomergeReason != reasonMergeFailed {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonMergeFailed)
	}
	// The rejected merge's combined `gh` output (branch-protection block,
	// auth failure, rate limit, real conflict, ...) must not be discarded --
	// it's the only thing that distinguishes those failure classes from one
	// another beyond the single generic reasonMergeFailed constant.
	if !strings.Contains(s.AutomergeDetail, "branch protection required review") {
		t.Fatalf("AutomergeDetail = %q, want it to contain the captured gh pr merge output", s.AutomergeDetail)
	}
	// The merge command's own error ("exit status 1", an otherwise
	// unclassified failure) must resolve to failureClassCommand -- classifying
	// this, the package's single most operationally important failure mode,
	// must not silently stay at the empty zero value.
	if s.AutomergeFailureClass != failureClassCommand {
		t.Fatalf("AutomergeFailureClass = %q, want %q", s.AutomergeFailureClass, failureClassCommand)
	}
	if n := mergeCallCount(calls); n != 1 {
		t.Fatalf("gh pr merge calls = %d, want exactly 1: a rejected merge must never be retried within the same tick", n)
	}
}

// TestTickAutomergeHoldsWhenHeadSHAUnknown pins that an empty HeadRefOID --
// e.g. a transient `gh pr view` gap -- holds automerge under its own distinct
// reason and issues no `gh pr merge` at all: an empty --match-head-commit
// value would let `gh` silently drop the TOCTOU pin from the merge mutation
// instead of failing closed.
func TestTickAutomergeHoldsWhenHeadSHAUnknown(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	pr := `{"number":42,"title":"Change","state":"OPEN","headRefName":"feature","headRefOid":"","baseRefName":"main","mergeable":"MERGEABLE","isDraft":false,"changedFiles":1,"additions":5,"deletions":2,"files":[{"path":"watch/internal/babysit/x.go"}],"url":"https://example/pr/42","closingIssuesReferences":[{"number":9}]}`
	var calls [][]string
	withScriptedCommands(t, []scriptedCall{
		{out: pr},
		{out: `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`},
		{out: `[]`},
		{out: `[]`},
		{out: `{"labels":[{"name":"automerge:ok"}]}`},
	}, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 60}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeReason != reasonHeadSHAUnknown {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonHeadSHAUnknown)
	}
	for _, c := range calls {
		if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
			t.Fatalf("gh pr merge must not be issued when the head SHA is unknown: %#v", calls)
		}
	}
}

// TestTickAutomergeSanitizesRejectedMergeDetail pins that a rejected merge's
// captured `gh` output -- multi-line and arbitrarily long -- is sanitized
// (newlines collapsed to spaces, truncated) before landing in
// s.AutomergeDetail, since it feeds a single-line log format downstream
// tooling substring-classifies and is also persisted onto State.
func TestTickAutomergeSanitizesRejectedMergeDetail(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	longOutput := "branch protection required review\nsecond line\nthird line: " + strings.Repeat("y", 250)
	script := automergeFirstPassScript()
	script = append(script, automergeGreenRecheckScript()...)
	script = append(script,
		scriptedCall{out: longOutput, err: errors.New("exit status 1")},
		// #886: the now-unconditional post-merge refetch -- still OPEN keeps
		// this test on the reasonMergeFailed/sanitization path rather than
		// falling into reasonMergeVerifyUnreadable.
		scriptedCall{out: automergeEligiblePR()},
	)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 60}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeReason != reasonMergeFailed {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonMergeFailed)
	}
	if strings.Contains(s.AutomergeDetail, "\n") {
		t.Fatalf("AutomergeDetail = %q, must not contain an embedded newline", s.AutomergeDetail)
	}
	if len(s.AutomergeDetail) > detailMaxLen+len("...") {
		t.Fatalf("AutomergeDetail length = %d, want capped at roughly %d", len(s.AutomergeDetail), detailMaxLen)
	}
}

// -- #850: feedback-hold reasons wired into automerge -------------------------

// TestTickFeedbackHoldsAutomerge is AC 5: an API/parsing failure, an unknown
// review state, or an unsupported feedback-key type each hold automerge under
// their own distinct reason constant -- asserting the exact AutomergeReason
// (per watch/docs/error-handling.md #446) and that no `gh pr merge` call ever
// appears in recorded calls.
func TestTickFeedbackHoldsAutomerge(t *testing.T) {
	unreadableThread := "not json"
	truncatedThread := `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[{"isResolved":false,"comments":{"totalCount":2,"nodes":[{"databaseId":5}]}}]}}}}}`

	for _, tc := range []struct {
		name        string
		pendingKeys []string
		extraScript []scriptedCall // the lazy GraphQL fetch, only for comment: keys
		wantReason  string
	}{
		{
			"unreadable: the GraphQL thread fetch itself fails (malformed body)",
			[]string{"comment:5"},
			[]scriptedCall{{out: unreadableThread}},
			reasonFeedbackUnreadable,
		},
		{
			"truncated: a thread's comments.totalCount exceeds its fetched node count",
			[]string{"comment:5"},
			[]scriptedCall{{out: truncatedThread}},
			reasonFeedbackTruncated,
		},
		{
			"unknown: a pending review key GitHub no longer reports at all (no GraphQL call needed)",
			[]string{"review:99"},
			nil,
			reasonReviewStateUnknown,
		},
		{
			"unsupported: a pending key of a type this classifier does not recognize (no GraphQL call needed)",
			[]string{"label:1"},
			nil,
			reasonFeedbackUnsupported,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFleetAutomergeEnabled(t, true)
			var calls [][]string
			// The lazy GraphQL thread fetch (tc.extraScript, when a
			// comment: key is pending) runs in tick() via
			// reconcileFeedback immediately before runAutomerge is
			// called -- i.e. before runAutomerge's own labels fetch, which
			// must come last in the script.
			script := []scriptedCall{
				{out: automergeEligiblePR()},
				{out: `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`},
				{out: `[]`},
				{out: `[]`},
			}
			script = append(script, tc.extraScript...)
			script = append(script, scriptedCall{out: `{"labels":[{"name":"automerge:ok"}]}`})
			withScriptedCommands(t, script, &calls)
			// #975: every subtest's pendingKeys entry is still un-launched
			// (a fail-closed feedback hold never clears it), so tick's
			// address-review launch trigger fires before the feedback-hold
			// reason is even assigned -- see the identical note on
			// TestTickAutomergeHeldOnRepairOrReviewPending above.
			originalTmuxHasSession := tmuxHasSession
			tmuxHasSession = func(string) (bool, error) { return true, nil }
			t.Cleanup(func() { tmuxHasSession = originalTmuxHasSession })

			s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 60, PendingKeys: tc.pendingKeys, LaunchSession: "work"}
			if _, _, err := tick(&s); err != nil {
				t.Fatal(err)
			}
			if s.AutomergeReason != tc.wantReason {
				t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, tc.wantReason)
			}
			for _, c := range calls {
				if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
					t.Fatalf("gh pr merge must not be issued while a feedback hold is in effect: %#v", calls)
				}
			}
			if !reflect.DeepEqual(s.PendingKeys, tc.pendingKeys) {
				t.Fatalf("PendingKeys = %v, want unchanged %v: a fail-closed hold must not clear the item", s.PendingKeys, tc.pendingKeys)
			}
		})
	}
}

// -- #854: post-merge verification -------------------------------------------

// TestTickAutomergePostMergeVerificationConfirmsMerged pins Decisions 5/6: a
// zero-exit `gh pr merge` must be followed by exactly one refetch, and
// AutomergeDecision == "merge" (plus the backoff reset) is recorded only
// once that refetch itself reports MERGED -- the exit status alone is never
// proof.
func TestTickAutomergePostMergeVerificationConfirmsMerged(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	mergedPR := `{"number":42,"title":"Change","state":"MERGED","headRefName":"feature","headRefOid":"abc","baseRefName":"main","mergeable":"MERGEABLE","isDraft":false,"changedFiles":1,"additions":5,"deletions":2,"files":[{"path":"watch/internal/babysit/x.go"}],"url":"https://example/pr/42","closingIssuesReferences":[{"number":9}]}`
	script := automergeFirstPassScript()
	script = append(script, automergeGreenRecheckScript()...)
	script = append(script,
		scriptedCall{out: "Merged pull request #42 (o/r)"},        // gh pr merge, zero exit
		scriptedCall{out: mergedPR},                               // the required post-merge refetch
		scriptedCall{out: "https://example/pr/42#issuecomment-1"}, // #1049: the post-merge attribution comment
	)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeDecision != "merge" {
		t.Fatalf("AutomergeDecision = %q, want \"merge\": confirmed by the post-merge refetch reporting MERGED", s.AutomergeDecision)
	}
	viewCalls := 0
	for _, c := range calls {
		if len(c) > 2 && c[1] == "pr" && c[2] == "view" {
			viewCalls++
		}
	}
	// #854: the tick's own fetch, the pre-merge re-evaluation's own fresh
	// `gh pr view` re-fetch (Decision 9), plus one post-merge verification
	// refetch (Q5: one refetch, no polling) -- three total.
	if viewCalls != 3 {
		t.Fatalf("pr view calls = %d, want exactly 3: the tick's own fetch, the pre-merge re-check's re-fetch, plus one post-merge verification refetch", viewCalls)
	}
	// #897: a matching headRefOid must still confirm success and post exactly
	// one attribution comment -- the new head-SHA guard must never suppress
	// this pre-existing, unchanged behavior.
	if n := commentCallCount(calls); n != 1 {
		t.Fatalf("gh pr comment calls = %d, want exactly 1: a matching head-SHA refetch must still post the attribution comment", n)
	}
}

// TestTickAutomergePostMergeVerificationIndeterminateWhenNotMerged pins
// Decision 6: a zero exit whose refetch reports a state other than MERGED
// (e.g. still OPEN -- GitHub replication lag, or the queue deferred the
// actual merge) is an indeterminate failure, never success.
func TestTickAutomergePostMergeVerificationIndeterminateWhenNotMerged(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	stillOpenPR := automergeEligiblePR() // state OPEN, unchanged after the "merge"
	script := automergeFirstPassScript()
	script = append(script, automergeGreenRecheckScript()...)
	script = append(script,
		scriptedCall{out: "Merged pull request #42 (o/r)"},
		scriptedCall{out: stillOpenPR},
	)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeDecision != "held" {
		t.Fatalf("AutomergeDecision = %q, want \"held\": a zero exit with a non-MERGED refetch is indeterminate, never success", s.AutomergeDecision)
	}
	if s.AutomergeReason != reasonMergeIndeterminate {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonMergeIndeterminate)
	}
}

// TestTickAutomergePostMergeVerificationReadFailureIsDistinctReason pins
// that a failed post-merge refetch is a distinct reason from an
// indeterminate-but-successfully-read non-MERGED state -- the two failure
// classes must never collapse together.
func TestTickAutomergePostMergeVerificationReadFailureIsDistinctReason(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	script := automergeFirstPassScript()
	script = append(script, automergeGreenRecheckScript()...)
	script = append(script,
		scriptedCall{out: "Merged pull request #42 (o/r)"},
		scriptedCall{out: "rate limited", err: errors.New("exit status 1")}, // the refetch itself fails
	)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeReason != reasonMergeVerifyUnreadable {
		t.Fatalf("AutomergeReason = %q, want %q: a refetch failure must be distinct from reasonMergeIndeterminate", s.AutomergeReason, reasonMergeVerifyUnreadable)
	}
}

// commentCallCount counts how many `gh pr comment` invocations appear in
// calls -- the automerge attribution comment (#1049) must fire on a
// confirmed merge and must never fire on any hold path, including the new
// #897 head-SHA-mismatch/empty holds below.
func commentCallCount(calls [][]string) int {
	n := 0
	for _, c := range calls {
		if len(c) > 2 && c[1] == "pr" && c[2] == "comment" {
			n++
		}
	}
	return n
}

// -- #897: post-merge head-SHA verification -----------------------------------
//
// executeMerge's refetch reporting state == "MERGED" is not, by itself, proof
// that headSHA (the exact commit --match-head-commit pinned) is the commit
// that actually got merged: another actor could have merged a different
// commit in the narrow race between babysit's own pre-merge validation and
// this post-merge refetch. Every case below must hold under the new
// reasonMergeHeadMismatch, with FailureClass empty (no `gh` transport error
// occurred), Detail naming both the pinned and observed head SHA, and zero
// `gh pr comment` calls -- the attribution comment must never fire for a
// merge babysit did not confirm it performed.

// mergedPRWithHeadRefOID renders a `pr view` MERGED-state fixture identical
// to automergeEligiblePR() except for headRefOid, so a test can isolate the
// post-merge refetch's reported head commit as the single mutated field.
func mergedPRWithHeadRefOID(headRefOID string) string {
	return fmt.Sprintf(`{"number":42,"title":"Change","state":"MERGED","headRefName":"feature","headRefOid":%q,"baseRefName":"main","mergeable":"MERGEABLE","isDraft":false,"changedFiles":1,"additions":5,"deletions":2,"files":[{"path":"watch/internal/babysit/x.go"}],"url":"https://example/pr/42","closingIssuesReferences":[{"number":9}]}`, headRefOID)
}

// TestTickAutomergePostMergeHeadMismatchZeroExitHoldsUnderDistinctReason is
// AC 1/2/4's zero-exit half: `gh pr merge` reports success, but the
// unconditional post-merge refetch reports MERGED at a *different* head
// commit than the one --match-head-commit pinned -- this must never be
// recorded as a confirmed merge, regardless of the merge command's own exit
// status.
func TestTickAutomergePostMergeHeadMismatchZeroExitHoldsUnderDistinctReason(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	mismatchedPR := mergedPRWithHeadRefOID("different")
	script := automergeFirstPassScript() // pinned head commit "abc" throughout
	script = append(script, automergeGreenRecheckScript()...)
	script = append(script,
		scriptedCall{out: "Merged pull request #42 (o/r)"}, // gh pr merge, zero exit
		scriptedCall{out: mismatchedPR},                    // post-merge refetch: MERGED, but at a different head commit
	)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeDecision != "held" {
		t.Fatalf("AutomergeDecision = %q, want \"held\": a refetch reporting MERGED at a different head commit must never be confirmed success", s.AutomergeDecision)
	}
	if s.AutomergeReason != reasonMergeHeadMismatch {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonMergeHeadMismatch)
	}
	if s.AutomergeFailureClass != "" {
		t.Fatalf("AutomergeFailureClass = %q, want empty: no gh transport failure occurred, only a merged-at-the-wrong-commit mismatch", s.AutomergeFailureClass)
	}
	if !strings.Contains(s.AutomergeDetail, "abc") || !strings.Contains(s.AutomergeDetail, "different") {
		t.Fatalf("AutomergeDetail = %q, want it to name both the pinned SHA (abc) and the observed SHA (different)", s.AutomergeDetail)
	}
	if n := commentCallCount(calls); n != 0 {
		t.Fatalf("gh pr comment calls = %d, want 0: a head-SHA mismatch must never post the attribution comment", n)
	}
	if n := mergeCallCount(calls); n != 1 {
		t.Fatalf("gh pr merge calls = %d, want exactly 1: no retry", n)
	}
}

// TestTickAutomergePostMergeHeadMismatchNonzeroExitHoldsUnderDistinctReason is
// AC 1/2/4's nonzero-exit half: the same head-SHA mismatch, but this time
// `gh pr merge` itself also exited nonzero -- Detail must still name both
// SHAs, plus fold in the merge command's own stderr (the mismatch branch
// returns directly and must never fall through to reasonMergeFailed, which
// would otherwise be the only place that stderr is captured).
func TestTickAutomergePostMergeHeadMismatchNonzeroExitHoldsUnderDistinctReason(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	mismatchedPR := mergedPRWithHeadRefOID("different")
	script := automergeFirstPassScript()
	script = append(script, automergeGreenRecheckScript()...)
	script = append(script,
		scriptedCall{out: "merge conflict detected mid-flight", err: errors.New("exit status 1")}, // gh pr merge, nonzero exit
		scriptedCall{out: mismatchedPR}, // post-merge refetch: MERGED, but at a different head commit
	)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeReason != reasonMergeHeadMismatch {
		t.Fatalf("AutomergeReason = %q, want %q: mismatch must hold under the new reason regardless of the merge command's own exit status", s.AutomergeReason, reasonMergeHeadMismatch)
	}
	if s.AutomergeReason == reasonMergeFailed {
		t.Fatal("AutomergeReason must never fall through to reasonMergeFailed once the refetch itself proves a head-commit mismatch")
	}
	if s.AutomergeFailureClass != "" {
		t.Fatalf("AutomergeFailureClass = %q, want empty", s.AutomergeFailureClass)
	}
	if !strings.Contains(s.AutomergeDetail, "abc") || !strings.Contains(s.AutomergeDetail, "different") {
		t.Fatalf("AutomergeDetail = %q, want it to name both the pinned SHA (abc) and the observed SHA (different)", s.AutomergeDetail)
	}
	if !strings.Contains(s.AutomergeDetail, "merge conflict detected mid-flight") {
		t.Fatalf("AutomergeDetail = %q, want it to also fold in the merge command's own stderr since it exited nonzero", s.AutomergeDetail)
	}
	if n := commentCallCount(calls); n != 0 {
		t.Fatalf("gh pr comment calls = %d, want 0", n)
	}
}

// TestTickAutomergePostMergeEmptyHeadRefOidHoldsFailClosed is AC 3: an empty
// headRefOid on the post-merge refetch must hold identically to an explicit
// mismatch (watch/docs/error-handling.md's default-deny rule -- absence is
// never proof), not be tolerated the way the separate pre-merge TOCTOU guard
// (automerge.go:1194) tolerates an empty headRefOid.
func TestTickAutomergePostMergeEmptyHeadRefOidHoldsFailClosed(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	emptyHeadRefOidPR := mergedPRWithHeadRefOID("")
	script := automergeFirstPassScript()
	script = append(script, automergeGreenRecheckScript()...)
	script = append(script,
		scriptedCall{out: "Merged pull request #42 (o/r)"}, // gh pr merge, zero exit
		scriptedCall{out: emptyHeadRefOidPR},               // post-merge refetch: MERGED, but headRefOid empty
	)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeDecision != "held" {
		t.Fatalf("AutomergeDecision = %q, want \"held\": an empty headRefOid must fail closed, never be read as proof of anything", s.AutomergeDecision)
	}
	if s.AutomergeReason != reasonMergeHeadMismatch {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonMergeHeadMismatch)
	}
	if s.AutomergeFailureClass != "" {
		t.Fatalf("AutomergeFailureClass = %q, want empty", s.AutomergeFailureClass)
	}
	if !strings.Contains(s.AutomergeDetail, "abc") {
		t.Fatalf("AutomergeDetail = %q, want it to name the pinned SHA (abc)", s.AutomergeDetail)
	}
	if !strings.Contains(s.AutomergeDetail, `observed ""`) {
		t.Fatalf("AutomergeDetail = %q, want it to name the observed value as explicitly empty (observed \"\")", s.AutomergeDetail)
	}
	if n := commentCallCount(calls); n != 0 {
		t.Fatalf("gh pr comment calls = %d, want 0", n)
	}
}

// -- #854: pre-merge re-evaluation --------------------------------------------

// TestTickAutomergePreMergeRecheckCatchesHeadSHAChange pins the pre-merge
// re-evaluation gate (Decision 9): immediately before issuing `gh pr merge`,
// the re-check's own fresh `gh pr view` re-fetch (merge.go's
// recheckAutomergeInputs) reports the PR's current head commit -- a head
// commit that changed between the first evaluation and the merge attempt
// (e.g. a last-second push) must hold, never merge on stale evidence, and
// never pin --match-head-commit to the stale SHA.
func TestTickAutomergePreMergeRecheckCatchesHeadSHAChange(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	movedPR := `{"number":42,"title":"Change","state":"OPEN","headRefName":"feature","headRefOid":"moved","baseRefName":"main","mergeable":"MERGEABLE","isDraft":false,"changedFiles":1,"additions":5,"deletions":2,"files":[{"path":"watch/internal/babysit/x.go"}],"url":"https://example/pr/42","closingIssuesReferences":[{"number":9}]}`
	script := automergeFirstPassScript() // the first, genuine full-evaluation pass (headRefOid "abc" throughout)
	script = append(script, automergeRecheckScript(
		movedPR, // the re-check's own pr view re-fetch reports a moved head
		`[{"bucket":"pass","name":"test","state":"SUCCESS"}]`,
		`[]`,
		`[]`,
		`{"labels":[{"name":"automerge:ok"}]}`,
		`{"automerge":{"maxChangedFiles":10,"maxDiffLines":500,"mergeMethod":"squash"}}`,
		queueProbeResponse(false, false, "moved"),
	)...)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	for _, c := range calls {
		if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
			t.Fatalf("gh pr merge must not be issued: the pre-merge re-check found a head SHA change since the first evaluation: %#v", calls)
		}
	}
	if s.AutomergeDecision == "merge" {
		t.Fatal("AutomergeDecision = \"merge\", want held: a stale-evidence merge on a changed head SHA must never succeed")
	}
	if s.AutomergeReason != reasonHeadSHAChanged {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonHeadSHAChanged)
	}
}

// TestTickAutomergePreMergeRecheckClearsConditionsWhenRecheckFetchFails pins
// the polish fix to #854's item 3: when the pre-merge recheck's own upstream
// re-fetch fails outright (never getting far enough to produce a second,
// fresh Conditions set), the first pass's all-"yes" Conditions must not
// survive onto the held decision -- otherwise logLine renders a self-
// contradictory "held: PR view unreadable [enabled=yes ... queue=yes]"
// during incident triage. Conditions must be cleared so every stage renders
// "-" (never reached) instead.
func TestTickAutomergePreMergeRecheckClearsConditionsWhenRecheckFetchFails(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	script := automergeFirstPassScript()                                    // the first, genuine full-evaluation pass: every stage passes
	script = append(script, scriptedCall{err: errors.New("exit status 1")}) // the recheck's own `gh pr view` re-fetch fails outright
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		// Like a rejected merge (TestTickAutomergeHeldWhenMergeRejected), a
		// recheck-phase failure is a logged hold inside runAutomerge, not a
		// tick error: it's retried next tick, no backoff escalation.
		t.Fatalf("tick returned an error on a recheck fetch failure, want nil: %v", err)
	}
	if s.AutomergeReason != reasonUpstreamPRUnreadable {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonUpstreamPRUnreadable)
	}
	if len(s.AutomergeConditions) != 0 {
		t.Fatalf("AutomergeConditions = %#v, want empty: the second pass never completed, so the first pass's all-passing Conditions must not survive onto this held decision", s.AutomergeConditions)
	}
}

// TestTickAutomergePreMergeRecheckFetchFailureSetsFailureClass pins the
// plan's Assumption that failure-class population is scoped to every read
// site that funnels into recordDecision with a subprocess error, including
// the recheck's own upstream read -- not just recordUpstreamReadFailure and
// merge execution/verification (#886). When recheckAutomergeInputs' `gh pr
// view` re-fetch fails outright (same fixture as
// TestTickAutomergePreMergeRecheckClearsConditionsWhenRecheckFetchFails),
// the persisted decision must carry a specific, non-empty
// classifyGhFailure(err) class -- not just a bare non-empty check, matching
// the class every other subprocess-error site in this ticket already sets.
func TestTickAutomergePreMergeRecheckFetchFailureSetsFailureClass(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	script := automergeFirstPassScript()                                    // the first, genuine full-evaluation pass: every stage passes
	script = append(script, scriptedCall{err: errors.New("exit status 1")}) // the recheck's own `gh pr view` re-fetch fails outright
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatalf("tick returned an error on a recheck fetch failure, want nil: %v", err)
	}
	if s.AutomergeFailureClass != failureClassCommand {
		t.Fatalf("AutomergeFailureClass = %q, want %q (classifyGhFailure of the recheck's %q error): a recheck-read failure is a classifiable gh subprocess error at the same kind of read site as recordUpstreamReadFailure, in scope per the plan's Assumption", s.AutomergeFailureClass, failureClassCommand, "exit status 1")
	}
}

// TestTickAutomergePreMergeRecheckHoldsOnChangedEvidence pins the rest of
// Decision 9's pre-merge re-evaluation matrix (the plan's Test Strategy):
// a label revoked, a check flipping to fail, a new non-bot review comment
// appearing, base-ref policy tightened, mergeable flipping to CONFLICTING,
// and the merge queue turning on -- each between the first evaluation and
// the merge attempt -- must hold under an existing reason constant with a
// "recheck: "-prefixed Detail (the plan's Assumption), issue no merge, and
// never merge on stale evidence.
func TestTickAutomergePreMergeRecheckHoldsOnChangedEvidence(t *testing.T) {
	greenPR := automergeEligiblePR()
	greenChecks := `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`
	greenComments := `[]`
	greenReviews := `[]`
	greenLabels := `{"labels":[{"name":"automerge:ok"}]}`
	greenPolicy := `{"automerge":{"maxChangedFiles":10,"maxDiffLines":500,"mergeMethod":"squash"}}`
	greenQueue := queueProbeResponse(false, false, "abc")
	conflictingPR := `{"number":42,"title":"Change","state":"OPEN","headRefName":"feature","headRefOid":"abc","baseRefName":"main","mergeable":"CONFLICTING","isDraft":false,"changedFiles":1,"additions":5,"deletions":2,"files":[{"path":"watch/internal/babysit/x.go"}],"url":"https://example/pr/42","closingIssuesReferences":[{"number":9}]}`

	for _, tc := range []struct {
		name                                                 string
		pr, checks, comments, reviews, labels, policy, queue string
		wantReason                                           string
	}{
		{"label revoked since evaluation", greenPR, greenChecks, greenComments, greenReviews, `{"labels":[]}`, greenPolicy, greenQueue, reasonLabelMissing},
		{"a check flips to fail since evaluation", greenPR, `[{"bucket":"fail","name":"test","state":"FAILURE"}]`, greenComments, greenReviews, greenLabels, greenPolicy, greenQueue, reasonCICheckFailed},
		{"a new non-bot review comment appears since evaluation", greenPR, greenChecks, `[{"id":77,"updated_at":"2026-01-02T00:00:00Z","user":{"login":"reviewer"}}]`, greenReviews, greenLabels, greenPolicy, greenQueue, reasonReviewPending},
		{"base-ref policy tightened since evaluation", greenPR, greenChecks, greenComments, greenReviews, greenLabels, `{"automerge":{"maxChangedFiles":10,"maxDiffLines":1,"mergeMethod":"squash"}}`, greenQueue, reasonTooManyLines},
		{"mergeable flips to CONFLICTING since evaluation", conflictingPR, greenChecks, greenComments, greenReviews, greenLabels, greenPolicy, greenQueue, reasonNotMergeable},
		{"merge queue turns on since evaluation", greenPR, greenChecks, greenComments, greenReviews, greenLabels, greenPolicy, queueProbeResponse(true, false, "abc"), reasonMergeQueueRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFleetAutomergeEnabled(t, true)
			var calls [][]string
			script := automergeFirstPassScript()
			script = append(script, automergeRecheckScript(tc.pr, tc.checks, tc.comments, tc.reviews, tc.labels, tc.policy, tc.queue)...)
			withScriptedCommands(t, script, &calls)

			s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
			if _, _, err := tick(&s); err != nil {
				t.Fatal(err)
			}
			if s.AutomergeReason != tc.wantReason {
				t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, tc.wantReason)
			}
			if !strings.HasPrefix(s.AutomergeDetail, "recheck") {
				t.Fatalf("AutomergeDetail = %q, want it prefixed with %q (the plan's Assumption: reuse existing reason constants with a recheck: marker, not a parallel constant set)", s.AutomergeDetail, "recheck")
			}
			if s.AutomergeDecision == "merge" {
				t.Fatal("AutomergeDecision = \"merge\", want held: the pre-merge re-check found changed evidence since the first evaluation")
			}
			for _, c := range calls {
				if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
					t.Fatalf("gh pr merge must not be issued: the pre-merge re-check found changed evidence: %#v", calls)
				}
			}
		})
	}
}

// -- #854: one decision per tick (upstream read failures) --------------------

// TestTickPersistsOneAutomergeDecisionOnUpstreamReadFailure pins Decision 7
// and the AC "upstream read failures still produce one persisted decision":
// a failure reading the PR, its checks, its comments, or its reviews must
// still record exactly one AutomergeReason/AutomergeCheckedAt, even though
// tick() itself still returns the underlying error. Previously these read
// failures aborted tick() before runAutomerge ever ran at all, leaving no
// automerge decision recorded for that tick.
func TestTickPersistsOneAutomergeDecisionOnUpstreamReadFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		responses  []string
		wantReason string
	}{
		{"pr view fails", nil, reasonUpstreamPRUnreadable},
		{"pr checks fails", []string{automergeEligiblePR()}, reasonUpstreamChecksUnreadable},
		{"comments read fails", []string{automergeEligiblePR(), `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`}, reasonUpstreamCommentsUnreadable},
		{"reviews read fails", []string{automergeEligiblePR(), `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`, `[]`}, reasonUpstreamReviewsUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFleetAutomergeEnabled(t, true)
			var calls [][]string
			script := make([]scriptedCall, 0, len(tc.responses)+1)
			for _, r := range tc.responses {
				script = append(script, scriptedCall{out: r})
			}
			// The seam errors on the next call past the scripted responses --
			// exactly the upstream read this subtest targets.
			script = append(script, scriptedCall{out: "rate limited", err: errors.New("exit status 1")})
			withScriptedCommands(t, script, &calls)

			s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 60}
			if _, _, err := tick(&s); err == nil {
				t.Fatal("tick: err = nil, want the upstream read failure to still surface as a tick error")
			}
			if s.AutomergeReason != tc.wantReason {
				t.Fatalf("AutomergeReason = %q, want %q: every enabled automerge tick must persist exactly one decision, including on an upstream read failure", s.AutomergeReason, tc.wantReason)
			}
			if s.AutomergeCheckedAt.IsZero() {
				t.Fatal("AutomergeCheckedAt was not stamped on the upstream-read-failure path")
			}
		})
	}
}

// TestTickRecordsKillSwitchReasonEvenWhenUpstreamReadAlsoFails pins Q4: with
// the fleet kill switch off AND an upstream read also failing, the recorded
// reason must stay reasonAutomergeDisabled (matching today's healthy-tick
// behavior), never the specific upstream-read failure.
func TestTickRecordsKillSwitchReasonEvenWhenUpstreamReadAlsoFails(t *testing.T) {
	// fleetConfigPath stays pinned to "" (disabled) by the package's
	// TestMain -- no withFleetAutomergeEnabled call needed.
	var calls [][]string
	withCommands(t, nil, &calls) // pr view fails immediately: no responses scripted
	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 60}
	if _, _, err := tick(&s); err == nil {
		t.Fatal("tick: err = nil, want the pr view failure to still surface as a tick error")
	}
	if s.AutomergeReason != reasonAutomergeDisabled {
		t.Fatalf("AutomergeReason = %q, want %q: with the kill switch off, the recorded reason must stay the kill-switch reason even when an upstream read also fails (Q4)", s.AutomergeReason, reasonAutomergeDisabled)
	}
}

// -- #854: one decision per tick (workflow launch failures, item 4) ----------

// TestTickRecordsAutomergeHoldWhenAddressReviewLaunchFails pins item 4: a
// failed launch() call (here, address-review, dispatched before runAutomerge
// ever runs) during an automerge-enabled tick must still persist and log
// exactly one automerge decision -- under reasonWorkflowLaunchFailed --
// before tick returns the underlying error, rather than silently leaving a
// stale decision from the previous tick displayed (Decision 7). #885 moves
// the address-review launch after reconcileFeedback's own end-of-tick pass,
// so the new comment:7 key's lazy GraphQL thread fetch (the 5th scripted gh
// response) now runs before the launch is even attempted.
func TestTickRecordsAutomergeHoldWhenAddressReviewLaunchFails(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	responses := []string{
		openPR(),
		`[]`,
		`[{"id":7,"updated_at":"2026-01-02T00:00:00Z","user":{"login":"reviewer"}}]`,
		`[]`,
		unresolvedThreadFor(7),
	}
	originalCommand := command
	originalExecGh := execGh
	i := 0
	execGh = func(args ...string) (string, string, error) {
		calls = append(calls, append([]string{"gh"}, args...))
		if i >= len(responses) {
			return "", "", fmt.Errorf("unexpected command: %s", strings.Join(args, " "))
		}
		out := responses[i]
		i++
		return out, "", nil
	}
	command = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte("boom"), errors.New("exit status 1")
	}
	// #975: a recorded session (plus a stubbed tmuxHasSession) is required so
	// this launch attempt reaches the command() self-exec below and fails
	// for the intended reason, rather than short-circuiting on the new
	// "no session recorded" gate before command() is ever called.
	originalTmuxHasSession := tmuxHasSession
	tmuxHasSession = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() {
		command = originalCommand
		execGh = originalExecGh
		tmuxHasSession = originalTmuxHasSession
	})

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 300, CurrentDelaySeconds: 900, LaunchSession: "work"}
	if _, _, err := tick(&s); err == nil {
		t.Fatal("tick: err = nil, want the address-review launch failure to still surface as a tick error")
	}
	if s.AutomergeReason != reasonWorkflowLaunchFailed {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonWorkflowLaunchFailed)
	}
	if s.AutomergeCheckedAt.IsZero() {
		t.Fatal("AutomergeCheckedAt was not stamped when the address-review launch failed")
	}
}

// TestTickRecordsAutomergeHoldWhenCIRepairLaunchFails pins item 4: a failed
// launch() call for ci-repair (failing CI checks, FixAttempts still below
// fixCap) during an automerge-enabled tick must still persist and log
// exactly one automerge decision -- under reasonWorkflowLaunchFailed --
// before tick returns the underlying error, mirroring
// TestTickRecordsAutomergeHoldWhenAddressReviewLaunchFails for the ci-repair
// launch() call site.
func TestTickRecordsAutomergeHoldWhenCIRepairLaunchFails(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	responses := []string{
		openPR(),
		`[{"bucket":"fail","name":"test","state":"FAILURE"}]`,
	}
	originalCommand := command
	originalExecGh := execGh
	i := 0
	execGh = func(args ...string) (string, string, error) {
		calls = append(calls, append([]string{"gh"}, args...))
		if i >= len(responses) {
			return "", "", fmt.Errorf("unexpected command: %s", strings.Join(args, " "))
		}
		out := responses[i]
		i++
		return out, "", nil
	}
	command = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte("boom"), errors.New("exit status 1")
	}
	// #975: see the identical note on TestTickRecordsAutomergeHoldWhenAddressReviewLaunchFails.
	originalTmuxHasSession := tmuxHasSession
	tmuxHasSession = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() {
		command = originalCommand
		execGh = originalExecGh
		tmuxHasSession = originalTmuxHasSession
	})

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 300, CurrentDelaySeconds: 900, FixAttempts: 0, LaunchSession: "work"}
	if _, _, err := tick(&s); err == nil {
		t.Fatal("tick: err = nil, want the ci-repair launch failure to still surface as a tick error")
	}
	if s.AutomergeReason != reasonWorkflowLaunchFailed {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonWorkflowLaunchFailed)
	}
	if s.AutomergeCheckedAt.IsZero() {
		t.Fatal("AutomergeCheckedAt was not stamped when the ci-repair launch failed")
	}
}

// TestTickRecordsAutomergeHoldWhenBabysitAttentionLaunchFails pins item 4: a
// failed launch() call for babysit-attention (failing CI checks, FixAttempts
// already at fixCap) during an automerge-enabled tick must still persist and
// log exactly one automerge decision -- under reasonWorkflowLaunchFailed --
// before tick returns the underlying error, mirroring
// TestTickRecordsAutomergeHoldWhenAddressReviewLaunchFails for the
// babysit-attention launch() call site.
func TestTickRecordsAutomergeHoldWhenBabysitAttentionLaunchFails(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	responses := []string{
		openPR(),
		`[{"bucket":"fail","name":"test","state":"FAILURE"}]`,
	}
	originalCommand := command
	originalExecGh := execGh
	i := 0
	execGh = func(args ...string) (string, string, error) {
		calls = append(calls, append([]string{"gh"}, args...))
		if i >= len(responses) {
			return "", "", fmt.Errorf("unexpected command: %s", strings.Join(args, " "))
		}
		out := responses[i]
		i++
		return out, "", nil
	}
	command = func(name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte("boom"), errors.New("exit status 1")
	}
	// #975: see the identical note on TestTickRecordsAutomergeHoldWhenAddressReviewLaunchFails.
	originalTmuxHasSession := tmuxHasSession
	tmuxHasSession = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() {
		command = originalCommand
		execGh = originalExecGh
		tmuxHasSession = originalTmuxHasSession
	})

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 300, CurrentDelaySeconds: 900, FixAttempts: fixCap, LaunchSession: "work"}
	if _, _, err := tick(&s); err == nil {
		t.Fatal("tick: err = nil, want the babysit-attention launch failure to still surface as a tick error")
	}
	if s.AutomergeReason != reasonWorkflowLaunchFailed {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonWorkflowLaunchFailed)
	}
	if s.AutomergeCheckedAt.IsZero() {
		t.Fatal("AutomergeCheckedAt was not stamped when the babysit-attention launch failed")
	}
}

// -- #854: detection-read pagination feeding an automerge hold ---------------

// TestTickHoldsAutomergeWhenCommentsDetectionReadPossiblyTruncated pins the
// pagination completeness signal (#854, pulled in from #862's scope): a
// comments detection read that comes back exactly page-sized (100 items,
// GitHub REST's default/max per_page) can never be proven complete without
// actually fetching a second page -- until the paginated fetcher confirms a
// short/empty terminating page, automerge must hold under its own
// truncation reason rather than silently treat the page as the full list.
func TestTickHoldsAutomergeWhenCommentsDetectionReadPossiblyTruncated(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	// A full-sized page (< feedbackPageSize items would terminate as
	// complete) that keeps coming back full all the way to maxFeedbackPages
	// -- the cap-exhaustion case: fetchPaged can never prove completeness
	// without the traversal itself producing a short/empty page, so the
	// comments detection read must exhaust the full page cap to genuinely
	// test the pagination-completeness signal, not merely "one full page".
	fullPage := func(offset int) string {
		items := make([]string, feedbackPageSize)
		for i := range items {
			// All from a "[bot]" login: the detection loop already ignores
			// bot comments, so this fixture isolates the pagination-
			// completeness question from the "new pending feedback" one.
			items[i] = fmt.Sprintf(`{"id":%d,"updated_at":"2025-01-01T00:00:00Z","user":{"login":"bot[bot]"}}`, offset*feedbackPageSize+i)
		}
		return "[" + strings.Join(items, ",") + "]"
	}

	script := []scriptedCall{
		{out: automergeEligiblePR()},
		{out: `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`},
	}
	for page := 0; page < maxFeedbackPages; page++ {
		script = append(script, scriptedCall{out: fullPage(page)})
	}
	script = append(script, scriptedCall{out: `[]`})                                   // reviews
	script = append(script, scriptedCall{out: `{"labels":[{"name":"automerge:ok"}]}`}) // closing issue #9 labels

	var calls [][]string
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeReason != reasonCommentsReadTruncated {
		t.Fatalf("AutomergeReason = %q, want %q: a full-page comments read cannot be proven complete without a paginated fetch confirming a short terminating page", s.AutomergeReason, reasonCommentsReadTruncated)
	}
	for _, c := range calls {
		if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
			t.Fatalf("gh pr merge must not be issued while the comments detection read is possibly truncated: %#v", calls)
		}
	}
}

// -- #885: pre-merge revalidation of ALL known feedback (AC 7) ---------------
//
// The pre-merge recheck (Decision 9) must reread authoritative GraphQL
// thread state -- not carry the first pass's already-computed FeedbackHold
// -- and must do so for previously-addressed keys too, per Q3's widened
// lazy gate ("any comment: key in pending union addressed"). Every case
// below sets an addressed "comment:"-prefixed key on State so tick's own
// first-pass reconcile ALSO fires the widened gate once (hence
// automergeFirstPassScriptWithFeedbackThread, not the bare
// automergeFirstPassScript), then asserts the recheck's own independent
// GraphQL read -- not the first pass's verdict -- decides the final hold.
// Every subtest asserts the exact AutomergeReason (watch/docs/error-
// handling.md #446) and that no `gh pr merge` call ever appears in calls
// (AC 7).

// automergeFirstPassScriptWithFeedbackThread extends automergeFirstPassScript
// with the widened GraphQL gate's thread-resolution call (Q3): when State
// carries an addressed "comment:"-prefixed key, tick's own reconcileFeedback
// call fires the lazy GraphQL fetch immediately after tick's own reviews
// fetch (position 4), before runAutomerge's labels fetch -- inserted here so
// first-pass evidence can carry a real addressed comment: key through to the
// pre-merge recheck.
func automergeFirstPassScriptWithFeedbackThread(thread string) []scriptedCall {
	return []scriptedCall{
		{out: automergeEligiblePR()},
		{out: `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`},
		{out: `[]`},
		{out: `[]`},
		{out: thread},
		{out: `{"labels":[{"name":"automerge:ok"}]}`},
		{out: `{"automerge":{"maxChangedFiles":10,"maxDiffLines":500,"mergeMethod":"squash"}}`},
		{out: `{"squash":true,"merge":false,"rebase":true}`},
		{out: queueProbeResponse(false, false, "abc")},
	}
}

// automergeRecheckScriptWithFeedbackThreadPages mirrors automergeRecheckScript
// but inserts the pre-merge revalidation's own fresh GraphQL thread call(s)
// (this ticket's core change) between the recheck's reviews re-fetch and its
// labels re-fetch -- the same slot tick's own reconcileFeedback uses.
// threadPages may hold more than one entry to model a multi-page (or
// page-cap-exhausted) GraphQL traversal.
func automergeRecheckScriptWithFeedbackThreadPages(pr, checks, comments, reviews string, threadPages []string, labels, policy, queue string) []scriptedCall {
	calls := []scriptedCall{{out: pr}, {out: checks}, {out: comments}, {out: reviews}}
	for _, p := range threadPages {
		calls = append(calls, scriptedCall{out: p})
	}
	calls = append(calls, scriptedCall{out: labels}, scriptedCall{out: policy}, scriptedCall{out: queue})
	return calls
}

// automergeRecheckScriptWithFeedbackThread is the single-page convenience
// wrapper around automergeRecheckScriptWithFeedbackThreadPages.
func automergeRecheckScriptWithFeedbackThread(pr, checks, comments, reviews, thread, labels, policy, queue string) []scriptedCall {
	return automergeRecheckScriptWithFeedbackThreadPages(pr, checks, comments, reviews, []string{thread}, labels, policy, queue)
}

// TestTickPreMergeRecheckRevalidatesFeedbackReopenedSinceFirstPass is AC 1 /
// AC 3 -- the plan's explicit "this case must fail against the current
// carry-over implementation" regression: the first pass sees the addressed
// thread still resolved (so it proceeds to the recheck), but the recheck's
// own independent GraphQL read reports it isResolved:false since. The
// pre-merge boundary must hold under the fresh reasonFeedbackReopened
// verdict, not silently merge on the first pass's already-stale clean
// verdict.
func TestTickPreMergeRecheckRevalidatesFeedbackReopenedSinceFirstPass(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	script := automergeFirstPassScriptWithFeedbackThread(resolvedThreadFor(5)) // first pass: thread still resolved
	script = append(script, automergeRecheckScriptWithFeedbackThread(
		automergeEligiblePR(),
		`[{"bucket":"pass","name":"test","state":"SUCCESS"}]`,
		`[]`,
		`[]`,
		unresolvedThreadFor(5), // the recheck's own fresh read: reopened since the first pass
		`{"labels":[{"name":"automerge:ok"}]}`,
		`{"automerge":{"maxChangedFiles":10,"maxDiffLines":500,"mergeMethod":"squash"}}`,
		queueProbeResponse(false, false, "abc"),
	)...)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900, AddressedKeys: []string{"comment:5"}}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeReason != reasonFeedbackReopened {
		t.Fatalf("AutomergeReason = %q, want %q: the pre-merge recheck must reread authoritative thread state, not carry the first pass's already-stale clean verdict", s.AutomergeReason, reasonFeedbackReopened)
	}
	if s.AutomergeDecision == "merge" {
		t.Fatal("AutomergeDecision = \"merge\", want held: the pre-merge recheck found the addressed thread reopened since the first evaluation")
	}
	for _, c := range calls {
		if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
			t.Fatalf("gh pr merge must not be issued: the pre-merge recheck found the addressed thread reopened: %#v", calls)
		}
	}
}

// TestTickPreMergeRecheckHoldsWhenAddressedKeyAbsentFromFreshThreadRead pins
// Q1's fail-closed extension applied at the pre-merge boundary: the
// recheck's own fresh thread read no longer reports the addressed key at
// all (deleted comment, force-push-purged thread) -- absence must hold
// under the existing reasonReviewStateUnknown, never be silently treated as
// resolved.
func TestTickPreMergeRecheckHoldsWhenAddressedKeyAbsentFromFreshThreadRead(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	absentThread := threadPage(false, "", threadNode(true, 1, 999)) // an unrelated thread; comment:5 is no longer reported at all
	script := automergeFirstPassScriptWithFeedbackThread(resolvedThreadFor(5))
	script = append(script, automergeRecheckScriptWithFeedbackThread(
		automergeEligiblePR(), `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`, `[]`, `[]`,
		absentThread,
		`{"labels":[{"name":"automerge:ok"}]}`,
		`{"automerge":{"maxChangedFiles":10,"maxDiffLines":500,"mergeMethod":"squash"}}`,
		queueProbeResponse(false, false, "abc"),
	)...)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900, AddressedKeys: []string{"comment:5"}}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeReason != reasonReviewStateUnknown {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonReviewStateUnknown)
	}
	for _, c := range calls {
		if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
			t.Fatalf("gh pr merge must not be issued: the recheck found the addressed key absent from a fresh thread read: %#v", calls)
		}
	}
}

// TestTickPreMergeRecheckHoldsOnTruncatedThreadRead covers both truncation
// shapes at the pre-merge boundary: a per-thread comments.totalCount
// mismatch, and a page-cap-exhausted traversal that never terminates on a
// short page -- both must hold under the distinct reasonFeedbackTruncated,
// never be treated as a complete-but-partial read.
func TestTickPreMergeRecheckHoldsOnTruncatedThreadRead(t *testing.T) {
	for _, tc := range []struct {
		name        string
		threadPages []string
	}{
		{
			"totalCount exceeds the fetched node count",
			[]string{threadPage(false, "", threadNode(false, 2, 5))},
		},
		{
			"page cap exhausted with hasNextPage still true",
			func() []string {
				pages := make([]string, maxThreadPages)
				for i := range pages {
					pages[i] = threadPage(true, fmt.Sprintf("C%d", i), threadNode(false, 1, 5))
				}
				return pages
			}(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFleetAutomergeEnabled(t, true)
			var calls [][]string
			script := automergeFirstPassScriptWithFeedbackThread(resolvedThreadFor(5))
			script = append(script, automergeRecheckScriptWithFeedbackThreadPages(
				automergeEligiblePR(), `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`, `[]`, `[]`,
				tc.threadPages,
				`{"labels":[{"name":"automerge:ok"}]}`,
				`{"automerge":{"maxChangedFiles":10,"maxDiffLines":500,"mergeMethod":"squash"}}`,
				queueProbeResponse(false, false, "abc"),
			)...)
			withScriptedCommands(t, script, &calls)

			s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900, AddressedKeys: []string{"comment:5"}}
			if _, _, err := tick(&s); err != nil {
				t.Fatal(err)
			}
			if s.AutomergeReason != reasonFeedbackTruncated {
				t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonFeedbackTruncated)
			}
			for _, c := range calls {
				if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
					t.Fatalf("gh pr merge must not be issued: %#v", calls)
				}
			}
		})
	}
}

// TestTickPreMergeRecheckHoldsOnUnreadableThreadReadWithFreshConditions pins
// the plan's explicit merge.go Files-to-Modify instruction: "route a
// revalidation fetch failure into in.FeedbackHold/FeedbackDetail (so the
// second evaluation still runs and produces fresh Conditions) rather than
// the err return path." The recheck's own thread read fails outright
// (malformed body) -- the pre-merge boundary must still complete a full
// second evaluateAutomerge pass (a fresh, distinct Conditions set, not the
// first pass's all-"yes" one, and not the "recheck fetch failed entirely,
// clear Conditions" path used for a PR-view/checks/comments/reviews-level
// recheck failure).
func TestTickPreMergeRecheckHoldsOnUnreadableThreadReadWithFreshConditions(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	script := automergeFirstPassScriptWithFeedbackThread(resolvedThreadFor(5))
	script = append(script, automergeRecheckScriptWithFeedbackThread(
		automergeEligiblePR(), `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`, `[]`, `[]`,
		"not json", // the recheck's own thread read fails outright
		`{"labels":[{"name":"automerge:ok"}]}`,
		`{"automerge":{"maxChangedFiles":10,"maxDiffLines":500,"mergeMethod":"squash"}}`,
		queueProbeResponse(false, false, "abc"),
	)...)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900, AddressedKeys: []string{"comment:5"}}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeReason != reasonFeedbackUnreadable {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonFeedbackUnreadable)
	}
	if len(s.AutomergeConditions) == 0 {
		t.Fatal("AutomergeConditions is empty, want a fresh, second-pass Conditions set (the second evaluation still ran)")
	}
	found := false
	for _, c := range s.AutomergeConditions {
		if c.Key == "review" {
			found = true
			if c.Pass {
				t.Fatalf("condition %q = %+v, want Pass=false: the second pass's own feedback hold must fail this stage", c.Key, c)
			}
		}
	}
	if !found {
		t.Fatalf("AutomergeConditions = %#v, want a \"review\" stage entry from the fresh second pass", s.AutomergeConditions)
	}
	for _, c := range calls {
		if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
			t.Fatalf("gh pr merge must not be issued: %#v", calls)
		}
	}
}

// TestTickPreMergeRecheckHoldsWhenReviewsReadNotProvenComplete pins the
// Test Strategy's "recheck's reviews read is not proven complete" case: the
// recheck's own reviews re-fetch (independent of the first pass's) must
// also be proven complete, not just the first pass's -- a full-page-to-cap
// reviews read at the pre-merge boundary must hold under
// reasonReviewsReadTruncated. Unlike the other cases in this section, this
// does not require an addressed comment: key -- ReviewsComplete is a
// pre-existing recheck input independent of #885's feedback-reclassification
// work, pinned here purely as a regression guard through the refactor.
func TestTickPreMergeRecheckHoldsWhenReviewsReadNotProvenComplete(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	script := automergeFirstPassScript() // no addressed comment: keys: the widened gate never fires on the first pass
	fullReviewsPage := func() string {
		items := make([]string, feedbackPageSize)
		for i := range items {
			items[i] = fmt.Sprintf(`{"id":%d,"state":"APPROVED","submitted_at":"2025-01-01T00:00:00Z","user":{"login":"filler%d"}}`, i, i)
		}
		return "[" + strings.Join(items, ",") + "]"
	}
	recheck := []scriptedCall{
		{out: automergeEligiblePR()},
		{out: `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`},
		{out: `[]`}, // comments
	}
	for page := 0; page < maxFeedbackPages; page++ {
		recheck = append(recheck, scriptedCall{out: fullReviewsPage()})
	}
	recheck = append(recheck,
		scriptedCall{out: `{"labels":[{"name":"automerge:ok"}]}`},
		scriptedCall{out: `{"automerge":{"maxChangedFiles":10,"maxDiffLines":500,"mergeMethod":"squash"}}`},
		scriptedCall{out: queueProbeResponse(false, false, "abc")},
	)
	script = append(script, recheck...)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeReason != reasonReviewsReadTruncated {
		t.Fatalf("AutomergeReason = %q, want %q: the recheck's own reviews re-fetch must be proven complete too, not just the first pass's", s.AutomergeReason, reasonReviewsReadTruncated)
	}
	for _, c := range calls {
		if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
			t.Fatalf("gh pr merge must not be issued: %#v", calls)
		}
	}
}

// TestTickPreMergeRecheckAllGreenWithAddressedKeysReconfirmedResolvedStillMerges
// is the Test Strategy's explicit regression: an all-green PR whose
// addressed keys all re-confirm resolved at both the first pass AND the
// recheck must still merge -- the new reads must not become a permanent
// hold on the happy path.
func TestTickPreMergeRecheckAllGreenWithAddressedKeysReconfirmedResolvedStillMerges(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	script := automergeFirstPassScriptWithFeedbackThread(resolvedThreadFor(5))
	script = append(script, automergeRecheckScriptWithFeedbackThread(
		automergeEligiblePR(), `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`, `[]`, `[]`,
		resolvedThreadFor(5), // the recheck's own fresh read: still resolved
		`{"labels":[{"name":"automerge:ok"}]}`,
		`{"automerge":{"maxChangedFiles":10,"maxDiffLines":500,"mergeMethod":"squash"}}`,
		queueProbeResponse(false, false, "abc"),
	)...)
	script = append(script,
		scriptedCall{out: "Merged pull request #42 (o/r)"}, // gh pr merge, zero exit
		scriptedCall{out: `{"number":42,"title":"Change","state":"MERGED","headRefName":"feature","headRefOid":"abc","baseRefName":"main","mergeable":"MERGEABLE","isDraft":false,"changedFiles":1,"additions":5,"deletions":2,"files":[{"path":"watch/internal/babysit/x.go"}],"url":"https://example/pr/42","closingIssuesReferences":[{"number":9}]}`}, // post-merge verification refetch
		scriptedCall{out: "https://example/pr/42#issuecomment-1"}, // #1049: the post-merge attribution comment
	)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900, AddressedKeys: []string{"comment:5"}}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeDecision != "merge" {
		t.Fatalf("AutomergeDecision = %q, want %q: an all-green PR whose addressed keys all re-confirm resolved must still merge, not become a permanent hold", s.AutomergeDecision, "merge")
	}
	mergeIssued := false
	for _, c := range calls {
		if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
			mergeIssued = true
		}
	}
	if !mergeIssued {
		t.Fatalf("gh pr merge was not issued: %#v", calls)
	}
}

// -- #886: fail closed on command ambiguity, merge uncertainty, kill-switch changes --

// mergeCallCount counts how many `gh pr merge` invocations appear in calls.
func mergeCallCount(calls [][]string) int {
	n := 0
	for _, c := range calls {
		if len(c) > 2 && c[1] == "pr" && c[2] == "merge" {
			n++
		}
	}
	return n
}

// hasCondition reports whether conds contains an entry for key that was
// reached with the given pass value -- used to assert a recheck hold went
// through the normal condition chain (Conditions populated) rather than the
// I/O-read-failure branch (Conditions cleared to nil).
func hasCondition(conds []conditionResult, key string, pass bool) bool {
	for _, c := range conds {
		if c.Key == key && c.Reached && c.Pass == pass {
			return true
		}
	}
	return false
}

// TestTickAutomergeMergeNonzeroExitButRefetchConfirmsMerged is merge-outcome
// matrix case (a) (AC 3): a `gh pr merge` invocation that itself exits
// nonzero (e.g. a client-side timeout or a transient network blip) must
// still be confirmed as a real success once the unconditional post-merge
// refetch reports MERGED -- recording merged==true, Reason=="", a non-empty
// diagnostic Detail (the command reported failure despite the confirmed
// merge), and exactly ONE `gh pr merge` call (no retry).
func TestTickAutomergeMergeNonzeroExitButRefetchConfirmsMerged(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	mergedPR := `{"number":42,"title":"Change","state":"MERGED","headRefName":"feature","headRefOid":"abc","baseRefName":"main","mergeable":"MERGEABLE","isDraft":false,"changedFiles":1,"additions":5,"deletions":2,"files":[{"path":"watch/internal/babysit/x.go"}],"url":"https://example/pr/42","closingIssuesReferences":[{"number":9}]}`
	script := automergeFirstPassScript()
	script = append(script, automergeGreenRecheckScript()...)
	script = append(script,
		scriptedCall{out: "context deadline exceeded", err: errors.New("exit status 1")}, // gh pr merge, nonzero exit
		scriptedCall{out: mergedPR},                               // the unconditional post-merge refetch
		scriptedCall{out: "https://example/pr/42#issuecomment-1"}, // #1049: the post-merge attribution comment
	)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	terminal, delay, err := tick(&s)
	if err != nil {
		t.Fatal(err)
	}
	if terminal {
		t.Fatal("tick must not treat the merge-confirming tick itself as terminal")
	}
	if delay != 60*time.Second || s.CurrentDelaySeconds != 60 {
		t.Fatalf("a confirmed merge must reset the backoff delay to the interval: delay=%v state=%d", delay, s.CurrentDelaySeconds)
	}
	if s.AutomergeDecision != "merge" {
		t.Fatalf("AutomergeDecision = %q, want \"merge\": a nonzero gh pr merge exit followed by a MERGED refetch must record confirmed success", s.AutomergeDecision)
	}
	if s.AutomergeReason != "" {
		t.Fatalf("AutomergeReason = %q, want empty on confirmed success", s.AutomergeReason)
	}
	if s.AutomergeDetail == "" {
		t.Fatal("AutomergeDetail = \"\", want a non-empty diagnostic noting the merge command itself reported failure despite the confirmed merge")
	}
	if n := mergeCallCount(calls); n != 1 {
		t.Fatalf("gh pr merge calls = %d, want exactly 1: a nonzero exit followed by a MERGED refetch must never retry the merge command", n)
	}
}

// TestTickAutomergeMergeNonzeroExitThenRefetchUnreadableNeverSucceeds is the
// nonzero-exit half of merge-outcome matrix case (d) (AC 4): regardless of
// what `gh pr merge` itself reported, an unreadable post-merge refetch must
// never record success -- distinct from
// TestTickAutomergePostMergeVerificationReadFailureIsDistinctReason, which
// only covers the zero-exit half.
func TestTickAutomergeMergeNonzeroExitThenRefetchUnreadableNeverSucceeds(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	script := automergeFirstPassScript()
	script = append(script, automergeGreenRecheckScript()...)
	script = append(script,
		scriptedCall{out: "context deadline exceeded", err: errors.New("exit status 1")}, // gh pr merge, nonzero exit
		scriptedCall{out: "rate limited", err: errors.New("exit status 1")},              // the post-merge refetch itself fails
	)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeDecision == "merge" {
		t.Fatal("AutomergeDecision = \"merge\", want held: an unreadable post-merge refetch must never record success, regardless of the merge command's own exit status")
	}
	if s.AutomergeReason != reasonMergeVerifyUnreadable {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonMergeVerifyUnreadable)
	}
	if n := mergeCallCount(calls); n != 1 {
		t.Fatalf("gh pr merge calls = %d, want exactly 1: no retry", n)
	}
}

// TestExecuteMerge_RealTimeoutOnMergeCallClassifiesFailureClassTimeout is the
// package-babysit half of #915's variant (4) split (parent #915's Decisions,
// AC2; the other half is
// dispatch.TestAutonomousChain_MergeAcceptedButClientSeesFailureIsSettledByPostMergeRefetch,
// internal/dispatch/chain_negative_babysit_test.go): a genuine errGhTimeout /
// failureClassTimeout classification "on the merge call" itself, produced
// through the REAL execGh (never scripted through the execGh seam, which
// withScriptedCommands above uses for every other executeMerge test) --
// mirrors TestExecGh_ParentContextDeadlineClassifiesAsTimeout's PATH-shim
// technique (gh_test.go), driving executeMerge directly (unexported,
// in-package) rather than a full tick.
//
// A per-call ghParentContext is load-bearing (plan Assumptions): in
// production ghParentContext is always context.Background(), and the
// post-merge refetch never inherits the merge call's own expired deadline.
// A single shared expired context here would make the refetch fail too,
// misattributing failureClassTimeout to reasonMergeVerifyUnreadable (the
// refetch's own failure reason) instead of reasonMergeFailed (the merge
// call's), contradicting #915's "on the merge call" wording. The fake `gh`
// shim uses "exec sleep 5" (never a bare "sleep 5" followed by another line)
// for the merge call only -- TestExecGh_ParentContextDeadlineClassifiesAsTimeout's
// own comment explains why: a bare "sleep 5\nexit 0\n" forks sleep as a
// child the shell's own SIGKILL never reaches, leaving an orphan holding the
// output pipe open for the full 5s regardless of ghParentContext's short
// deadline. Detail is deliberately never asserted non-empty here (plan Step
// 8): the killed shim writes nothing to either stream before it dies.
func TestExecuteMerge_RealTimeoutOnMergeCallClassifiesFailureClassTimeout(t *testing.T) {
	// Defense-in-depth (never expected to matter given the PATH-prepended
	// shim below, but cheap insurance against a future reordering of the
	// t.Setenv("PATH", ...) call or the shim install ever letting the real
	// `gh` binary resolve and act on ambient credentials): neutralize every
	// gh auth-resolution env var, mirroring dispatch.newChainHarness's
	// credential-neutralization block (security review #914 finding #1).
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_ENTERPRISE_TOKEN", "")
	t.Setenv("GITHUB_ENTERPRISE_TOKEN", "")
	t.Setenv("GH_HOST", "")

	shimDir := t.TempDir()
	logPath := filepath.Join(shimDir, "invocations.log")
	quotedLog := "'" + strings.ReplaceAll(logPath, "'", `'\''`) + "'"
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + quotedLog + "\n" +
		"if [ \"$1\" = pr ] && [ \"$2\" = merge ]; then\n" +
		"  exec sleep 5\n" +
		"fi\n" +
		// The post-merge refetch's own `gh pr view` response: a minimal,
		// genuinely-parseable prView body reporting anything other than
		// MERGED, so executeMerge falls through to the mergeErr != nil ->
		// reasonMergeFailed branch rather than the "refetch confirms MERGED"
		// branch.
		"printf '%s' '{\"number\":42,\"state\":\"OPEN\"}'\n"
	if err := os.WriteFile(filepath.Join(shimDir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	// PATH-prepended (mirrors TestExecGh_OversizedOutputIsExplicitTruncationError,
	// never installFakeGH, which replaces PATH wholesale and would make
	// `sleep`/`[`/`echo` unresolvable inside the shim's own /bin/sh).
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	shortCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	t.Cleanup(cancel)
	firstCall := true
	original := ghParentContext
	ghParentContext = func() context.Context {
		if firstCall {
			firstCall = false
			return shortCtx
		}
		return context.Background()
	}
	t.Cleanup(func() { ghParentContext = original })

	s := &State{PR: "42", Repo: "o/r"}
	start := time.Now()
	merged, decision := executeMerge(s, "abc123", automergeDecision{Merge: true})
	elapsed := time.Since(start)

	if merged {
		t.Fatal("executeMerge: merged = true, want false on a genuine merge-call timeout")
	}
	if decision.Reason != reasonMergeFailed {
		t.Fatalf("decision.Reason = %q, want %q", decision.Reason, reasonMergeFailed)
	}
	if decision.FailureClass != failureClassTimeout {
		t.Fatalf("decision.FailureClass = %q, want %q (a genuine errGhTimeout classification through the real execGh)", decision.FailureClass, failureClassTimeout)
	}
	if elapsed >= 5*time.Second {
		t.Fatalf("executeMerge took %v, want it bounded by ghParentContext's short deadline on the merge call, not the shim's full 5s sleep", elapsed)
	}
	if elapsed >= ghTimeout {
		t.Fatalf("executeMerge took %v, want it well within ghTimeout (%v)", elapsed, ghTimeout)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading shim invocation log: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("shim invocations = %v, want exactly 2 (the killed merge call, then the post-merge refetch)", lines)
	}
	if !strings.HasPrefix(lines[0], "pr merge ") {
		t.Fatalf("first shim invocation = %q, want a %q-prefixed `gh pr merge` call", lines[0], "pr merge ")
	}
	if !strings.HasPrefix(lines[1], "pr view ") {
		t.Fatalf("second shim invocation = %q, want a %q-prefixed `gh pr view` call (the single authoritative post-merge refetch)", lines[1], "pr view ")
	}
}

// TestTickAutomergeRecheckKillSwitchHoldsMatrix pins AC 5/6: the final
// pre-merge re-evaluation re-reads the fleet kill switch as its last step
// (recheckAutomergeInputs' loadFleetSwitch() call), and every distinct way
// that re-read can come back disabled -- an explicit false, a config file
// that vanished, one that is unreadable (a directory), one with malformed
// JSON, and one whose automerge.enabled key is simply absent -- holds under
// its own distinct reason and never issues `gh pr merge`, even though the
// first-pass read (before the two full evaluation-script prefixes below) saw
// the switch enabled.
func TestTickAutomergeRecheckKillSwitchHoldsMatrix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		secondPass string
		wantReason string
	}{
		{"fleet switch flips from enabled to explicitly disabled between passes", `{"automerge":{"enabled":false}}`, reasonKillSwitchDisabled},
		{"fleet config missing at recheck", "missing", reasonKillSwitchConfigMissing},
		{"fleet config unreadable (a directory, not chmod 000 -- root-proof) at recheck", "unreadable", reasonKillSwitchConfigUnreadable},
		{"fleet config malformed JSON at recheck", "{not json", reasonKillSwitchConfigMalformed},
		{"fleet config's automerge.enabled key absent at recheck", `{"automerge":{}}`, reasonKillSwitchEnabledAbsent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFleetAutomergeSequence(t, `{"automerge":{"enabled":true}}`, tc.secondPass)
			var calls [][]string
			script := automergeFirstPassScript()
			script = append(script, automergeGreenRecheckScript()...)
			withScriptedCommands(t, script, &calls)

			s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
			if _, _, err := tick(&s); err != nil {
				t.Fatal(err)
			}
			if n := mergeCallCount(calls); n != 0 {
				t.Fatalf("gh pr merge calls = %d, want 0: the fleet kill switch changed at recheck time: %#v", n, calls)
			}
			if s.AutomergeDecision == "merge" {
				t.Fatal("AutomergeDecision = \"merge\", want held: an enabled-to-disabled fleet switch change must block the merge")
			}
			if s.AutomergeReason != tc.wantReason {
				t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, tc.wantReason)
			}
		})
	}
}

// TestTickAutomergeRecheckKillSwitchExplicitDisableRendersEnabledNo pins the
// kill-switch hold's Conditions rendering: runAutomerge's failReason != ""
// branch sets a single synthetic enabled=no entry (mirroring
// evaluateAutomerge's own first stage), distinct from the I/O-read-failure
// branch that clears Conditions to nil entirely, so the resulting
// Conditions must contain that genuine enabled=no entry, matching the
// logLine format a human reads during incident triage.
func TestTickAutomergeRecheckKillSwitchExplicitDisableRendersEnabledNo(t *testing.T) {
	withFleetAutomergeSequence(t, `{"automerge":{"enabled":true}}`, `{"automerge":{"enabled":false}}`)
	var calls [][]string
	script := automergeFirstPassScript()
	script = append(script, automergeGreenRecheckScript()...)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if !hasCondition(s.AutomergeConditions, "enabled", false) {
		t.Fatalf("AutomergeConditions = %#v, want an enabled=no (Reached=true, Pass=false) entry: the recheck must render through the normal condition chain, not clear Conditions like an I/O read failure would", s.AutomergeConditions)
	}
}

// TestTickAutomergeFailureClassClearsOnCleanTickAfterFailure pins that
// recordDecision assigns AutomergeFailureClass unconditionally: a failed
// tick records a non-empty class, and the very next clean, merge-issuing
// tick must clear it rather than leave a stale class from the previous
// failure displayed.
func TestTickAutomergeFailureClassClearsOnCleanTickAfterFailure(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	withCommands(t, nil, &calls) // pr view fails immediately: no responses scripted
	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 60}
	if _, _, err := tick(&s); err == nil {
		t.Fatal("tick: err = nil, want the pr view failure to surface as a tick error")
	}
	if s.AutomergeFailureClass == "" {
		t.Fatal("AutomergeFailureClass = \"\", want non-empty after a failed upstream read")
	}

	calls = nil
	mergedPR := `{"number":42,"title":"Change","state":"MERGED","headRefName":"feature","headRefOid":"abc","baseRefName":"main","mergeable":"MERGEABLE","isDraft":false,"changedFiles":1,"additions":5,"deletions":2,"files":[{"path":"watch/internal/babysit/x.go"}],"url":"https://example/pr/42","closingIssuesReferences":[{"number":9}]}`
	script := automergeFirstPassScript()
	script = append(script, automergeGreenRecheckScript()...)
	script = append(script,
		scriptedCall{out: "Merged pull request #42 (o/r)"},
		scriptedCall{out: mergedPR},
		scriptedCall{out: "https://example/pr/42#issuecomment-1"}, // #1049: the post-merge attribution comment
	)
	withScriptedCommands(t, script, &calls)
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeFailureClass != "" {
		t.Fatalf("AutomergeFailureClass = %q, want cleared on the following clean tick, not stale from the previous failure", s.AutomergeFailureClass)
	}
}

// TestTickPaginatedReadTimeoutProducesSiteReasonAndTimeoutClass is a
// regression test for the pagination.go %w-chain fix (#886): previously
// fetchPaged's mid-pagination failure wrap used "%w: %s: %s" (the trailing
// %s stringifies the underlying gh error, breaking errors.Is/errors.As from
// ever reaching it) -- a paginated comments/reviews read that times out must
// still produce both its own site reason (reasonUpstreamCommentsUnreadable /
// reasonUpstreamReviewsUnreadable) AND classifyGhFailure(err) ==
// failureClassTimeout, which was impossible while the chain was broken.
func TestTickPaginatedReadTimeoutProducesSiteReasonAndTimeoutClass(t *testing.T) {
	for _, tc := range []struct {
		name        string
		extraOKresp int // how many additional scripted OK responses before the paginated read that times out
		wantReason  string
	}{
		{"comments read times out", 0, reasonUpstreamCommentsUnreadable},
		{"reviews read times out", 1, reasonUpstreamReviewsUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFleetAutomergeEnabled(t, true)
			responses := []string{automergeEligiblePR(), `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`}
			for i := 0; i < tc.extraOKresp; i++ {
				responses = append(responses, `[]`) // comments read completes cleanly before the reviews read times out
			}
			var calls [][]string
			originalCommand := command
			originalExecGh := execGh
			i := 0
			execGh = func(args ...string) (string, string, error) {
				calls = append(calls, append([]string{"gh"}, args...))
				if i < len(responses) {
					out := responses[i]
					i++
					return out, "", nil
				}
				return "", "", errGhTimeout
			}
			command = func(name string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string{name}, args...))
				return []byte(""), nil
			}
			t.Cleanup(func() {
				command = originalCommand
				execGh = originalExecGh
			})

			s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 60}
			if _, _, err := tick(&s); err == nil {
				t.Fatal("tick: err = nil, want the timed-out paginated read to surface as a tick error")
			}
			if s.AutomergeReason != tc.wantReason {
				t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, tc.wantReason)
			}
			if s.AutomergeFailureClass != failureClassTimeout {
				t.Fatalf("AutomergeFailureClass = %q, want %q: the paginated-read error chain must preserve errGhTimeout through errPaginationFailed's wrapping (the %%w-chain fix) so classifyGhFailure can still see it", s.AutomergeFailureClass, failureClassTimeout)
			}
		})
	}
}

// TestAutomergeDecisionLogLineRendersDetailOnMergedBranchAndFailureClass pins
// two of logLine's #886 changes together: a merged (Merge==true) decision
// with a non-empty Detail (e.g. matrix case (a)'s "command reported failure
// despite the confirmed merge" diagnostic) must render that Detail -- the
// pre-#886 logLine only ever rendered Detail on the held branch -- and a
// non-empty FailureClass must render as " class=<class>".
func TestAutomergeDecisionLogLineRendersDetailOnMergedBranchAndFailureClass(t *testing.T) {
	d := automergeDecision{
		PR:           "42",
		Merge:        true,
		Detail:       "gh pr merge exited nonzero but the refetch confirmed MERGED",
		FailureClass: failureClassCommand,
		Conditions:   []conditionResult{{Key: "enabled", Reached: true, Pass: true}},
	}
	got := d.logLine()
	if !strings.Contains(got, "gh pr merge exited nonzero but the refetch confirmed MERGED") {
		t.Fatalf("logLine() = %q, want it to render Detail on the merged branch too", got)
	}
	if !strings.Contains(got, " class="+failureClassCommand) {
		t.Fatalf("logLine() = %q, want it to render \" class=%s\"", got, failureClassCommand)
	}
}

// TestAutomergeDecisionLogLineOmitsClassWhenEmpty pins the other half: an
// empty FailureClass (the common case -- most holds and every clean merge
// have no failure class at all) must render no "class=" substring, keeping
// TestAutomergeDecisionLogLine's pre-#886 exact-format assertion valid.
func TestAutomergeDecisionLogLineOmitsClassWhenEmpty(t *testing.T) {
	d := automergeDecision{PR: "42", Merge: false, Reason: reasonLabelMissing}
	got := d.logLine()
	if strings.Contains(got, "class=") {
		t.Fatalf("logLine() = %q, want no class= substring when FailureClass is empty", got)
	}
}
