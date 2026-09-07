# The autonomous loop

> Refine a ticket, walk away, review the merged PR.

cenci's default posture is human-gated: you approve the refined ticket, you approve
the plan, you approve the merge. Five opt-in switches remove those gates one at a
time, up to a loop that runs **refine → plan → implement → PR → merge → next ticket**
with no human touch between refinement and merge.

This page is the map: what each switch buys, how to turn them on, what still stops
the machine, and how to read a decision when it doesn't do what you expected. The
exhaustive reference for each piece lives elsewhere — see
[Where the details live](#where-the-details-live) at the bottom.

## The five switches

Nothing here is on by default, and no switch implies another. Each one is
independently reversible.

| # | Switch | Where | Default | What it removes |
|---|---|---|---|---|
| 1 | `planning.autonomy: "lean"` | repo `.cenci/config.json` (committed) | `"interactive"` | The **plan-review gate**. A plan with no escalations approves itself and implementation continues in the same session. |
| 2 | `dispatch.planRefined: true` | `~/.config/cenci/config.json` | `false` | The **manual planning launch**. `cenci dispatch` starts planning sessions for `Refined` tickets and re-plans stale plans by itself. |
| 3 | `automerge` policy block | repo `.cenci/config.json` (committed) | absent = deny | Nothing on its own — it *defines* the risk envelope automerge is allowed to act inside. |
| 4 | `automerge.enabled: true` | `~/.config/cenci/config.json` | `false` | The **merge gate**. `cenci babysit` merges the PR itself once every condition holds. |
| 5 | `planning.attended: true` | `~/.config/cenci/config.json` | `false` | The **override on switch 1, per machine**, for when a human is at the keyboard right now and could just answer a clarifying question. `cenci dispatch` stops picking lean repos up for unattended planning here, and a planning session you launch yourself in a `"lean"` repo plans *interactively* — it asks you instead of posting the question to the ticket, and stops for plan review instead of self-approving. |

Switches 1 and 3 are per-repo and committed, so the repo decides its own autonomy.
Switches 2 and 4 are fleet-wide kill switches on your machine: they can only ever
*permit* what a repo already opted into, never grant it. Turning on `planRefined`
fleet-wide does nothing to a repo that hasn't committed `planning.autonomy: "lean"`.
Switch 5 is fleet-wide too, but runs the other direction: it can only ever *narrow*
what a repo already opted into (turning `"lean"` into a denial on this machine),
never grant lean to a repo that hasn't committed it, and never mask a distinct
reason a repo was already denied for (missing/malformed config, an unreadable
probe, an unconfirmed fetch each keep their own reason). It narrows in two places
for the same reason — dispatch stops starting unattended planning sessions here, and
a planning session that does run treats the repo as `"interactive"`; see
[The attended override](#the-attended-override) below.

## What the loop looks like

```
  ┌─ you: /cenci:refine 42 ──────────────────────────────────────┐
  │   ticket scoped, acceptance criteria written                 │
  │   Confirmation Gate → you grant (or withhold) automerge:ok   │
  └──────────────────────────────┬───────────────────────────────┘
                                 │  label: Refined
                                 ▼
             cenci dispatch  ──── planning pickup ────▶  plan written
              (switch 2)                                 (switch 1:
                                 │                        self-approved)
                                 │  label: Planned + Working
                                 ▼
                         implementation runs ───▶ tests, review agents, PR
                                 │
                                 │  label: In Review
                                 ▼
                        cenci babysit supervises
                        CI green? feedback clear?
                                 │
                                 │  switches 3 + 4 + automerge:ok
                                 ▼
                              squash merge ───▶ label: Implemented
                                 │
                                 │  merged PR shifts shared files
                                 ▼
                    the next ticket's plan goes stale ───▶ auto re-plan
                                 │                          (switch 2)
                                 └────────▶ and around again
```

The re-plan step is what makes it a *loop* rather than a one-shot: after a dependency
merges, a sibling plan written against the old tree is detected as stale and
re-planned automatically instead of being skipped as unusable.

Two off-ramps exist and both are normal:

- **`Input Needed`** — planning hit one of five escalation classes (security-sensitive,
  destructive/irreversible, contradicts the refined ticket, genuine product ambiguity,
  scope blowup). It writes a draft plan, posts the question on the ticket, and stops
  — unless attended mode is on for this machine, in which case planning asks you directly instead
  (see [The attended override](#the-attended-override)).
  The posted question opens with a cenci banner telling you that replying on the ticket
  is what resumes the run. Answer on the ticket and dispatch resumes it on its next pass
  — no manual re-run.
- **A held merge** — babysit logs exactly why and retries next tick. Merge by hand any
  time; babysit never fights you for it.

## Quick start

### 0. Get dispatch working manually first

Autonomy is an amplifier, not a starting point. Before enabling anything below,
confirm an ordinary dispatch pickup works: enroll the repo, leave the loop off, and
run one pass by hand against a ticket that already has an approved plan.

```bash
cenci dispatch enroll                # from inside the repo
cenci dispatch status
cenci dispatch                       # one-off pass, fleet-wide
```

`enroll` detects the repo and directory but cannot guess where its windows should
spawn: set that repo's `repos[].session` to an existing tmux session name in
`~/.config/cenci/config.json` (enroll prints a reminder). A repo with no session, or
one naming a session that doesn't exist, is skipped for the whole pass.

If a `Planned` ticket still isn't picked up, fix that first — every switch below runs
through the same gate chain (assignee, dependencies, capacity, budget, quiet hours).

### 1. Let plans approve themselves

In the repo's committed `.cenci/config.json`:

```json
{
  "planning": { "autonomy": "lean" }
}
```

Any value other than the exact string `"lean"` — including a missing block — means
`"interactive"`, unchanged behavior.

`/cenci:configure` offers `planning.autonomy` when the key is absent, defaulting to
`interactive`.

**Commit and push this to `main`.** Dispatch reads it from
`refs/remotes/origin/main` after a successful `git fetch`, never from your working
tree and never from local `HEAD`. An unpushed local edit grants nothing; a revocation
pushed to `origin/main` takes effect on the next pass.

A plan approved this way says so. Its front matter carries `approval: lean`, and the
plan comment posted to the ticket opens with the auto-approved banner instead of the
ordinary one:

```markdown
> 🤖 **cenci** — implementation plan posted by `/cenci:implement` (planning — auto-approved, no human review).
```

Two other paths reach a plan without human review and are labelled distinctly:
`approval: trivial` (triage judged the ticket trivial and skipped planning entirely)
and `approval: lean-resumed` (planning escalated, you answered the questions on the
ticket, and the plan those answers produced was never shown to you). `approval: human`
is the ordinary path where you read the plan and launched the run. Plans written before
this key existed carry none — that means unrecorded, not unapproved.

#### The attended override

`planning.autonomy: "lean"` is a property of the repo, not of whoever is sitting in
front of it. When you launch `/cenci:implement` yourself, being asked a question and
answering it in one turn beats having it posted to the ticket and picked up a pass
later. `cenci planning attended on` (switch 5) says so, fleet-wide on this machine:

```bash
cenci planning attended on             # or: off
cenci planning attended status --json  # {"attended":true,...}
```

With it on, a planning session in a `"lean"` repo behaves as `"interactive"`:

- a clarifying question is asked directly instead of being posted to the ticket — no
  `Input Needed` label, no `awaiting-input` draft, and no waiting for the next dispatch pass;
- a plan with no escalations is **not** self-approved: it is saved, the session stops for
  you to read it, and its front matter records `approval: human`, not `approval: lean`.

This is an override, not a sixth switch: it only ever suspends switch 1, never grants it.
An `"interactive"` repo is unaffected — attended mode gives it nothing it did not already
have. The trivial fast path (which never consults autonomy) and a draft resumed after an
earlier unattended escalation (still `approval: lean-resumed`) are unaffected too.

**In the sandbox.** The host's `~/.config/cenci/config.json` is invisible inside a `cenci
sandbox` container, so the flag is forwarded at exec time as `CENCI_ATTENDED=1` or
`CENCI_ATTENDED=0` — always explicitly, never "unset means off". Toggling it on the host
takes effect on the next `cenci open`, with no container rebuild. Sessions `cenci
dispatch` launches are pinned to `CENCI_ATTENDED=0` whatever the host flag says: a
detached tmux window must never stall on a question nobody is there to answer.

**The resolution order** a planning session follows, first match wins:

1. `CENCI_ATTENDED=1` → attended; plan interactively.
2. `CENCI_ATTENDED=0` → use the repo's `planning.autonomy` as-is.
3. Variable absent (an ordinary host run) → `cenci planning attended status --json`, read its `attended` field.
4. Anything else, including a failed or unparseable query → use the repo's `planning.autonomy`, and say so in one line.

Nothing reads `~/.config/cenci/config.json` directly — the flag is only ever resolved
through the `cenci` binary or the forwarded variable.

### 2. Let dispatch start planning sessions

In `~/.config/cenci/config.json`:

```jsonc
// ~/.config/cenci/config.json — your machine's fleet config, not the repo's
{
  "dispatch": {
    "planRefined": true,
    "planStalenessTolerance": 5,
    "concurrencyCap": 3,
    "dailyQuota": 20,
    "quietHours": { "startHour": 22, "endHour": 7 }
  }
}
```

The switch itself doesn't need hand-editing:

```bash
cenci dispatch plan-refined on       # or: off
cenci dispatch plan-refined status   # fleet flag + this repo's remote-confirmed autonomy + combined verdict
```

writes `planRefined` with an atomic, key-preserving update (creating the file if
it doesn't exist yet); the tuning fields above (`planStalenessTolerance`, caps,
quiet hours) remain hand-edited.

`planRefined` turns two terminal skips into work: a `Refined` ticket with no plan
file becomes a planning pickup, and a `Planned` ticket whose plan has fallen more
than `planStalenessTolerance` commits behind becomes an autonomous re-plan.

> **Trust boundary.** A planning pickup consumes the ticket body and comments as its
> primary input, with no author-authorization check on that text. Do not enable
> `planRefined` for a repo that accepts issues from untrusted parties. (The
> `Input Needed` resume path is stricter: the replying author must currently hold
> `admin` or `write` on the repo, re-resolved every pass.)

> **Attended mode (switch 5).** When you're sitting at this machine's keyboard and
> could just answer a clarifying question yourself, `cenci planning attended on`
> suppresses unattended planning pickups/re-plans for lean repos here — narrowing
> only, per machine: it never grants lean to a repo that hasn't committed it, and a
> repo whose commit was already denied for another reason keeps that reason. `cenci
> planning attended status` shows the fleet flag, this repo's remote-confirmed
> autonomy, `dispatch.planRefined`, and the same combined verdict `dispatch
> plan-refined status` prints — the two commands can never disagree. It also changes how
> a planning session that *does* run behaves; see [The attended override](#the-attended-override).

### 3. Declare what a merge is allowed to touch

In the repo's committed `.cenci/config.json`. This block is **deny-by-default**:
absent, unreadable, or malformed means no PR merges, and there are no built-in
fallback thresholds.

```json
{
  "automerge": {
    "protectedPaths": [
      "*.github/workflows/*",
      "*install.sh",
      "*/security/*"
    ],
    "maxChangedFiles": 25,
    "maxDiffLines": 800,
    "mergeMethod": "squash"
  }
}
```

- `maxChangedFiles` and `maxDiffLines` are **required** whenever the block exists.
  Missing or non-positive makes the whole block malformed, which denies.
- `protectedPaths` are globs where `*` matches any character including `/`,
  case-insensitive. Any changed file matching any pattern denies that PR. A pattern
  ending in `/` with no trailing `*` matches that directory and everything under it
  (e.g. `flow/skills/` protects the whole directory, not a literal-string match).
  Each pattern is anchored against the **whole repo-relative path from the root**, so
  a bare pattern with no leading `*` (e.g. `install.sh`) only matches a file literally
  at the repo root — prefix with `*` to match anywhere (e.g. `*install.sh`).
- `mergeMethod` is read for compatibility, but only `squash` is ever executed —
  `merge` or `rebase` produces a logged hold, not a merge.
- The block is always read from the **PR's base branch**, so a PR can never widen its
  own policy to approve itself.

In a monorepo, put a block on each `projects[]` entry (a file falls to the entry with
the longest matching `path` prefix, then to the top-level block). When one PR spans
several blocks the effective policy is the *most restrictive* merge: the minimum of
each cap and the union of every `protectedPaths`.

`/cenci:configure` offers to scaffold this block only when `automerge` is absent — an
existing block is reported verbatim and never re-prompted, narrowed, or removed.

### 4. Arm the merge

Fleet switch, in `~/.config/cenci/config.json`:

```jsonc
// ~/.config/cenci/config.json — `enabled` lives here and nowhere else
{
  "automerge": { "enabled": true }
}
```

Or, without hand-editing:

```bash
cenci automerge on       # or: off
cenci automerge status   # fleet switch + this repo's per-scope policy summary
```

Then, per ticket, grant `automerge:ok` at `/cenci:refine`'s Confirmation Gate. This
is the one thing in the loop that is always a human decision and has no repo-level
default. It is **never inherited** — a split child, a companion design ticket, and a
followup each earn it on their own merit, or don't. A PR that closes several issues
needs the grant on *every* one of them.

The refiner withholds by default for security-sensitive paths, release/CI workflow
files, visually verifiable UI work, irreversible migrations — and whenever it's
uncertain.

### 5. Start the loop

```bash
cenci dispatch loop on      # recurring fleet-wide passes
cenci dispatch loop status
```

A good first run: refine one small ticket with `automerge:ok` granted, then watch it
land. `cenci status` shows live sessions; babysit's decision line (below) shows why a
PR did or didn't merge.

## What still stops the machine

These are not configurable away, and they're the reason the loop is safe to leave
running.

**Planning refuses to guess.** Even in lean mode, a plan is only self-approved when
*all* of these hold. Each can only disqualify — none can ever promote a ticket onto
the fast path:

- no escalation in the five named classes;
- no unresolved `### Open Questions` in the planner's output;
- no file in `### Files to Modify`/`### Files to Create` matching the sensitive-path
  set (auth, session, credential, token, `.pem`, `.env`, permission, rbac, crypto,
  payment, migration, schema, … unioned with your `security.sensitivePaths`);
- size estimate is not `L` and there's no split recommendation;
- no `awaiting-input` draft already on disk for the ticket.

Anything inconclusive — an unreadable `.plans/` directory, a malformed config —
fails closed to the interactive path.

Planning also auto-adopts a settled posture: a posture already settled verbatim in a `Refined`, trusted-author ticket's `### Decisions`/`### Assumptions (auto-adopted)` is auto-adopted rather than re-asked — the decision already reached at `/cenci:refine`'s Confirmation Gate is not confirmed a second time. This narrows only the confirm/overrule trigger, never its cap priority, and only when the delegation's forwarded provenance is positively verified (a `Refined` label plus a trusted author association, and a quotable bullet the codebase doesn't contradict); unverifiable provenance falls back to asking: missing provenance, a missing `Refined` label, an untrusted or unrecognized author association, ticketless mode, or a failed resume-time provenance read all ask exactly as before.

**The merge chain is fail-closed at every link.** "Green" requires at least one check
to be `pass`; every other check may be `pass` or `skipping` — a paths-filtered
monorepo's unaffected-project checks (routinely `skipping`) no longer hold automerge
forever. A `fail`, `pending`, `cancel`, empty, or unrecognized bucket still holds under
its own reason, in the order checks appear; an all-`skipping` set with zero `pass`
holds too, under its own distinct reason separate from "no checks reported". This is a
merge-gate relaxation, not a new compensating control: a *required* check that gets
skipped by a paths-filter misconfiguration no longer holds automerge on its own.
Babysit adds no new probe for that case — the backstop is GitHub's own merge refusal,
which surfaces as its own distinct hold reason (logged and retried, never bypassed) if
the merge is actually rejected.
Review feedback resolution is GitHub-authoritative — pushing a commit does not clear
a thread; only `isResolved`, a `DISMISSED` review, or a newer `APPROVED` does. Any
state babysit cannot positively confirm (unreadable, truncated pagination, unknown,
unsupported) holds rather than proceeding.

**The verdict is re-checked immediately before mutating.** The fleet switch, the
feedback state, and the PR's head SHA are all re-read at merge time. A thread
reopened, a check regressed, a commit pushed, or the kill switch flipped between
evaluation and merge holds the merge — each under its own distinct reason, so a
late flip is never confused with an ordinary first-pass hold.

**Merges are squash-only** and never `--delete-branch` (a PR worktree still
references the branch). A merge rejected by branch protection is logged and retried,
never bypassed. A zero-exit `gh pr merge` isn't taken as proof: babysit refetches
once and requires the refetch to report `MERGED` **at the head commit babysit
pinned** — a refetch reporting `MERGED` at a different (or empty) head commit
holds under its own distinct reason, never confirmed success, since another actor
could have merged a different commit in the race between babysit's own
validation and this refetch. Otherwise (not `MERGED` at all) the tick is
indeterminate, not successful.

**Arming from a sandbox is forwarded, not local.** Inside a `cenci sandbox` container,
sandboxed arming is forwarded to the host daemon, which spawns the supervisor host-side —
`cenci babysit` never starts a supervisor inside the container itself. A rejected forward is
**not armed**: implement's Phase 9 prints the reason and the host re-arm command instead of
claiming the PR is being watched, and the loop stalls honestly on that PR — an unarmed PR
never reaches an automerge tick — rather than silently pretending it is supervised.

## Reading a decision

Every enabled tick logs exactly one line per PR, including ticks that failed to read
upstream state:

```
babysit: automerge PR #42 held: ticket lacks automerge:ok [enabled=yes label=no ci=- review=- mergeable=- headsha=- policy=- files=- filecap=- lines=- protected=- method=- queue=-]
```

The bracket is the condition chain in evaluation order. `yes` = passed, `no` = the
stage that failed, `-` = never reached because an earlier stage short-circuited. So
the line above reads: the fleet switch is on, the label check failed, nothing after
it was evaluated.

| Key | Stage | Fails when |
|---|---|---|
| `enabled` | Fleet kill switch | `automerge.enabled` is not `true` in `~/.config/cenci/config.json` |
| `label` | Per-ticket grant | The PR closes no issue, an issue's labels are unreadable, or any closed issue lacks `automerge:ok` |
| `ci` | CI green (≥1 `pass`, rest `pass`/`skipping`) | No checks reported, zero `pass` with the rest `skipping`, or any check's bucket is `fail`, `pending`, `cancel`, empty, or unrecognized |
| `review` | Feedback | CI repair in flight, pending feedback, a reopened resolution, or a detection read that couldn't be proven complete |
| `mergeable` | PR state | Draft, `MERGEABLE` unknown, or not mergeable |
| `headsha` | Head commit | The PR's head SHA is unreadable at evaluation time |
| `files` | Diff readability | Zero changed files, or a truncated file list |
| `policy` | Policy block | `.cenci/config.json` on the base branch is unreadable, absent, or malformed |
| `filecap` | `maxChangedFiles` | The diff changes more files than the cap |
| `lines` | `maxDiffLines` | Additions + deletions exceed the cap |
| `protected` | `protectedPaths` | A changed file matches a protected glob |
| `method` | Merge method | Policy method isn't `squash`, or the repo disallows/doesn't report squash |
| `queue` | Merge queue | The PR requires or is in a merge queue, or the queue probe was unreadable |

A trailing `class=<class>` appears when the hold came from a `gh` failure
(command/timeout/cancelled/truncated/parse). Every hold has its own distinct reason
string — dozens of them, never collapsed into a shared one — precisely so a log line
tells you which link broke rather than a generic "not ready".

Once the `ci` stage is reached, a non-zero count of `skipping` checks renders as a
suffix, e.g. `ci=yes(skipped=6)` or `ci=no(skipped=9)` — diagnostic only, it never
changes the verdict. A zero skipped count renders the plain `ci=yes`/`ci=no`, and an
unreached `ci` stage still renders the plain `-` (as in the example above, where
`label` failed first and `ci` was never evaluated).

Three reasons need a human and will not clear on their own: `review feedback state
unreadable`, `review feedback state unknown` (GitHub stopped reporting a comment or
thread — deleted or purged), and `unsupported review feedback type`. Merge those by
hand.

## Knowing a merge was automatic

The log line above lives in the supervisor's own log and expires with it. `gh` merges
under *your* GitHub account, so on GitHub an automerged PR would otherwise look exactly
like you clicking "Squash and merge".

So a confirmed merge also leaves a comment on the PR itself: the cenci attribution
banner, the head commit the merge was pinned to, and the same condition-chain bracket —
the durable half of the record, readable months later by anyone auditing the ticket.

```markdown
> 🤖 **cenci** — merged automatically by `cenci babysit` (automerge policy). No human approved this merge.
```

Two things it deliberately is not. It is **not proof**: the banner literal is public and
`gh` posts under your identity, so anyone can write a byte-identical comment — it records
what babysit did, it does not authenticate it. And it is **not part of the merge
decision**: it is posted after the merge is already confirmed, so a comment that fails to
post costs you an audit record and nothing else. Held and unconfirmed ticks leave no
comment at all — if there is no comment, no automerge landed.

## Turning it off

| To stop… | Do this | Takes effect |
|---|---|---|
| All merging, everywhere | `cenci automerge off` (sets `automerge.enabled: false`) | Next tick, including a tick already mid-evaluation |
| Merging for one repo | Remove/narrow the repo's `automerge` block on the base branch | Next tick |
| Merging for one ticket | Remove `automerge:ok` from the issue | Next tick |
| Autonomous planning, everywhere | `cenci dispatch plan-refined off` | Next pass |
| Autonomous planning for one repo | Push `planning.autonomy` off `"lean"` to `origin/main` | Next pass with a successful fetch |
| Autonomous planning on this machine only, while a human is around | `cenci planning attended on` | Next dispatch pass; and the next planning session you launch |
| All dispatch | `cenci dispatch loop off` | Immediately; in-flight sessions finish |

A revocation pushed to `origin/main` is honored even if your local checkout still has
`"lean"` cached — the remote object is the only authority.

## Known limits

Accepted and documented, not bugs:

- **Re-plans are unbounded.** Nothing caps how often a ticket can be re-planned. A
  successful re-plan rewrites the plan's commit baseline, which self-limits the common
  case, but an over-broad `stalenessPaths` can re-plan repeatedly. `dailyQuota` and
  `concurrencyCap` are the rate limiter; raising `planStalenessTolerance` raises the
  trigger threshold.
- **Sibling serialization is inert for planning pickups.** It's derived from plan-file
  front matter, which doesn't exist yet for a `Refined` ticket, so several children of
  one parent can enter planning in the same pass. Declared blocked-by chains
  still serialize.
- **A persistently failing resume can loop.** A session that claims `Working` and then
  fails restores `Input Needed` in-session, so dispatch re-resumes it next pass, with
  no attempt counter guarding that specific loop. Bounding it is deferred.
- **Staleness is scoped, not absolute.** A plan whose `stalenessPaths` under-scopes its
  real dependencies can report `fresh` while a relevant change landed outside that
  scope.
- **Merge state can lag by one supervision interval.** babysit re-evaluates on its own
  cadence (`babysitInterval`, default `15m`), so a PR can sit merge-ready for up to one
  interval.

## Where the details live

| For… | Read… |
|---|---|
| The full board lifecycle, labels, and every transition | [Orchestration recipe](orchestration.md) |
| Dispatch gates, pickup rules, reconciliation, config reference | [cenci-watch README — Auto-dispatch](../watch/README.md#auto-dispatch-cenci-dispatch) |
| Every automerge condition, in full | [cenci-watch README — Automerge](../watch/README.md#automerge-cenci-babysit) |
| The `automerge` config schema, field by field | [configure skill](../flow/skills/configure/SKILL.md) |
| Lean planning as the planner actually executes it | [Phase 1 — Plan](../flow/skills/implement/phases/phase-1-plan.md) |
| How a session resolves attended vs. the repo's autonomy | [Phase 1 — Plan, `## Resolve Planning Autonomy`](../flow/skills/implement/phases/phase-1-plan.md) |
| Which test pins which claim on this page | [Pipeline coverage map](pipeline-coverage-map.md) |
| Where the supervisor runs, forwarded arming, and the arming/host verification recipe | [cenci-watch README — Automerge](../watch/README.md#automerge-cenci-babysit) |
