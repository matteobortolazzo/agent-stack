package babysit

import (
	"errors"
	"strings"
	"testing"
)

// -- automerge attribution comment (#1049) ------------------------------------
//
// A merge issued by `gh pr merge` lands under the operator's own GitHub
// identity, so before #1049 an automerged PR was indistinguishable on GitHub
// from a human clicking "Squash and merge" -- the only record was the tick's
// stdout log line and the supervisor's own state file, both of which expire
// with log retention. These tests pin the durable record: exactly one PR
// comment per confirmed merge, none on any other verdict, and a comment
// failure that never disturbs the merge verdict itself.

// attributionCommentCalls returns every `gh pr comment` invocation in calls.
func attributionCommentCalls(calls [][]string) [][]string {
	var found [][]string
	for _, c := range calls {
		if len(c) > 2 && c[0] == "gh" && c[1] == "pr" && c[2] == "comment" {
			found = append(found, c)
		}
	}
	return found
}

// commentBody extracts the value passed to --body in a `gh pr comment` call.
func commentBody(t *testing.T, call []string) string {
	t.Helper()
	for i, a := range call {
		if a == "--body" && i+1 < len(call) {
			return call[i+1]
		}
	}
	t.Fatalf("gh pr comment call carries no --body argument: %#v", call)
	return ""
}

// TestConfirmedMergePostsExactlyOneAttributionComment is #1049's primary
// acceptance criterion: a merge confirmed by the post-merge refetch posts one
// PR comment carrying the attribution banner, the pinned head SHA, and the
// same condition-chain bracket the tick's own log line renders.
func TestConfirmedMergePostsExactlyOneAttributionComment(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	script := automergeFirstPassScript()
	script = append(script, automergeGreenRecheckScript()...)
	script = append(script,
		scriptedCall{out: "Merged pull request #42 (o/r)"},
		scriptedCall{out: `{"number":42,"title":"Change","state":"MERGED","headRefName":"feature","headRefOid":"abc","baseRefName":"main","mergeable":"MERGEABLE","isDraft":false,"changedFiles":1,"additions":5,"deletions":2,"files":[{"path":"watch/internal/babysit/x.go"}],"url":"https://example/pr/42","closingIssuesReferences":[{"number":9}]}`},
		scriptedCall{out: "https://example/pr/42#issuecomment-1"},
	)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}

	posted := attributionCommentCalls(calls)
	if len(posted) != 1 {
		t.Fatalf("gh pr comment calls = %d, want exactly 1 on a confirmed merge: %#v", len(posted), calls)
	}
	call := posted[0]
	if call[3] != "42" {
		t.Fatalf("gh pr comment target = %q, want the supervised PR %q", call[3], "42")
	}
	if !containsPair(call, "--repo", "o/r") {
		t.Fatalf("gh pr comment must pin --repo o/r: %#v", call)
	}

	body := commentBody(t, call)
	if !strings.HasPrefix(body, mergeAttributionBanner) {
		t.Fatalf("comment body must open with the attribution banner.\n got: %q\nwant prefix: %q", body, mergeAttributionBanner)
	}
	// The pinned head SHA is what makes the comment auditable: it names the
	// exact commit --match-head-commit constrained the merge to.
	if !strings.Contains(body, "abc") {
		t.Fatalf("comment body must name the pinned head commit: %q", body)
	}
	// The bracket is the whole point of the durable record -- it must be the
	// same rendering the log line carries, not a re-derived summary.
	wantBracket := automergeDecision{
		Conditions: allStagesPassed(),
	}.conditionBracket()
	if !strings.Contains(body, wantBracket) {
		t.Fatalf("comment body must carry the full condition-chain bracket %q: %q", wantBracket, body)
	}
	// Attribution is never a trust signal (flow/docs/comment-attribution.md):
	// gh posts under the operator's own account, so the comment must say so
	// rather than reading as proof of who merged.
	if !strings.Contains(body, mergeAttributionTrustNote) {
		t.Fatalf("comment body must carry the trust-signal disclaimer: %q", body)
	}
}

// TestConfirmedMergeAttributionBodyCarriesSkippedSuffix pins #1129 AC 11 on
// the durable attribution record, not just the log line: a merge that went
// green via the new "at least one pass, the rest skipping" rule must carry
// the "(skipped=N)" diagnostic on the "ci" stage in the posted comment body
// too, since the comment is rendered from the same conditionBracket as
// logLine (mergeAttributionBody / attribution.go:83).
func TestConfirmedMergeAttributionBodyCarriesSkippedSuffix(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	skippedChecks := `[{"bucket":"pass","name":"test","state":"SUCCESS"},{"bucket":"skipping","name":"a","state":"SKIPPED"},{"bucket":"skipping","name":"b","state":"SKIPPED"}]`
	var calls [][]string
	script := []scriptedCall{
		{out: automergeEligiblePR()},
		{out: skippedChecks},
		{out: `[]`},
		{out: `[]`},
		{out: `{"labels":[{"name":"automerge:ok"}]}`},
		{out: `{"automerge":{"maxChangedFiles":10,"maxDiffLines":500,"mergeMethod":"squash"}}`},
		{out: `{"squash":true,"merge":false,"rebase":true}`},
		{out: queueProbeResponse(false, false, "abc")},
	}
	script = append(script, automergeRecheckScript(
		automergeEligiblePR(),
		skippedChecks,
		`[]`,
		`[]`,
		`{"labels":[{"name":"automerge:ok"}]}`,
		`{"automerge":{"maxChangedFiles":10,"maxDiffLines":500,"mergeMethod":"squash"}}`,
		queueProbeResponse(false, false, "abc"),
	)...)
	script = append(script,
		scriptedCall{out: "Merged pull request #42 (o/r)"},
		scriptedCall{out: `{"number":42,"title":"Change","state":"MERGED","headRefName":"feature","headRefOid":"abc","baseRefName":"main","mergeable":"MERGEABLE","isDraft":false,"changedFiles":1,"additions":5,"deletions":2,"files":[{"path":"watch/internal/babysit/x.go"}],"url":"https://example/pr/42","closingIssuesReferences":[{"number":9}]}`},
		scriptedCall{out: "https://example/pr/42#issuecomment-1"},
	)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}

	posted := attributionCommentCalls(calls)
	if len(posted) != 1 {
		t.Fatalf("gh pr comment calls = %d, want exactly 1 on a confirmed merge: %#v", len(posted), calls)
	}
	body := commentBody(t, posted[0])
	if !strings.Contains(body, "ci=yes(skipped=2)") {
		t.Fatalf("comment body must carry the skipped-count suffix on the ci stage: %q", body)
	}
}

// allStagesPassed builds the all-green condition set a fully-eligible merge
// produces, for asserting the rendered bracket.
func allStagesPassed() []conditionResult {
	conds := make([]conditionResult, 0, len(automergeStageKeys))
	for _, k := range automergeStageKeys {
		conds = append(conds, conditionResult{Key: k, Reached: true, Pass: true})
	}
	return conds
}

// containsPair reports whether call contains flag immediately followed by value.
func containsPair(call []string, flag, value string) bool {
	for i, a := range call {
		if a == flag && i+1 < len(call) && call[i+1] == value {
			return true
		}
	}
	return false
}

// TestHeldTickPostsNoAttributionComment pins the negative for an ordinary
// condition-chain hold: no merge happened, so there is nothing to attribute.
func TestHeldTickPostsNoAttributionComment(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	script := []scriptedCall{
		{out: automergeEligiblePR()},
		{out: `[{"bucket":"pass","name":"test","state":"SUCCESS"}]`},
		{out: `[]`},
		{out: `[]`},
		{out: `{"labels":[{"name":"enhancement"}]}`}, // no automerge:ok -> held
	}
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 60}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeReason != reasonLabelMissing {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonLabelMissing)
	}
	if posted := attributionCommentCalls(calls); len(posted) != 0 {
		t.Fatalf("a held tick must post no attribution comment: %#v", posted)
	}
}

// TestIndeterminateMergePostsNoAttributionComment pins Decision 6's boundary:
// a zero-exit `gh pr merge` whose refetch does not report MERGED is never
// treated as success, so it must never leave a "merged automatically" comment
// on a PR that is still open.
func TestIndeterminateMergePostsNoAttributionComment(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	script := automergeFirstPassScript()
	script = append(script, automergeGreenRecheckScript()...)
	script = append(script,
		scriptedCall{out: "Merged pull request #42 (o/r)"}, // zero exit
		scriptedCall{out: automergeEligiblePR()},           // refetch: still OPEN
	)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 60}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeReason != reasonMergeIndeterminate {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonMergeIndeterminate)
	}
	if posted := attributionCommentCalls(calls); len(posted) != 0 {
		t.Fatalf("an indeterminate merge must post no attribution comment: %#v", posted)
	}
}

// TestUnverifiableMergePostsNoAttributionComment pins the other non-success
// executeMerge outcome: the post-merge refetch itself was unreadable, so the
// merge is unproven and must not be attributed.
func TestUnverifiableMergePostsNoAttributionComment(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	script := automergeFirstPassScript()
	script = append(script, automergeGreenRecheckScript()...)
	script = append(script,
		scriptedCall{out: "Merged pull request #42 (o/r)"},
		scriptedCall{out: "network unreachable", err: errors.New("exit status 1")}, // refetch fails
	)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 60}
	if _, _, err := tick(&s); err != nil {
		t.Fatal(err)
	}
	if s.AutomergeReason != reasonMergeVerifyUnreadable {
		t.Fatalf("AutomergeReason = %q, want %q", s.AutomergeReason, reasonMergeVerifyUnreadable)
	}
	if posted := attributionCommentCalls(calls); len(posted) != 0 {
		t.Fatalf("an unverifiable merge must post no attribution comment: %#v", posted)
	}
}

// TestAttributionCommentFailureLeavesMergeConfirmed pins the constraint that
// makes this feature safe to add to the merge path at all: the comment is
// strictly post-hoc. A failed `gh pr comment` is recorded in the decision's
// Detail and nothing else -- it never downgrades the confirmed merge back to
// held, never sets a FailureClass (which would make the tick's log line read
// as a failed merge), and never triggers a second merge attempt.
func TestAttributionCommentFailureLeavesMergeConfirmed(t *testing.T) {
	withFleetAutomergeEnabled(t, true)
	var calls [][]string
	script := automergeFirstPassScript()
	script = append(script, automergeGreenRecheckScript()...)
	script = append(script,
		scriptedCall{out: "Merged pull request #42 (o/r)"},
		scriptedCall{out: `{"number":42,"title":"Change","state":"MERGED","headRefName":"feature","headRefOid":"abc","baseRefName":"main","mergeable":"MERGEABLE","isDraft":false,"changedFiles":1,"additions":5,"deletions":2,"files":[{"path":"watch/internal/babysit/x.go"}],"url":"https://example/pr/42","closingIssuesReferences":[{"number":9}]}`},
		scriptedCall{out: "could not add comment: rate limited", err: errors.New("exit status 1")},
	)
	withScriptedCommands(t, script, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "codex", IntervalSeconds: 60, CurrentDelaySeconds: 900}
	if _, _, err := tick(&s); err != nil {
		t.Fatalf("a failed attribution comment must not turn the tick into an error: %v", err)
	}
	if s.AutomergeDecision != "merge" {
		t.Fatalf("AutomergeDecision = %q, want \"merge\": a failed attribution comment must never downgrade a confirmed merge", s.AutomergeDecision)
	}
	if s.AutomergeReason != "" {
		t.Fatalf("AutomergeReason = %q, want empty on a confirmed merge", s.AutomergeReason)
	}
	if !strings.Contains(s.AutomergeDetail, "rate limited") {
		t.Fatalf("AutomergeDetail = %q, want the captured gh pr comment failure output", s.AutomergeDetail)
	}
	// FailureClass drives the log line's " class=" suffix. A confirmed merge
	// whose only failure was a cosmetic comment must not be classified as a
	// gh failure -- that would read as a failed merge during incident triage.
	if s.AutomergeFailureClass != "" {
		t.Fatalf("AutomergeFailureClass = %q, want empty: the merge itself succeeded", s.AutomergeFailureClass)
	}
	if n := mergeCallCount(calls); n != 1 {
		t.Fatalf("gh pr merge calls = %d, want exactly 1: a failed comment must never retry the merge", n)
	}
	if posted := attributionCommentCalls(calls); len(posted) != 1 {
		t.Fatalf("gh pr comment calls = %d, want exactly 1: a failed comment must not be retried within the tick", len(posted))
	}
}

// TestAlreadyMergedTickPostsNoSecondAttributionComment pins the once-only
// guarantee across ticks: once `pr view` itself reports MERGED, tick
// short-circuits into the relabel branch and never re-enters the automerge
// path, so a supervisor still running (or restarted) after the merge cannot
// accumulate a second comment.
func TestAlreadyMergedTickPostsNoSecondAttributionComment(t *testing.T) {
	var calls [][]string
	merged := `{"number":42,"title":"Done","state":"MERGED","url":"https://example/pr/42","closingIssuesReferences":[{"number":9}]}`
	withCommands(t, []string{merged, `{}`, `{}`, `{"parent":null}`}, &calls)

	s := State{PR: "42", Repo: "o/r", Agent: "claude", IntervalSeconds: 60, CurrentDelaySeconds: 60}
	terminal, _, err := tick(&s)
	if err != nil || !terminal {
		t.Fatalf("tick = terminal %v, err %v", terminal, err)
	}
	if posted := attributionCommentCalls(calls); len(posted) != 0 {
		t.Fatalf("an already-merged tick must post no attribution comment: %#v", posted)
	}
}

// TestMergeAttributionBodyMatchesLogLineBracket ties the two renderings
// together: the comment exists so the durable record on the PR matches what
// the daemon logged. A future edit that re-derives the bracket for one and
// not the other silently breaks that correspondence.
func TestMergeAttributionBodyMatchesLogLineBracket(t *testing.T) {
	d := automergeDecision{PR: "42", Merge: true, Conditions: allStagesPassed()}
	body := mergeAttributionBody("deadbeef", d)
	bracket := d.conditionBracket()

	if !strings.Contains(body, bracket) {
		t.Fatalf("body must embed the shared bracket rendering %q: %q", bracket, body)
	}
	if !strings.Contains(d.logLine(), bracket) {
		t.Fatalf("log line must embed the same bracket rendering %q: %q", bracket, d.logLine())
	}
	if !strings.Contains(body, "deadbeef") {
		t.Fatalf("body must name the pinned head commit: %q", body)
	}
}

// TestMergeAttributionBodyHandlesUnknownHeadSHA pins the degenerate input:
// runAutomerge merges with a pinned SHA, but the body renderer must not emit
// an empty backtick pair if it is ever called without one.
func TestMergeAttributionBodyHandlesUnknownHeadSHA(t *testing.T) {
	body := mergeAttributionBody("", automergeDecision{PR: "42", Merge: true})
	if strings.Contains(body, "commit ``") {
		t.Fatalf("body must not render an empty code span for a missing head SHA: %q", body)
	}
	if !strings.Contains(body, "unknown") {
		t.Fatalf("body must name a missing head SHA explicitly: %q", body)
	}
}
