package dispatch

// Ticket #920 (child of #915): the babysit evaluation-boundary adversarial
// negative variants -- reopened feedback (1), a same-timestamp review
// tie-break resolved by review ID (2), a non-check `gh` nonzero exit with
// parseable JSON that must still fail closed (3), and the fleet automerge
// kill switch flipped strictly between the first evaluation and the pre-merge
// recheck (5) -- each its own named test driving real babysit.Run over the
// chain fake (chainfake_test.go), asserting the exact stop-reason constant,
// the exact per-route `gh` call counts, and the absence of any mutation,
// merge, or spawn command on the hold path. Variant (4) (merge accepted by
// the server but the client sees a failure) is split across two packages per
// #915's Decisions: the chain-level half proving the single authoritative
// post-merge refetch settles the outcome lives here
// (TestAutonomousChain_MergeAcceptedButClientSeesFailureIsSettledByPostMergeRefetch);
// its companion, a genuine errGhTimeout/failureClassTimeout classification
// through the real execGh, lives in package babysit
// (automerge_test.go's TestExecuteMerge_RealTimeoutOnMergeCallClassifiesFailureClassTimeout).
//
// Never call t.Parallel() anywhere in this file (chainfake_test.go's
// os.Args[0] mutation is process-global, and babysit's localRepoRoot needs a
// stable t.Chdir) -- mirrors chain_e2e_test.go's own rule.

import (
	"slices"
	"strings"
	"testing"

	"github.com/matteobortolazzo/cenci/watch/v2/internal/babysit"
)

// The following mirror internal/babysit's own unexported automerge reason
// constants (automerge.go) verbatim, alongside chain_e2e_test.go's existing
// mirrored block (this package cannot import them -- they are unexported in
// a sibling package) -- exact literal strings for content-specific
// assertions against the persisted AutomergeReason/AutomergeFailureClass,
// per automerge_test.go's TestEvaluateAutomergeConditionMatrix discipline
// (never a bare true/false check).
const (
	// chainReasonFeedbackReopened mirrors babysit.reasonFeedbackReopened
	// (automerge.go) -- fires whenever a previously-AddressedKeys entry is
	// reclassified back to pending against fresh GitHub state, whether the
	// discovery mechanism is a later timestamp (variant 1) or a same-
	// timestamp review-ID tie-break (variant 2).
	chainReasonFeedbackReopened = "review feedback reopened"
	// chainReasonUpstreamPRUnreadable mirrors babysit.reasonUpstreamPRUnreadable
	// (automerge.go) -- the one-decision-per-tick reason for an unreadable
	// `gh pr view`, including a nonzero exit whose stdout still happens to be
	// parseable JSON (variant 3): ghJSON fails closed on any non-nil execGh
	// error before ever attempting to decode.
	chainReasonUpstreamPRUnreadable = "PR view unreadable"
	// chainReasonKillSwitchDisabled mirrors babysit.reasonKillSwitchDisabled
	// (automerge.go) -- recheckAutomergeInputs' final pre-merge re-read of
	// the fleet automerge.enabled switch coming back explicitly false,
	// distinct from chainReasonAutomergeDisabled below (the first-pass hold,
	// which never distinguishes *why* the switch is off).
	chainReasonKillSwitchDisabled = "fleet automerge switch explicitly disabled"
	// chainReasonAutomergeDisabled mirrors babysit.reasonAutomergeDisabled
	// (automerge.go) -- evaluateAutomerge's own first-stage hold when the
	// fleet switch is off at the FIRST evaluation. Named here only so
	// variant (5) can assert its recheck kill-switch reason is explicitly
	// NOT this one.
	chainReasonAutomergeDisabled = "automerge disabled (fleet config)"
	// chainFailureClassCommand mirrors babysit.failureClassCommand (gh.go)
	// -- classifyGhFailure's fail-closed default class for a `gh` failure
	// that classifies as none of cancelled/timeout/truncated/parse (an
	// ordinary nonzero exit, per #886).
	chainFailureClassCommand = "command"
)

// chainPRViewFields mirrors babysit.prViewFields (babysit.go) verbatim --
// the exact `--json` field list every `gh pr view` call in package babysit
// requests, needed here (this package cannot import babysit's unexported
// constant) to build the exact-argv assertions AC1 requires.
const chainPRViewFields = "number,title,state,headRefName,headRefOid,mergedAt,closingIssuesReferences,url,baseRefName,mergeable,mergeStateStatus,isDraft,changedFiles,additions,deletions,files"

// chainInvocationsWithPrefix generalizes chainMergeInvocations
// (chain_e2e_test.go, which stays untouched and keeps its own narrower "pr
// merge "-only helper): filters world's invocation log down to entries
// beginning with prefix, for exact-argv/exact-count assertions against any
// route, not just merges.
func chainInvocationsWithPrefix(w *ghWorld, prefix string) []string {
	var out []string
	for _, inv := range w.Invocations {
		if strings.HasPrefix(inv, prefix) {
			out = append(out, inv)
		}
	}
	return out
}

// assertNoHoldPathSideEffects asserts the exact absence -- exact
// len(...)==0, never "non-empty" (watch/docs/test-strategy.md rule 22) -- of
// mutation (`gh issue edit`), merge (`gh pr merge`), and spawn (`cenci run`)
// commands: every hold-path variant in this file must prove not merely that
// automerge held, but that it held cleanly, with no side effect landing on
// GitHub or dispatching a workflow.
func assertNoHoldPathSideEffects(t *testing.T, h *chainHarness) {
	t.Helper()
	world := h.world()
	if calls := chainInvocationsWithPrefix(world, "pr merge "); len(calls) != 0 {
		t.Fatalf("gh pr merge invocations = %v, want none on a hold path", calls)
	}
	if calls := chainInvocationsWithPrefix(world, "issue edit "); len(calls) != 0 {
		t.Fatalf("gh issue edit invocations = %v, want none on a hold path", calls)
	}
	if invs := chainCenciInvocations(t, h.cenciLogPath); len(invs) != 0 {
		t.Fatalf("cenci run invocations = %v, want none on a hold path", invs)
	}
}

// chainRouteCallCounts asserts world's invocation log carries exactly want[prefix]
// invocations for every prefix named in want -- a shared per-route exact-count
// assertion for the recheck-boundary variants below, each of which drives
// tick's own first evaluation plus recheckAutomergeInputs' full pre-merge
// re-fetch (merge.go:73).
func chainRouteCallCounts(t *testing.T, world *ghWorld, want map[string]int) {
	t.Helper()
	for prefix, wantN := range want {
		if n := len(chainInvocationsWithPrefix(world, prefix)); n != wantN {
			t.Fatalf("%q invocations = %d, want %d", prefix, n, wantN)
		}
	}
}

// -- 3. Non-check nonzero exit with parseable JSON stdout still fails closed -

// TestAutonomousChain_UpstreamPRReadNonzeroExitWithParseableJSONStillFails
// proves #915's variant (3): `gh pr view`'s handler writes its ordinary,
// well-formed JSON stdout, but the ambiguous-success injection still makes
// the client see a nonzero exit -- ghJSON (babysit.go) must fail closed on
// the nonzero exit alone, never attempt to trust the still-parseable stdout.
// setFleetAutomergeEnabled(true) is load-bearing here (plan Assumptions):
// recordUpstreamReadFailure silently rewrites the reason to
// reasonAutomergeDisabled when the fleet switch is off, which would make
// this test pass for the wrong reason.
func TestAutonomousChain_UpstreamPRReadNonzeroExitWithParseableJSONStillFails(t *testing.T) {
	h := newChainHarness(t)
	h.commitAndPushConfig(chainRepoConfigLean)
	setFleetAutomergeEnabled(t, true)

	h.seedIssue(101, "Granted ticket", "body", []string{"In Review", "automerge:ok"}, []string{"octocat"})
	headSHA := gitTest(t, h.local, "rev-parse", "HEAD")
	h.seedPR(ghPR{
		Number: 103, HeadRefName: "main", HeadRefOID: headSHA, BaseRefName: "main",
		ClosingIssues: []int{101}, Mergeable: "MERGEABLE", ChangedFiles: 1, Files: []string{"base.txt"},
	})
	h.seedChecks(103, ghCheck{Bucket: "pass", Name: "build", State: "SUCCESS"})

	// `pr view` is tick's very first gh call (after repository()'s own `repo
	// view`), so OnCall:1 targets it before any other route is ever reached.
	h.seedAmbiguity(ghAmbiguity{Route: "pr view", OnCall: 1, Kind: "ambiguous-success"})

	restore := chainChdir(t, h.local)
	defer restore()
	stateDir := t.TempDir()
	err := babysit.Run(babysit.Options{PR: "103", Agent: "claude", StateDir: stateDir, Once: true})
	if err == nil {
		t.Fatal("expected babysit.Run to propagate the pr-view upstream-read failure (a nonzero exit with parseable JSON stdout must still fail closed), got nil error")
	}

	st := readBabysitState(t, stateDir)
	if st.AutomergeReason != chainReasonUpstreamPRUnreadable {
		t.Fatalf("AutomergeReason = %q, want %q", st.AutomergeReason, chainReasonUpstreamPRUnreadable)
	}
	if st.AutomergeFailureClass != chainFailureClassCommand {
		t.Fatalf("AutomergeFailureClass = %q, want %q", st.AutomergeFailureClass, chainFailureClassCommand)
	}

	// The exact, ordered two-element invocation list: tick fails before ever
	// reaching `pr checks`, so nothing else was ever called.
	wantInvocations := []string{
		"repo view --json nameWithOwner --jq .nameWithOwner",
		"pr view 103 --repo o/r --json " + chainPRViewFields,
	}
	if invs := h.world().Invocations; !slices.Equal(invs, wantInvocations) {
		t.Fatalf("gh invocations = %v, want exactly %v", invs, wantInvocations)
	}
	assertNoHoldPathSideEffects(t, h)
}

// -- 5. Fleet kill switch flipped between evaluations -------------------------

// TestAutonomousChain_FleetKillSwitchDisabledBetweenEvaluationsIsDistinctFromFirstPassDisabled
// proves #915's variant (5): the fleet automerge.enabled switch is flipped
// off strictly between tick's first evaluation (which saw it enabled) and
// recheckAutomergeInputs' own final pre-merge re-read (#886, merge.go:149) --
// this must record chainReasonKillSwitchDisabled, never the ordinary
// first-pass chainReasonAutomergeDisabled, since the first pass genuinely
// saw the switch on.
func TestAutonomousChain_FleetKillSwitchDisabledBetweenEvaluationsIsDistinctFromFirstPassDisabled(t *testing.T) {
	h := newChainHarness(t)
	h.commitAndPushConfig(chainRepoConfigLean)
	setFleetAutomergeEnabled(t, true)

	h.seedIssue(101, "Granted ticket", "body", []string{"In Review", "automerge:ok"}, []string{"octocat"})
	headSHA := gitTest(t, h.local, "rev-parse", "HEAD")
	h.seedPR(ghPR{
		Number: 103, HeadRefName: "main", HeadRefOID: headSHA, BaseRefName: "main",
		ClosingIssues: []int{101}, Mergeable: "MERGEABLE", ChangedFiles: 1, Files: []string{"base.txt"},
	})
	h.seedChecks(103, ghCheck{Bucket: "pass", Name: "build", State: "SUCCESS"})

	// OnCall:2 targets recheckAutomergeInputs' own re-read of `pr view`, not
	// tick's first evaluation (OnCall:1) -- the "state-change" kind mutates
	// world (rewriting $XDG_CONFIG_HOME/cenci/config.json to
	// automerge.enabled:false) the moment this call fires, before the
	// recheck's OWN loadFleetSwitch() re-read (its final step) ever runs.
	h.seedAmbiguity(ghAmbiguity{Route: "pr view", OnCall: 2, Kind: "state-change", Payload: "disable-fleet-automerge"})

	restore := chainChdir(t, h.local)
	defer restore()
	stateDir := t.TempDir()
	if err := babysit.Run(babysit.Options{PR: "103", Agent: "claude", StateDir: stateDir, Once: true}); err != nil {
		t.Fatalf("babysit.Run returned unexpected error: %v", err)
	}

	st := readBabysitState(t, stateDir)
	if st.AutomergeReason != chainReasonKillSwitchDisabled {
		t.Fatalf("AutomergeReason = %q, want %q", st.AutomergeReason, chainReasonKillSwitchDisabled)
	}
	if st.AutomergeReason == chainReasonAutomergeDisabled {
		t.Fatal("AutomergeReason must be the distinct recheck kill-switch reason, never the ordinary first-pass reasonAutomergeDisabled (the first evaluation genuinely saw the switch enabled)")
	}
	// A single synthetic "enabled" condition (mirroring evaluateAutomerge's
	// own first stage), not every stage rendering "-" as if nothing had been
	// reached (merge.go's own doc comment on this branch).
	conds := st.AutomergeConditions
	if len(conds) != 1 {
		t.Fatalf("AutomergeConditions = %+v, want exactly one synthetic condition", conds)
	}
	if conds[0].Key != "enabled" || !conds[0].Reached || conds[0].Pass {
		t.Fatalf("AutomergeConditions[0] = %+v, want {Key:\"enabled\", Reached:true, Pass:false}", conds[0])
	}

	// Per-route call counts: every recheck read runs unconditionally, in
	// full, before the kill-switch re-read (a local file read, no gh call)
	// ever short-circuits anything -- so tick's first pass plus the recheck
	// both fully complete, each route called exactly twice.
	chainRouteCallCounts(t, h.world(), map[string]int{
		"pr view ":     2,
		"pr checks ":   2,
		"issue view ":  2,
		"api graphql ": 2,
	})
	assertNoHoldPathSideEffects(t, h)
}

// -- 1. Resolved feedback reopened between evaluations -------------------------

// TestAutonomousChain_ResolvedFeedbackReopenedBetweenEvaluationsHoldsTheMerge
// proves #915's variant (1): mirrors the positive e2e's phase-F fixture
// (chain_e2e_test.go) -- a reviewer's earlier CHANGES_REQUESTED review,
// since superseded by their own later APPROVED review, resolves within
// tick's own first-pass reconcileFeedback call (producing AddressedKeys ==
// ["review:1"] naturally, before the PendingKeys \ LaunchedKeys launch
// trigger is ever evaluated) -- then the SAME review is reopened (a fresh
// CHANGES_REQUESTED, at a later timestamp) strictly between that first
// evaluation and recheckAutomergeInputs' own pre-merge re-read, discovered
// only by revalidateFeedback's non-mutating re-classification of
// AddressedKeys (#885).
func TestAutonomousChain_ResolvedFeedbackReopenedBetweenEvaluationsHoldsTheMerge(t *testing.T) {
	h := newChainHarness(t)
	h.commitAndPushConfig(chainRepoConfigLean)
	setFleetAutomergeEnabled(t, true)

	h.seedIssue(101, "Granted ticket", "body", []string{"In Review", "automerge:ok"}, []string{"octocat"})
	headSHA := gitTest(t, h.local, "rev-parse", "HEAD")
	h.seedPR(ghPR{
		Number: 103, HeadRefName: "main", HeadRefOID: headSHA, BaseRefName: "main",
		ClosingIssues: []int{101}, Mergeable: "MERGEABLE", ChangedFiles: 1, Files: []string{"base.txt"},
	})
	h.seedChecks(103, ghCheck{Bucket: "pass", Name: "build", State: "SUCCESS"})
	h.seedReview(103, "alice", "CHANGES_REQUESTED", "2026-01-01T00:00:00Z")
	h.seedReview(103, "alice", "APPROVED", "2026-01-02T00:00:00Z")

	// OnCall:2 targets recheckAutomergeInputs' own re-read of `pr view`, NOT
	// tick's first evaluation (OnCall:1): alice's review:1 is reopened (a new
	// CHANGES_REQUESTED, timestamped by the fixture's own time.Now(), later
	// than both seeded reviews) strictly between the two evaluations.
	h.seedAmbiguity(ghAmbiguity{Route: "pr view", OnCall: 2, Kind: "state-change", Payload: "reopen-review:alice"})

	restore := chainChdir(t, h.local)
	defer restore()
	stateDir := t.TempDir()
	if err := babysit.Run(babysit.Options{PR: "103", Agent: "claude", StateDir: stateDir, Once: true}); err != nil {
		t.Fatalf("babysit.Run returned unexpected error: %v", err)
	}

	st := readBabysitState(t, stateDir)
	if st.AutomergeReason != chainReasonFeedbackReopened {
		t.Fatalf("AutomergeReason = %q, want %q", st.AutomergeReason, chainReasonFeedbackReopened)
	}
	wantDetail := "recheck: review:1: review feedback reopened"
	if st.AutomergeDetail != wantDetail {
		t.Fatalf("AutomergeDetail = %q, want %q", st.AutomergeDetail, wantDetail)
	}
	if want := []string{"review:1"}; !slices.Equal(st.AddressedKeys, want) {
		t.Fatalf("AddressedKeys = %v, want %v (resolved within tick's own first pass, before the recheck discovered the reopen)", st.AddressedKeys, want)
	}

	chainRouteCallCounts(t, h.world(), map[string]int{
		"pr view ":     2,
		"pr checks ":   2,
		"issue view ":  2,
		"api graphql ": 2,
	})
	assertNoHoldPathSideEffects(t, h)
}

// -- 2. Same-timestamp review tie-break resolved by review ID -----------------

// TestAutonomousChain_SameTimestampReviewTieBreakDecidesReopenByReviewID
// proves #915's variant (2): identical fixture to variant (1) above, except
// the reopened review carries the EXACT SAME submittedAt timestamp as the
// already-APPROVED review, distinguishable only by review ID
// (latestEffectiveReview's tie-break, #885's AC 4). This still lands on the
// same chainReasonFeedbackReopened constant as variant (1) -- distinct
// mechanism, not distinct reason (planning Q2): variant (1)'s reopen wins by
// a strictly later timestamp, this one wins because it is the higher-ID
// review among two identically-timestamped reviews. A hypothetical
// timestamp-only ">" comparator (ignoring review ID on a tie) would instead
// have kept the APPROVED review "latest" and let this merge -- the pure
// order-independence of the ID tie-break itself is already covered by
// TestLatestEffectiveReviewSameTimestampTieBreak* in feedback_test.go and is
// deliberately not re-proven here; this test proves only that the chain
// wires that tie-break into a real automerge hold.
func TestAutonomousChain_SameTimestampReviewTieBreakDecidesReopenByReviewID(t *testing.T) {
	h := newChainHarness(t)
	h.commitAndPushConfig(chainRepoConfigLean)
	setFleetAutomergeEnabled(t, true)

	h.seedIssue(101, "Granted ticket", "body", []string{"In Review", "automerge:ok"}, []string{"octocat"})
	headSHA := gitTest(t, h.local, "rev-parse", "HEAD")
	h.seedPR(ghPR{
		Number: 103, HeadRefName: "main", HeadRefOID: headSHA, BaseRefName: "main",
		ClosingIssues: []int{101}, Mergeable: "MERGEABLE", ChangedFiles: 1, Files: []string{"base.txt"},
	})
	h.seedChecks(103, ghCheck{Bucket: "pass", Name: "build", State: "SUCCESS"})
	const approvedAt = "2026-01-02T00:00:00Z"
	h.seedReview(103, "alice", "CHANGES_REQUESTED", "2026-01-01T00:00:00Z")
	h.seedReview(103, "alice", "APPROVED", approvedAt)

	// The reopened review's submittedAt is approvedAt VERBATIM -- identical
	// to the already-served APPROVED review, so only the higher review ID
	// (this new review is seeded last, so it gets the highest ID) decides
	// which one is "latest".
	h.seedAmbiguity(ghAmbiguity{Route: "pr view", OnCall: 2, Kind: "state-change", Payload: "reopen-review:alice:" + approvedAt})

	restore := chainChdir(t, h.local)
	defer restore()
	stateDir := t.TempDir()
	if err := babysit.Run(babysit.Options{PR: "103", Agent: "claude", StateDir: stateDir, Once: true}); err != nil {
		t.Fatalf("babysit.Run returned unexpected error: %v", err)
	}

	st := readBabysitState(t, stateDir)
	if st.AutomergeReason != chainReasonFeedbackReopened {
		t.Fatalf("AutomergeReason = %q, want %q (same reason as variant (1), decided this time by the review-ID tie-break rather than a strictly later timestamp)", st.AutomergeReason, chainReasonFeedbackReopened)
	}
	wantDetail := "recheck: review:1: review feedback reopened"
	if st.AutomergeDetail != wantDetail {
		t.Fatalf("AutomergeDetail = %q, want %q", st.AutomergeDetail, wantDetail)
	}
	if want := []string{"review:1"}; !slices.Equal(st.AddressedKeys, want) {
		t.Fatalf("AddressedKeys = %v, want %v", st.AddressedKeys, want)
	}

	// Content-specific proof the tie-break, not merely a timestamp
	// comparison, decided this: the served review set for PR #103 must carry
	// two same-timestamp reviews whose higher review ID is the reopened
	// CHANGES_REQUESTED one.
	reviews := h.world().Reviews[103]
	var sameTimestamp []ghReview
	for _, r := range reviews {
		if r.SubmittedAt == approvedAt {
			sameTimestamp = append(sameTimestamp, r)
		}
	}
	if len(sameTimestamp) != 2 {
		t.Fatalf("reviews at submittedAt=%q = %+v, want exactly 2 (the original APPROVED plus the reopened CHANGES_REQUESTED)", approvedAt, sameTimestamp)
	}
	var highestID ghReview
	for _, r := range sameTimestamp {
		if r.ID > highestID.ID {
			highestID = r
		}
	}
	if highestID.State != "CHANGES_REQUESTED" {
		t.Fatalf("the higher-ID same-timestamp review = %+v, want State=CHANGES_REQUESTED (the reopened review must be the ID tie-break's winner)", highestID)
	}

	chainRouteCallCounts(t, h.world(), map[string]int{
		"pr view ":     2,
		"pr checks ":   2,
		"issue view ":  2,
		"api graphql ": 2,
	})
	assertNoHoldPathSideEffects(t, h)
}

// -- 4(a). Merge accepted server-side, client sees a failure -------------------

// TestAutonomousChain_MergeAcceptedButClientSeesFailureIsSettledByPostMergeRefetch
// proves #915's variant (4)(a): the fake `gh pr merge` handler performs the
// REAL squash merge (world mutation lands, chainPRMerge's own logic runs
// unmodified) before the "ambiguous-success" injection overwrites the
// client-visible exit code to nonzero -- the single, unconditional post-merge
// `gh pr view` refetch (merge.go's executeMerge) is the sole source of truth,
// never the merge command's own exit code (#886). A real "implement-101"
// feature branch + commit + push is required (plan Assumptions): with
// HeadRefName "main" the fixture's own squashMergeOntoOrigin would `git
// commit` with nothing staged and fail the merge for the wrong reason.
//
// This is only the chain-level (package dispatch) half of variant (4)'s
// two-package split (#921 Q5, coverage-map fact for #916): it proves the
// single authoritative post-merge refetch settles the outcome, never the
// merge command's own client-visible exit code. It does NOT exercise a real
// errGhTimeout/failureClassTimeout classification through execGh -- that is
// the companion half, package babysit's
// TestExecuteMerge_RealTimeoutOnMergeCallClassifiesFailureClassTimeout
// (internal/babysit/automerge_test.go).
func TestAutonomousChain_MergeAcceptedButClientSeesFailureIsSettledByPostMergeRefetch(t *testing.T) {
	h := newChainHarness(t)
	h.commitAndPushConfig(chainRepoConfigLean)
	setFleetAutomergeEnabled(t, true)

	h.seedIssue(101, "Granted ticket", "body", []string{"In Review", "automerge:ok"}, []string{"octocat"})

	gitTest(t, h.local, "checkout", "-b", "implement-101")
	commitFile(t, h.local, "feature.txt", "impl")
	gitTest(t, h.local, "push", "origin", "implement-101")
	headSHA := gitTest(t, h.local, "rev-parse", "HEAD")
	gitTest(t, h.local, "checkout", "main")

	h.seedPR(ghPR{
		Number: 103, HeadRefName: "implement-101", HeadRefOID: headSHA, BaseRefName: "main",
		ClosingIssues: []int{101}, Mergeable: "MERGEABLE", ChangedFiles: 1, Additions: 1, Files: []string{"feature.txt"},
	})
	h.seedChecks(103, ghCheck{Bucket: "pass", Name: "build", State: "SUCCESS"})

	h.seedAmbiguity(ghAmbiguity{Route: "pr merge", OnCall: 1, Kind: "ambiguous-success"})

	restore := chainChdir(t, h.local)
	defer restore()
	stateDir := t.TempDir()
	if err := babysit.Run(babysit.Options{PR: "103", Agent: "claude", StateDir: stateDir, Once: true}); err != nil {
		t.Fatalf("babysit.Run returned unexpected error: %v", err)
	}

	st := readBabysitState(t, stateDir)
	if st.AutomergeDecision != "merge" {
		t.Fatalf("AutomergeDecision = %q, want %q (the post-merge refetch must settle the outcome regardless of the merge command's own client-visible failure)", st.AutomergeDecision, "merge")
	}
	if st.AutomergeReason != "" {
		t.Fatalf("AutomergeReason = %q, want empty on confirmed success", st.AutomergeReason)
	}
	if st.AutomergeDetail == "" {
		t.Fatal("AutomergeDetail = \"\", want a non-empty diagnostic noting the merge command's own client-visible failure despite the confirmed merge")
	}

	mergeCalls := chainInvocationsWithPrefix(h.world(), "pr merge ")
	wantMerge := "pr merge 103 --repo o/r --squash --match-head-commit " + headSHA
	if !slices.Equal(mergeCalls, []string{wantMerge}) {
		t.Fatalf("gh pr merge invocations = %v, want exactly [%q] (no second merge command)", mergeCalls, wantMerge)
	}

	invs := h.world().Invocations
	mergeIdx := slices.Index(invs, wantMerge)
	if mergeIdx == -1 || mergeIdx+1 >= len(invs) {
		t.Fatalf("gh pr merge invocation not found with a following call: %v", invs)
	}
	wantRefetch := "pr view 103 --repo o/r --json " + chainPRViewFields
	if invs[mergeIdx+1] != wantRefetch {
		t.Fatalf("invocation immediately after gh pr merge = %q, want the single authoritative post-merge refetch %q", invs[mergeIdx+1], wantRefetch)
	}

	if got := h.world().PRs[103].effectiveState(); got != "MERGED" {
		t.Fatalf("PR #103 state = %q, want %q", got, "MERGED")
	}
}
