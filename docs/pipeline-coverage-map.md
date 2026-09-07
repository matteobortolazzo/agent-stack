# Pipeline coverage map

This document is the committed source of truth tying every acceptance
criterion in the `#661` idempotent-refine/autonomous-chain split and the
repository activation that enables the completed chain
(`#876`-`#886`, `#912`-`#916`) — and every runtime-behavior claim restated by
this split's reconciled docs — to a named, currently-existing test. It is
enforced by a fail-closed sync check in `flow/scripts/run-checks.sh`
(the `--- 1.5. Coverage-map sync check ---` block), which runs as part of
both CI's `flow-test` job and this repo's flow `gateCommand`
(`docs/health-gates.md`).

## Token grammar

Every inline-code span (`` `...` ``) anywhere in this document is a token
the sync check parses:

- A token matching `^Test[A-Za-z0-9_]+$` is a **Go-test-name token** —
  checked against `func <name>(` in any `watch/**/*_test.go` file.
- A token ending in the suite-file suffix (and containing no `::`) is a
  **flow-suite-path token** — checked as an existing regular file, repo-root
  relative (e.g. `flow/tests/adversarial-chain.test.sh`).
- A token containing the suite-file suffix followed by a `::` separator is a
  **path-and-literal token** — the part before `::` is checked as an
  existing regular file, and the part after `::` (the literal) must appear
  verbatim, on a single physical source line, in that file, as in
  `flow/tests/adversarial-chain.test.sh::SCENARIO_COUNT=8` below. Every
  literal used in this document is an exact substring of an existing suite
  comment/variable, never a paraphrase, per
  `flow/docs/shell-scripting-gotchas.md`'s line-wrapping-safety rule.

The check fails closed on: a missing/unreadable map, zero tokens extracted
overall, zero Go-test tokens, or zero flow-suite tokens. It also runs a
**backward** (anti-rot) invariant scoped to the adversarial suites (every
`func Test*` in `watch/internal/dispatch/chain*_test.go` and both
`flow/tests/adversarial-chain.test.sh` /
`flow/tests/escalation-hardstop-matrix.test.sh` must appear somewhere in
this map), an **AC4** invariant (every suite named in
`## Adversarial suite bounds` must not appear in `run-checks.sh`'s
`EXCLUDE` allowlist), and a **registration** invariant (`flow-ci.yml`'s
`flow:` path filter must list this file's path and a `watch/**` test glob).

Map rot in the other direction (a non-adversarial test renamed without
updating this doc) is caught only by the forward check, on the next PR that
disturbs the row citing the renamed test — this is deliberate: a repo-wide
backward invariant over all 62 flow suites plus every watch package would
be noise, not signal.

## Acceptance-criterion coverage

### #876 — idempotent child/design-ticket creation

| AC | Claim | Test |
|---|---|---|
| 1 | Timeout after GitHub accepts a POST is recovered without creating a duplicate. | `flow/skills/refine/scripts/ensure-issue.test.sh::AC-7 (a):` |
| 2 | Crash after POST but before numeric-ID persistence is recovered by the exact creation marker. | `flow/skills/refine/scripts/ensure-issue.test.sh::crash between POST and number persistence recovers via the` |
| 3 | A known issue with a wrong title, body, labels, assignee, or milestone is repaired and reverified rather than recreated. | `flow/skills/refine/scripts/ensure-issue.test.sh::AC-7 (c):` |
| 4 | Zero, one, and multiple marker-match cases have distinct outcomes; multiple matches fail closed. | `flow/skills/refine/scripts/ensure-issue.test.sh::AC-7 (d):` |
| 5 | Retry after successful native-subissue linking does not relink incorrectly or create another child. | `flow/skills/refine/scripts/ensure-issue.test.sh::after the link failure must not create a second child.` |
| 6 | Restart after any creation/link/verification boundary resumes from durable checkpoint state. | `flow/skills/refine/scripts/ensure-issue.test.sh::AC1 (#913): real process kill mid-` |
| 7 | Behavior-level tests exercise actual producer payloads and fake GitHub responses for accepted-then-timeout, malformed response, mismatch repair, duplicate marker, and partial-link failure. | `flow/skills/refine/scripts/ensure-issue.test.sh::AC-7 (b):` |

### #877 — remote-confirmed lean autonomy grant

| AC | Claim | Test |
|---|---|---|
| 1 | Remote `lean` plus fleet grant and successful fetch authorizes planning; local-only `lean` does not. | `TestRunOnce_LeanRepoConfigPassesAutonomyGate` |
| 2 | Remote revocation to interactive is honored even when local main still contains lean after the last successful pass. | `TestProbeRepoAutonomy_RemoteRevokedToInteractive_LocalMainStillLean_DeniesAtRemoteRef` |
| 3 | Fetch failure holds planning/replanning distinctly and launches no process. | `TestRunOnce_FetchOutage_HoldsPlanningLaunchesNothing_OrdinaryPlannedStillDispatches` |
| 4 | Ordinary eligible implementation behavior under fetch failure is explicitly tested and documented. | `TestRunOnce_FetchOutage_HoldsPlanningLaunchesNothing_OrdinaryPlannedStillDispatches` |
| 5 | Local-ahead main cannot supply the autonomy grant; remote config remains authoritative. | `TestProbeRepoAutonomy_LocalAheadUnpushedLean_CannotGrant_RemoteRefDenies` |
| 6 | Dry-run and real pass use the same fetched object and produce the same authorization/staleness decision. | `TestProbeRepoAutonomies_DryRunAndRealPassAgreeUsingSameFreshRef` |
| 7 | Mixed-fleet tests cover remote/local combinations, fetch outage, forced stale refs, malformed config, branch changes, and timeout. | `TestRunOnce_MixedFleet_RemoteLocalCombinationsAndFailureModes` |

### #878 — proposal confirmation is the first mutation

| AC | Claim | Test |
|---|---|---|
| 1 | No GitHub write occurs before the complete proposal and manifest are explicitly confirmed. | `flow/tests/refine-write-order.test.sh::Any prefix of the pre-gate command inventory: zero` |
| 2 | Declining leaves title, body, labels, assignees, milestone, and native subissues state-for-state unchanged. | `flow/tests/refine-write-order.test.sh::Decline: zero gh/git calls at all in the declined branch` |
| 3 | Confirming performs ownership claim and `Working` transition only after confirmation and before subsequent writes. | `flow/tests/refine-write-order.test.sh::canonical section -- both docs carry it` |
| 4 | A post-confirmation ownership conflict stops without editing the proposal/body or creating children. | `flow/tests/refine-write-order.test.sh::re-verify exclusive ownership` |
| 5 | Parent metadata drift affecting automation/browser/visual authorization stops for reconfirmation; cosmetic inherited-label drift is handled explicitly and tested. | `flow/tests/refine-write-order.test.sh::stop and ask for a fresh confirmation` |
| 6 | Claude and Codex procedures describe one non-contradictory mutation boundary. | `flow/tests/refine-write-order.test.sh::refine/codex.md Write order parity block` |
| 7 | Behavioral tests assert the exact write order and prove zero writes for decline and pre-confirmation interruption. | `flow/tests/refine-write-order.test.sh::Non-vacuity self-test: a deliberately broken COPY of the declined-branch` |

### #879 — parent-audit close/hold retry idempotency

| AC | Claim | Test |
|---|---|---|
| 1 | `close -> hold` retry removes every effective parent-closing reference before push/recovery continues. | `flow/tests/parent-close-reconcile.test.sh::Scenario 1: close -> hold` |
| 2 | `hold -> close` retry adds and verifies the parent-closing reference. | `flow/tests/parent-close-reconcile.test.sh::Scenario 2: hold -> close` |
| 3 | Unchanged verdict retries remain idempotent and do not create empty commits or duplicate PRs. | `flow/tests/parent-close-reconcile.test.sh::Scenario 3: unchanged verdict, both directions` |
| 4 | A newly added/open sibling forces `hold` even when plan front matter says `isLastChild: true`. | `flow/tests/parent-close-reconcile.test.sh::the shape that forces hold even when isLastChild is true` |
| 5 | An unreadable audit, commit message, PR body, or GitHub closing-reference read fails closed. | `flow/tests/parent-close-reconcile.test.sh::Failure injection: an unreadable closing-refs read must be` |
| 6 | Tests cover failures after commit, push, PR creation, label transition, and parent cascade for both verdict directions. | `flow/tests/parent-close-reconcile.test.sh::Scenario 6: failure-after-X re-entry converges, no duplicates` |

### #880 — escalation/resume crash recovery

| AC | Claim | Test |
|---|---|---|
| 1 | Re-escalation persists and verifies its nonce before any comment POST. | `flow/tests/resume-abort-contract.test.sh::persist-nonce precedes the POST literal, which precedes the` |
| 2 | Crash after nonce persistence, POST acceptance, readback, comment-ID persistence, label mutation, planner delegation, candidate-plan assembly, and final replacement is recoverable without duplicate active anchors. | `flow/tests/resume-abort-contract.test.sh::bidirectional one-to-one coverage: every` |
| 3 | Every hard stop after `Working` immediately restores `Input Needed` and a valid awaiting-input draft before stopping. | `flow/tests/resume-abort-contract.test.sh::## Restore Awaiting-Input State` |
| 4 | A malformed candidate final plan cannot destroy or replace the last valid awaiting-input draft. | `flow/tests/resume-abort-contract.test.sh::Candidate-plan path: assembled and validated before ever replacing the` |
| 5 | The exact stored comment ID and nonce remain the only active anchor; replies to orphan/old anchors never resume planning. | `flow/tests/resume-abort-contract.test.sh::Repair Escalation Anchor case (iii): mint+persist a fresh nonce, so` |
| 6 | Fresh resumes avoid re-exploration; stale/unknown resumes re-plan with human answers fixed. | `TestResumeCrossLane_UnknownStaleRuleStatedInPhase1PlanMatchesDraftFreshnessValues` |
| 7 | Cross-lane behavioral tests use the real producer front matter/comment payload and the real dispatch consumer parser. | `TestResumeCrossLane_ProducerTemplateThroughRealClassifyComments` |

### #881 — open-PR pagination completeness gate

| AC | Claim | Test |
|---|---|---|
| 1 | A linked PR beyond the first 200 open PRs still blocks duplicate dispatch. | `TestOpenPRInventory_LinkedPRBeyondFirstPageBound_StillDetected` |
| 2 | Pagination truncation, cap exhaustion, malformed page, timeout, and mid-pagination failure gate with distinct reasons. | `TestOpenPRInventory_CapExhaustion_PageBoundReachedWithHasNextPageTrue` |
| 3 | Empty and small complete inventories preserve current eligible behavior. | `TestOpenPRInventory_EmptyInventory_OneCallComplete` |
| 4 | Multiple PRs closing one issue and one PR closing multiple issues are mapped correctly. | `TestOpenPRInventory_MultiMapping_SplitAcrossPageBoundary` |
| 5 | Dry-run and real pass render/consume the same decision. | `TestRunOnce_DryRunAndRealPassProduceIdenticalOpenPRGateReason` |
| 6 | Tests assert bounded call counts and prove no implementation/planning process is spawned on incomplete inventory. | `TestApplyDispatchOrdinaryPlannedPickup_IncompleteOpenPRInventory_NeverSpawnsOrClaims` |

### #882 — write-permission-gated resume authorization

| AC | Claim | Test |
|---|---|---|
| 1 | Current `push`, `maintain`, and `admin` users can answer and resume. | `TestFetchWritePermission_GrantedForAdminAndWrite` |
| 2 | Organization members without repository write permission cannot resume. | `TestFetchWritePermission_DeniedForReadTriageNone` |
| 3 | Read/triage collaborators, former collaborators, outside commenters, bots, and apps cannot resume. | `TestClassifyComments_AllCandidatesDenied_Unauthorized` |
| 4 | Permission revocation between comments/passes is honored on the next probe. | `TestResolveAnswerProbes_PermissionCacheDoesNotPersistAcrossPasses` |
| 5 | Permission API error, timeout, truncation, malformed JSON, missing field, and future unknown values fail closed with distinct bounded reasons. | `TestFetchWritePermission_UnknownValueForFutureString` |
| 6 | Manual and automatic resume use the same producer payload and authorization contract. | `TestResumeCrossLane_ProducerTemplateThroughRealClassifyComments` |
| 7 | Tests prove no label mutation or process launch occurs for every unauthorized/unknown path. | `TestApplyDispatchResumeUnauthorizedProbe_NeverSpawnsOrClaims` |

### #883 — reconciliation state crash-durability

| AC | Claim | Test |
|---|---|---|
| 1 | A crash during save leaves either the previous complete state or the new complete state, never truncated final JSON. | `TestStateStoreSaveInjectedWriteFailureLeavesPreviousStateIntact` |
| 2 | Missing state initializes cleanly; unreadable, malformed, unknown-schema, or integrity-invalid state stops all GitHub recovery mutations. | `TestApplyReconcileAbortsOnSentinelLoadErrorZeroMutatorCalls` |
| 3 | A load failure is not overwritten by a later save in the same pass. | `TestApplyReconcileAbortsOnSentinelLoadErrorZeroSaveCalls` |
| 4 | Observation timestamps and apply-failure counters survive restart exactly. | `TestStateStoreSaveThenLoadRoundTripPreservesObservationsAndApplyFailures` |
| 5 | Temp files are scoped, permissioned, and cleaned/recovered without following unsafe symlinks. | `TestStateStoreSaveDoesNotFollowPlantedSymlinkAtLegacyTempName` |
| 6 | Tests inject partial write, fsync, rename, permission, decode, and migration failures and assert zero mutator calls on every unsafe path. | `TestStateStoreSaveInjectedSyncFailureLeavesPreviousStateIntact` |

### #884 — plan-inventory fail-closed ambiguity handling

| AC | Claim | Test |
|---|---|---|
| 1 | Missing `.plans` and an existing empty `.plans` directory are verified absence; unreadable directory is a distinct hold. | `TestRead_MissingPlansDir_ReturnsAbsent` |
| 2 | Filename/front-matter ticket mismatch is held and reported without dispatch. | `TestSelect_FilenameFrontMatterMismatch_BothKeysHeldWithSameReason` |
| 3 | Two or more healthy plans claiming one ticket are held as ambiguous regardless of sort order. | `TestSelect_TwoHealthyDuplicateClaims_AmbiguousRegardlessOfSortOrder` |
| 4 | Broken-plus-healthy duplicates do not silently become healthy; the full inventory verdict is explicit. | `TestSelect_BrokenPlusHealthyDuplicate_HealthyFirst_AmbiguousNeverHealthy` |
| 5 | Manual `plan-check`, dispatch, resume probing, and reconciliation select the same plan or the same failure reason. | `TestParity_IdentityMismatch_BothSidesHoldSameFailureClass` |
| 6 | Permission errors, partial writes, symlink/path anomalies, duplicate files, malformed front matter, and staleness failures have explicit behavior tests. | `TestRead_SymlinkEntry_ClassifiesPathAnomaly_NeverRead` |
| 7 | Only proven complete absence can launch a `Refined` planning pickup. | `TestSelect_IncompleteInventory_AlwaysBroken` |

### #885 — final-evaluation feedback-state re-read

| AC | Claim | Test |
|---|---|---|
| 1 | Resolved-then-reopened inline threads hold the final merge and return to pending. | `TestClassifyFeedbackReopensAddressedCommentKey` |
| 2 | Reopened threads are detected even when their comment ID already exists in `AddressedKeys`. | `TestReconcileFeedbackGraphQLGateWidenedToAddressedCommentKeys` |
| 3 | The final pre-merge evaluation rereads authoritative thread state rather than carrying the first-pass verdict. | `TestTickPreMergeRecheckRevalidatesFeedbackReopenedSinceFirstPass` |
| 4 | Same-timestamp `APPROVED`/`CHANGES_REQUESTED` sequences select the higher review ID regardless of response ordering. | `TestLatestEffectiveReviewSameTimestampTieBreakHigherIDWins` |
| 5 | Mixed reopened, resolved, new, deleted, and unknown feedback states fail closed correctly. | `TestClassifyFeedbackMixedState` |
| 6 | Supervisor restart preserves correct resolution-episode and launch-dedup behavior. | `TestTickAfterSchema2RestartDoesNotRelaunchAlreadyLaunchedReopen` |
| 7 | Exact tests prove no merge command is issued for every reopened/unknown/truncated path. | `TestTickPreMergeRecheckHoldsOnTruncatedThreadRead` |

### #886 — merge-command evidence supervisor

| AC | Claim | Test |
|---|---|---|
| 1 | Non-check `gh` commands with nonzero exit and valid JSON fail closed. | `TestExecGh_NonzeroExitReturnsGhExitError` |
| 2 | `gh pr checks` accepts only the documented pending exit with complete valid JSON; other failures remain errors. | `TestClassifyGhFailure_PrecedenceMatrix` |
| 3 | Nonzero merge exit followed by `MERGED` records confirmed success and allows normal lifecycle reconciliation. | `TestTickAutomergeMergeNonzeroExitButRefetchConfirmsMerged` |
| 4 | Any merge attempt followed by unreadable or readable non-merged state never records success. | `TestTickAutomergeMergeNonzeroExitThenRefetchUnreadableNeverSucceeds` |
| 5 | An enabled-to-disabled fleet switch change between evaluations prevents the merge command. | `TestTickAutomergeRecheckKillSwitchHoldsMatrix` |
| 6 | Missing/malformed/unreadable final kill-switch config holds distinctly. | `TestTickAutomergeRecheckKillSwitchExplicitDisableRendersEnabledNo` |
| 7 | Timeout, cancellation, truncation, and stderr/stdout separation tests prove the supervisor cannot hang or silently accept partial evidence. | `TestExecuteMerge_RealTimeoutOnMergeCallClassifiesFailureClassTimeout` |

### #912 — golden fixture and adversarial-chain orchestration (1/5 of the split)

| AC | Claim | Test |
|---|---|---|
| 1 | A committed golden fixture encodes the post-refinement issue graph, and the flow driver and the watch chain suite each assert against it independently. | `TestGoldenGraph_SchemaInvariants` |
| 2 | `flow/tests/adversarial-chain/lib.sh` orchestrates the existing extractors/oracles as stage drivers, producing one ordered recorded `gh`/`git` stream across refine to escalation/resume to parent-close. | `flow/tests/adversarial-chain.test.sh::AC2: one ordered recorded gh/git stream spans refine ->` |
| 3 | Declined refinement and every pre-confirmation interruption produce zero write-classified commands in the recorded stream. | `flow/tests/adversarial-chain.test.sh::AC3: declined refinement and every pre-confirmation` |
| 4 | Parent-audit `close -> hold` and `hold -> close` retries reconcile commit message, PR body, and GitHub closing references, and an open sibling forces hold. | `flow/tests/adversarial-chain.test.sh::AC4: parent-audit close<->hold retries reconcile commit` |

### #913 — `ensure-issue.sh` real-process crash injection (2/5 of the split)

| AC | Claim | Test |
|---|---|---|
| 1 | `ensure-issue.sh` is crash-tested by real process kill between subcommands against the real on-disk checkpoint. | `flow/skills/refine/scripts/ensure-issue.test.sh::AC1 (#913): real process kill mid-` |
| 2 | Every `HS-*` row in `phase-1-plan.md` implementing one of the seven named boundary kinds is exercised across all three sections by truncate-and-re-enter replay. | `flow/tests/resume-abort-contract.test.sh::Strengthened bidirectional check: verifying an HS-* ID token appears` |

### #914 — watch chain suite full positive scenario (3/5 of the split)

| AC | Claim | Test |
|---|---|---|
| 1 | The watch chain suite runs the full positive scenario against real production code with fake external boundaries only. | `TestAutonomousChain_PositiveEndToEnd` |
| 2 | Restart/reload is exercised for every persisted state type; authorization, retry budgets, and active identities survive unchanged. | `TestAutonomousChain_ReconcileStateSurvivesReExec` |
| 3 | The chain fake models ambiguous success, truncation, timeout, pagination, and state change between first and final evaluation, with a fixed scenario count. | `TestChainFake_ScenarioInventoryIsFixed` |

### #915 — dispatch-boundary and babysit-boundary negative variants (4/5 and 5/5 of the split)

| AC | Claim | Test |
|---|---|---|
| 1 | Variants (1), (2), (3), and (5) — the babysit evaluation-boundary group — each have their own named test. | `TestAutonomousChain_ResolvedFeedbackReopenedBetweenEvaluationsHoldsTheMerge` |
| 2 | Variant (4) is proven by two named tests, including a companion test in `package babysit` producing a genuine timeout classification through the real `execGh`. | `TestExecuteMerge_RealTimeoutOnMergeCallClassifiesFailureClassTimeout` |
| 3 | Variants (6), (7), and (8) each have their own named test per distinct stop reason. | `TestAutonomousChain_UnreadablePlanDirectoryHoldsEveryTicketInTheRepo` |
| 4 | Variants (9) and (10) each have their own named test per distinct stop reason. | `TestAutonomousChain_CorruptReconcileStateHoldsWithoutMutatingOrOverwriting` |
| 5 | Any new fault-injection capability is expressed on the existing mechanism, and `chainAmbiguityScenarios`/`chainScenarioCount` are updated so the scenario-inventory test stays green. | `TestChainFake_ScenarioInventoryIsFixed` |
| 6 | `make test` in `watch/` passes with the complete negative-variant suite present, and each variant's named test carries a doc comment recording the facts this coverage map must cite. | `TestAutonomousChain_MergeAcceptedButClientSeesFailureIsSettledByPostMergeRefetch` |

### #916 — coverage map, doc reconciliation, and the #887 split close (5/5 of the split)

| AC | Claim | Test |
|---|---|---|
| 1 | A committed coverage-map doc maps every acceptance criterion in `#876`-`#886`/`#912`-`#916` and every reconciled runtime-behavior claim to a named test; unmapped ACs are explicit followup rows. | `flow/scripts/run-checks.test.sh::Case 17: Happy path -- a well-formed fixture (valid map, all tokens` |
| 2 | A single bash check in `run-checks.sh` scans flow and watch test sources and fails on a mapped AC/doc claim naming no existing test or a disappeared test; the map path and `watch/**` test globs are registered in `flow-ci.yml`'s `flow` path filter. | `flow/scripts/run-checks.test.sh::Case 9: Missing coverage map file.` |
| 3 | `watch/README.md`, `docs/orchestration.md`, `flow/docs/pipeline-safety.md`, `.cenci/config.json` examples, and the lifecycle-label descriptions state the final authority model. | `TestProbeRepoAutonomy_RemoteRevokedToInteractive_LocalMainStillLean_DeniesAtRemoteRef`, `TestFetchWritePermission_GrantedForAdminAndWrite`, `flow/tests/refine-write-order.test.sh::re-verify exclusive ownership`, `TestTickAutomergeSquashOnlyHoldsOnNonSquashPolicy` |
| 4 | The coverage map records each adversarial suite's bounded scenario count and runtime ceiling, and no adversarial suite appears in `run-checks.sh`'s `EXCLUDE` allowlist. | `flow/scripts/run-checks.test.sh::Case 15: An adversarial suite injected into EXCLUDE in the fixture's` |
| 5 | A final verification run passes `bash scripts/run-checks.sh` in `flow/` and complete `make test` in `watch/` together. | `flow/scripts/run-checks.test.sh`, `TestChainFake_ScenarioInventoryIsFixed` |
| 6 | `#887` is closed by this child's PR after every sibling closes and the parent-close audit is current; the PR description explicitly flags `#661` as requiring manual/follow-up closure. | none — procedural close-audit criterion, not test-observable; see Followups |

### #661 — repository activation

| AC | Claim | Test |
|---|---|---|
| 1 | This repository states its planning autonomy explicitly (one of the two documented values) while missing or malformed authorization remains fail-closed in the runtime implemented by the split. | `flow/tests/repo-activation-config.test.sh`, `TestRunOnce_LeanRepoConfigPassesAutonomyGate` |
| 2 | Flow, watch, and sandbox each have positive bounded squash policies, while root-owned files remain ineligible without a top-level policy. | `flow/tests/repo-activation-config.test.sh`, `TestRepositoryAutomergePolicyDogfood` |
| 3 | Representative control-plane, runtime, and supply-chain paths are protected through the production matcher; representative docs and tests remain eligible. | `TestRepositoryAutomergePolicyDogfood` |
| 4 | Config-only changes trigger both the authored-config contract and the production policy-resolver dogfood test in CI. | `flow/tests/repo-activation-config.test.sh` |

## Doc-claim coverage

Per Q1/Q2's answer, scope is AC3's four authority-model claim families plus
any other runtime claim this PR's own reconciliation edits introduce. Each
row names the test that proves the underlying runtime behavior the doc
prose restates; per Q2, three of the four surfaces already stated the
model correctly and needed no prose change (verification-only), and
`docs/orchestration.md` needed one gap-fill (see the Implementation notes
below).

| Claim | Doc surface(s) | Test |
|---|---|---|
| Remote-confirmed lean config is authoritative over local-only lean. | `watch/README.md`, `docs/orchestration.md`, `flow/skills/configure/SKILL.md` | `TestProbeRepoAutonomy_RemoteRevokedToInteractive_LocalMainStillLean_DeniesAtRemoteRef` |
| Current write permission (not the commenter's permission at plan time) authorizes resume answers. | `watch/README.md`, `docs/orchestration.md` | `TestResolveAnswerProbes_PermissionCacheDoesNotPersistAcrossPasses` |
| Refinement's proposal confirmation is the true first mutation boundary (pre-mutation confirmation). | `flow/docs/pipeline-safety.md`, `docs/orchestration.md` | `flow/tests/refine-write-order.test.sh::re-verify exclusive ownership` |
| Merges are squash-only, and the merge evaluation always uses current (immediately-before-merge) feedback state, not a stale first-pass verdict. | `docs/orchestration.md` | `TestTickAutomergeSquashOnlyHoldsOnNonSquashPolicy`, `TestTickPreMergeRecheckRevalidatesFeedbackReopenedSinceFirstPass` |

## Adversarial suite bounds

| Suite | Scenario count | Runtime ceiling | EXCLUDE status |
|---|---|---|---|
| `flow/tests/adversarial-chain.test.sh::SCENARIO_COUNT=8` | 8 (pinned, ~1.0s measured) | 30s | not excluded |
| `flow/tests/escalation-hardstop-matrix.test.sh::HARDSTOP_SCENARIO_COUNT=21` | 21 (pinned, ~1.2s measured) | 30s | not excluded |
| `watch/internal/dispatch/chain*_test.go::TestChainFake_ScenarioInventoryIsFixed` | fixed inventory, pinned in-suite (~5.5s measured) | 60s | not excluded |

`run-checks.sh`'s `EXCLUDE` array is confirmed empty (`EXCLUDE=()`); every
suite above is discovered and executed on every run.

### Complete `watch/internal/dispatch/chain*_test.go` roster

The sync check's backward invariant requires every `func Test*` in
`watch/internal/dispatch/chain*_test.go` to appear somewhere in this map.
The scenario tests already cited by name in the AC tables above are not
repeated below; every remaining named test in those files is rostered here
so none can silently drop out of the map on a future rename.

| Test | Coverage note |
|---|---|
| `TestAutonomousChain_CancelledCIStopsBeforeMerge` | Positive-chain negative branch: cancelled CI status halts before merge. |
| `TestAutonomousChain_UnresolvedReviewStopsBeforeMerge` | Positive-chain negative branch: an unresolved review halts before merge. |
| `TestAutonomousChain_NonLeanRepoConfigStopsBeforeUnattendedPlanning` | Interactive-config repo never launches unattended planning. |
| `TestAutonomousChain_StaleResumeRoutesToReplanAndCannotBeStampedFreshByAnAnswer` | A stale resume routes to replan; an answer alone cannot re-stamp it fresh. |
| `TestAutonomousChain_FailedMainSyncStopsDependentPickupAtRepoGate` | A failed main-sync result gates every dependent pickup at the repo level. |
| `TestAutonomousChain_PermissionDeniedAnswererNeverResumes` | The already-covered organization-member/read-collaborator authorization clause (#915 AC3's cited precedent). |
| `TestAutonomousChain_PipelineStageStateSurvivesReload` | Persisted pipeline-stage state survives an in-process reload (#914 AC2). |
| `TestAutonomousChain_BabysitDecisionStateSurvivesReload` | Persisted babysit-decision state survives an in-process reload (#914 AC2). |
| `TestAutonomousChain_DuplicatePlanClaimsHoldTheTicket` | #915 variant (7): duplicate healthy plan claims hold rather than dispatch. |
| `TestAutonomousChain_PlanIdentityMismatchHoldsBothClaimedTickets` | #915 variant (7): filename/front-matter identity mismatch holds both claimed tickets. |
| `TestAutonomousChain_LinkedPRBeyondFirstTwoHundredResultsStillPreventsDuplicateDispatch` | #915 variant (8): pagination beyond the first 200 results still blocks duplicate dispatch. |
| `TestAutonomousChain_OpenPRPaginationCapExhaustedHoldsPickup` | #915 variant (8): an incomplete pagination verdict holds pickup. |
| `TestAutonomousChain_PermissionProbeFetchFailureNeverResumes` | #915 variant (10): a permission-probe fetch failure never resumes. |
| `TestAutonomousChain_LocalOnlyLeanGrantNeverAuthorizesPlanning` | #915 variant (10): a local-only (unpushed) lean grant never authorizes planning. |
| `TestAutonomousChain_RemotelyRevokedLeanGrantStopsPlanning` | #915 variant (10): a remotely revoked lean grant stops planning. |
| `TestAutonomousChain_FleetKillSwitchDisabledBetweenEvaluationsIsDistinctFromFirstPassDisabled` | #915 variant (5): a fleet automerge kill-switch flip between evaluations renders a distinct reason from first-pass-disabled. |
| `TestAutonomousChain_SameTimestampReviewTieBreakDecidesReopenByReviewID` | #915 variant (2): same-timestamp review histories resolve deterministically by review ID. |
| `TestAutonomousChain_UpstreamPRReadNonzeroExitWithParseableJSONStillFails` | #915 variant (3): a non-check `gh` nonzero exit with parseable JSON remains a failure. |
| `TestChainFake_AmbiguousSuccess` | Chain-fake self-test: models an ambiguous-success (server accepted, client saw a nonzero exit) route. |
| `TestChainFake_Indeterminate` | Chain-fake self-test: models an indeterminate evidence route. |
| `TestChainFake_Truncation` | Chain-fake self-test: models a bounded-output truncation route. |
| `TestChainFake_Timeout` | Chain-fake self-test: models a deadline-exceeded route. |
| `TestChainFake_PaginationHasNextPage` | Chain-fake self-test: models an incomplete-pagination `hasNextPage` route. |
| `TestChainFake_PaginationEndCursorMalformed` | Chain-fake self-test: models a malformed pagination cursor route. |
| `TestChainFake_PaginationTotalCountExceedsCap` | Chain-fake self-test: models a pagination cap-exhaustion route. |
| `TestChainFake_PaginationNestedTruncation` | Chain-fake self-test: models nested (closing-issues) pagination truncation. |
| `TestChainFake_StateChangeBetweenEvaluations` | Chain-fake self-test: models GitHub-side state changing between first and final evaluation. |
| `TestChainGhFakeHelper` | Chain-fake harness self-test for the shared `ghWorld` fake `gh` boundary. |
| `TestChainReconcileHelper` | Chain-fake harness self-test for the shared reconciliation-state helper. |
| `TestGoldenGraph_RoundTripsThroughCollectAndDecide` | #912 AC1: the golden fixture round-trips through `CollectTickets`/`Decide` unchanged. |
| `TestGoldenGraph_PlanFrontMatterRoundTripsThroughCheckPlan` | #912 AC1: the golden fixture's plan front matter round-trips through `CheckPlan` unchanged. |
| `TestTruncateReconcileStateProducesDecodeError` | Reconciliation-state fault injection: a truncated state file produces a decode error, never a silent empty state. |

## Followups

- **#916 AC6** ("#887 is closed by this child's PR after every sibling
  closes and the parent-close audit is current") is a procedural,
  process-level criterion evaluated by Phase 9's audit against this ticket's
  `parentId: 887` front matter and the four siblings' closed state — it has
  no code-level runtime behavior a named test could assert, so it is listed
  here rather than in the AC-coverage table's `Test` column.
