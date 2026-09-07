package babysit

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// graphQLQueueResponse is the `gh api graphql` response envelope for the
// merge-queue probe. Errors is checked explicitly and separately from Data,
// mirroring feedback.go's graphQLThreadsResponse (#822: a 200 response with
// a non-empty errors[] must never be treated as empty-but-valid data).
// IsInMergeQueue/IsMergeQueueEnabled are pointer fields so a GraphQL field
// genuinely absent from the response is distinguishable from an explicit
// false (dispatch/config.go:102-123's absent-vs-false pattern).
type graphQLQueueResponse struct {
	Data struct {
		Repository struct {
			PullRequest struct {
				IsInMergeQueue      *bool  `json:"isInMergeQueue"`
				IsMergeQueueEnabled *bool  `json:"isMergeQueueEnabled"`
				HeadRefOID          string `json:"headRefOid"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// mergeQueueQuery also fetches headRefOid alongside the two merge-queue
// fields, but that field is not currently consumed for the pre-merge
// head-SHA re-check (Decision 9): recheckAutomergeInputs discards it and
// instead sources the freshest head SHA from its own `gh pr view` re-fetch
// (merge.go's own prView.HeadRefOID), so a push landing between the first
// evaluation and this point is still caught before `gh pr merge` is ever
// attempted.
const mergeQueueQuery = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){isInMergeQueue isMergeQueueEnabled headRefOid}}}`

// recheckAutomergeInputs re-fetches every input the pre-merge re-evaluation
// (Decision 9, #854) requires, immediately before runAutomerge ever issues
// `gh pr merge` on a first-pass Merge==true verdict: PR view (head SHA,
// mergeable, changed files), checks, closing-issue labels, base-ref policy,
// paginated comments/reviews (feeding a read-only new-feedback detection
// pass via detectNewFeedbackKeys), a full revalidation of every known
// feedback key -- pending AND previously addressed -- via revalidateFeedback
// (#885: this pass rereads authoritative GraphQL thread/review state fresh,
// it never carries the first pass's already-computed FeedbackHold/
// FeedbackDetail forward, since a thread or review can be reopened strictly
// between the two passes), merge-queue state, and -- as its final step, the
// smallest possible window before `gh pr merge` is ever issued -- a fresh
// re-read of the fleet automerge.enabled kill switch via loadFleetSwitch
// (#886), replacing the old "Enabled: first.Enabled" carry-over.
// AllowedMethods carries over unchanged from first (repo settings, not PR
// state, per the plan's Assumption). revalidateFeedback is read-only --
// unlike tick's own reconcileFeedback, it never mutates s, so a reopen
// discovered here holds this merge attempt but is only persisted (and its
// address-review relaunch dispatched) by the *next* tick's own reconcile
// pass; this preserves the "no double-bookkeeping in the recheck" property
// #854 already established (reconcileFeedback's own doc comment). A
// revalidateFeedback fetch failure is routed into FeedbackHold/FeedbackDetail
// below, not the err return path, so the second evaluateAutomerge pass still
// runs and produces its own fresh Conditions rather than the first pass's
// stale all-"yes" set. On any of the four earlier upstream-read failures
// (PR/checks/comments/reviews), returns a distinct reasonUpstream*Unreadable
// reason for the caller to hold under (reusing the existing
// one-decision-per-tick constants, per the plan's Assumption) rather than a
// new parallel constant set. On any kill-switch hold from loadFleetSwitch,
// returns a zero-value automergeInputs and one of the five
// reasonKillSwitch* reasons instead (#886) -- distinct from an upstream read
// failure, since every other read here already succeeded.
func recheckAutomergeInputs(s *State, first automergeInputs) (automergeInputs, string, error) {
	var pr prView
	if err := ghJSON(&pr, "pr", "view", s.PR, "--repo", s.Repo, "--json", prViewFields); err != nil {
		return automergeInputs{}, reasonUpstreamPRUnreadable, err
	}
	checks, err := fetchChecks(s.PR, s.Repo)
	if err != nil {
		return automergeInputs{}, reasonUpstreamChecksUnreadable, err
	}
	comments, commentsComplete, err := fetchPaged[comment]("repos/" + s.Repo + "/pulls/" + s.PR + "/comments")
	if err != nil {
		return automergeInputs{}, reasonUpstreamCommentsUnreadable, err
	}
	reviews, reviewsComplete, err := fetchPaged[review]("repos/" + s.Repo + "/pulls/" + s.PR + "/reviews")
	if err != nil {
		return automergeInputs{}, reasonUpstreamReviewsUnreadable, err
	}

	revalidatedPending, verdict := revalidateFeedback(s, reviews, reviewsComplete)

	in := automergeInputs{
		Checks:            checks,
		RepairPending:     s.RepairPending,
		FeedbackHold:      verdict.Hold,
		FeedbackDetail:    verdict.Detail,
		IsDraft:           pr.IsDraft,
		Mergeable:         pr.Mergeable,
		MergeStateStatus:  pr.MergeStateStatus,
		HeadRefOID:        pr.HeadRefOID,
		ChangedFiles:      pr.ChangedFiles,
		Additions:         pr.Additions,
		Deletions:         pr.Deletions,
		CommentsComplete:  commentsComplete,
		ReviewsComplete:   reviewsComplete,
		AllowedMethods:    first.AllowedMethods,
		AllowedMethodsErr: first.AllowedMethodsErr,
	}
	for _, i := range pr.ClosingIssuesReferences {
		in.ClosingIssues = append(in.ClosingIssues, i.Number)
	}
	for _, f := range pr.Files {
		in.Files = append(in.Files, f.Path)
	}
	if len(in.ClosingIssues) > 0 {
		in.IssueLabels, in.LabelsErr = fetchClosingIssueLabels(s.Repo, in.ClosingIssues)
	}

	newKeys, _ := detectNewFeedbackKeys(s, comments, reviews, s.LastCommentAt)
	in.PendingKeys = append(append([]string{}, revalidatedPending...), newKeys...)

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

	inQueue, enabled, _, queueErr := fetchMergeQueueState(s.Repo, s.PR)
	in.QueueInMergeQueue = inQueue
	in.QueueEnabled = enabled
	in.QueueErr = queueErr
	in.QueueProbed = true

	// The fleet kill switch's final re-read (#886, AC 5/6): the very last
	// step, after every other upstream read above, so the window between
	// confirming automerge is still enabled and actually issuing
	// `gh pr merge` is as small as possible. Any hold here returns a
	// zero-value automergeInputs -- every other field this function just
	// spent a full round of `gh` calls populating is discarded, matching the
	// existing reasonUpstream*Unreadable early-return shape above -- and the
	// caller (runAutomerge) never issues the merge.
	killSwitchEnabled, holdReason := loadFleetSwitch()
	if holdReason != "" {
		return automergeInputs{}, holdReason, nil
	}
	in.Enabled = killSwitchEnabled

	return in, "", nil
}

// fetchMergeQueueState probes pr's merge-queue state via `gh api graphql`
// (Decision 4, Q3): gh pr view exposes no queue fields on this gh version,
// so this is the only signal. headRefOID is the PR's current head commit as
// reported by this same query, but it is not currently used by
// recheckAutomergeInputs's pre-merge re-evaluation -- that function sources
// its freshest head-SHA signal from its own `gh pr view` re-fetch instead.
func fetchMergeQueueState(repo, pr string) (inQueue, enabled *bool, headRefOID string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return nil, nil, "", fmt.Errorf("malformed repo %q, want owner/name", repo)
	}
	stdout, stderr, err := execGh("api", "graphql",
		"-f", "query="+mergeQueueQuery,
		"-f", "owner="+owner,
		"-f", "name="+name,
		"-F", "number="+pr,
	)
	if err != nil {
		return nil, nil, "", fmt.Errorf("%s: %w", strings.TrimSpace(stderr), err)
	}
	var resp graphQLQueueResponse
	if err := json.Unmarshal([]byte(stdout), &resp); err != nil {
		return nil, nil, "", fmt.Errorf("decode merge-queue probe: %s", strings.TrimSpace(stdout))
	}
	if len(resp.Errors) > 0 {
		return nil, nil, "", errors.New(resp.Errors[0].Message)
	}
	pull := resp.Data.Repository.PullRequest
	return pull.IsInMergeQueue, pull.IsMergeQueueEnabled, pull.HeadRefOID, nil
}

// executeMerge issues `gh pr merge <pr> --repo <repo> --squash
// --match-head-commit <headSHA>` exactly once -- never --delete-branch,
// never --merge/--rebase (epic #661 Decision 4; #854 squash-only) -- then
// unconditionally performs exactly one post-merge refetch (Q5: one refetch,
// no polling), regardless of the merge command's own exit code (#886): a
// client-side timeout or transient network blip can make `gh pr merge`
// itself report failure even though the merge actually landed, so the
// refetch -- not the exit code -- is the sole source of truth. Five
// outcomes: (a) the refetch itself is unreadable -> reasonMergeVerifyUnreadable,
// never success, regardless of what the merge command reported; (b) the
// refetch reports state == MERGED with a headRefOid that is non-empty and
// byte-equal to headSHA -> confirmed success, regardless of the merge
// command's exit code (a nonzero exit here gets a diagnostic Detail noting
// the discrepancy, since it's otherwise silently lost); (c) the refetch
// reports state == MERGED but headRefOid is empty or does not equal headSHA
// (#897) -> reasonMergeHeadMismatch, never success, regardless of the merge
// command's exit code -- fail closed (watch/docs/error-handling.md's
// default-deny rule: an empty headRefOid is not proof of anything) and
// returns directly, so this never falls through to reasonMergeFailed below;
// (d) the refetch is readable, not MERGED, and the merge command exited
// nonzero -> reasonMergeFailed, with the merge command's own stderr (or
// stdout, if stderr is empty) as Detail; (e) the refetch is readable, not
// MERGED, and the merge command exited zero -> reasonMergeIndeterminate
// (Decision 6): the exit status alone is never proof. Returns whether the
// merge was confirmed, plus decision updated with the final
// Merge/Reason/Detail/FailureClass.
func executeMerge(s *State, headSHA string, decision automergeDecision) (bool, automergeDecision) {
	mergeStdout, mergeStderr, mergeErr := execGh("pr", "merge", s.PR, "--repo", s.Repo, "--squash", "--match-head-commit", headSHA)

	var fresh prView
	if verifyErr := ghJSON(&fresh, "pr", "view", s.PR, "--repo", s.Repo, "--json", prViewFields); verifyErr != nil {
		decision.Merge = false
		decision.Reason = reasonMergeVerifyUnreadable
		decision.Detail = sanitizeDetail(verifyErr.Error())
		decision.FailureClass = classifyGhFailure(verifyErr)
		return false, decision
	}

	if fresh.State == "MERGED" {
		if fresh.HeadRefOID == "" || fresh.HeadRefOID != headSHA {
			decision.Merge = false
			decision.Reason = reasonMergeHeadMismatch
			decision.FailureClass = ""
			detail := fmt.Sprintf("PR reports MERGED but the post-merge refetch's head commit does not match the pinned head commit: pinned %q, observed %q", headSHA, fresh.HeadRefOID)
			if mergeErr != nil {
				stderrOrStdout := strings.TrimSpace(mergeStderr)
				if stderrOrStdout == "" {
					stderrOrStdout = strings.TrimSpace(mergeStdout)
				}
				if stderrOrStdout != "" {
					detail = fmt.Sprintf("%s; gh pr merge exited nonzero: %s", detail, stderrOrStdout)
				}
			}
			decision.Detail = sanitizeDetail(detail)
			return false, decision
		}
		decision.Merge = true
		decision.Reason = ""
		decision.Detail = ""
		if mergeErr != nil {
			decision.Detail = sanitizeDetail(fmt.Sprintf("gh pr merge exited nonzero (%v) but PR is MERGED; treating as success", mergeErr))
		}
		return true, decision
	}

	if mergeErr != nil {
		decision.Merge = false
		decision.Reason = reasonMergeFailed
		detail := strings.TrimSpace(mergeStderr)
		if detail == "" {
			detail = strings.TrimSpace(mergeStdout)
		}
		decision.Detail = sanitizeDetail(detail)
		decision.FailureClass = classifyGhFailure(mergeErr)
		return false, decision
	}

	decision.Merge = false
	decision.Reason = reasonMergeIndeterminate
	decision.Detail = ""
	return false, decision
}
