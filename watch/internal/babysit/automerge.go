package babysit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Automerge skip/hold reasons (#824). Every stage of the condition chain has
// its own distinct constant -- never collapsed into another -- per
// watch/docs/error-handling.md #446 and watch/docs/go-gotchas.md #598: a
// regression that collapses two failure classes into the same reason string
// must be caught by a content-specific test assertion, not a bare
// true/false check.
const (
	reasonAutomergeDisabled = "automerge disabled (fleet config)"
	reasonNoClosingIssue    = "PR closes no issue"
	reasonLabelUnreadable   = "closing issue labels unreadable"
	reasonLabelMissing      = "ticket lacks automerge:ok"
	reasonNoChecks          = "no CI checks reported"
	reasonRepairPending     = "CI repair pending"
	reasonReviewPending     = "review feedback pending"
	// The four #850 feedback-hold reasons: distinct from reasonReviewPending
	// (an ordinary, resolvable hold) because none of these states can ever
	// resolve on their own -- an unreadable/truncated/unknown/unsupported
	// feedback state holds automerge indefinitely and needs a human merge.
	reasonFeedbackUnreadable  = "review feedback state unreadable"
	reasonFeedbackTruncated   = "review feedback state truncated"
	reasonReviewStateUnknown  = "review feedback state unknown"
	reasonFeedbackUnsupported = "unsupported review feedback type"
	// reasonFeedbackReopened (#885) is a revoked-resolution class, distinct
	// from the plain, resolvable reasonReviewPending: it fires only when a
	// key that was previously in AddressedKeys is reclassified back to
	// pending against fresh GitHub state (a resolved thread reopened, or a
	// dismissed/superseded CHANGES_REQUESTED review became blocking again),
	// never for an ordinary first-time pending key. Carries the reopened
	// key name(s) in Detail (Q2).
	reasonFeedbackReopened      = "review feedback reopened"
	reasonDraft                 = "PR is a draft"
	reasonMergeableUnknown      = "mergeable state unknown"
	reasonNotMergeable          = "PR not mergeable"
	reasonHeadSHAUnknown        = "PR head commit SHA unknown"
	reasonNoChanges             = "PR has no changed files"
	reasonDiffTruncated         = "diff file list truncated"
	reasonPolicyUnreadable      = "automerge policy unreadable"
	reasonPolicyAbsent          = "automerge policy absent"
	reasonPolicyMalformed       = "automerge policy malformed"
	reasonTooManyFiles          = "too many changed files"
	reasonTooManyLines          = "too many changed lines"
	reasonProtectedPath         = "touches a protected path"
	reasonMergeMethodDisallowed = "merge method disallowed on repo"
	reasonMergeMethodUnknown    = "repo allowed merge methods unknown"
	// reasonMergeFailed is not part of evaluateAutomerge's pure chain -- it
	// classifies the actual `gh pr merge` attempt runAutomerge makes after a
	// Merge==true verdict, e.g. a branch-protection rejection.
	reasonMergeFailed = "gh pr merge failed"
)

// #854 additions: strict pass-only CI bucket holds, squash-only execution,
// the merge-queue gate, merge execution/verification, detection-read
// pagination truncation, and per-upstream-read tick failures. Each stays its
// own distinct constant per watch/docs/error-handling.md #446 -- never
// collapsed into an existing reason, even though several are conceptually
// related to one already above.
const (
	// automergeCIHold's per-bucket holds (Decisions 1/2, #1129): CI is green
	// when at least one check is "pass" and every other check is "pass" or
	// "skipping" -- a paths-filtered monorepo's skipped, unaffected-project
	// checks must not hold automerge forever. reasonNoChecks (zero checks at
	// all) already exists above and is reused unchanged. These are the
	// remaining buckets the classifier distinguishes individually: fail,
	// pending, cancel (gh 2.97's closed bucket set), an empty bucket string
	// (malformed), and any future/unknown bucket value. reasonCIAllChecksSkipped
	// covers the zero-pass, all-skipping case, distinct from reasonNoChecks.
	reasonCICheckFailed      = "CI check failed"
	reasonCICheckPending     = "CI check pending"
	reasonCICheckCancelled   = "CI check cancelled"
	reasonCIAllChecksSkipped = "all CI checks skipped"
	reasonCIBucketEmpty      = "CI check bucket empty"
	reasonCIBucketUnknown    = "CI check bucket unrecognized"

	// Squash-only execution (Decision 3): a resolved policy mergeMethod
	// other than "squash" holds here, evaluated before any allowed-merge-
	// methods fetch -- distinct from reasonMergeMethodDisallowed, which is
	// the repo's own settings rejecting the (already-squash) method.
	reasonMergeMethodNotSquash = "merge method is not squash"

	// Merge-queue gate (Decision 4, Q3): an explicit isInMergeQueue or
	// isMergeQueueEnabled true holds under reasonMergeQueueRequired --
	// automerge must never enqueue as a side effect. A probe error or
	// either field left absent (nil, not merely false) holds under
	// reasonMergeQueueUnknown: absence must never be read as "not
	// required".
	reasonMergeQueueRequired = "PR requires the merge queue"
	reasonMergeQueueUnknown  = "merge queue state unknown"

	// reasonHeadSHAChanged is runAutomerge's pre-merge TOCTOU guard (Decision
	// 9): the pre-merge re-evaluation's own fresh `gh pr view` re-fetch
	// reports the PR's current head commit, so a push landing between the
	// first evaluation and the merge attempt is caught here -- distinct
	// from reasonHeadSHAUnknown (an empty SHA at evaluation time) and from
	// a rejected `gh pr merge` (reasonMergeFailed), since this hold happens
	// before the merge command is ever issued at all.
	reasonHeadSHAChanged = "PR head commit changed since evaluation"

	// Detection-read pagination (#854, pulled in from #862's scope): a
	// cap-exhausted (or otherwise unproven-complete) comments/reviews
	// detection read forces an automerge hold under its own reason --
	// distinct from the pre-existing #850 feedback-resolution truncation
	// reasons (reasonFeedbackTruncated etc.), which cover the separate
	// GraphQL review-thread resolution read, not this detection read.
	reasonCommentsReadTruncated = "PR comments detection read truncated"
	reasonReviewsReadTruncated  = "PR reviews detection read truncated"

	// Post-merge verification (Decisions 5/6): a zero-exit `gh pr merge`
	// followed by a single refetch that does not report MERGED is
	// indeterminate, never success (Q5: one refetch, no polling); a refetch
	// that itself fails to read is a distinct reason from an indeterminate
	// but successfully-read non-MERGED state.
	reasonMergeIndeterminate    = "gh pr merge exited zero but PR is not MERGED"
	reasonMergeVerifyUnreadable = "post-merge verification read failed"
	// reasonMergeHeadMismatch (#897): the post-merge refetch reports MERGED,
	// but its headRefOid is either empty or does not byte-equal the headSHA
	// executeMerge was called with (the same commit --match-head-commit
	// pinned) -- a refetch reporting MERGED is not, by itself, proof that
	// babysit's own merge is what landed; another actor could have merged a
	// different commit in the race between the pre-merge head-SHA validation
	// and this refetch. Covers both the mismatch and the empty-headRefOid
	// case (fail closed per watch/docs/error-handling.md's default-deny
	// rule -- absence is never proof); Detail distinguishes them. Sets no
	// FailureClass: no `gh` transport failure occurred, only a refetch that
	// disproves this was babysit's own merge.
	reasonMergeHeadMismatch = "PR merged at a different head commit"

	// One-decision-per-tick (Decision 7): every enabled automerge tick
	// persists and logs exactly one full decision, including an upstream
	// PR/check/comment/review read failure -- previously these read
	// failures aborted tick() before runAutomerge ever ran, leaving no
	// automerge decision recorded at all for that tick.
	reasonUpstreamPRUnreadable       = "PR view unreadable"
	reasonUpstreamChecksUnreadable   = "CI checks unreadable"
	reasonUpstreamCommentsUnreadable = "PR comments unreadable"
	reasonUpstreamReviewsUnreadable  = "PR reviews unreadable"

	// reasonWorkflowLaunchFailed covers the same one-decision-per-tick gap
	// (Decision 7) for the three launch() call sites in tick() (babysit-
	// attention, ci-repair, address-review): a failed workflow dispatch
	// previously returned tick's error without ever recording an automerge
	// decision, leaving a stale decision from the previous tick displayed.
	reasonWorkflowLaunchFailed = "workflow launch failed"
)

// The five kill-switch recheck reasons (#886): recheckAutomergeInputs' final
// pre-merge re-read of the fleet automerge.enabled switch (loadFleetSwitch)
// distinguishes each way that re-read can come back disabled -- distinct
// from reasonAutomergeDisabled (the first-pass hold in evaluateAutomerge's
// own "enabled" stage, which never distinguishes *why* it's disabled) so a
// switch that flips (or a config that breaks) strictly between the first
// evaluation and the merge attempt is distinguishable in logs/state from an
// ordinary first-pass disabled tick.
const (
	reasonKillSwitchDisabled         = "fleet automerge switch explicitly disabled"
	reasonKillSwitchConfigMissing    = "fleet automerge config missing"
	reasonKillSwitchConfigUnreadable = "fleet automerge config unreadable"
	reasonKillSwitchConfigMalformed  = "fleet automerge config malformed"
	reasonKillSwitchEnabledAbsent    = "fleet automerge config enabled key absent"
)

// automergeStageKeys is the fixed, ordered set of condition-chain stages
// logLine renders -- independent of the order evaluateAutomerge actually
// reaches them, since a stage not yet reached always renders "-". "queue" is
// #854's new terminal stage (Decision 4): appended last since it is always
// the final gate evaluateAutomerge reaches on an otherwise-passing chain.
var automergeStageKeys = []string{"enabled", "label", "ci", "review", "mergeable", "headsha", "policy", "files", "filecap", "lines", "protected", "method", "queue"}

// conditionResult records one evaluated stage of the automerge condition
// chain, for the full-verdict log line and for State persistence (the
// supervisor's detached mode has no stdout, so the decision must survive a
// save/load round trip -- #824 Assumption).
type conditionResult struct {
	Key     string `json:"key"`
	Reached bool   `json:"reached"`
	Pass    bool   `json:"pass"`
}

// automergeDecision is evaluateAutomerge's pure verdict, plus the PR number
// runAutomerge fills in for logLine.
type automergeDecision struct {
	PR     string
	Merge  bool
	Reason string
	// Detail is the underlying `gh` failure output for a hold, when
	// available (e.g. a rejected merge's combined output, or a wrapped
	// fetch error's message) -- purely diagnostic, layered onto the stable
	// Reason constant rather than replacing it, so the reason string a
	// caller matches on never changes shape.
	Detail     string
	Conditions []conditionResult
	// SkippedChecks is the number of "skipping"-bucket checks counted at
	// evaluation time (#1129 AC 11) -- purely diagnostic, rendered as a
	// "(skipped=N)" suffix on the "ci" stage of conditionBracket when
	// non-zero, so an operator can see at a glance how many paths-filtered
	// checks were treated as pass-like. Not persisted onto State (no
	// stateSchemaVersion bump): both consumers (logLine, the post-merge
	// attribution comment) render from the live decision, never from
	// State.AutomergeConditions.
	SkippedChecks int
	// FailureClass is the orthogonal "cause" axis to Reason's "site" axis
	// (#886): which kind of underlying `gh` failure (command/timeout/
	// cancelled/truncated/parse) produced this decision, via
	// classifyGhFailure, when the hold stemmed from a `gh` failure at all.
	// "" for every ordinary condition-chain hold and every clean merge.
	FailureClass string
}

// logLine renders one line per tick, avoiding the " skip:" / " dispatch "
// substrings lazyboards classifies decision lines on (mainsync.go:217-219's
// rule applied to this new log line). Detail renders on both the merged and
// held branches (#886) -- a confirmed merge can still carry a diagnostic
// Detail (e.g. a nonzero `gh pr merge` exit the post-merge refetch
// overrode), and it must not be silently dropped just because the verdict
// was ultimately a success. FailureClass renders as " class=<class>" inside
// the bracketed condition-chain segment, only when non-empty.
func (d automergeDecision) logLine() string {
	verdict := "merged"
	if !d.Merge {
		verdict = "held: " + d.Reason
	}
	if d.Detail != "" {
		verdict += " (" + d.Detail + ")"
	}
	return fmt.Sprintf("babysit: automerge PR #%s %s %s", d.PR, verdict, d.conditionBracket())
}

// conditionBracket renders the bracketed condition-chain segment shared by
// logLine above and the post-merge attribution comment (#1049): every stage
// in automergeStageKeys order, plus the " class=<class>" suffix when
// FailureClass is non-empty. Extracted so the durable record left on the PR
// and the line written to the operator's log are the same rendering rather
// than two independently-drifting ones.
//
// The "ci" stage additionally grows a "(skipped=N)" suffix (#1129 AC 11)
// when the rendered symbol is not "-" (the stage was actually reached) AND
// SkippedChecks > 0 -- gated on the rendered symbol, not merely on the field
// being non-zero, so a SkippedChecks value computed once at the top of
// evaluateAutomerge (before an earlier stage like "label" ever reaches "ci"
// at all) never leaks onto an unreached "ci=-" stage.
func (d automergeDecision) conditionBracket() string {
	parts := make([]string, len(automergeStageKeys))
	for i, k := range automergeStageKeys {
		symbol := conditionSymbol(d.Conditions, k)
		if k == "ci" && symbol != "-" && d.SkippedChecks > 0 {
			symbol += fmt.Sprintf("(skipped=%d)", d.SkippedChecks)
		}
		parts[i] = k + "=" + symbol
	}
	bracket := strings.Join(parts, " ")
	if d.FailureClass != "" {
		bracket += " class=" + d.FailureClass
	}
	return "[" + bracket + "]"
}

// conditionSymbol renders "yes"/"no"/"-" for key: "-" when the stage was
// never reached (short-circuited by an earlier failing stage).
func conditionSymbol(conds []conditionResult, key string) string {
	for _, c := range conds {
		if c.Key != key {
			continue
		}
		if !c.Reached {
			return "-"
		}
		if c.Pass {
			return "yes"
		}
		return "no"
	}
	return "-"
}

// automergeInputs is the full, explicit input to evaluateAutomerge -- mirrors
// dispatch.Decide's Inputs discipline (decide.go:38-49): every field a pure
// decision needs, no hidden I/O.
type automergeInputs struct {
	Enabled bool

	ClosingIssues []int
	IssueLabels   map[int][]string
	LabelsErr     error

	RepairPending bool
	PendingKeys   []string
	// FeedbackHold is the feedback-resolution verdict for this evaluation
	// pass -- reconcileFeedback's (tick's own mutating resolution pass) or
	// revalidateFeedback's (the pre-merge recheck's read-only pass, #885):
	// one of the five feedback-hold reason constants (unreadable, truncated,
	// unknown, unsupported, or reopened) when GitHub's review-feedback state
	// -- across both PendingKeys and previously-AddressedKeys -- fails to
	// positively confirm resolution, "" when clean. It is checked after
	// RepairPending and before len(PendingKeys) so a fail-closed feedback
	// state never masquerades as the ordinary reasonReviewPending hold. Both
	// callers reread authoritative GitHub state fresh for their own pass --
	// neither ever carries a stale verdict forward from an earlier pass
	// (#885: the pre-merge recheck previously carried the first pass's
	// already-computed verdict, which let a PR merge with feedback reopened
	// strictly between the two passes). FeedbackDetail is the raw,
	// unsanitized diagnostic string -- like LabelsErr/PolicyErr/
	// AllowedMethodsErr below, it is sanitized exactly once, inside
	// evaluateAutomerge, not at this struct's construction site.
	FeedbackHold   string
	FeedbackDetail string

	IsDraft   bool
	Mergeable string
	// HeadRefOID is the PR's head commit SHA at the moment the condition
	// chain was evaluated -- runAutomerge pins `gh pr merge` to this exact
	// value via --match-head-commit (the TOCTOU guard). An empty value here
	// (e.g. a transient `gh pr view` gap) would make `gh` silently omit
	// --match-head-commit's expectedHeadOid from the merge mutation, so the
	// TOCTOU guard must fail closed rather than merge unpinned.
	HeadRefOID string

	ChangedFiles int
	Additions    int
	Deletions    int
	Files        []string

	// PolicyErr is set when fetchPolicy itself failed (unreadable/non-JSON).
	// PolicyReason is set by resolvePolicy when Policy is nil for a reason
	// other than "not fetched yet" (absent or malformed) -- runAutomerge's
	// lazy-fetch loop uses the absence of both plus a nil Policy as its
	// "policy stage not yet reached" sentinel.
	PolicyErr    error
	PolicyReason string
	Policy       *effectivePolicy

	MergeMethod       string
	AllowedMethods    map[string]bool
	AllowedMethodsErr error

	// Checks is automergeCIHold's input (#854, #1129): unlike ciStatus()
	// (babysit.go), the pre-collapsed verdict that feeds BlocksClose (#787)
	// and keeps its own, separately-evolving semantics, Checks carries every
	// check bucket so automergeCIHold can distinguish fail/pending/cancel/
	// empty/unknown buckets individually (treating skipping as pass-like)
	// instead of collapsing them into a single "not green" reason.
	Checks []check

	// Merge-queue gate inputs (#854, Q3): pointer fields so an absent
	// GraphQL field is distinguishable from an explicit false
	// (dispatch/config.go:102-123's absent-vs-false pattern) -- an absent
	// isInMergeQueue/isMergeQueueEnabled must never be read as "not
	// required". QueueProbed disambiguates runAutomerge's lazy-fetch "not
	// yet probed" trial sentinel (QueueProbed == false, both fields still
	// zero) from a probe that has genuinely run and come back with nothing
	// (QueueProbed == true, both fields nil) -- a real GraphQL response
	// shaped like {"pullRequest":null} (deleted/renumbered/inaccessible PR,
	// transient null-propagation, zero exit, no top-level errors[]) decodes
	// to exactly that same both-nil shape, and without this flag it would
	// be indistinguishable from "queue stage not yet reached" and pass
	// through as if the probe had never run at all.
	QueueInMergeQueue *bool
	QueueEnabled      *bool
	QueueErr          error
	QueueProbed       bool

	// Detection-read completeness (#854): false when the comments or
	// reviews detection read hit its pagination cap while still returning
	// full pages (or otherwise could not be proven complete) -- forces
	// reasonCommentsReadTruncated/reasonReviewsReadTruncated rather than
	// silently treating a partial detection read as complete.
	CommentsComplete bool
	ReviewsComplete  bool
}

// automergeCIHold classifies checks under the #1129 relaxed-skipping rule
// (Decisions 1/2): CI is green when at least one check is "pass" and every
// other check is "pass" or "skipping" -- a paths-filtered monorepo's
// skipped, unaffected-project checks must not hold automerge forever.
// Otherwise, buckets are scanned in the order checks appear treating
// "skipping" as pass-like (never itself a holding bucket), and the first
// genuinely non-pass bucket found wins its own distinct reason -- unlike
// ciStatus's severity-priority ordering (fail beats pending), automerge's
// contract denies on ANY non-pass/non-skipping bucket, so first-found-in-
// order is the simplest, most predictable contract to specify and test. An
// empty checks slice reuses the existing reasonNoChecks ("no CI checks
// reported") rather than a new constant; a zero-pass, all-skipping set holds
// under its own distinct reasonCIAllChecksSkipped instead.
func automergeCIHold(checks []check) string {
	if len(checks) == 0 {
		return reasonNoChecks
	}
	tally := countBuckets(checks)
	if tally.Pass >= 1 && tally.Pass+tally.Skipping == tally.Total {
		return ""
	}
	for _, c := range checks {
		switch c.Bucket {
		case "pass":
			continue
		case "skipping":
			continue
		case "fail":
			return reasonCICheckFailed
		case "pending":
			return reasonCICheckPending
		case "cancel":
			return reasonCICheckCancelled
		case "":
			return reasonCIBucketEmpty
		default:
			return reasonCIBucketUnknown
		}
	}
	// Every check is "skipping" (zero "pass") -- the green rule above
	// requires at least one pass, so this loop falls through here rather
	// than returning a per-bucket reason: distinct from reasonNoChecks
	// (zero checks at all).
	return reasonCIAllChecksSkipped
}

// automergeQueueHold classifies the merge-queue GraphQL probe's result
// under Decision 4 (Q3): an explicit isInMergeQueue or isMergeQueueEnabled
// true holds under reasonMergeQueueRequired (automerge must never enqueue
// as a side effect); a probe error holds under reasonMergeQueueUnknown; and
// a probe result with exactly one of the two fields nil (a malformed/partial
// GraphQL response) also holds under reasonMergeQueueUnknown -- absence is
// never read as "not required".
//
// Both fields nil with no error is ambiguous by itself: it's both
// runAutomerge's lazy-fetch "not yet probed" trial sentinel (mirroring the
// policy/allowed-methods absent-sentinel pattern) AND the shape a genuine
// GraphQL response with a null pullRequest decodes to (deleted/renumbered/
// inaccessible PR, transient null-propagation, zero exit, no top-level
// errors[]). probed disambiguates the two: probed == false means the
// lazy-fetch loop hasn't triggered the probe yet, so this passes through
// (the loop reads that as "every earlier stage passed" and fetches for
// real); probed == true means the probe genuinely ran and came back with
// nothing, which must hold under reasonMergeQueueUnknown rather than pass
// through as if the probe had never run (#854 fix -- previously both cases
// were indistinguishable and a genuinely-probed null response silently
// passed as if the probe were still pending).
func automergeQueueHold(inQueue, enabled *bool, err error, probed bool) string {
	if err != nil {
		return reasonMergeQueueUnknown
	}
	if inQueue == nil && enabled == nil {
		if !probed {
			return ""
		}
		return reasonMergeQueueUnknown
	}
	if inQueue == nil || enabled == nil {
		return reasonMergeQueueUnknown
	}
	if *inQueue || *enabled {
		return reasonMergeQueueRequired
	}
	return ""
}

// evaluateAutomerge is pure: identical automergeInputs yield an identical
// automergeDecision, no I/O. It follows the condition chain's exact order
// (see the plan's "Condition chain" section) and returns the first failing
// stage's reason -- mirrors dispatch.Decide's first-failing-gate-wins
// discipline (decide.go:51-53).
func evaluateAutomerge(in automergeInputs) automergeDecision {
	var conds []conditionResult
	// Computed once, up front, so both the "held" and "merged" return paths
	// below carry the same diagnostic count (#1129 AC 11) -- independent of
	// which stage the chain actually reaches, since a hold at an earlier
	// stage (e.g. "label") must still render "ci=-" with no suffix via
	// conditionBracket's own reached-symbol gate, not by this value being
	// zero.
	skipped := countBuckets(in.Checks).Skipping
	fail := func(key, reason string) automergeDecision {
		conds = append(conds, conditionResult{Key: key, Reached: true, Pass: false})
		return automergeDecision{Merge: false, Reason: reason, Conditions: conds, SkippedChecks: skipped}
	}
	pass := func(key string) {
		conds = append(conds, conditionResult{Key: key, Reached: true, Pass: true})
	}

	// 1. automerge.enabled (fleet).
	if !in.Enabled {
		return fail("enabled", reasonAutomergeDisabled)
	}
	pass("enabled")

	// 2. Closing issues exist; labels readable; all carry automerge:ok.
	if len(in.ClosingIssues) == 0 {
		return fail("label", reasonNoClosingIssue)
	}
	if in.LabelsErr != nil {
		d := fail("label", reasonLabelUnreadable)
		d.Detail = sanitizeDetail(in.LabelsErr.Error())
		return d
	}
	for _, issue := range in.ClosingIssues {
		if !hasAutomergeOK(in.IssueLabels[issue]) {
			return fail("label", reasonLabelMissing)
		}
	}
	pass("label")

	// 3. CI (Decisions 1/2, #854, relaxed by #1129): green when at least one
	// check is "pass" and every other check is "pass" or "skipping";
	// fail/pending/cancel/empty/unknown buckets each hold under their own
	// distinct reason via automergeCIHold.
	if hold := automergeCIHold(in.Checks); hold != "" {
		return fail("ci", hold)
	}
	pass("ci")

	// 4. !RepairPending && detection reads proven complete && no feedback
	// hold && len(PendingKeys) == 0.
	if in.RepairPending {
		return fail("review", reasonRepairPending)
	}
	// #854: a comments/reviews detection read that could not be proven
	// complete (pagination cap exhausted, or otherwise unproven) forces a
	// hold under its own distinct reason -- checked before the ordinary
	// feedback-hold/PendingKeys checks, since an incomplete read means
	// PendingKeys itself may be missing entries GitHub actually reports.
	if !in.CommentsComplete {
		return fail("review", reasonCommentsReadTruncated)
	}
	if !in.ReviewsComplete {
		return fail("review", reasonReviewsReadTruncated)
	}
	// #850: FeedbackHold is checked before the ordinary PendingKeys check so
	// an unreadable/truncated/unknown/unsupported feedback state is never
	// collapsed into the resolvable reasonReviewPending hold.
	if in.FeedbackHold != "" {
		d := fail("review", in.FeedbackHold)
		d.Detail = sanitizeDetail(in.FeedbackDetail)
		return d
	}
	if len(in.PendingKeys) != 0 {
		return fail("review", reasonReviewPending)
	}
	pass("review")

	// 5. !isDraft; mergeable == MERGEABLE.
	if in.IsDraft {
		return fail("mergeable", reasonDraft)
	}
	if in.Mergeable == "UNKNOWN" {
		return fail("mergeable", reasonMergeableUnknown)
	}
	if in.Mergeable != "MERGEABLE" {
		return fail("mergeable", reasonNotMergeable)
	}
	pass("mergeable")

	// 5b. The head commit SHA the merge will be pinned to (--match-head-
	// commit) must actually be known -- an empty value would make `gh`
	// silently drop the pin instead of failing closed.
	if in.HeadRefOID == "" {
		return fail("headsha", reasonHeadSHAUnknown)
	}
	pass("headsha")

	// 6. changedFiles > 0; len(files) == changedFiles (no gh truncation).
	if in.ChangedFiles <= 0 {
		return fail("files", reasonNoChanges)
	}
	if len(in.Files) != in.ChangedFiles {
		return fail("files", reasonDiffTruncated)
	}
	pass("files")

	// 7. Policy fetch/resolve.
	if in.PolicyErr != nil {
		d := fail("policy", reasonPolicyUnreadable)
		d.Detail = sanitizeDetail(in.PolicyErr.Error())
		return d
	}
	if in.Policy == nil {
		reason := in.PolicyReason
		if reason == "" {
			reason = reasonPolicyAbsent
		}
		return fail("policy", reason)
	}
	pass("policy")

	// 8. Diff-size caps. The file-count cap gets its own "filecap" key,
	// distinct from stage 6's "files" (changedFiles > 0 && no truncation):
	// reusing "files" here would let stage 6's earlier passing entry win in
	// conditionSymbol's first-match lookup, rendering "files=yes" on a hold
	// this stage actually produced.
	if in.ChangedFiles > in.Policy.MaxChangedFiles {
		return fail("filecap", reasonTooManyFiles)
	}
	pass("filecap")
	if in.Additions+in.Deletions > in.Policy.MaxDiffLines {
		return fail("lines", reasonTooManyLines)
	}
	pass("lines")

	// 9. No changed file matches a protected-path glob.
	for _, f := range in.Files {
		for _, p := range in.Policy.ProtectedPaths {
			matched, err := matchesProtected(p, f)
			if err != nil || matched {
				// A malformed pattern here is defensive-only: resolvePolicy
				// already rejects empty patterns as reasonPolicyMalformed
				// before an effectivePolicy is ever produced. Default-deny
				// per watch/docs/error-handling.md rather than silently
				// ignore a matcher error.
				return fail("protected", reasonProtectedPath)
			}
		}
	}
	pass("protected")

	// 10. Squash-only execution (Decision 3, #854): mergeMethod stays
	// readable for configuration compatibility, but only "squash" ever
	// executes -- any other resolved policy mergeMethod holds here, before
	// any allowed-merge-methods fetch, so a non-squash policy costs zero
	// extra gh calls beyond the policy fetch itself.
	if in.MergeMethod != "squash" {
		return fail("method", reasonMergeMethodNotSquash)
	}
	if in.AllowedMethodsErr != nil {
		d := fail("method", reasonMergeMethodUnknown)
		d.Detail = sanitizeDetail(in.AllowedMethodsErr.Error())
		return d
	}
	if !in.AllowedMethods["squash"] {
		return fail("method", reasonMergeMethodDisallowed)
	}
	pass("method")

	// 11. Merge-queue gate (Decision 4, #854): terminal stage, evaluated
	// only once every earlier stage has passed -- a lazy GraphQL probe
	// plugs into runAutomerge's fetch loop the same way policy/allowed-
	// methods do.
	if hold := automergeQueueHold(in.QueueInMergeQueue, in.QueueEnabled, in.QueueErr, in.QueueProbed); hold != "" {
		return fail("queue", hold)
	}
	pass("queue")

	return automergeDecision{Merge: true, Reason: "", Conditions: conds, SkippedChecks: skipped}
}

// detailMaxLen bounds sanitizeDetail's output length in bytes. Truncation
// itself is rune-boundary-safe (see sanitizeDetail) so a multi-byte UTF-8
// character straddling this cutoff is never split mid-encoding.
const detailMaxLen = 200

// sanitizeDetail collapses every C0 control character (not just "\n") and
// truncates detail to a bounded length before it's assigned onto
// automergeDecision.Detail -- Detail is raw `gh` failure output, unbounded
// and potentially multi-line, but it feeds into logLine's single-line-per-
// tick format that downstream tooling (lazyboards) substring-classifies,
// and it's also persisted onto State. Stripping "\n" alone left "\r" and
// other control bytes (form feed, vertical tab, ANSI escape sequences, ...)
// free to break that one-line contract or corrupt the persisted state file;
// every rune below 0x20, plus DEL (0x7f), is replaced with a single space.
func sanitizeDetail(detail string) string {
	var sb strings.Builder
	sb.Grow(len(detail))
	for _, r := range detail {
		if r < 0x20 || r == 0x7f {
			sb.WriteByte(' ')
			continue
		}
		sb.WriteRune(r)
	}
	sanitized := sb.String()
	if len(sanitized) > detailMaxLen {
		// Walk back from the byte cutoff to the nearest rune start so a
		// multi-byte UTF-8 character straddling detailMaxLen is never split
		// mid-encoding in the persisted state file or log line.
		cut := detailMaxLen
		for cut > 0 && !utf8.RuneStart(sanitized[cut]) {
			cut--
		}
		sanitized = sanitized[:cut] + "..."
	}
	return sanitized
}

// hasAutomergeOK reports whether labels contains the automerge:ok grant.
func hasAutomergeOK(labels []string) bool {
	for _, l := range labels {
		if l == "automerge:ok" {
			return true
		}
	}
	return false
}

// -- policy types and resolution (#824) --------------------------------------

// policyBlock is one on-disk "automerge" block (top-level or per-project).
// MaxChangedFiles/MaxDiffLines are pointer fields so an absent or explicit
// non-positive value is distinguishable from a legitimately small cap and
// treated as malformed (per dispatch/config.go:102-123's pointer-field
// pattern and the plan's Assumption on per-field default-deny).
type policyBlock struct {
	ProtectedPaths  []string `json:"protectedPaths"`
	MaxChangedFiles *int     `json:"maxChangedFiles"`
	MaxDiffLines    *int     `json:"maxDiffLines"`
	MergeMethod     string   `json:"mergeMethod"`
}

// projectPolicy is one entry of the repo config's "projects" array, scoped to
// what resolvePolicy needs: the project's path prefix and its own automerge
// block (nil when the project sets none).
type projectPolicy struct {
	Path      string       `json:"path"`
	Automerge *policyBlock `json:"automerge"`
}

// repoConfigFile is the base-ref .cenci/config.json shape resolvePolicy reads.
type repoConfigFile struct {
	Automerge *policyBlock    `json:"automerge"`
	Projects  []projectPolicy `json:"projects"`
}

// effectivePolicy is the fully-resolved, validated policy resolvePolicy
// produces for a specific set of changed files.
type effectivePolicy struct {
	ProtectedPaths  []string
	MaxChangedFiles int
	MaxDiffLines    int
	MergeMethod     string
}

// resolvePolicy resolves cfg against files: each file is owned by the
// longest-path-prefix project in cfg.Projects (falling back to the top-level
// block when the owning project sets none, or when no project owns the
// file); the effective policy is the most-restrictive merge (min of each
// numeric cap, union of protectedPaths) across every block actually
// applicable to files. Any file with no applicable block at all, any
// malformed cap (absent or non-positive MaxChangedFiles/MaxDiffLines, an
// empty protectedPaths pattern), and any mergeMethod disagreement across
// applicable blocks all deny with a distinct reason -- Q2's "absent block
// denies, no fallback to built-in defaults" applied consistently.
func resolvePolicy(cfg repoConfigFile, files []string) (effectivePolicy, string) {
	var blocks []*policyBlock
	for _, f := range files {
		b := ownerBlock(cfg, f)
		if b == nil {
			return effectivePolicy{}, reasonPolicyAbsent
		}
		if !containsBlock(blocks, b) {
			blocks = append(blocks, b)
		}
	}
	if len(blocks) == 0 {
		return effectivePolicy{}, reasonPolicyAbsent
	}

	var out effectivePolicy
	var mergeMethod string
	protected := map[string]bool{}
	for i, b := range blocks {
		if b.MaxChangedFiles == nil || *b.MaxChangedFiles <= 0 {
			return effectivePolicy{}, reasonPolicyMalformed
		}
		if b.MaxDiffLines == nil || *b.MaxDiffLines <= 0 {
			return effectivePolicy{}, reasonPolicyMalformed
		}
		for _, p := range b.ProtectedPaths {
			if p == "" {
				return effectivePolicy{}, reasonPolicyMalformed
			}
			protected[p] = true
		}
		method := b.MergeMethod
		if method == "" {
			method = "squash"
		}
		if i == 0 {
			mergeMethod = method
			out.MaxChangedFiles = *b.MaxChangedFiles
			out.MaxDiffLines = *b.MaxDiffLines
			continue
		}
		if method != mergeMethod {
			return effectivePolicy{}, reasonPolicyMalformed
		}
		if *b.MaxChangedFiles < out.MaxChangedFiles {
			out.MaxChangedFiles = *b.MaxChangedFiles
		}
		if *b.MaxDiffLines < out.MaxDiffLines {
			out.MaxDiffLines = *b.MaxDiffLines
		}
	}
	out.MergeMethod = mergeMethod
	for p := range protected {
		out.ProtectedPaths = append(out.ProtectedPaths, p)
	}
	return out, ""
}

// ownerBlock returns the automerge block that applies to file: the longest
// projects[].path prefix match's own block, falling back to the top-level
// block when that project sets none (or no project owns file at all). Nil
// means no block applies at all (Q2: absent, not built-in defaults).
func ownerBlock(cfg repoConfigFile, file string) *policyBlock {
	var best *projectPolicy
	for i := range cfg.Projects {
		p := &cfg.Projects[i]
		if !fileOwnedByProject(file, p.Path) {
			continue
		}
		if best == nil || len(p.Path) > len(best.Path) {
			best = p
		}
	}
	if best != nil && best.Automerge != nil {
		return best.Automerge
	}
	return cfg.Automerge
}

// fileOwnedByProject reports whether file falls under path: an exact match or
// anything below path/ -- never a bare string-prefix match, so a project
// path "watch" never accidentally claims a sibling directory "watching/...".
func fileOwnedByProject(file, path string) bool {
	if path == "" {
		return false
	}
	return file == path || strings.HasPrefix(file, path+"/")
}

// containsBlock reports whether blocks already holds b (identity, not deep
// equality) -- resolvePolicy validates/merges each distinct applicable block
// exactly once even when multiple files share the same owning block.
func containsBlock(blocks []*policyBlock, b *policyBlock) bool {
	for _, x := range blocks {
		if x == b {
			return true
		}
	}
	return false
}

// matchesProtected reports whether path matches the glob pattern: "*" spans
// "/" (a wildcard shell glob would not, but a protected-path denylist must
// catch "everything under this directory"), matching is case-insensitive,
// and every literal segment is regexp.QuoteMeta-escaped (watch/docs/go-
// gotchas.md #528) so a metacharacter in a hand-written pattern (".", "+",
// etc.) is never silently reinterpreted. An empty pattern is malformed --
// unescaped, it would otherwise compile to a matches-everything regex.
//
// A pattern ending in "/" (a bare directory-prefix entry, no trailing "*")
// is treated as "this directory and everything under it": the config's
// shipped protected-path entries are directory prefixes like
// "watch/internal/sandbox/", and requiring every such entry to be hand-
// rewritten with a trailing "*" would make them silently dead against any
// real file path (they can only ever match that exact literal string
// otherwise).
//
// The compiled regex uses the "i" (case-insensitive) and "s" (dot matches
// \n) flags together, and \A/\z anchors rather than ^/$, so a changed-file
// path with an embedded newline cannot dodge a "*"-wildcard pattern that
// spans it.
func matchesProtected(pattern, path string) (bool, error) {
	if pattern == "" {
		return false, errors.New("empty protected-path pattern")
	}
	var sb strings.Builder
	sb.WriteString(`(?is)\A`)
	segments := strings.Split(pattern, "*")
	for i, s := range segments {
		if i > 0 {
			sb.WriteString(".*")
		}
		sb.WriteString(regexp.QuoteMeta(s))
	}
	if strings.HasSuffix(pattern, "/") && !strings.HasSuffix(pattern, "*") {
		sb.WriteString(".*")
	}
	sb.WriteString(`\z`)
	re, err := regexp.Compile(sb.String())
	if err != nil {
		return false, err
	}
	return re.MatchString(path), nil
}

// -- fleet enable switch (#824) -----------------------------------------------

// fleetConfigPath is a test seam over defaultFleetConfigPath, mirroring the
// package's existing command/processOwned seam shape. "" disables automerge
// entirely (loadFleetEnabled's default-deny path).
var fleetConfigPath = defaultFleetConfigPath

// defaultFleetConfigPath resolves the fleet config.json location:
// $XDG_CONFIG_HOME/cenci/config.json, falling back to
// ~/.config/cenci/config.json, "" when no home is known. Mirrors
// internal/run.defaultConfigPath, duplicated locally rather than imported:
// internal/run imports internal/daemon, which imports this package, and
// importing internal/run here would close that cycle.
func defaultFleetConfigPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "cenci", "config.json")
}

// fleetAutomergeFile is the fleet config's relevant shape: only the
// automerge.enabled kill switch, a pointer so an explicit false is
// distinguishable from unset (dispatch/config.go:102-123's pattern).
type fleetAutomergeFile struct {
	Automerge struct {
		Enabled *bool `json:"enabled"`
	} `json:"automerge"`
}

// loadFleetEnabled reports the fleet-wide automerge.enabled kill switch
// (default false): missing path, unreadable file, malformed JSON, or an
// absent/false enabled key all resolve to disabled -- automerge only ever
// turns on with an explicit "enabled": true. A thin wrapper over
// loadFleetSwitch, discarding the specific hold reason: the first-pass
// caller (runAutomerge's own in.Enabled) has never needed to distinguish
// *why* the switch is off, only recheckAutomergeInputs' final pre-merge
// re-read does (#886).
func loadFleetEnabled() bool {
	enabled, _ := loadFleetSwitch()
	return enabled
}

// loadFleetSwitch reports the fleet-wide automerge.enabled kill switch, plus
// -- when disabled -- which of the five distinct ways it came back disabled
// (#886): an explicit "enabled": false, a missing config path/file
// (fleetConfigPath's "" seam value, or os.ReadFile's fs.ErrNotExist --
// per watch/docs/go-gotchas.md, errors.Is(err, fs.ErrNotExist), never the
// os/exec-only os.IsNotExist), an unreadable file (e.g. a directory,
// permission denied), malformed JSON, or a valid JSON document whose
// "enabled" key is simply absent. holdReason is "" only when enabled is
// true.
func loadFleetSwitch() (enabled bool, holdReason string) {
	path := fleetConfigPath()
	if path == "" {
		return false, reasonKillSwitchConfigMissing
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, reasonKillSwitchConfigMissing
		}
		return false, reasonKillSwitchConfigUnreadable
	}
	var f fleetAutomergeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return false, reasonKillSwitchConfigMalformed
	}
	if f.Automerge.Enabled == nil {
		return false, reasonKillSwitchEnabledAbsent
	}
	if !*f.Automerge.Enabled {
		return false, reasonKillSwitchDisabled
	}
	return true, ""
}

// -- gh fetch orchestration (#824) -------------------------------------------

// fetchPolicy reads .cenci/config.json from repo's baseRef -- never the PR's
// head ref (Q1): a PR must never be able to widen its own policy to self-
// approve by editing the file on its own branch. Non-JSON/unreadable output
// is reported as reasonPolicyUnreadable. baseRef is query-escaped before
// interpolation: a branch ref name can legally contain characters (e.g. "#",
// "&") that would otherwise corrupt the query string.
func fetchPolicy(repo, baseRef string) (repoConfigFile, error) {
	stdout, stderr, err := execGh("api", "-H", "Accept: application/vnd.github.raw", "repos/"+repo+"/contents/.cenci/config.json?ref="+url.QueryEscape(baseRef))
	if err != nil {
		return repoConfigFile{}, fmt.Errorf("%s: %s: %w", reasonPolicyUnreadable, strings.TrimSpace(stderr), err)
	}
	var cfg repoConfigFile
	if err := json.Unmarshal([]byte(stdout), &cfg); err != nil {
		return repoConfigFile{}, fmt.Errorf("%s: %w", reasonPolicyUnreadable, err)
	}
	return cfg, nil
}

// issueLabelsResp is the `gh issue view --json labels` response shape.
type issueLabelsResp struct {
	Labels []struct{ Name string } `json:"labels"`
}

// fetchClosingIssueLabels fetches each closing issue's labels in order,
// stopping at the first failure (LabelsErr) so the caller can report
// reasonLabelUnreadable rather than papering over a partial fetch.
func fetchClosingIssueLabels(repo string, issues []int) (map[int][]string, error) {
	result := make(map[int][]string, len(issues))
	for _, n := range issues {
		var resp issueLabelsResp
		if err := ghJSON(&resp, "issue", "view", strconv.Itoa(n), "--repo", repo, "--json", "labels"); err != nil {
			return result, err
		}
		names := make([]string, 0, len(resp.Labels))
		for _, l := range resp.Labels {
			names = append(names, l.Name)
		}
		result[n] = names
	}
	return result, nil
}

// allowedMethodsResp is the repo-settings probe's --jq-projected shape.
type allowedMethodsResp struct {
	Squash bool `json:"squash"`
	Merge  bool `json:"merge"`
	Rebase bool `json:"rebase"`
}

// fetchAllowedMethods probes the repo's allowed merge methods (branch-
// protection/repo settings), so a configured mergeMethod that the repo
// itself disallows (e.g. allow_merge_commit: false) is caught before
// attempting a merge that would hard-fail.
func fetchAllowedMethods(repo string) (map[string]bool, error) {
	var resp allowedMethodsResp
	if err := ghJSON(&resp, "api", "repos/"+repo, "--jq", "{squash:.allow_squash_merge,merge:.allow_merge_commit,rebase:.allow_rebase_merge}"); err != nil {
		return nil, err
	}
	return map[string]bool{"squash": resp.Squash, "merge": resp.Merge, "rebase": resp.Rebase}, nil
}

// recordDecision persists d onto s (for the detached supervisor, whose
// cmd.Stdout is nil) and emits automerge's one-line-per-tick log line
// (Decision 7, #854) -- the single place both runAutomerge's own outcomes
// and tick's upstream-read-failure paths (via recordHold) funnel through, so
// every enabled automerge tick persists and logs exactly one full decision.
func recordDecision(s *State, d automergeDecision, merged bool) {
	d.PR = s.PR
	s.AutomergeReason = d.Reason
	s.AutomergeDetail = d.Detail
	s.AutomergeConditions = d.Conditions
	// Assigned unconditionally, every call (#886): a decision with no
	// FailureClass (d.FailureClass == "", the ordinary case for every
	// condition-chain hold and every clean merge) must clear a stale class a
	// previous failed tick left behind, not merely leave it untouched.
	s.AutomergeFailureClass = d.FailureClass
	s.AutomergeCheckedAt = time.Now().UTC()
	if merged {
		s.AutomergeDecision = "merge"
	} else {
		s.AutomergeDecision = "held"
	}
	fmt.Println(d.logLine())
}

// recordHold persists reason/detail/failureClass as a held automerge
// decision -- shared by tick's upstream-read-failure paths (which never
// reach runAutomerge or evaluateAutomerge at all) and any other hold
// recorded outside the pure condition chain.
func recordHold(s *State, reason, detail, failureClass string) {
	recordDecision(s, automergeDecision{Reason: reason, Detail: detail, FailureClass: failureClass}, false)
}

// recordUpstreamReadFailure records reason as a held automerge decision for
// an upstream PR/checks/comments/reviews read failure (Decision 7) -- with
// the fleet kill switch off, the recorded reason stays reasonAutomergeDisabled
// even though an upstream read also failed (Q4), matching today's healthy-
// tick behavior rather than surfacing the specific read failure when
// automerge was never going to run anyway. FailureClass is set via
// classifyGhFailure(err) regardless (#886) -- it's the diagnostic cause of
// the read failure itself, independent of whether automerge was even going
// to run this tick.
func recordUpstreamReadFailure(s *State, reason string, err error) {
	class := classifyGhFailure(err)
	if !loadFleetEnabled() {
		reason = reasonAutomergeDisabled
	}
	recordHold(s, reason, sanitizeDetail(err.Error()), class)
}

// runAutomerge evaluates the automerge condition chain for pr and, on a
// Merge==true verdict, issues `gh pr merge --squash` (never --delete-branch,
// per epic #661 Decision 4 -- PR worktrees still reference the branch and
// delete_branch_on_merge is off on this repo), pinned to the exact head
// commit evaluateAutomerge actually evaluated via --match-head-commit -- so
// a push landing between evaluation and merge can never get content merged
// that was never checked. It fetches lazily: labels are fetched only when
// automerge is enabled and the PR closes an issue; the base-ref policy, the
// repo's allowed-merge-methods, and the merge-queue/head-SHA probe are each
// fetched only once evaluateAutomerge's trial verdict (with that stage's
// inputs still zero-valued) shows every earlier stage already passed -- so
// a PR held by an earlier stage costs zero extra `gh` calls beyond the
// labels fetch. verdict is reconcileFeedback's already-computed result
// (#850, generalized by #885 to reclassify AddressedKeys too): tick calls
// reconcileFeedback once, unconditionally, right after the reviews fetch --
// before this tick's new keys are even detected via detectNewFeedbackKeys and
// appended onto PendingKeys, and before the single PendingKeys-\-LaunchedKeys
// address-review launch (#897: reordered so a brand-new comment on an
// already-resolved thread is not silently classified away in the same tick it
// lands) -- immediately before calling runAutomerge, so s.PendingKeys/
// s.AddressedKeys already reflect GitHub-authoritative resolution (including
// any reopen) by the time runAutomerge reads them below. It persists exactly one decision via
// recordDecision and returns whether a merge was actually issued this tick,
// so tick can reset the backoff delay.
func runAutomerge(s *State, pr prView, checks []check, verdict feedbackVerdict, commentsComplete, reviewsComplete bool) bool {
	in := automergeInputs{
		Enabled:          loadFleetEnabled(),
		Checks:           checks,
		RepairPending:    s.RepairPending,
		PendingKeys:      s.PendingKeys,
		FeedbackHold:     verdict.Hold,
		FeedbackDetail:   verdict.Detail,
		IsDraft:          pr.IsDraft,
		Mergeable:        pr.Mergeable,
		HeadRefOID:       pr.HeadRefOID,
		ChangedFiles:     pr.ChangedFiles,
		Additions:        pr.Additions,
		Deletions:        pr.Deletions,
		CommentsComplete: commentsComplete,
		ReviewsComplete:  reviewsComplete,
	}
	for _, i := range pr.ClosingIssuesReferences {
		in.ClosingIssues = append(in.ClosingIssues, i.Number)
	}
	for _, f := range pr.Files {
		in.Files = append(in.Files, f.Path)
	}
	if in.Enabled && len(in.ClosingIssues) > 0 {
		in.IssueLabels, in.LabelsErr = fetchClosingIssueLabels(s.Repo, in.ClosingIssues)
	}

	policyFetched, methodsFetched, queueFetched := false, false, false
	var decision automergeDecision
	for {
		decision = evaluateAutomerge(in)
		if !policyFetched && decision.Reason == reasonPolicyAbsent {
			policyFetched = true
			cfg, err := fetchPolicy(s.Repo, pr.BaseRefName)
			switch {
			case err != nil:
				in.PolicyErr = err
			default:
				if policy, reason := resolvePolicy(cfg, in.Files); reason != "" {
					in.PolicyReason = reason
				} else {
					in.Policy = &policy
					in.MergeMethod = policy.MergeMethod
				}
			}
			continue
		}
		if !methodsFetched && decision.Reason == reasonMergeMethodDisallowed {
			methodsFetched = true
			methods, err := fetchAllowedMethods(s.Repo)
			if err != nil {
				in.AllowedMethodsErr = err
			} else {
				in.AllowedMethods = methods
			}
			continue
		}
		// Merge-queue gate (Decision 4, #854): the terminal stage of the
		// first, trial evaluation pass -- probed lazily exactly once every
		// earlier stage's trial verdict already passes. QueueProbed is set
		// unconditionally right after the fetch (#854 fix, item 1): a
		// genuinely-probed null/malformed response must hold under
		// reasonMergeQueueUnknown via automergeQueueHold, never pass
		// through as if the probe had never run.
		if !queueFetched && decision.Merge {
			queueFetched = true
			inQueue, enabled, _, err := fetchMergeQueueState(s.Repo, s.PR)
			in.QueueInMergeQueue = inQueue
			in.QueueEnabled = enabled
			in.QueueErr = err
			in.QueueProbed = true
			continue
		}
		break
	}

	// Pre-merge re-evaluation (Decision 9, #854): immediately before ever
	// issuing `gh pr merge`, re-fetch everything the first pass evaluated --
	// PR view (head SHA, mergeable, files), checks, labels, base-ref
	// policy, paginated comments/reviews (feeding a fresh new-feedback
	// detection pass), and merge-queue state -- and re-run the same
	// evaluator against this freshly-fetched automergeInputs (the plan's
	// chosen design: one evaluator, one decision shape, one log line).
	// AllowedMethods carries over unchanged from the first pass (repo
	// settings, not PR state, per the plan's Assumption). The fleet kill
	// switch, by contrast, is re-read fresh as recheckAutomergeInputs' own
	// final step, immediately before this point (#886): the smallest
	// possible window between confirming automerge is still enabled and
	// actually issuing `gh pr merge`. Only a still-Merge==true second
	// verdict proceeds to executeMerge; every hold here reuses an existing
	// reason constant with a "recheck: "-prefixed Detail rather than a
	// parallel constant set, per the plan's Assumption -- except the fleet
	// kill-switch holds below, which use their own distinct reasonKillSwitch*
	// constants instead (#886).
	if decision.Merge {
		recheck, failReason, err := recheckAutomergeInputs(s, in)
		switch {
		case err != nil:
			decision.Merge = false
			decision.Reason = failReason
			decision.Detail = sanitizeDetail("recheck: " + err.Error())
			decision.FailureClass = classifyGhFailure(err)
			// The second pass never ran, so pass 1's Conditions (all "yes",
			// since decision.Merge was true) would render as a self-
			// contradictory "held: ... [enabled=yes ... queue=yes]" log line
			// during incident triage -- clear it so every stage renders "-"
			// (never reached) instead, distinct from the "recheck ran and
			// found a regression" branch below, which sets fresh Conditions
			// from recheckDecision and must not be touched here.
			decision.Conditions = nil
		case failReason != "":
			// The fleet kill switch itself changed between the first
			// evaluation and this final pre-merge check (#886, AC 5/6):
			// unlike the err != nil branch above, recheckAutomergeInputs'
			// own upstream reads all succeeded here, so this is a genuine
			// disabled-switch verdict, not an I/O failure -- rendered with a
			// single synthetic "enabled=no" Condition (mirroring
			// evaluateAutomerge's own first stage) so the log line reads
			// naturally, rather than every stage rendering "-" as if nothing
			// had been reached at all.
			decision.Merge = false
			decision.Reason = failReason
			decision.Detail = ""
			decision.Conditions = []conditionResult{{Key: "enabled", Reached: true, Pass: false}}
		default:
			recheckDecision := evaluateAutomerge(recheck)
			// The TOCTOU guard (Decision 9): a head commit that changed
			// between the first evaluation and this point must hold under
			// reasonHeadSHAChanged, never merge on stale evidence, even if
			// the fresh commit would otherwise independently evaluate
			// green (its checks/mergeable state may not even be settled
			// yet).
			if recheck.HeadRefOID != "" && in.HeadRefOID != "" && recheck.HeadRefOID != in.HeadRefOID {
				recheckDecision.Merge = false
				recheckDecision.Reason = reasonHeadSHAChanged
				recheckDecision.Detail = ""
			}
			if recheckDecision.Merge {
				in = recheck
			} else {
				detail := "recheck"
				if recheckDecision.Detail != "" {
					detail = "recheck: " + recheckDecision.Detail
				}
				recheckDecision.Detail = detail
			}
			decision = recheckDecision
		}
	}

	merged := false
	if decision.Merge {
		merged, decision = executeMerge(s, in.HeadRefOID, decision)
		if merged {
			// #1049: the merge is already confirmed by executeMerge's own
			// refetch, so this is a pure audit-trail write -- it runs only on
			// a confirmed merge (never on a held, indeterminate, or
			// verify-unreadable outcome) and cannot change the verdict.
			decision = postMergeAttributionComment(s, in.HeadRefOID, decision)
		}
	}

	recordDecision(s, decision, merged)
	return merged
}
