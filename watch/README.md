# cenci-watch

> Part of [cenci](../README.md) — the **attention layer**. See the root README for
> the one-command install and how the isolation, workflow, and attention layers fit together.

Stop hunting through terminals to find the session that needs you. cenci-watch turns
Claude Code and Codex hooks into shared live state for tmux and optional desktop
surfaces.

![A lazyboards board in a tmux window; in the tmux window list below, cenci-watch marks one agent window blue and running, one red and needing input, and one green and done](../docs/assets/cenci-tmux.png)

*Live agent state in the tmux window list: `▶ 6-implement` running (blue), `! 12-refine`
needs input (red), `✓ 9-refine` done (green). cenci-watch supplies the symbols and state
colors; the board is [lazyboards](https://github.com/matteobortolazzo/lazyboards) and the
surrounding tmux theme is user-provided.*

The same four states appear everywhere:

- **▶ blue** — running (generating, tool use, thinking)
- **✓ green** — done (finished, waiting for next prompt)
- **! red** — need input (permission dialog)
- **~ dim** — idle (fresh prompt, no task yet)

When the agent exits or cenci stops, the original window name is restored.

![The Cenci DMS widget showing aggregate status counts and a popout with five agent sessions](../docs/assets/cenci-dms-widget.png)

*Optional [DankMaterialShell](plugin/dms/README.md) integration: the bar pill
summarizes every live agent session; click it to find work that is running, done,
or waiting for input.*

Other integrations are available for [Waybar](#waybar-config),
[noctalia-shell](plugin/noctalia/README.md), [GNOME Shell](plugin/gnome/README.md),
[KDE Plasma](plugin/plasma/README.md), and the
[macOS menu bar](plugin/macos/README.md).

**tmux appearance:** no tmux theme is bundled or required. cenci-watch augments
tmux's default window list automatically and applies the state colors above. If
your theme replaces `window-status-format` or `window-status-current-format`, wire
its two stable user variables into the theme instead; see
[Custom status-format integration](#custom-status-format-integration).

## How live status reaches you

![cenci-watch routes Claude Code and Codex hook events to tmux and desktop status surfaces](../docs/assets/cenci-surfaces.svg)

1. **The agent reports a lifecycle event.** Native Claude Code and Codex hooks send
   session start, activity, input, completion, and exit events over a local Unix socket.
2. **One daemon turns events into shared state.** It keys status by agent session,
   coalesces updates, and removes stale sessions when a pane or paneless run disappears.
3. **Every surface reads the same answer.** tmux updates interactively; desktop widgets
   consume the read-only `cenci widget-json` snapshot.

Normal state changes are push-driven—there is no polling loop watching agent processes.
Only stale-session cleanup runs periodically.

### Architecture details

The core daemon keys state by agent session id, maps hook events to statuses, and owns
the paneless TTL sweep. All window work is delegated to an injected frontend:

- **tmux frontend** (`internal/frontend/tmux/`): the one interactive frontend — window rename, style, pane-based stale sweep, renumber migration.
- **status JSON** (`internal/frontend/status/`): read-only broadcast in the [Waybar custom module protocol](https://github.com/Alexays/Waybar/wiki/Module:-Custom); consumed by `cenci widget-json` (hidden alias `waybar`) and the Waybar, noctalia, [DMS](plugin/dms/README.md), [GNOME Shell](plugin/gnome/README.md), [KDE Plasma](plugin/plasma/README.md), and macOS menu bar ([SwiftBar](https://swiftbar.app), [setup](plugin/macos/README.md)) display widgets.

## Installation

The easiest path is the [one-command installer](../docs/getting-started.md), which
also wires the desktop bar widget for whichever bar it detects:

```bash
curl -fsSL -o install.sh https://github.com/matteobortolazzo/cenci/releases/latest/download/install.sh
curl -fsSL -o install.sh.bundle https://github.com/matteobortolazzo/cenci/releases/latest/download/install.sh.bundle
cosign verify-blob --bundle install.sh.bundle \
  --certificate-identity-regexp '^https://github\.com/matteobortolazzo/cenci/\.github/workflows/watch-release\.yml@refs/(heads/main|tags/watch/v[0-9]+\.[0-9]+\.[0-9]+)$' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  install.sh
bash install.sh
```

Requires [cosign](https://docs.sigstore.dev/system_config/installation/) — the installer
verifies its own bytes against the release before running, and fails closed with no
fallback to an unverified ref. The legacy one-liner still works and re-execs itself
through this same verified path:

```bash
curl -fsSL https://raw.githubusercontent.com/matteobortolazzo/cenci/main/install.sh | bash
```

Set `CENCI_REF=main` (or pass `--ref main`) to explicitly opt into bleeding-edge,
unverified main instead (unsafe; development use only).

The installer handles Claude Code, Codex, or both. The binary and daemon
self-bootstrap from the active client's plugin cache on the first session.

On both install and `cenci update`, the installer auto-detects each present
GUI bar — GNOME Shell, KDE Plasma, DankMaterialShell, and noctalia — and, with a
per-bar prompt (default yes), installs and **reloads** its widget so widget
changes are visible immediately. macOS SwiftBar is wired the same way. Because a
running panel restart is disruptive, each bar is gated behind its own prompt.
Waybar is the exception: its config is hand-managed, so the installer only prints
the module snippet to add (see [Waybar config](#waybar-config)) and the
`pkill -SIGUSR2 waybar` reload hint — it writes nothing. GNOME first installs of a
brand-new extension dir still need a Shell reload (X11 `Alt+F2`, `r`) or relogin
(Wayland); update reloads live via the extension's disable→enable toggle.

### Advanced / development: standalone client installation

```bash
# Register the repo as a marketplace (works with private repos too)
claude plugin marketplace add matteobortolazzo/cenci

# Install the plugin (persists across sessions)
claude plugin install cenci-watch

codex plugin marketplace add matteobortolazzo/cenci
codex plugin add cenci-watch@cenci
```

On the first `SessionStart` after install, the plugin downloads the `cenci`
binary matching the plugin version (with checksum verification) into the plugin's
`bin/` directory, symlinks it onto your writable `$PATH` (`~/.local/bin`), and
starts the daemon. Bootstrap runs detached and never blocks the agent, so the
very first session may take a moment before status appears; the daemon then
persists for all later sessions.

The on-`$PATH` symlink is re-pointed on every session (so it follows version
bumps) and lets bare `cenci` invocations resolve — shell, tmux `run-shell`,
Codex hooks, and waybar-from-shell. **GUI/compositor bars** (DMS, noctalia) are
different: they inherit the *login* PATH, which usually lacks `~/.local/bin`. To
make them find the binary, `install.sh` offers a one-time `sudo` link into
`/usr/local/bin` (which every GUI login PATH includes) chained through the
`~/.local/bin` link, so it too survives version bumps. Decline it and the bar
widgets fall back to their `cenciPath` / `CENCI_BIN` overrides.

Use `cenci update` for normal updates. Standalone development installs can use
the corresponding client marketplace update command; the next session re-bootstraps
the matching binary.

Codex will ask you to review/trust new hooks. Use `/hooks` in Codex if the hooks are listed as pending review.

**Trust model:** Codex hash-pins `hooks.json`, so every plugin update that changes
the file changes its hash and requires re-trusting the hooks via `/hooks` in Codex.

The marketplace plugin includes a native Codex manifest at
`plugin/.codex-plugin/plugin.json`, which loads the Codex-specific hooks from
`plugin/codex/hooks.json`.
Plugins and their bundled hooks are stable and enabled by default in current Codex
releases — no feature flag is required.

The Codex hooks self-bootstrap the binary and shared daemon on `SessionStart` even in
a Codex-only installation—see the
[Codex plugin README](plugin/codex/README.md#binary-and-daemon-self-bootstrapping).

## Dispatching workflows (`cenci run`)

`cenci run` launches a coding-agent CLI for a workflow in a detached tmux
window, owning the `<number>-<skill>` window name that ties board cards, tmux windows,
and watcher snapshots together. It replaces the personal dispatch scripts that used to
live in `~/.config/lazyboards/scripts/`.

```bash
# Refine/implement ticket 40 with Claude in the current tmux session
cenci run implement 40

# Inspect the resolution without spawning anything
cenci run implement 40 --dry-run
# session: my-tmux-session
# window:  40-implement
# command: claude -- '/cenci:implement 40'
```

Positional args are `<workflow> [ticket-id | task description] [additional context]`;
flags may follow them. Everything after the workflow is forwarded verbatim as the
skill argument (`/cenci:<workflow> $ARGUMENTS`), so the same free-text forms the skills
accept work here too — no quoting needed:

```bash
# Ticket id plus additional context → window 40-implement (context still reaches the skill)
cenci run implement 40 focus on the API layer

# Ticketless task description → window add-dark-mode-toggle
cenci run implement add dark mode toggle
```

When the first token is a numeric ticket id, the window is named `<number>-<skill>`
(the skill being the workflow: `refine` / `implement`) — short, uniform, and
matched by external tools on the number prefix, so the ticket title is deliberately
omitted. `--slug` and trailing context do not change a numbered window's name. A
non-numeric first token (a free-text task description) has no ticket number and keeps a
descriptive slug: `--slug` if given, else the whole description slugified.

| Flag | Purpose |
|------|---------|
| `--agent <name>` | Agent to launch (`claude`, `codex`, …); default from config, else `claude` |
| `--sandbox` / `--no-sandbox` | Sandbox is the default (`claude`→`cenci open`, the container being the mandatory runtime); `--no-sandbox` is the host opt-out. Both override the config default |
| `--model <model>` | Model override passed to the agent (substituted into `{model}`, else appended as `--model`) |
| `--session <name>` | Target tmux session (default: the current session) |
| `--dir <path>` | Working directory the window starts in (default: current); prepended as a `cd '<dir>' &&` prefix on the launched command, visible in `--dry-run` output too |
| `--slug <slug>` | Window-name slug for free-text runs; ignored for numeric tickets (named `<number>-<skill>`) |
| `--config <path>` | Config file (default: `$XDG_CONFIG_HOME/cenci/config.json`) |
| `--dry-run` | Print the resolved session, window name, and command without spawning |

A board column action shrinks to a single line:

```yaml
command: "cenci run implement {number}"
```

### The join key survives the daemon

`run` creates the window with `automatic-rename off`. When the daemon later tracks it,
it sees the window is manually named and preserves `<number>-<skill>` instead of
overwriting it with the detected task name — so the join key flows through to the
status snapshot's `window_name`.

### Grouped-session guard

New windows propagate to every session in a tmux session group, so `run` refuses to
spawn into a grouped session (non-zero exit, no window created). Pass an ungrouped
`--session` to target a specific session.

### Configuration

Built-in Go templates cover Claude `refine`/`implement` with zero config. An
optional `config.json` (respecting `$XDG_CONFIG_HOME`, or `--config`) overrides the
defaults and adds agents or workflows — the tokens `{ticket}` and `{model}` are
substituted at launch. Launches run inside the cenci-sandbox container by default; the
`"sandbox"` field below is optional and, when set to `false`, opts every launch out to
the host (the same as passing `--no-sandbox`):

```json
{
  "defaultAgent": "claude",
  "sandbox": false,
  "agents": {
    "claude": {
      "command": "claude",
      "sandboxCommand": "cenci open",
      "workflows": {
        "implement": { "args": ["--", "/cenci:implement {ticket}"] }
      }
    },
    "codex": {
      "command": "codex",
      "model": "gpt-5.6-sol",
      "workflows": {
        "implement": { "args": ["exec", "/cenci:implement {ticket}"] }
      }
    },
    "opencode": {
      "command": "opencode",
      "workflows": {
        "implement": { "args": ["run", "implement {ticket}"] }
      }
    }
  }
}
```

Only the built-in Claude templates ship today; Codex and opencode require a
`config.json` entry. Until one is configured, `--agent codex` exits with a helpful "no
launch template" error.

**OpenCode:** this adapter requires OpenCode 1.18.3 or newer — `cenci-installer
doctor` enforces this same minimum when it detects OpenCode on the host (see
[Installer integration](#installer-integration-cenci-doctor-cenci-update-cenci-uninstall)
below). Model/provider resolution follows the same override chain as the
`--model` flag above: an explicit `--model` flag on the command line overrides
the agent's `model` in `config.json`, which in turn overrides whatever
provider/model OpenCode itself would default to when neither is set.

Known limitations: no `TokenReader` is wired for OpenCode yet, so it has no
usage-budget headroom tracking (see [Budget headroom](#budget-headroom)
below); and it has no one-token `open` shortcut yet (see the shortcut table
below) — launch it with `--agent opencode`.

## Auto-dispatch (`cenci dispatch`)

Once planning is human-gated and a planned plan shows up on the board as the
`Planned` state, *picking it up* is pure policy — no LLM in the dispatcher.
`cenci dispatch` walks the configured repos, matches each `Planned` ticket to
its planned `.plans/<id>-*.md` file, requires the ticket's sole assignee to match
the active account returned by `gh api user`, checks capacity/budget gates, and —
for every ticket that clears them — runs exactly what a human would press:
`cenci run implement .plans/<file> --agent <chosen>`. The intelligence stays
inside the dispatched sessions; the dispatcher is config plus a pure decision
function.

Unassigned tickets, tickets owned by another GitHub user, and tickets with multiple
assignees are logged as explicit skips. Git commit `user.name` and `user.email` are
not used for ownership. If the active GitHub login cannot be resolved, the pass fails
closed and launches nothing.

```bash
# Print the decision table without spawning anything
cenci dispatch --dry-run
# owner/name#45 skip: not Planned
# owner/name#78 dispatch (claude, 78-add-cache.md): dispatch

# Run a single pass
cenci dispatch --once

# Run continuously, re-evaluating every 5 minutes
cenci dispatch --interval 5m

# Run a single failure-reconciliation pass (recover stranded dispatched work)
cenci dispatch --reconcile
```

| Flag | Purpose |
|------|---------|
| `--once` | Run a single dispatch pass then exit (the default when neither `--once` nor `--interval` is given) |
| `--interval <dur>` | Re-run on this interval (e.g. `5m`); mutually exclusive with `--once` |
| `--reconcile` | Run one failure-reconciliation pass instead of a dispatch pass (see [Failure reconciliation](#failure-reconciliation)); pair with a cron entry |
| `--dry-run` | Print the decision (or reconciliation) table and never merge; it still runs the local-main sync's `git fetch` for real (updates each enrolled repo's remote-tracking refs only, so the decision table reflects an accurate gate) — see [Pickup rules and gates](#pickup-rules-and-gates) |
| `--config <path>` | Config file (default: `$XDG_CONFIG_HOME/cenci/config.json`) |
| `--model <model>` | Model override for every session dispatched this pass — overrides `dispatch.model` and `agents.*.model` in `config.json`. With `--interval`, re-applied on every tick (a config reload can't drop it). |

Every ticket yields exactly one logged decision — dispatched or skipped, always with
a reason — so nothing fails silently. When a model is pinned (via `--model` or
`dispatch.model`), the resolved value is logged once per pass
(`dispatch: model override "..."`), so a dispatched session's model is never a
silent surprise — without a pin, it falls back to `agents.*.model`, or otherwise
whatever ambient default the agent CLI itself resolves.

### Enrollment (`cenci dispatch enroll|unenroll|status`)

The `dispatch.repos` list (below) is normally managed with these verbs instead of
hand-editing `config.json` — this is how lazyboards (and humans) register a repo for
dispatch:

```bash
cenci dispatch enroll   [--dir <path>] [--config <path>] [--session <name>]
cenci dispatch unenroll [--dir <path>] [--config <path>] [--repo owner/name]
cenci dispatch status   [--dir <path>] [--config <path>] [--json]
```

| Verb | Flags | Behavior |
|------|-------|----------|
| `enroll` | `--dir` (default cwd), `--config`, `--session` | Detects `owner/name` and the absolute dir from `--dir`'s git `origin` remote, then adds/updates the `repos` entry. Idempotent: a second run prints `Already enrolled owner/name (dir)` instead of duplicating the entry. `--session <name>` is optional and sets `repos[].session` — the tmux session that repo's dispatches target (see [Configuration](#configuration)). Omitting it preserves any existing session unchanged (this is how lazyboards' `d`-panel toggle, which never passes `--session`, avoids blanking an already-configured repo). An empty or whitespace-only `--session` is a usage error (exit `2`) rather than a silent clear — there is no un-set verb; unenrolling is how a repo leaves dispatch. Whenever the resulting entry's session is empty — on a fresh enrollment or a no-flag re-enrollment of an already session-less repo — the result line names the consequence and the fix: `Enrolled owner/name (dir); no tmux session set -- dispatch will skip this repo until you run: cenci dispatch enroll --session <name> (config: /abs/path)`. When the resulting entry has a session, the line instead ends `→ session <name>`. |
| `unenroll` | `--dir`, `--config`, `--repo owner/name` | Removes the `repos` entry. Idempotent: unenrolling a repo that isn't enrolled exits `0` with `Not enrolled: owner/name`. `--repo` unenrolls by name without touching git — use it when the repo's directory has moved or been deleted. `--repo` and an explicitly-passed `--dir` are mutually exclusive (exit `2`) since only one can identify the target. |
| `status` | `--dir`, `--config`, `--json` | Prints the current enrollment state without mutating anything. `--json` emits a single pinned line: `{"repo":"owner/name","dir":"/abs/path","enrolled":true,"session":"a-work","loop":{...}}`; when not enrolled, `dir` is still the **detected** absolute dir (not empty) and `session` is empty, even if the config file doesn't exist yet. Human output likewise names the session: `Enrolled owner/name (dir) → session a-work`, or `Enrolled owner/name (dir); no tmux session set` when unset. |

Exit codes are consistent across all three verbs: `0` when the verb ran successfully
(enrolled/not-enrolled is a result, not a failure), `1` on a detection/IO error
(`cenci dispatch <verb>: <reason>` on stderr), `2` on bad flags (including the
`--repo`/`--dir` conflict above).

### Loop toggle (`cenci dispatch loop on|off|status`)

Toggles and reports the embedded fleet dispatch loop (`dispatch.loopEnabled`,
see [Configuration](#configuration)) without hand-editing `config.json`:

```bash
cenci dispatch loop on     [--config <path>] [--json]
cenci dispatch loop off    [--config <path>] [--json]
cenci dispatch loop status [--config <path>] [--json]
```

| Verb | Behavior |
|------|----------|
| `on` | Sets `dispatch.loopEnabled: true` (defaulting `dispatch.daemonInterval` to `"5m"` if unset), then prints the resolved state. |
| `off` | Sets `dispatch.loopEnabled: false`, then prints the resolved state. |
| `status` | Prints the resolved state without mutating anything. |

All three print the same resolved `DispatchState` — human-readable by default, or the
raw JSON object with `--json` (e.g. `{"enabled":true,"daemon_running":false,"interval":"5m",...}`).
When the daemon was reached but its snapshot couldn't be read for a reason other than
plain unreachability (permission-denied socket, corrupt/truncated snapshot), the
resolved state carries a `resolve_error` field with the real error text (rendered as a
`resolve_error: <text>` line in human output) — distinct from the loop's own redacted
`last_error`, and distinct from a daemon that simply isn't running, which stays silent.

**Breaking change:** the loop no longer auto-enables from a bare `daemonInterval`. It
now defaults to disabled and only dispatches once `loopEnabled` is explicitly set to
`true`. Existing installs that relied on `daemonInterval` alone must run `cenci
dispatch loop on` (or set `loopEnabled: true` directly) after upgrading, or dispatch
will silently stop.

A running `cenci daemon` always starts the embedded dispatch supervisor loop at
startup — `dispatch.loopEnabled` purely controls whether it *performs* passes, not
whether it runs. The loop reloads its config on a hardcoded 60s check interval (not
configurable) to pick up `loopEnabled` changes, so `dispatch loop on`/`off` take effect
within ≤60s of a running daemon, with no daemon restart and no new inbound IPC. While
disabled, the loop still wakes every 60s — it skips the dispatch/reconcile passes but
clears any stale failed-window badges and headroom overlays within that same window.
While enabled, configuration is still polled at least every 60 seconds, but the
dispatch/reconcile pair runs only at the configured `daemonInterval` deadline (and
immediately after enabling). Interval edits recalculate that deadline from the prior
pass completion without creating an extra pass. The loop publishes live state so that
`cenci dispatch status --json`'s `"loop"` object (`enabled`, `pass_running`,
`last_run_at`, `last_dispatched`, `last_skipped`, `last_error`) now reflects the live
daemon end-to-end, not just a config fallback. `last_dispatched` counts successful
spawns (not merely dispatch decisions), and `last_error` is intentionally redacted to
`dispatch_pass_failed`, `reconcile_pass_failed`, `reconcile_state_unreadable`
(ticket #883, a persistent reconciliation-state corruption hold — see "Corrupt
reconciliation state" below), `dispatch_session_unconfigured`, or
`dispatch_session_missing` (ticket #927: at least one enrolled repo's `session` is
unset, or names a tmux session absent from the server); detailed errors stay in
daemon logs.

This daemon-embedded path (`dispatch loop on` alongside a running `cenci daemon`)
is the canonical way to run dispatch continuously. `cenci dispatch --interval
<duration>` (see [Auto-dispatch](#auto-dispatch-cenci-dispatch)) remains a
separate, standalone loop for running dispatch directly from the CLI without a daemon.
It stops and exits nonzero on its first config or pass error, except a per-repo
session skip (`dispatch_session_unconfigured`/`dispatch_session_missing`, ticket
#927): that is logged and the loop keeps ticking, since it names a single
misconfigured repo rather than a fleet-wide failure. One-shot, dry-run, and
reconcile invocations still exit nonzero on any pass error, including a session
skip — so the misconfiguration is loud interactively.

### Planning-pickup toggle (`cenci dispatch plan-refined on|off|status`)

Toggles and reports the fleet-wide `dispatch.planRefined` switch (see
[Planning pickup and autonomous re-plan](#planning-pickup-and-autonomous-re-plan))
without hand-editing `config.json`, mirroring the loop toggle above:

```bash
cenci dispatch plan-refined on     [--config <path>] [--json]
cenci dispatch plan-refined off    [--config <path>] [--json]
cenci dispatch plan-refined status [--config <path>] [--dir <path>] [--json]
```

`on`/`off` persist the toggle with the same atomic, key-preserving write as
`enroll` and `loop`, creating the config file (and parent directory) when none
exists yet. All three verbs then print the resolved state; `--json` emits a
single pinned object, e.g.
`{"enabled":true,"config":"/abs/path","repo":"owner/name","repo_autonomy":"lean","authorized":true,"attended":false}`.

Because the fleet flag alone never authorizes a planning pickup (#851/#877),
`status` run inside a git repository (or with `--dir`) also reports that repo's
half of the grant chain: `repo_autonomy` is the committed `planning.autonomy`
verdict read at `refs/remotes/origin/main` **as of the last fetch** (the command
never fetches; a dispatch pass always fetches first, so its live decision can be
fresher), and `authorized` is the combined verdict — `true` only when the fleet
flag is on, the fleet-wide [attended mode](#attended-mode-cenci-planning-attended-onoffstatus)
switch is off, **and** the remote-confirmed value is exactly `"lean"`. A directory
that isn't a git repo omits the repo fields; a repo with no fetched
`origin/main` ref reports `unreadable`, the same fail-closed verdict the
dispatch gate itself uses. A malformed fleet `planning` block makes `status`
exit 1 with the error on stderr, same as a malformed `dispatch` block — never a
silent, confidently-wrong verdict.

### Attended mode (`cenci planning attended on|off|status`)

Toggles and reports the fleet-wide `planning.attended` narrowing switch — "a
human is at the keyboard on this machine right now" — without hand-editing
`config.json`:

```bash
cenci planning attended on     [--config <path>] [--json]
cenci planning attended off    [--config <path>] [--json]
cenci planning attended status [--config <path>] [--dir <path>] [--json]
```

`planning.attended` lives in its own top-level `planning` block in
`~/.config/cenci/config.json` — **fleet-scoped**, and distinct from the
repo-committed `planning.autonomy` key in each repo's own `.cenci/config.json`;
the two files silently ignore each other's key of the same parent-block name.
When on, every dispatch pass on this machine narrows any repo whose committed
`planning.autonomy` resolves `"lean"` to a deny for that pass, suppressing
unattended `Refined` planning pickups and autonomous re-plans there — it can
only ever *reduce* what a repo already opted into, never grant lean to a repo
that hasn't. `on`/`off` persist the toggle with the same atomic,
key-preserving write every fleet switch here uses, creating the config file
(and parent directory) when none exists yet.

`status` prints the fleet flag, and — inside a git repository or with `--dir`
— that repo's remote-confirmed `planning.autonomy` verdict
(`repo_autonomy`, unnarrowed: this probe always reports the repo's true
committed value, never the attended-narrowed one), `dispatch.planRefined`
(`plan_refined`), and the same three-factor combined `authorized` verdict
`cenci dispatch plan-refined status` prints — the two commands can never
disagree about whether a repo's pickups would actually fire. `--json` emits
`{"attended":true,"config":"/abs/path","repo":"owner/name","repo_autonomy":"lean","authorized":false,"plan_refined":true}`.
A malformed `planning` block makes `status` exit 1 with the error on stderr,
never a silent "off".

### Pickup rules and gates

A ticket is dispatched only when **all** of these hold, evaluated in order (the first
failing gate is the logged skip reason):

1. **local `main` sync** — the ticket's repo's local `main` checkout synced cleanly
   with `origin/main` this pass (see [Local main sync](#local-main-sync) below);
1.5. **plan inventory** — the repo's whole `.plans` directory was proven completely
   readable this pass (see [Plan inventory](#plan-inventory) below); an unreadable or
   partially-read directory holds **every** ticket in the repo, ordinary/resume/
   planning pickup alike, since a partial enumeration can never prove no second file
   claims any given ticket;
2. carries `Planned`, **or** carries `Refined` (and not yet `Planned`) in a
   lean-planning repo with `dispatch.planRefined: true` — a *planning pickup* (see
   [Planning pickup and autonomous re-plan](#planning-pickup-and-autonomous-re-plan)
   below) — **or** carries `Input Needed` with a human answer newer than the
   escalation anchor (see [Escalation auto-resume](#escalation-auto-resume) below);
   not `Blocked`, has a provably-complete open-PR inventory with no open linked PR
   (see [Open-PR inventory completeness](#open-pr-inventory-completeness) below),
   and its persisted pipeline stage is not `finalized`;
3. a matching plan for the ticket exists with `status: planned` (or, on the resume
   path, `status: awaiting-input`) — **skipped for a planning pickup**, which by
   definition has no plan file matched yet;
4. the plan is fresh — default-branch commits since its `planCommitSha` are within
   `planStalenessTolerance` (else, with `dispatch.planRefined: false`, terminal
   `plan stale, re-plan`; with it `true`, an autonomous *re-plan* dispatch instead
   — see [Planning pickup and autonomous
   re-plan](#planning-pickup-and-autonomous-re-plan) below); when the plan's front
   matter lists `stalenessPaths`, only commits touching those paths are counted
   (see [Path-aware staleness](#path-aware-staleness) below). **Deliberately
   skipped on the resume and planning-pickup paths** — a draft that waited on a
   human is almost always commits-behind, and applying this gate there would mean
   it could never re-resume; a planning pickup has no plan file to measure
   staleness against in the first place;
5. **ticket dependency gate** — every issue the ticket is blocked by is closed
   (else `waiting on dependency #N`, or `dependency #N unresolved` when
   that issue's state couldn't be determined; see
   [Ticket dependency gate](#ticket-dependency-gate) below);
6. **siblings serialize** — if the plan is a child (`isChild: true`), it waits while
   any sibling (same `parentId`) is active (`Working`, an open PR, or a running
   window) or was already dispatched this pass, so at most one child per parent runs
   at a time;
7. the daemon is reachable (else `daemon unreachable` — never dispatch on unknown
   state);
8. fewer than `needInputThreshold` windows are awaiting input;
9. `running + dispatched-this-pass` is below `concurrencyCap`;
10. the daily quota is not yet spent;
11. the current local time is outside `quietHours`;
12. the resolved agent still has budget (see [Usage budgets](#usage-budgets) — when
    `agentLimits` is set this is computed from real token usage, otherwise from the
    static `agentBudgetFloors`).

Gate 8's `needInputThreshold` reads only `StatusSummary.NeedInput` — a live
session waiting mid-turn for a permission prompt or similar. It deliberately
never reads `StatusSummary.Escalated` (ticket #826: a ticket the unattended
planner escalated, `Input Needed`). The two are different concepts and must
never be conflated: a single planner escalation must never freeze dispatch
for the whole fleet the way `needInputThreshold` windows-awaiting-input can.

#### Local main sync

Before evaluating any ticket, each dispatch pass runs `git fetch origin` and a
fast-forward-only merge of `origin/main` into `main`, once per enrolled repo — but
only when the repo's checkout is currently on `main`. This runs unconditionally, even
on a pass with zero collected tickets, and only ever touches `main` — never `--reset`,
`--force`, or a branch other than `main`.

Each repo's sync lands in one of six outcomes:

- **`MainSyncSkipped`** — no `dir` is configured for the repo at all
  (`dispatch.repos[].dir` is empty). The zero value: "no sync attempted," distinct
  from every state below. Ungated: every ticket in the repo proceeds to the next
  gate.
- **`MainSyncSynced`** — `main` is now caught up with (or was already at or ahead
  of) `origin/main`. Ungated: every ticket in the repo proceeds to the next gate.
- **`MainSyncFetchFailed`** — `git fetch origin` itself failed (network, auth,
  unresolvable remote). Left ungated for ordinary `Planned` pickup deliberately:
  transient, and self-heals next pass. #877 changes this for planning pickup and
  autonomous re-plan specifically — see [Planning pickup and autonomous
  re-plan](#planning-pickup-and-autonomous-re-plan)'s "Remote-confirmed
  authorization" note: a fetch failure this pass holds both with a distinct
  retryable reason, even though ordinary dispatch in the same repo is
  unaffected.
- **`MainSyncNotMain`** — the checkout is currently on a branch other than `main`.
  **Gated.**
- **`MainSyncDetached`** — the checkout's `HEAD` is detached (on no branch at all).
  **Gated.**
- **`MainSyncMissing`** — a `dir` is configured but that directory does not exist
  on disk. **Gated.** Distinct from `MainSyncSkipped` (no `dir` configured at all)
  and from the pre-existing "not a git repository"/diverged/merge-failed
  `MainSyncFailed`/`MainSyncDiverged` states (also gated, unchanged from before).

**User-visible behavior change:** `MainSyncNotMain`, `MainSyncDetached`, and
`MainSyncMissing` gate **every ticket in that repo this pass** — ordinary `Planned`
pickups included, not only planning pickups or autonomous re-plan. An enrolled repo
parked on a feature branch, left detached, or whose configured directory has gone
missing from disk used to keep dispatching ordinary `Planned` tickets normally; it
no longer does — it is skipped exactly like the pre-existing `MainSyncDiverged`/
`MainSyncFailed` gated states. Rationale: plan freshness and the pipeline-stage gate
below are both computed against the local tree, so a checkout state that isn't
trustworthy for the sync itself isn't trustworthy for any other pickup either — see
gate 1 in [Pickup rules and gates](#pickup-rules-and-gates).

This gate is scoped to `main` only — repos whose default branch isn't literally named
`main` are never synced or gated by this rule. `--dry-run` still performs the real
`git fetch` and classification (an accurate decision table needs it) but never runs
the merge itself.

**Dry-run staleness parity.** When a fast-forward is pending (local `main` is
strictly behind `origin/main`), dry-run and the subsequent real pass share one
resolved `FreshRef` per repo per pass: the fetched `origin/main` blob rather than
local `HEAD`. Both plan staleness (gate 4 in [Pickup rules and
gates](#pickup-rules-and-gates)) and the repo-local lean-authorization config read
(below) are evaluated against that same `FreshRef`, so a dry-run's rendered
decision table matches exactly what the subsequent real (fast-forwarded) pass would
produce. In every other outcome — already up to date, local `main` ahead, or any
gated state — `FreshRef` is `HEAD`.

**Authorization-ref parity (#877).** `AutonomyRef` is a separate per-repo value
from `FreshRef`, resolved once per pass alongside it: non-empty (always the
fully-qualified `refs/remotes/origin/main`) if and only if this pass's `git fetch
origin` succeeded, empty on every pre-fetch or fetch-failed path. Unlike
`FreshRef`, `AutonomyRef` never falls back to local `HEAD` — the repo-local
lean-authorization config read (below) always reads at `AutonomyRef`, so it is
identical between a dry-run pass and its subsequent real pass (the fetch runs in
both; only the merge doesn't), giving the same authorization decision either way.

Gate 2's pipeline-stage check reads the ticket's persisted `cenci pipeline` state
(`.cenci/pipeline/<id>.json`) and skips only on `finalized` — deliberately not "at or
past `executed`": `cenci pipeline execute` fires at the *start* of Phase 2, so an
`executed`-based threshold would also swallow every agent that crashed
mid-implementation, exactly the population the reconciler's crash-recovery retry
flips back to `Planned` to be re-dispatched. Three read outcomes:

- **no state file → dispatch.** `.cenci/pipeline/` is gitignored and expendable; "no
  pipeline run here" is the normal case and must not block dispatch. This is a
  deliberate, documented permissive exception — the only one in this gate chain.
- **readable, known stage → gate on it.** Only `finalized` skips; every other known
  stage (`prepared`, `waiting_for_input`, `waiting_for_plan_approval`, `plan_approved`,
  `executed`, `reviewed`) dispatches normally.
- **unreadable, undecodable, or an unrecognized stage value → skip.** Default-deny:
  broken input is treated as blocking, not as absent input.

The two operator-visible skip reasons are `pipeline finalized (reset to
re-dispatch)` and `pipeline state unreadable`. `cenci pipeline reset <id>` (see
[Mechanics verbs](#mechanics-verbs-label-worktree-worktree-cleanup-artifact-plan-check-reset))
is the remedy for both — it deletes the ticket's state file, so a `finalized` ticket
with no PR (or a corrupted state file) becomes dispatchable again. Set
`dispatch.pipelineStageGate` to `false` to disable the gate entirely (see
[Configuration](#configuration)).

**Limitation:** the probe reads `dispatch.repos[].dir` verbatim. If `dir` points at a
linked worktree rather than the ticket's main checkout, the probe will not see the
main checkout's pipeline state (the state file lives at the main checkout's git
root) and the gate fails open there — matching pre-#732 behavior.

#### Plan inventory

Every dispatch pass enumerates each enrolled repo's `.plans` directory exactly once
per repo, sharing one authoritative contract (`internal/planfile`) with manual
`cenci pipeline plan-check`, resume-answer probing, and reconciliation, so all four
consumers select the same plan file or report the same failure — never disagree about
which plan belongs to a ticket. The directory read resolves to one of five states:

- **absent** — `.plans` does not exist at all. Verified absence.
- **empty** — `.plans` exists but holds zero entries. Verified absence.
- **complete** — the directory was fully enumerated; every entry's health and
  identity is known.
- **unreadable** — `.plans` could not be opened at all (permission denied, or the
  path is not a directory). **Holds every ticket in the repo.**
- **partial** — enumeration started but failed partway through. The entries read
  before the failure are known, but completeness is not proven. **Holds every
  ticket in the repo,** exactly like unreadable.

Only `absent` and `empty` are verified absence — the only two states that permit a
fresh `Refined` planning pickup (see [Planning pickup and autonomous
re-plan](#planning-pickup-and-autonomous-re-plan) below). `unreadable` and `partial`
gate rule 1.5 in [Pickup rules and gates](#pickup-rules-and-gates): a directory that
could not be fully read can never prove that no second file also claims a given
ticket, so it holds ordinary `Planned` dispatch, resume, and planning pickup alike,
for every ticket in that repo — not merely the ticket whose own plan file happened to
be unreadable.

Within a `complete` (or `partial`) read, each entry's identity requires the file's
numeric filename prefix (`.plans/<id>-<slug>.md`) and its front-matter `ticketId` to
agree — front matter alone is no longer authoritative. A file whose filename and front
matter disagree (or whose front matter carries no `ticketId` at all — a legacy plan
pre-dating the field is no longer exempt) holds **both** the filename's claimed ticket
and the front matter's claimed ticket, with the same "plan file ticket identity
mismatch" reason. Two or more claims on one ticket — any mix of healthy and broken
files — are an ambiguity hold, never resolved first-wins and never silently collapsed
into a healthy single, regardless of directory sort order. A file with no numeric
filename prefix at all (a legitimate ticketless plan, `.plans/<slug>.md`) is logged as
a bounded anomaly but holds nothing — it never gates a real ticket.

#### Open-PR inventory completeness

Ticket #881 replaces the pre-#881 single capped `gh pr list --limit 200` call
(which silently treated a hit cap as complete) with a bounded, cursor-paginated
`gh api graphql` traversal of the repo's open PRs' `closingIssuesReferences`,
100 PRs per page, up to 20 pages (2,000 PRs total) per repo per dispatch/reconcile
pass. In a repo with more than 200 open PRs, a linked PR outside the old
capped window could be misclassified as "no open PR" and dispatched again,
creating duplicate implementation work — this traversal walks every page
instead of stopping at the first one.

`pageInfo.hasNextPage` — not the cheaper `totalCount` pre-flight check — is the
authoritative completeness signal: `totalCount` only short-circuits a
pathologically large repo to one call without wasting the full page budget
proving what a single number already answered. Explicit `orderBy: {field:
CREATED_AT, direction: ASC}` cursor paging keeps the traversal deterministic;
correctness never depends on that order. A partial inventory (the traversal
stopped early) still counts every PR actually seen toward `HasOpenPR` — strictly
safer than discarding partial results — while the completeness gate below fires
regardless, so an incomplete read never quietly reads as "no PR".

When completeness cannot be proven, gate 2 holds **every** ticket in the
affected repo (never just the tickets a problem PR happens to reference) with
one of five distinct reasons:

- `open PR state incomplete: pagination cap exhausted` — either the `totalCount`
  pre-flight exceeded the 2,000-record bound, or the 20-page bound was reached
  while the server still reported more pages remaining. The repo legitimately
  has more open PRs than this probe is bounded to traverse.
- `open PR state incomplete: closing-issue references truncated` — at least one
  PR's own `closingIssuesReferences` connection (bounded at 50 per PR) itself
  overflowed. One pathological PR closing more than 50 issues holds the whole
  repo, not just its own linked issues — an operator-visible hold, not silent
  truncation.
- `open PR state unreadable: malformed page` — a non-JSON response body, a
  GraphQL `errors[]` payload, a response exceeding the probe's bounded stdout
  cap, or a page reporting more results are available but with no cursor to
  advance on.
- `open PR state unreadable: probe timed out` — a `gh api graphql` call was
  killed by its bound before completing.
- `open PR state unreadable: pagination failed` — an ordinary `gh` command
  failure partway through pagination (e.g. a rate limit) — the traversal broke,
  not merely hit a bound.

The reconciler defers identically: at both the crash-cleanup and ordinary
failure label-mutation sites, a non-complete probe blocks the label mutation
entirely (preserving, never dropping, the grace-period clock) rather than
trusting an unverified `HasOpenPR` — see [Open-PR-inventory-completeness defer
in `watch/docs/dispatch-reconcile.md`](docs/dispatch-reconcile.md#open-pr-inventory-completeness-defer-ticket-881).

**Remedy for a repo legitimately above the pagination bound:** there is no
config knob to raise it — a repo with more than 2,000 open PRs (or a single PR
closing more than 50 issues) needs its open-PR count or linkage brought back
under the bound (e.g. by closing/merging stale PRs) before this gate clears.
Dry-run and real dispatch consume the exact same completeness verdict, so
`--dry-run` surfaces the same hold an operator would otherwise only discover
mid-pass.

#### Escalation auto-resume

Ticket #827 teaches dispatch to auto-resume an `Input Needed` ticket (the unattended
planner's escalation label, ticket #826) once a human answers its question, rather
than waiting for a fresh `/cenci:implement` run to pick the draft back up manually.
Ticket #849 hardens the anchor identity this resume depends on: rather than scanning
the comment thread for the last comment that *looks like* an anchor, each `Input
Needed` ticket's persisted plan front matter carries the exact identity of its own
escalation comment — a per-escalation nonce (`escalationNonce`) plus the immutable
numeric comment ID the REST comments API returned when the question was posted
(`escalationCommentId`).

Each pass runs a targeted, per-ticket REST probe for every `Input Needed` ticket —
`gh api "repos/<owner>/<repo>/issues/<n>/comments?per_page=100" --paginate`, never a
bulk fleet-wide read — and classifies the comment thread by exact ID: the anchor is
the comment whose numeric `id` equals the plan's stored `escalationCommentId`, and it
is verified by confirming its blockquote-stripped body contains the exact marker
`<!-- cenci-planner-escalation:<escalationNonce> -->` (blockquote lines stripped
first, so GitHub's "Quote reply" copying the marker verbatim into a reply is never
misread as a second anchor). A comment counts as a human answer iff it is positioned
after that verified anchor, its own body — also blockquote-stripped — contains no
`<!-- cenci-` marker, its author login is neither `*[bot]` nor `app/*` and its
`user.type` is not `"Bot"` (the REST API's first-class bot flag, replacing the
pre-#849 login-shape-only heuristic), its author association is one of `OWNER`,
`MEMBER`, or `COLLABORATOR`, AND (#882) the replying login currently holds `admin`
or `write` permission on this repository, resolved via the authoritative
`gh api repos/<owner>/<repo>/collaborators/<login>/permission` endpoint (its
top-level `permission` field is the sole accepted signal — `role_name` is never
consulted, since custom organization roles would make it an open set). The
association check alone is now a coarse, cheap prefilter — load-bearing only in
that it gates whether the write-permission probe is even called, never final
authorization on its own: `CONTRIBUTOR`, `FIRST_TIME_CONTRIBUTOR`, and `NONE`
associations are still never authorized, but an `OWNER`/`MEMBER`/`COLLABORATOR`
association no longer suffices by itself either. A read/triage collaborator, a
removed collaborator, or an organization member without this repository's current
write access is denied even with a qualifying association — this closes the gap
where `MEMBER` meant organization membership without repo access, and
`COLLABORATOR` association did not itself prove current `push`/`maintain`/`admin`.
Permission is resolved fresh from the authoritative endpoint every pass, cached
only within that one pass (deduplicating repeat answers from the same login,
bounded per repository), and never carried across passes — a login's earlier
authorization is never treated as still current. Any permission-probe failure —
an API error, a timeout, truncated output, malformed JSON, a missing `permission`
field, or an unrecognized value — fails closed with its own distinct reason (see
the reason list below), exactly like an unresolved comments probe. This requires
the dispatch daemon's own `gh` token to itself have push access to every enrolled
repository, since the collaborator-permission endpoint requires it. See
[Cenci-authored comment markers in
`watch/docs/dispatch-reconcile.md`](docs/dispatch-reconcile.md#cenci-authored-comment-markers)
for why every comment cenci posts must carry one of these markers.

When answered, gate 2 dispatches the ticket exactly like an ordinary `Planned` pickup
— every other gate (assignee, dependency, sibling, capacity, budget, quiet hours)
applies identically — except: gate 2 accepts `Input Needed` in place of `Planned`,
gate 3 requires the matched plan's `status: awaiting-input` (the persisted draft the
unattended planner stopped on) rather than `status: planned`, and gate 4 (plan
freshness) is skipped entirely, mirroring `CheckPlan`'s own
awaiting-input-before-staleness short-circuit — a draft that waited days on a human
is almost always commits-behind, and gating on that would mean it could never
re-resume. `applyDispatch` swaps the `Input Needed` label for `Working` **before**
relaunching (not after, the way an ordinary dispatch claims its label) — a failed
swap skips the spawn entirely, since a failed *post*-spawn claim on the ordinary path
is safely recovered by the reconciler next pass, but here it would leave the ticket at
`Input Needed` forever and re-resume it every subsequent pass.

The relaunched session re-delegates to the `planner` agent against the same draft
(its `## Architectural Context` is treated as prior exploration — no re-exploration),
appends the human's answers to the ticket's `### Decisions`, and either finalizes the
plan (`status: planned`, back onto the ordinary dispatch rail next pass) or
re-escalates on an incomplete answer rather than guessing. A resumed decision renders
in the log/`--dry-run` table exactly like any other dispatch:

```
owner/name#42 dispatch (claude, 42-slug.md): resume — human answered
```

Every other probe outcome skips with its own distinct reason: `escalation still
awaiting a human answer` (no qualifying comment yet), `escalation answer probe
failed` (the `gh` call itself errored, returned malformed JSON, or its
`--paginate` payload exceeded the probe's stdout cap), and `escalation answer
probe unrecognized` (an internal default-deny case). Two reasons are specific to
the anchor itself (#849): `escalation anchor missing or malformed` fires when the
matched plan never recorded a usable `escalationNonce`/`escalationCommentId` pair
at all (nonce failing its `^[0-9a-f]{32}$` format, or a comment ID that is absent
or not a positive integer); `escalation anchor comment not found or nonce
mismatch` fires when the pair is well-formed but the stored comment ID can't be
found in the thread, or the comment at that ID doesn't carry the exact nonce
marker. Both are fail-closed exactly like every other probe outcome — dispatch
never repairs an anchor itself, it only reports the gap; repair is exclusively a
human-triggered `/cenci:implement <id>` run (see `## Repair Escalation Anchor` in
`flow/skills/implement/phases/phase-1-plan.md`). `draft not awaiting input` fires
when the matched plan's `status` isn't `awaiting-input` (e.g. it was already
finalized).

**Write-permission reasons (#882).** Once a candidate reply passes the cheap
association prefilter, its author's CURRENT repository write permission must also
positively resolve `admin` or `write`; every other outcome fails closed with its
own distinct reason, never collapsed into a single generic denial: `replying login
lacks current repository write permission` (the endpoint positively returned
`read`, `triage`, or `none`), `replying login shape invalid` (the login failed
GitHub's own login grammar before ever reaching the endpoint — a path-injection
guard), `write-permission probe failed` (the `gh api .../permission` call itself
errored), `write-permission probe timed out`, `write-permission probe response
truncated` (the response exceeded its bounded stdout cap), `write-permission probe
response malformed` (undecodable JSON), `write-permission probe response missing
permission field` (e.g. a response carrying only `role_name`, never consulted as a
substitute), `write-permission probe returned unrecognized value` (a future/unknown
GitHub permission value, or the internal default-deny case), and `write-permission
lookup cap reached` (the per-pass, per-repository lookup budget was exhausted).
When several post-anchor candidates produce different non-authorized outcomes, an
unresolved/error class is reported over a positively-denied one — both still deny
resume, only the operator-facing reason differs, since an unreliable verdict is not
the same as a resolved deny.

**Bot detection now has a real flag, not just a login-shape guess** (#849): the REST
comments API's `user.type == "Bot"` field is checked alongside the pre-existing
`*[bot]`/`app/*` login-shape heuristic (`gh issue view --json comments`, the
pre-#849 call, exposed only `author.login`, no dedicated bot flag). A self-hosted
automation posting under a plain user login with `user.type` still `"User"` would
still be misread as a human reply and trigger a resume — bounded impact, same as
before: the flow side then finds no answers to the open questions and re-escalates
rather than guessing.

#### Planning pickup and autonomous re-plan

> Turning this on, and what it combines with: [The autonomous
> loop](../docs/autonomous-loop.md). This section is the exhaustive gate reference.

Ticket #828 makes gate 2 stage-aware: with `dispatch.planRefined: true` (default
`false`), a `Refined` ticket with no matched plan file becomes a *planning pickup*
instead of a terminal `not Planned` skip, and a stale `Planned` plan becomes an
autonomous *re-plan* instead of a terminal `plan stale, re-plan` skip. Both launch
`cenci run implement`; every other gate (assignee, dependency, sibling
serialization, capacity, budget, quiet hours) applies identically to an ordinary
`Planned` pickup, because it is literally the same gate chain — see [Ticket
dependency gate](#ticket-dependency-gate) above.

**Lean-planning repos only.** `dispatch.planRefined` remains the fleet-wide kill
switch, but it is no longer sufficient authorization on its own (#851): after the
local main sync above, dispatch also reads the repo's own committed
`.cenci/config.json` at the remote-confirmed `AutonomyRef` (via `git show
refs/remotes/origin/main:.cenci/config.json`, never local `HEAD` and never the
working tree) and requires the literal value `planning.autonomy == "lean"`
before treating a planning pickup or autonomous re-plan as authorized. A
missing, unreadable, malformed, or non-`"lean"` repo config denies both with its
own distinct skip reason — exactly as if `dispatch.planRefined` were `false` for
that repo. Enabling `dispatch.planRefined` fleet-wide can no longer override a
repo that hasn't itself opted into lean planning.

A machine can additionally narrow its own lean repos further with the
fleet-wide [`planning.attended` switch](#attended-mode-cenci-planning-attended-onoffstatus)
(#1086): when on, a lean repo's planning pickups and autonomous re-plans are
suppressed on that machine specifically, with their own distinct skip reason
naming attended mode as the cause — for when a human is sitting at this
machine's keyboard and could just answer a clarifying question instead.

**Remote-confirmed authorization (#877).** The config read above requires a
successful `git fetch origin` in *this pass* — it is never read from local
`HEAD`, and a repo whose local `main` checkout is ahead of `origin/main` with
unpushed commits cannot use its own unpushed config to grant (or revoke)
authorization; only the fetched remote object counts. Symmetrically, a lean
grant that was revoked on the remote (`origin/main`'s config no longer says
`"lean"`) is honored immediately, even if the local checkout's last successful
pass still has `"lean"` cached in its working tree or `HEAD`. **User-visible
behavior change:** when `git fetch origin` fails this pass
(`MainSyncFetchFailed` above), planning pickup and autonomous re-plan are held
with a distinct retryable reason and launch nothing for that repo — where they
previously fell back to whatever `planning.autonomy` local `HEAD` last had
committed. Ordinary `Planned` pickup in the same repo is unaffected by the same
fetch failure; only freshness-dependent planning/re-plan authorization is gated
by it.

**Trust boundary.** Enabling `dispatch.planRefined` treats the `Refined` label
(plus the existing assignee-ownership gate) as sufficient authorization to
launch an unattended planning session whose primary input is the ticket's own
body/comment text. Unlike the [escalation auto-resume](#escalation-auto-resume)
path (#827/#882), which requires the human reply's author to currently hold
`admin` or `write` repository permission (resolved via the authoritative
collaborator-permission endpoint, not merely an `authorAssociation` value read
off the comment), there is no author-authorization check on the issue text a
planning pickup consumes. Do not enable this flag on a repo that accepts
externally-authored issues from untrusted parties.

**Launch shapes.** A planning pickup launches with the bare ticket number
(`cenci run implement <n>`) since there is no plan file yet for the implement
session's Phase 1 to discover. An autonomous re-plan appends the
`--replan-requested` escape hatch (`cenci run implement "<n> replan"`) so the
unattended session's stale-plan branch skips Phase 1's human-confirmation gate
instead of hanging as a `Working` ticket waiting on a prompt that will never
come. Both claim `Working` synchronously after a successful spawn, exactly like
an ordinary dispatch.

**Sibling-serialization limitation (accepted, documented).** Gate 6 (sibling
serialization) is derived entirely from the plan file's `isChild`/`parentId`
front matter, which doesn't exist yet for a `Refined`-with-no-plan ticket, so it
is inert for a planning pickup. A parent with several `Refined` children can have
planning sessions launched for multiple siblings in the same pass, up to
`concurrencyCap`. The existing ticket dependency gate still serializes
any chain the refiner actually declared, and `concurrencyCap`/`dailyQuota` bound
the rest — there is no new sibling-detection logic for this case.

**Unbounded re-plan (accepted, documented).** Nothing caps how many times a
ticket can be autonomously re-planned; a successful re-plan rewrites
`planCommitSha`, which self-limits the common case, but a repo with an
over-broad `stalenessPaths` value can re-plan repeatedly. `dailyQuota` and
`concurrencyCap` are the rate limiter, and raising `planStalenessTolerance`
raises the trigger threshold.

A crashed planning pickup (a `Working` ticket that still carries `Refined`, not
`Planned`, with no plan file) is recovered by the reconciler back to plain
`Refined` rather than `Planned` — see [Failure
reconciliation](#failure-reconciliation) below.

#### Path-aware staleness

In a monorepo, unrelated commits elsewhere in the tree shouldn't invalidate a plan
scoped to one project. A plan's front matter may set an optional flat key
`stalenessPaths` — comma-separated, repo-relative paths (e.g.
`stalenessPaths: watch` or `stalenessPaths: watch, flow`). When
present, gate 3 above counts only default-branch commits that touch those paths
(`git rev-list --count <planCommitSha>..HEAD -- <paths...>`); when absent, it falls
back to whole-repo commit counting as before. The `cenci` plan template (see its
`/implement` plan phase) records this field from the project directories the plan
touches.

#### Ticket dependency gate

A ticket declares its blockers using **GitHub's native issue dependencies** — the
"Blocked by" relationship in the issue sidebar's Relationships section. This is the
preferred form and the one `/cenci:refine` writes:

```bash
gh issue edit <ticket> --repo <owner>/<repo> --add-blocked-by <blocker>
```

Native links cost nothing extra to read: each blocker's state arrives inline with
the ticket in the collector's single `gh issue list --json ...,blockedBy` call, so
no follow-up API call is made per dependency. This requires **gh >= 2.94.0**; an
older gh does not know the `blockedBy` JSON field and the collector fails the pass
with an explicit upgrade message rather than silently ignoring every native link.

For **in-flight tickets refined before native links existed**, the legacy body-text
form is still read: a line-anchored `Depends on #N` reference — case-insensitive,
tolerant of an optional leading `- `/`* ` list marker and arbitrary trailing text
(e.g. `- Depends on #822 (local main sync) since ...`). `Related to #N` and
`Parallel with #N` never match — only the literal `Depends on` phrase gates
dispatch. Only bare same-repo `#N` syntax is recognized; cross-repo `owner/repo#N`
references are out of scope. A ticket is gated on the **union** of both sources.

For a legacy prose reference, the issue's openness resolves against the pass's own
collected open-issue set first (no extra API call); a number outside that set falls
back to a `gh issue view` call, memoized once per repo per pass so N tickets
depending on the same out-of-window issue cost exactly one call, not N (bounded by
a per-pass call cap to protect against a ticket body listing an unbounded number of
dependencies). A number declared both natively and in prose is never resolved
twice — the native state wins and no fallback call is made.

Gate 5 above blocks the ticket while any dependency is still open
(`waiting on dependency #N`) and fails closed with a distinct reason
(`dependency #N unresolved`) when a state can't be determined at all.

Because `DependsOn` is keyed by bare issue number, a native link is only usable
when it points into the **same repo**. GitHub permits blocked-by links to other
repos in the same organization, but such a link's number would collide with a
same-numbered local issue, so it is refused rather than mis-keyed: the ticket is
gated with `native dependency unusable: "..."` naming the offending URL. The same
fail-closed reason covers a blocker list GitHub returned only partially.

This gate blocks both planning pickup and implementation pickup (see [Planning
pickup and autonomous re-plan](#planning-pickup-and-autonomous-re-plan) above,
ticket #828) — a plan written before its dependency merges is considered stale on
arrival by gate 4's path-aware staleness check in the common case (a same-repo
dependency touching shared files), so there's little practical benefit to
planning before a dependency resolves.

### Configuration

Dispatch reads the same `config.json` as `run`, under a top-level `"dispatch"` block
(defaults apply when it is absent):

```json
{
  "dispatch": {
    "repos": [
      { "repo": "owner/name", "dir": "/path/to/repo", "session": "a-work" }
    ],
    "concurrencyCap": 3,
    "needInputThreshold": 1,
    "dailyQuota": 20,
    "quietHours": { "startHour": 22, "endHour": 7 },
    "planStalenessTolerance": 5,
    "pipelineStageGate": true,
    "planRefined": false,
    "gracePeriod": "5m",
    "retryBudget": 2,
    "daemonInterval": "5m",
    "defaultAgent": "claude",
    "model": "claude-sonnet-5",
    "agentPreference": ["claude", "codex"],
    "agentBudgetFloors": { "claude": 0.1, "codex": 0.1 },
    "agentLimits": {
      "claude": { "fiveHourTokens": 20000000, "weeklyTokens": 300000000 },
      "codex":  { "fiveHourTokens": 15000000, "weeklyTokens": 200000000 }
    }
  }
}
```

| Key | Default | Purpose |
|-----|---------|---------|
| `repos` | — | Repos to scan; each entry requires `repo`, `dir` (holds that repo's `.plans/` and git tree), and `session` (the tmux session that repo's dispatched windows target — never an ambient/current session). Normally managed via `cenci dispatch enroll`/`unenroll` (see [Enrollment](#enrollment-cenci-dispatch-enrollunenrollstatus)) rather than hand-edited, though hand-editing remains supported. A repo whose `session` is empty or names a tmux session that doesn't exist is skipped entirely for that pass (`dispatch: no target tmux session for <repo>; set repos[].session in <config path>`, or the equivalent "not found in tmux" line naming the remedy `tmux new-session -d -s <name>`) — every other enrolled repo still dispatches normally in the same pass. |
| `concurrencyCap` | `3` | Max concurrent running sessions (counts in-flight windows plus this pass's dispatches) |
| `needInputThreshold` | `1` | Pause dispatch when at least this many windows await input |
| `dailyQuota` | `20` | Max dispatches per process run (resets on restart) |
| `quietHours` | none | Local-clock window to suppress dispatch; `startHour > endHour` wraps midnight, `start == end` disables |
| `planStalenessTolerance` | `5` | Max commits a plan may fall behind before it is skipped as stale (see [Path-aware staleness](#path-aware-staleness) for scoping the count via `stalenessPaths`) |
| `pipelineStageGate` | `true` | Skip tickets whose persisted `cenci pipeline` stage is `finalized` (see [Pickup rules and gates](#pickup-rules-and-gates)); set `false` to disable the gate entirely |
| `planRefined` | `false` | Enable stage-aware planning pickup of `Refined` tickets and autonomous re-plan of stale plans, in lean-planning repos only (see [Planning pickup and autonomous re-plan](#planning-pickup-and-autonomous-re-plan)); the fleet-wide kill switch, gated per-repo against that repo's own remote-confirmed `planning.autonomy` config (#877); managed via `cenci dispatch plan-refined on\|off\|status` (see [Planning-pickup toggle](#planning-pickup-toggle-cenci-dispatch-plan-refined-onoffstatus)) |
| `gracePeriod` | `5m` | How long the failure signal must hold continuously before the reconciler recovers a stranded ticket (Go duration string) |
| `retryBudget` | `2` | Retries (`Working` → `Planned`) a stranded ticket gets before it is marked `dispatch-failed`; an explicit `0` disables retries |
| `daemonInterval` | none | Dispatch cadence once the embedded loop is enabled (Go duration string); setting this alone does **not** start dispatch — see `loopEnabled`. Configuration is independently polled at least every 60 seconds; nonpositive values use a 60s internal fallback but are not reported as a configured interval |
| `loopEnabled` | `false` | Explicitly toggles the embedded fleet dispatch loop; managed via `cenci dispatch loop on\|off` (see [Loop toggle](#loop-toggle-cenci-dispatch-loop-onoffstatus)). Defaults to disabled — a bare `daemonInterval` no longer auto-enables the loop; run `dispatch loop on` (or set `loopEnabled: true` directly) to start dispatching |
| `defaultAgent` | `claude` | Agent used when a ticket has no `agent:<name>` label |
| `model` | none | Model override for every dispatched session (overrides `agents.*.model`); the `--model` CLI flag overrides this. Pin this to avoid a dispatched session silently inheriting whatever ambient/account-level default model is active at spawn time |
| `agentPreference` | none | Fallback agent order tried when the primary agent (label or `defaultAgent`) is out of budget; first agent with budget wins |
| `agentBudgetFloors` | none | Per-agent budget floor (see [Usage budgets](#usage-budgets)); with `agentLimits` it is a headroom safety margin, without it a static `Remaining` where `0` pins the agent to "budget exhausted" |
| `agentLimits` | none | Per-agent token caps enabling real usage accounting; each agent takes `fiveHourTokens` and/or `weeklyTokens` (omit or `0` to disable that window) |
| `claudeSessionDir` | `~/.claude/projects` | Override for the Claude session-JSONL directory scanned for output-token usage |
| `codexDBPath` | `~/.codex/state_5.sqlite` | Override for the Codex SQLite DB queried for per-thread token usage (requires the `sqlite3` CLI) |

An agent is routed per ticket from an `agent:<name>` label, falling back to
`defaultAgent`, then to the `agentPreference` list.

A separate top-level `"planning"` block (sibling to `"dispatch"`, not nested
inside it) carries the fleet-scoped [attended mode](#attended-mode-cenci-planning-attended-onoffstatus)
switch:

```json
{
  "planning": {
    "attended": false
  }
}
```

| Key | Default | Purpose |
|-----|---------|---------|
| `attended` | `false` | Fleet-wide narrowing switch: when `true`, suppresses unattended `Refined` planning pickups and autonomous re-plans for every lean-planning repo on this machine (see [Planning pickup and autonomous re-plan](#planning-pickup-and-autonomous-re-plan)); managed via `cenci planning attended on\|off\|status` (see [Attended mode](#attended-mode-cenci-planning-attended-onoffstatus)). **Distinct from** the repo-committed `planning.autonomy` key in each repo's own `.cenci/config.json` — same parent block name, different file, different purpose; each file silently ignores the other's key |

### Usage budgets

Each candidate agent must clear a budget gate before it can be dispatched. There are two
modes, chosen per agent by whether `agentLimits` is configured:

- **Real usage accounting** (when `agentLimits[agent]` is set) — cenci reads the
  agent's own local session data to compute how much of each rolling window remains:
  Claude from output-token counts in its session JSONL (`claudeSessionDir`), Codex from
  per-thread `tokens_used` in its SQLite DB (`codexDBPath`, via the `sqlite3` CLI). The
  tightest window's headroom (`0.0`–`1.0`) minus the agent's `agentBudgetFloors` value
  is the remaining budget; a positive value passes. Set the floor as a safety margin to
  stop dispatching before the true cap. A missing/unreadable data source is treated as
  the safe direction (no budget) rather than dispatching blind.
- **Static floor** (no `agentLimits[agent]`) — the `agentBudgetFloors` value is used
  directly as the remaining budget, so `0` pins the agent to "budget exhausted" and any
  positive value lets it dispatch. An agent with neither a limit nor a floor is
  unlimited.

**OpenCode:** no `TokenReader` is wired for OpenCode yet — `buildBudgetProvider`
only builds one for `claude` and `codex`. So once any agent's `agentLimits` is
configured, OpenCode falls into the no-reader path: it uses its
`agentBudgetFloors.opencode` value if set, otherwise it is always `Unlimited`.
Real usage-based accounting for OpenCode is future work. The same gap means
OpenCode is always omitted from the per-agent headroom map, so `cenci
widget-json`'s headroom percentage (see [Budget headroom](#budget-headroom))
never includes it — OpenCode shows no headroom until a provider-specific
reader is added in a future ticket.

### Failure reconciliation

Auto-pickup alone stalls silently on failure: a dispatched session that dies mid-flight
leaves its ticket in `Working`, and pickup requires `Planned`, so the ticket is stranded
forever. Reconciliation is the recovery half — a pass that detects stranded work and
either re-queues or surfaces it.

A `Working` ticket is treated as stranded only when **all** hold: it has no live tmux
window (gone, or `stopped`), no open linked PR, and that signal has held continuously for
`gracePeriod`. When it strands:

- **under `retryBudget`** → `Working` → `Planned` plus an attempt comment; the plan file
  still exists, so the ticket re-enters the dispatch queue naturally;
- **at `retryBudget`** → `Working` → `dispatch-failed`, surfaced for a human and never
  touched again.

**Crashed planning pickup (ticket #828).** A `Working` ticket that still carries
`Refined` with no matched plan file is a crashed *planning*
pickup (`Planned` may or may not also be present — see
[Planning pickup and autonomous
re-plan](#planning-pickup-and-autonomous-re-plan) above), not a crashed
implementation run. Recovering it with the ordinary `Working` → `Planned` retry
above would dead-end it at `plan-invalid` next pass (`Planned` added, still no
plan file). Instead, under `retryBudget` it recovers with only `Working` removed
— no label added — so it returns to plain `Refined` and is re-picked as a
planning candidate next pass. At `retryBudget` it still escalates to
`dispatch-failed` exactly like any other stranded `Working` ticket; only the
retry branch's label payload is stage-aware.

The inverse leak — a `Planned` ticket whose planned plan file cannot be read — becomes
`plan-invalid` (also grace-gated, to tolerate a plan that is mid-write or has not yet
synced). An orphan `.plans/` file whose ticket is not open is reported in the log only, no
mutation.

`dispatch-failed` and `plan-invalid` tickets have no tmux window, so the reconciler feeds
the daemon synthetic `failed` entries that the status output and the noctalia/dms widgets
render loud (`failed` outranks every other state). Attempt counts are stored as durable
hidden-marker comments on the ticket, so recovery survives cron invocations and daemon
restarts. A pass never acts blind: if the daemon snapshot or a ticket's attempt count
cannot be read, it defers rather than guess.

**Escalated tickets (ticket #826).** A ticket labeled `Input Needed` (the unattended
planner's escalation label, applied by `cenci pipeline label <id> --transition
input-needed`) is recorded separately and never touched again: no recovery is
attempted, and it is never counted into the `Failed` list above. This is deliberate —
the escalating run stopped cleanly (it is not a stranded/crashed session), so treating
it as a dispatch failure or retrying it would be wrong. The reconciler feeds the
daemon a synthetic `escalated` entry (distinct status from `failed`), which the status
output counts into `StatusSummary.Escalated`, never `StatusSummary.Failed` or
`StatusSummary.NeedInput`.

`Input Needed` is no longer only a reconciler no-op, though (ticket #827): it is also
now a *dispatch* input. The reconciler's own handling above is unchanged — it still
never touches an `Input Needed` ticket — but each dispatch pass separately probes it
for a human answer and auto-resumes it once one arrives; see [Escalation
auto-resume](#escalation-auto-resume) above.

Run it two ways:

```bash
# Cron path: one recovery pass per invocation
cenci dispatch --reconcile

# Daemon-embedded: set dispatch.daemonInterval and the daemon runs the combined
# dispatch + reconcile loop itself, on that interval
```

> **Host requirement:** reconciliation reads each repo's local `.plans/` directory, so run
> it on the host where plans are persisted. A `Planned` ticket whose plan file lives only
> on another host is grace-gated but will eventually be marked `plan-invalid`.

**Corrupt reconciliation state (ticket #883).** Reconciliation's grace-observation and
apply-retry-counter state lives at
`$XDG_STATE_HOME/cenci/reconcile.json` (falling back to `~/.local/state/cenci/reconcile.json`
when `XDG_STATE_HOME` is unset). It is safety state, not disposable cache — resetting it
resets every ticket's grace clock and apply-retry budget, which can trigger premature or
indefinitely repeated recovery decisions.

Every save is crash-safe: a same-directory randomized temp file is written, fsynced, and
atomically renamed over the final path (fsyncing the directory too), so a crash mid-save
always leaves either the previous complete state or the new complete state, never
truncated JSON.

Any non-absence read failure — unreadable (permission/IO), malformed/truncated JSON,
unknown/unsupported schema version, or integrity-invalid (empty ticket key, zero-value
observation timestamp, or a negative apply-failures counter) — holds the **entire
reconcile pass**: no GitHub label edits or comments are applied, the corrupt file is
never overwritten by a later save in the same pass, `last_error` reads
`reconcile_state_unreadable`, and the failed/escalated badges from the last successful
pass are retained rather than cleared (a held reconciler must never render as "all
healthy" in `cenci status`/waybar/noctalia). A missing state file is **not** corruption —
it is valid empty initial state, so a first run still reconciles normally. The **dispatch
pass continues independently**: reconciliation's hold never blocks new ticket pickup.

**Manual recovery:** inspect `reconcile.json`, then either restore a known-good backup or
delete the file. Deleting it resets grace clocks and apply-retry counters, but durable
attempt counts survive as hidden-marker ticket comments, so the retry budget itself is
not lost. `cenci dispatch --reconcile` exits nonzero the entire time the hold is active.

## Closing agent windows (`cenci close`)

External tools that clean up finished agent windows (for example a kanban board's
"column cleanup" hook) need to kill the *exact* tmux window an agent is running in —
not just a same-named window in whatever session the caller happens to be in. A
bare `tmux kill-window -t =<window-name>` only resolves within the caller's own
tmux session, so it silently no-ops against windows running elsewhere, or — if you
run one tool instance per session — ends up reaping only that instance's own
session's windows instead of the intended target.

`cenci close` fixes this by resolving the target from the daemon's live window
registry instead of guessing a tmux target:

```bash
# Close every window for ticket 42 (matches "42" or "42-<skill>"), skipping any
# that are currently running or waiting for input
cenci close 42

# Close a free-text/slug window by its exact name
cenci close add-dark-mode-toggle

# Preview what would happen without killing anything
cenci close 42 --dry-run

# Close even running/need-input windows
cenci close 42 --force
```

Behavior:
- Reads a single snapshot from the daemon; if the daemon is unreachable, it exits 1
  and kills nothing (fail-safe).
- A numeric target matches every window named exactly that number or prefixed
  `<number>-` (e.g. `42` matches `42` and `42-refine`, but not `420-anything`).
  A non-numeric target matches windows by exact name.
- Windows whose status is `running` or `need-input` are skipped (and reported)
  unless `--force` is given. The daemon remembers the skip and closes the window
  itself once it observes that session end, so a caller never needs to retry —
  no second `cenci close` invocation is required.
- Windows whose ticket is still owned by a live `cenci babysit` supervisor whose
  PR's CI is not green are skipped too, again unless `--force` is given:

  ```
  skip 782-implement (main:3): babysit supervising PR #790, CI not green — will close once CI passes (or use --force now)
  ```

  This covers the window `/cenci:implement` leaves behind — Phase 9 arms the
  supervisor *before* relabeling the ticket to `In Review` (which fires a
  board's cleanup hook), so the guard is already live before the label swap
  can move the card, and the session is already idle while CI is still
  running. The daemon applies the same guard to a deferred pending-close,
  re-checking it on its sweep instead of killing at session end, and closes
  the window on its own once the guard clears.
- No matching windows is not an error — it exits 0 with no output, so it's safe
  to run unconditionally after a window may already be gone.

Caveats on the babysit guard:
- The guard is default-deny: a close is allowed only once CI reads fully
  green. Any other verdict — failing, pending, unknown, or "never polled" —
  blocks, subject to the settle-grace escape below.
- It reads `cenci babysit`'s state files (`$XDG_STATE_HOME/cenci/babysit`) and
  makes **no** network calls — a board refresh may run `cenci close` constantly.
  That state is refreshed once per supervision interval (default `15m`), so a
  window can stay closable-but-open for up to one interval after CI turns green.
- Every *state-file* read failure (no state directory, unreadable or corrupt
  file) fails *open*: the window closes. A machine that never runs
  `cenci babysit` behaves exactly as it did before. This is distinct from a
  *live* supervisor recording CI status `unknown` in the state file — because
  the PR has zero checks configured yet, because the supervisor's own
  `gh pr view`/`gh pr checks` call is genuinely failing (not a benign "checks
  still pending" or "no checks reported" shape), or because the checks that do
  exist are all in an unusable bucket (`cancel`, an empty bucket string, or a
  value the guard doesn't recognize). All three causes hold the window closed
  for up to `checksSettleGrace` (10 minutes) from when `unknown` was first
  observed for the current commit — not unbounded — after which the guard
  stops blocking even if the supervisor's polls keep failing, so a lost
  network connection, bad `gh` auth, or a stray unrecognized check bucket
  cannot wedge the window open forever with no self-heal. The unusable-bucket
  cause is new behavior: such a PR used to read as green and close
  immediately, and now instead blocks for up to that same 10-minute window.
  Use `cenci close --force` to close anyway, or `cenci babysit stop <pr>` to
  stop the supervisor sooner.
- `cenci close` scopes the match to the current checkout's repo root, but the
  daemon's deferred re-check has no repo context (a registered pending-close
  carries none) and matches on ticket number alone. Two repos babysitting PRs
  that close the same issue number concurrently cross-match; the only effect is
  a window staying open longer, and `--force` overrides.
- Arming from inside a sandboxed `/cenci:implement` run forwards to the host
  daemon instead of forking a supervisor locally; the daemon acknowledges the
  arm request as soon as the host-side parent process has started, before
  that parent's `gh pr view` call and state-file write actually complete,
  leaving a brief (sub-second-to-low-single-digit-second) window where arming
  reported success but the guard is not yet live. A host-run
  `/cenci:implement` has no such gap — the guard is live before the process
  that requested the arm returns.

This is the recommended cleanup command for any tool driving cenci-managed
tmux windows, e.g. a kanban board's column-cleanup hook:

```yaml
cleanup: 'cenci close {number}'
```

### Automerge (`cenci babysit`)

> Turning this on, and what it combines with: [The autonomous
> loop](../docs/autonomous-loop.md). This section is the exhaustive condition
> reference.

The same `cenci babysit` supervisor that watches CI and review feedback can also
merge a PR itself, once every one of these holds on a given tick:

Arming (`cenci babysit <pr> --agent <agent>`, run from a host tmux pane)
resolves the tmux session and start directory it will target — the current
tmux session (`$TMUX_PANE`) and `git rev-parse --show-toplevel`, or the
explicit `--session`/`--dir` flags when passed — and persists both into the
state file *before* the first poll. Every later `ci-repair`/`babysit-attention`/
`address-review` launch targets that recorded session explicitly, rather than
whatever tmux pane happens to be live by the time a much-later tick actually
fires; if the recorded session is gone, the launch fails loudly (retried next
tick) instead of silently falling back to another session or creating one.
Arming outside tmux still succeeds — it prints a warning that no repair
window can be opened and records an empty session. The detached supervisor's
stdout/stderr are written to a `0600` append-mode log file under the state
directory (one per repo/PR, printed on arming), so a failed repair launch is
readable there instead of being silently discarded.

Running `cenci babysit`/`cenci babysit stop` from inside a `cenci sandbox`-launched
container (`CENCI_SANDBOX=1`) never supervises locally — the container has no host
tmux to arm against. Instead it forwards the request to the host daemon over the
event socket and reports one of three outcomes: **armed**, **not armed** (the host
rejected the request; the reason is relayed verbatim), or **arm status unknown**
(the host didn't respond within 5s — verify or repair by running
`cenci babysit <pr> --agent <agent>` from a host tmux pane, which safely no-ops if a
supervisor is already running).

On a forwarded arm request, the host daemon resolves the target checkout itself by
inspecting running sandbox containers' `/workspace` bind sources under both docker
and podman and matching each source's `origin` remote against the request's
`owner/repo` — the container never supplies a host path. This requires the repo to
be running under exactly one sandboxed checkout: zero matches nacks **host repo not
found**, and two or more running sandboxes of the same repo (e.g. two worktrees)
nacks **ambiguous** rather than guessing which one to target. A failed or
unparsable container inspect nacks as a **probe failed** rather than being treated
as "no match". The daemon also resolves the tmux session from the forwarded pane;
an unresolvable pane nacks separately. All of these, plus a resolution that runs
past its time budget, are relayed to the container as their own distinguishable
reason, the same way an ordinary "not armed" reason is. The host also bounds the
rate of forwarded arm requests — a burst past that bound nacks with its own
distinguishable reason and is safe to retry once the rate has settled.

- The fleet-wide kill switch `automerge.enabled` is `true` in
  `~/.config/cenci/config.json` (default `false` — off everywhere until set).
  Managed via `cenci automerge on|off|status` — the same atomic,
  key-preserving config write as `cenci dispatch loop`, creating the file
  when none exists. `status` (and every toggle) also prints an informational
  per-scope summary of the current repo's `.cenci/config.json` policy blocks
  (working tree; enforcement below always reads the PR's base branch), plus
  `--json` for scripting.
- Every issue the PR closes carries the `automerge:ok` label — a human grant made
  at refinement time, per-ticket, with no repo-level default.
- CI is green, no CI repair is in flight, and no review feedback is pending —
  resolution is GitHub-authoritative, not push-based: a pushed commit no longer
  clears feedback by itself. An inline comment thread clears only when GitHub
  reports it `isResolved`; a `CHANGES_REQUESTED` review clears only when it's
  `DISMISSED` or the reviewer's latest effective review is `APPROVED`. "Green"
  requires at least one check to exist and be `pass`; every other check may be
  `pass` or `skipping` — a paths-filtered monorepo's unaffected-project checks
  (routinely `skipping`) no longer hold automerge on their own. `fail`,
  `pending`, `cancel`, an empty bucket string (malformed), and any
  future/unrecognized bucket value each still hold under their own distinct
  reason rather than being folded into a generic "not green"; an all-`skipping`
  set with zero `pass` holds too, under its own distinct reason.
- Comment and review detection reads are fully paginated, with an explicit
  completeness signal — pagination stopping short (page cap hit, mid-read
  failure) never gets silently treated as a complete read. Detection still
  fires normally for any feedback key actually found on the pages read; a
  truncated read additionally forces its own hold reason on top of that, since
  babysit can't yet rule out a fresher offstage item. An inline comment whose
  first line is the cenci attribution banner is excluded from detection
  entirely — cenci's own address-review replies never count as new feedback
  and never re-trigger a dispatch.
- Any review-feedback state babysit can't positively confirm holds automerge
  under its own reason: unreadable (API/parse failure), truncated (incomplete
  pagination), unknown (GitHub stopped reporting the item, or reported an
  unrecognized review state), or unsupported (a feedback key type babysit
  doesn't recognize). An item GitHub stops reporting at all — deleted comment,
  purged thread — holds indefinitely, whether it was still pending or already
  addressed; merge it manually.
- Immediately before merge, babysit revalidates every known feedback key —
  still-pending and previously-addressed alike — against fresh GitHub thread
  and review state, rather than reusing the tick's earlier verdict. If a
  previously-addressed inline comment's thread now comes back unresolved, it
  moves back to pending and holds the merge under its own reason, distinct
  from ordinary pending feedback, since GitHub revoked a resolution babysit
  had already relied on.
- The PR is not a draft and GitHub reports it `MERGEABLE`.
- The PR doesn't require a merge queue or other deferred-merge handling — a
  GraphQL probe (`isInMergeQueue`/`isMergeQueueEnabled`) checked as the final
  gate before mutation. Required, already-queued, or unreadable/unknown queue
  state all hold; babysit never enqueues a PR as a side effect.
- The diff stays within the supervised repo's `.cenci/config.json` `automerge`
  policy block (`protectedPaths`, `maxChangedFiles`, `maxDiffLines`, `mergeMethod`)
  — read from the PR's **base branch**, never its own head branch, so a PR can
  never widen its own policy to self-approve. An unreadable, malformed, or absent
  block denies automerge outright; there is no built-in fallback threshold.
  `mergeMethod` stays readable for configuration compatibility, but only
  `squash` is ever executed: a `merge` or `rebase` policy holds under its own
  reason instead of being validated or executed.

A denied or held tick is logged once, e.g.:

```
babysit: automerge PR #42 held: ticket lacks automerge:ok [enabled=yes label=no ci=- review=- mergeable=- headsha=- policy=- files=- filecap=- lines=- protected=- method=- queue=-]
```

The bracket renders every condition-chain stage in evaluation order: `yes` passed,
`no` is the stage that failed, `-` was never reached because an earlier stage
short-circuited. Once the `ci` stage is reached, a non-zero count of `skipping`
checks renders as a suffix, e.g. `ci=yes(skipped=6)` or `ci=no(skipped=9)` —
diagnostic only, it never changes the verdict; a zero count renders the plain
`ci=yes`/`ci=no`, and an unreached `ci` stage still renders the plain `-`. A trailing
`class=<class>` appears when the hold stemmed from a `gh` failure. See
[The autonomous loop](../docs/autonomous-loop.md#reading-a-decision) for a per-key
legend.

When every condition passes, babysit merges with `gh pr merge --squash` (never
`--delete-branch` — a PR worktree still references the branch) after confirming
squash is an allowed merge method on the repo. A merge rejected by branch
protection is logged and retried on the next tick, never bypassed. A zero-exit
`gh pr merge` doesn't by itself prove the merge landed, so babysit refetches
the PR exactly once afterward and requires it to report `MERGED` **at the
head commit babysit pinned** (the same SHA `--match-head-commit` validated):
a refetch reporting `MERGED` at a *different* head commit, or with an empty
head commit, holds under its own distinct reason instead of being read as
confirmed success — another actor could have merged a different commit in
the narrow race between babysit's own validation and this refetch. A
zero-exit result that isn't `MERGED` at all on that single refetch is treated
as indeterminate, never as success. See `flow/skills/configure/SKILL.md`'s
`automerge` schema section for the full field reference.

Once that refetch confirms `MERGED`, babysit posts one comment to the PR
recording that the merge was automatic — the cenci attribution banner, the head
commit `--match-head-commit` pinned the merge to, and the same condition-chain
bracket the log line above renders, so the durable record on the PR and the
supervisor log agree verbatim:

```markdown
> 🤖 **cenci** — merged automatically by `cenci babysit` (automerge policy). No human approved this merge.

Squash-merged, pinned to head commit `abc1234`. Every automerge policy condition below passed on a full re-evaluation immediately before the merge was issued:

    [enabled=yes label=yes ci=yes review=yes mergeable=yes headsha=yes policy=yes files=yes filecap=yes lines=yes protected=yes method=yes queue=yes]
```

`gh` merges and comments under the operator's own GitHub account, so without
this comment an automerged PR is indistinguishable from a human clicking
"Squash and merge". For the same reason the comment is **attribution, never
proof** — it records what babysit did, it does not authenticate it (see
`flow/docs/comment-attribution.md`).

The comment is strictly post-hoc: it is posted only after the merge is already
confirmed, exactly once (a later tick short-circuits on `MERGED` before
automerge is evaluated at all), and a failed `gh pr comment` is reported in the
tick's decision detail without ever downgrading the merge or retrying it. Held,
indeterminate, and unverifiable ticks post nothing.

## Pipeline stage commands (`cenci pipeline`)

`cenci pipeline <stage> <id>` drives the implement pipeline's state machine —
the deterministic "what stage is ticket `<id>` in, what's next" logic that
used to live in the flow skill's prose now lives here instead, exercised by
Go tests rather than prompt interpretation:

```bash
cenci pipeline prepare 42            # new -> prepared (confirms the ticket exists via `gh issue view`)
cenci pipeline plan 42               # prepared -> waiting_for_plan_approval (or waiting_for_input -> waiting_for_plan_approval on resume)
cenci pipeline await-input 42        # prepared -> waiting_for_input (unattended planner escalation; ticket #826)
cenci pipeline plan 42 --approve     # waiting_for_plan_approval -> plan_approved (run after human approval)
cenci pipeline execute 42            # plan_approved -> executed (blocked before a plan is approved)
cenci pipeline review 42             # executed -> reviewed
cenci pipeline finalize 42           # reviewed -> finalized
```

Every stage prints a structured JSON contract to stdout:

```json
{ "state": "waiting_for_plan_approval", "next_actions": [], "artifacts": [], "warnings": [], "errors": [] }
```

`next_actions`, `artifacts`, `warnings`, and `errors` are always present
(empty arrays, never `null`, when there's nothing to report). `artifacts`
currently holds only the pipeline state file's path; richer artifact
tracking is a follow-up.

Behavior:
- State persists per-repo at `.cenci/pipeline/<id>.json`, resolved from the
  **main checkout's** git root — not the current working directory's own
  `git rev-parse --show-toplevel` — so the same file is reachable whether the
  command runs from the main checkout (`prepare`/`plan`/`execute`) or from a
  linked worktree created for the ticket (`review`/`finalize`). Concurrent
  invocations for the same ticket serialize through a file lock, retried
  with deterministic backoff on contention.
- Every stage command is a monotonic no-op once the ticket's persisted stage
  is already at or past that command's target: it returns the persisted
  stage unchanged (never rewound, never jumped to the target), a
  `warnings[]` entry reading `already at stage "<current>"; <command> is a
  no-op` (`<command>` renders as `plan --approve` for the approve variant),
  empty `errors[]`, and exits `0`. The stage machine never moves backward —
  a domain error now means strictly "too early" (the persisted stage is
  before the command's required predecessor) or a corrupt/unrecognized
  persisted stage value.
- `execute` is blocked until the plan has been explicitly approved
  (`plan --approve`); running it any earlier is a domain error, not a
  transition.
- `await-input` (ticket #826) is the unattended planner-escalation stage:
  `prepared -> waiting_for_input`, reusing bare `plan`'s own "too early"
  sentinel (a ticket that never reached `prepared` cannot escalate either).
  Bare `plan` accepts **either** `prepared` or `waiting_for_input` as its
  predecessor and always lands at `waiting_for_plan_approval` — the
  escalation-resume path (a human answers the open questions on the ticket,
  then planning continues) and the never-escalated path converge on the
  same target. Re-escalating a ticket already at or past
  `waiting_for_plan_approval` is the ordinary monotonic no-op (never a
  rewind): the persisted stage is returned unchanged with the usual
  `already at stage "<current>"; await-input is a no-op` warning.
- `plan --approve` self-adopts a pre-stage-tracking plan (ticket #688,
  closing #718): when the persisted stage is `new` or `prepared` — strictly
  below `waiting_for_input` (ticket #826 retargeted this gate from
  `waiting_for_plan_approval`, so a ticket already parked at
  `waiting_for_input` — escalated, blocked on a human — is never silently
  adopted) — and exactly one healthy claim resolves for `<id>` under the
  shared plan-inventory identity contract (see [Plan
  inventory](#plan-inventory) above; front matter's `status` also must NOT be
  `awaiting-input`), the persisted stage is treated as if it were already
  `waiting_for_plan_approval` before the transition runs, landing directly at
  `plan_approved` instead of failing with "invalid pipeline transition". The
  `status: awaiting-input` check (ticket #826) closes a hole the stage
  retarget alone cannot: if the `.cenci/pipeline/<id>.json` state file itself
  was deleted, the persisted stage reads as `new`/`prepared` regardless of
  what the draft on disk says, so a still-awaiting-input draft must be
  checked directly. This is purely local/offline evidence — no `gh`/`git`
  freshness re-check — and surfaces a `warnings[]` entry reading `adopted
  plan file <path> as stage "waiting_for_plan_approval" (persisted stage was
  "<old-stage>"; no prior plan approval was recorded)`; it is informational,
  not a failure. **Unified identity rule (ticket #884):** a plan file whose
  front matter carries no `ticketId` at all (a legacy plan pre-dating the
  field) is no longer exempt from adoption — it needs a `ticketId` line
  added, or a re-plan (`plan --replan`), to become adoptable again. Bare
  `plan` (no `--approve`) is unaffected and keeps its own strict
  precondition (an ambiguous/missing/malformed plan file, or a `ticketId`
  mismatch, falls back to today's unmodified error, unchanged).
- A transition that's invalid for the ticket's current stage (e.g. `execute`
  before `plan --approve`, or a ticket `gh issue view` can't find) is a
  **domain error**: the full JSON contract still prints on stdout with
  `errors` populated, and the process exits `1`.
- Malformed CLI usage (unknown/missing stage, missing or non-numeric `<id>`,
  an unrecognized flag, `--approve` on any stage but `plan`, or a trailing
  unexpected argument) prints a one-line hint to stderr and exits `2` —
  no JSON is printed for these.

### Mechanics verbs (`label`, `worktree`, `worktree-cleanup`, `artifact`, `plan-check`, `reset`)

Alongside the six stage transitions, `cenci pipeline` also exposes the
deterministic side-effect mechanics that used to live in flow's skill prose:
label lifecycle, worktree create/cleanup, artifact tracking, and read-only
plan-file discovery/validation/freshness. Each verb renders the same JSON
contract as the stage commands.

```bash
cenci pipeline label 42 --transition working                     # verifies/claims exclusive gh assignee ownership, applies "Working"
cenci pipeline label 42 --transition input-needed                 # applies "Input Needed", removes "Working" (ticket #826 escalation swap; no ownership check)
cenci pipeline label 42 --transition planned [--trivial]          # applies "Planned", removes "Working" unless --trivial
cenci pipeline label 42 --transition in-review [--parent 10]      # applies "In Review", removes "Working"; --parent cascades to the parent ticket
cenci pipeline worktree 42 --slug add-thing                       # git worktree add .worktrees/42-add-thing -b feature/42-add-thing
cenci pipeline worktree 42 --attach /path/to/existing-worktree     # reuse mode: validates against `git worktree list --porcelain`, records Branch+WorktreePath, creates nothing
cenci pipeline worktree-cleanup 42 --slug add-thing               # removes both the worktree dir and the branch (creation-failure rollback only)
cenci pipeline artifact 42 --plan .plans/42-add-thing.md --branch feature/42-add-thing --session runId=abc123
cenci pipeline artifact 42 --get                                  # read-only fetch of the current artifacts
cenci pipeline plan-check 42 [--replan-requested] [--repo-slug OWNER/REPO]
                                                                   # discovers/validates .plans/42-*.md and classifies it
cenci pipeline reset 42   # deletes .cenci/pipeline/42.json: stage returns to "new", all recorded artifacts dropped from tracking
```

Behavior:
- `label`'s four transitions each require the ticket's persisted pipeline
  stage to be at or past a minimum (`working` requires `prepared` or later,
  `input-needed` requires `waiting_for_input` or later, `planned` requires
  `waiting_for_plan_approval` or later, `in-review` requires `finalized`); a
  stage short of the minimum is a domain error. Unlike the stage commands,
  label transitions are never no-ops — they always perform their `gh` work
  when the minimum is met, even when reapplied from a later stage (e.g. a
  resume, a re-escalation, or a re-plan over an already-executed ticket).
  `working` additionally mirrors the `ticket-ownership` skill: verify
  exclusive gh-assignee ownership, auto-claiming only when the ticket is
  unassigned, never replacing an existing assignee. `input-needed` carries
  no ownership check (the ticket is being handed *back* to a human, not
  claimed).
- `worktree` rolls back any partial worktree/branch state on failure;
  `worktree-cleanup` removes both the worktree and its branch and is used
  only on that rollback path (not on a "baseline gate failed" retry path,
  which deliberately keeps the worktree/branch around).
- `worktree --attach PATH` (ticket #688, closing #718) is the reuse-mode
  counterpart to `--slug`: exactly one of `--slug`/`--attach` is required
  (neither or both is a usage error, exit `2` — conflicting inputs are
  never silently resolved). `PATH` (relative paths resolve against the
  main checkout root) must already be a registered worktree of this repo
  per `git worktree list --porcelain`; it never creates a worktree or
  branch, and it never attaches the **main checkout**. A path that isn't a
  registered worktree of this repo (missing, a plain directory, or a
  worktree of a *different* repo) is a domain error naming `--slug` as the
  remedy; the main checkout or a detached-HEAD worktree are domain errors
  naming the specific reason. On success it records `Branch` (derived from
  git, never trusted from a flag) and the resolved `WorktreePath` exactly
  like `--slug` does; re-attaching the same path is an idempotent no-op,
  while attaching a *different* path than what was previously recorded
  emits a `warnings[]` entry reading `replaced tracked worktree <old> with
  <new>`. `worktree-cleanup` does **not** remove an attached worktree —
  it is documented as a creation-failure rollback path only, and attach
  creates nothing to roll back.
- `artifact` records/reads `PlanPath`, `Branch`, `WorktreePath`, `PRURL`,
  `PRNumber`, and `--session KEY=VALUE` metadata (repeatable; merges into the
  persisted map without clobbering keys from earlier calls) on the pipeline
  state file.
- `plan-check` discovers the ticket's plan via the same shared plan-inventory
  contract dispatch uses (see [Plan inventory](#plan-inventory) above),
  validates the single healthy match's front matter/required sections/slug,
  and then — in order — checks `--replan-requested` first, then the front
  matter's `status` for `awaiting-input` (ticket #826: a draft persisted by
  the unattended escalation path, blocked on a human answering the open
  questions on the ticket), and only then (unless either short-circuits)
  computes a deterministic freshness verdict from git commits-behind (scoped
  to the plan's `stalenessPaths`) and the ticket's `gh issue view`
  state/`updatedAt`. The `awaiting-input` check runs with zero `git`/`gh`
  calls, same as `--replan-requested`. It never gates on or mutates the
  persisted pipeline stage — read-only, like `artifact --get`. Its JSON
  output adds two fields beyond the shared contract: `decision` (one of
  `resume` | `stale` | `replan` | `awaiting-input` | `none` | `multiple`)
  and `plan` (the validated front-matter metadata, present on every
  decision except `none` and `multiple`).
  - **Exit-code framing deliberately deviates from the other mechanics
    verbs' blanket "errors[] non-empty → exit 1"**: `none` (no plan file
    yet — the everyday first-run outcome) and `multiple` (ambiguous — ask
    the human which file; now also covers two or more identity-based claims
    on the same ticket, not just literal duplicate filenames) both carry a
    populated `errors[]` for observability but exit `0`, since both are
    normal continuation branches, not failures. Only an empty/unrecognized
    `decision` — the plan file exists but is malformed; its freshness
    genuinely could not be determined (an invalid/unreachable
    `planCommitSha`, a git failure, or a `gh issue view` failure); the
    repo's `.plans` directory itself could not be fully read (unreadable or
    partially enumerated — see [Plan inventory](#plan-inventory) above); or
    a plan file's numeric filename prefix and front-matter `ticketId`
    disagree — is a hard-stop domain error and exits `1`. Each of these four
    sources is detectable via `errors.Is` and carries a content-distinct
    message. Usage errors (missing/non-numeric `<id>`, unrecognized flag,
    trailing positional) still exit `2` with a one-line stderr hint, same as
    every other verb.
- `--repo-slug OWNER/REPO` (`label` and `plan-check`) and `--slug SLUG`
  (worktree-cleanup, required; worktree, required unless `--attach PATH` is
  given instead) are additional flags on top of the stage commands'
  `--state-dir`/`--repo` test hooks.
- `reset` deletes the ticket's persisted state file outright — delete-everything
  semantics, not a stage rewrite: stage returns to `new` and every recorded
  artifact (branch, worktree path, PR URL/number, plan path) is dropped from
  tracking. It never refuses based on the current stage — including
  `finalized`, and including mid-run, since a human may need to rewind a
  ticket while an agent is still working — and it never calls `gh` and never
  touches labels. The branch, worktree, PR, and plan file the dropped
  artifacts pointed at all survive on disk/GitHub; they are merely untracked,
  and `reset`'s warnings enumerate exactly which ones were dropped. A
  surviving worktree is the practical consequence to watch for: a subsequent
  `cenci pipeline worktree <id> --slug <slug>` against it fails with
  "worktree or branch already exists" until the operator cleans it up.
  `reset` is idempotent — when no state file exists it is a no-op that still
  exits `0` with the warning `no pipeline state for <id>; nothing to reset`.
  It also recovers a corrupt/undecodable state file (deletes it, exits `0`
  with a decode warning), which makes it the documented remedy for the
  dispatcher's `pipeline state unreadable` skip (see
  [Pickup rules and gates](#pickup-rules-and-gates)). A delete failure (e.g.
  permissions) reports the full `{state, next_actions, artifacts, warnings,
  errors}` contract with `errors[]` populated and `state` set to whatever
  stage is genuinely still on disk, and exits `1`.

## Sandbox management and session launching (`cenci sandbox`, `cenci open`)

This section is the CLI reference for the sandbox surface (per
[docs/cli-conventions.md](../docs/cli-conventions.md), the shortcut table below
is the single documented copy). The launcher is implemented natively in this
binary (`internal/sandbox/launcher`); the container image and runtime assets it
launches ship with the [cenci-sandbox](../sandbox/README.md) plugin, resolved
from the installed plugin automatically (`CENCI_SANDBOX_ASSETS=<dir>` overrides
the resolution for development).

On a host with both Docker and Podman installed, every management/diagnostic
command below spans both engines instead of collapsing to one preferred
runtime: `ls`, `stop`, `prune`, `update-agent`, `update-plugins` (scoped and
`--all`), `diagnose`, and `support-bundle` all enumerate every installed
runtime and act on whichever one(s) actually own the target, so a
Docker-backed container is never invisible merely because Podman is also on
PATH. `ls` tags every row with a `RUNTIME` column, and `stop` tags every
stopped-container line as `stopped <name> (<runtime>)`; a same-name container
existing independently under both engines shows up as two distinct,
runtime-tagged entries rather than being silently deduplicated. Bare
`update-agent` and `update-agent --unpin` update the shared agent-CLI volume
in every runtime that already has it, bootstrapping it in the preferred
(Podman-first) runtime only when it exists nowhere; `update-agent --all`
instead sweeps every supported agent, refreshing its volume in every runtime
that already owns it, but never bootstraps a volume for an (agent, runtime)
pair that has none. A failed per-runtime query still lets the healthy
runtime's output through, plus a stderr error and a non-zero exit — never a
silently empty result. (Launch-time runtime selection — Docker-first for
`dind` mode, Podman-first otherwise — is unaffected; see
`resolveLaunchContext` in `launch.go`.)

A bare `cenci sandbox update-agent` against a volume pinned to an exact
version (via a prior `--version`) now refuses and exits 2, naming `--unpin`
and `--version` as the ways forward, instead of silently updating past the
pin — a deliberate behavior change from earlier versions of this command.

```bash
# One-shot maintenance verbs
cenci sandbox build             # build cenci-sandbox:latest (or the repo image if <repo>/.cenci/Dockerfile exists); builds the base first if missing;
                                 # names any running sandboxes still on the superseded image
cenci sandbox build --check     # report freshness only, build nothing (0 = current, non-zero = rebuild needed or error);
                                 # used by the installer to skip its rebuild prompt when nothing needs to rebuild
cenci sandbox build-base        # build cenci-sandbox-base:<content-hash> + :latest alias
cenci sandbox prune             # remove superseded base tags, dangling images, stopped *-cenci-* containers
cenci sandbox prune --images    # …and prompt ([y/N], default deny) for per-repo images (cenci-sandbox-<slug>:latest)
cenci sandbox prune --volumes   # …and prompt ([y/N], default deny) for home and shared CLI volumes
                                # --images and --volumes are independent; combine them for both prompts
cenci sandbox update-agent [--agent claude|codex|opencode] [--version <exact-semver>]
                                # atomically update the host-global, workload-read-only CLI volume;
                                # refuses (exit 2) if the volume is pinned, naming --unpin/--version
cenci sandbox update-agent --unpin
                                # clear the volume's version pin (if any), then update to latest;
                                # usage error if combined with --version
cenci sandbox update-agent --all
                                # refresh every agent-CLI volume that already exists, across every
                                # supported agent and every runtime that owns it; never bootstraps a
                                # volume that doesn't exist; ignores TTL/backoff; skips a pinned
                                # volume with a notice instead of refusing; usage error if combined
                                # with an explicit --agent, --version, or --unpin
cenci sandbox update-plugins [--agent claude|codex|opencode] [--name <n>]
                                # force-refresh the plugins inside the container/volume (ttl 0)
cenci sandbox update-plugins --all
                                # force-refresh plugins in every running sandbox container on the host;
                                # usage error if combined with an explicit --agent or --name
cenci sandbox reseed-creds      # alias for: cenci open --reseed-creds
cenci sandbox reap-orphans      # kill container-side agent processes whose tmux pane is gone

# List / stop sandbox containers
cenci sandbox ls                # NAME/STATUS/IMAGE/RUNTIME table across every installed runtime
cenci sandbox stop              # stops every claude-cenci-*/codex-cenci-*/opencode-cenci-* container
                                 # across every installed runtime; prints "stopped <name> (<runtime>)"
cenci sandbox stop agentstack   # only containers whose name contains "agentstack"

# Launch or attach an interactive session
cenci open ch                   # claude + haiku
cenci open xs                   # codex + gpt-5.6-sol
cenci open --agent codex --model gpt-5.6-terra --name mybox
cenci open --agent opencode --name mybox
cenci open ch -- --resume       # forward flags after -- straight to the agent CLI
cenci open ch --dry-run -- --resume
                                 # print the exact launch commands (and posture) without creating anything
cenci open --dind                # force-enable nested Docker (Sysbox-isolated), overriding a repo's sandbox.dind config
cenci open --no-dind             # force-disable nested Docker, overriding a repo's sandbox.dind config
```

Nested Docker is a **Linux-only host capability**: Sysbox must be installed on
the machine running `dockerd`, and macOS runs it inside Docker Desktop's
unmodifiable VM. On macOS a dind request — from `--dind` or from the repo's
`sandbox.dind` config — therefore does not fail the launch: the sandbox starts
without nested Docker and prints a warning naming `CENCI-SANDBOX-DIND-002`
(pass `--no-dind`, or set `"dind": false`, to silence it). Only work that
actually needs an in-container Docker daemon (Testcontainers, `docker
build`/`docker run` in tests) is affected. On Linux, an unregistered
`sysbox-runc` remains a hard launch failure with host install pointers, since
there it is a fixable setup gap. See
[the failure atlas](../docs/failure-atlas.md#cenci-sandbox-dind-002).

Beyond the explicit `update-agent` verb, every `cenci open` launch also
best-effort refreshes an already-populated shared agent-CLI volume when it is
stale: the default TTL is 24h (override with
`CENCI_SANDBOX_AGENT_CLI_TTL_HOURS=<hours>`; `0` disables auto-refresh
entirely), and a refresh attempt is throttled to at most once per hour via a
1h `last_attempt` backoff so an offline/captive-portal host doesn't eat the
`npm` timeout cost on every launch. The refresh runs **in the background**: the
launcher starts the isolated updater detached (a short-lived container named
`cenci-agent-cli-refresh-<agent>`, auto-removed on exit) and the launch
proceeds immediately on the existing version — the next launch picks up the
refreshed CLI. This keeps a full-download refresh (the codex platform binary
is ~130MB on the wire) from stalling `cenci open` for minutes on a slow
connection; only the first-ever bootstrap of a missing/empty volume still
waits, since there is nothing to launch with until it finishes. A volume
pinned via `cenci sandbox update-agent --version <exact-semver>` skips the
refresh entirely while stale, printing a one-line notice naming the pinned
version and the `cenci sandbox update-agent <agent> --unpin` remedy to resume
automatic updates; a pinned volume still inside the TTL launches silently. A
refresh that fails to start only warns to stderr — the launch still proceeds
on the existing (already-populated) version, and a refresh that fails after
starting simply leaves the volume on its current version until the backoff
allows a retry. The staleness check (and any refresh) is skipped entirely
when attaching to an already-running scoped container, so attach stays
instant. Security note: because the shared agent-CLI updater has network
access, this makes an automatic, network-enabled CLI update happen every TTL
period on any host that keeps launching sandboxes — set
`CENCI_SANDBOX_AGENT_CLI_TTL_HOURS=0` or pin the agent's version
(`cenci sandbox update-agent --version <exact-semver>`) to opt out.

`open`'s one-token shortcuts (recognized only as the first argument):

| Shortcut | Agent | Model |
|---|---|---|
| `ch` / `cs` / `co` / `cf` | claude | haiku / sonnet / opus / fable |
| `xl` / `xt` / `xs` | codex | gpt-5.6-luna / gpt-5.6-terra / gpt-5.6-sol |

OpenCode has no one-token shortcut yet — launch it with `--agent opencode`.

Without a shortcut or `--model`, the model defaults per agent (claude→`sonnet`,
codex→`gpt-5.6-terra`, opencode→ no default, config-driven). A shortcut and a
conflicting explicit `--agent` (e.g. `open ch --agent codex`) is a usage error
rather than silently picking one; all usage errors (unknown flag, unknown
verb, stray positional, conflicts) exit 2.
Supported flags: `--agent`, `--model`, `--name`, `--shell`, `--dind`,
`--no-dind`, `--host-network`, `--reseed-creds`, `--dry-run`; anything after a
bare `--` is forwarded to the agent CLI verbatim (this is also how
single-dash agent flags like `-p "prompt"` are passed:
`cenci open -- -p "prompt"`). For the final attach, `open` execs the
container runtime (replacing the `cenci` process) so the interactive session
owns the TTY and its exit code propagates.

`--dry-run` renders the branch a real launch would actually take, followed by
the full `cenci audit` posture breakdown (see below), without creating any
container, volume, network, or daemon, and without attaching. It performs
read-only container-disposition probes (`ps`/`inspect`) to decide which
branch a real launch would take, then prints one of: an attach-only report
(no create argv) when a compatible container is already scoped and running;
the same hard error a real launch would return when the running container
predates the shared read-only agent CLIs; or the detached container-create
command plus the interactive agent-attach command, each clearly labeled, when
no compatible container is running. The branch decision and both argvs come
from the exact same construction helpers the real launch path calls, so they
can never drift from what a real `open` would actually run, and `--dry-run`
faithfully mirrors a real launch's own failure modes (unknown agent,
`--dind`/`--no-dind` conflicts, missing container runtime, failed dind
preflight, missing codex/opencode auth) instead of always printing a
best-effort argv. When creating a new container, whether the launch would
include cenci's wiring mounts can depend on a side effect a real launch
performs (starting the events daemon on demand) that the read-only preview
never performs itself; when that outcome can't be determined read-only, the
create argv omits the wiring mounts and the report says so explicitly instead
of claiming to be exact. Any forwarded secret env var (`OPENAI_API_KEY`,
`ANTHROPIC_API_KEY`, `CONTEXT7_API_KEY`) renders as a bare `-e NAME` — no
`=value` at all, since the runtime CLI (docker/podman) resolves `NAME`
client-side from its own inherited environment rather than from argv. This
also means the value never appears in `ps` output while an attached session
is running (the exec handoff replaces the process image via `syscall.Exec`,
so the assembled argv is what `ps` shows for the rest of the session); as
with `audit` (below), mounted host *paths* are shown in full, so review the
output before sharing it. Trailing `--` forwarded args (e.g. `cenci open -- --api-key
sk-...`) are echoed verbatim in the attach argv and are **not** redacted, so a
secret passed as a forwarded flag appears in clear — review those too before
sharing. Works with agent+model shortcuts, `--dind`, `--host-network`, and a
trailing `--` forwarded-args section.

**`cn` alias:** a copy or symlink of the `cenci` binary named `cn` behaves as
`cenci open <args>` — `cn xs` is exactly `cenci open xs`. It is the one alias
binary; the retired `cenci-sand` name is a tombstone that prints a migration
map and exits 2.

## Diagnosing a sandbox session (`cenci diagnose`)

```bash
cenci diagnose                              # read-only report on the default (bare `cenci open`) session
cenci diagnose --name mysession             # read-only report on the claude-cenci-mysession session
cenci diagnose --name mysession --agent codex      # same, for codex-cenci-mysession
cenci diagnose --name mysession --verify           # re-run the diagnostic probes and report pass/fail instead of the full report
```

`cenci diagnose [--name <session>] [--agent claude|codex|opencode]` prints a
read-only report on a sandbox session: container status/exit, the timestamped
startup marker (surfaced verbatim, same precedence as `open`'s launch-failure
diagnostics), recent container logs, mounted volumes, daemon/event-socket
reachability, and the image base + plugin manifest versions ("unknown" when a
best-effort read fails). Omitting `--name` diagnoses the default session,
using the same scope resolution as `sandbox update-plugins`. Every failure is
annotated with a registered [error code](../docs/error-codes.md) — see the
[failure atlas](../docs/failure-atlas.md) for its recovery procedure — and a
fatal/degraded/warning severity.

`--verify` re-runs the same read-only probes behind the recovery commands
diagnose surfaces (daemon reachability, container existence) and prints a
`[pass]`/`[fail]` line per check instead of the full report, so after running
a suggested recovery command you can confirm it actually worked. Like the
full report, `--verify` never launches, attaches, or executes a recovery
command itself — only the existing read-only dial/inspect probes.

Unlike `open`, `diagnose` never launches, attaches, or wires the daemon — it
only reads. It is also a report, not a pass/fail gate: a successful render
exits 0 even when it finds fatal or degraded issues; only usage errors
(leftover positional argument, unknown flag, invalid `--agent`) and
cwd/home/runtime resolution failures exit non-zero (2 for usage errors, 1
otherwise).

Note: recent logs and mount paths in the report may contain sensitive data
(secrets, credentials, host paths) — review before sharing this output.

## Effective security posture (`cenci audit`)

```bash
cenci audit                                 # text report for the current repo/session context
cenci audit --agent codex                   # audit as codex instead of claude
cenci audit --dind                          # audit as if launched with nested Docker (sysbox-runc) enabled
cenci audit --host-network                  # audit as if launched with host network mode
cenci audit --json                          # same report as stable JSON, for scripting/CI
```

`cenci audit [--agent claude|codex|opencode] [--name NAME] [--dind] [--no-dind]
[--host-network] [--reseed-creds] [--json]` reports the effective security
posture the sandbox launcher **would apply** for the current repo/agent/flags:
mounted host paths and whether each is read-only or read-write, forwarded
environment variable NAMES only (never values), network mode, credential
sources staged into the session (`~/.claude/.credentials.json`,
`~/.codex/auth.json`, `~/.config/gh/hosts.yml`, and opencode's auth file) and
whether each is present, named persistent volumes, and whether a
repository-specific image or the shared monolith image is in play.

Posture is **running when possible, planned otherwise** — a stable `basis`
discriminator (`"running"` or `"planned"`) tells you which. When the
current repo/agent's scoped container is actually running, `audit` inspects
it read-only (`ps`/`inspect` only — never a mutating call) and reports its
*real, current* mounts (ro/rw), image, network mode, nested-Docker state,
and any detectable boundary weakening (e.g. a container that was actually
launched with `--host-network`, or one with a host Docker/Podman socket
mounted into it) — even if the current `cenci audit` invocation didn't pass
that flag. When no scoped container is running (never launched, stopped, or
stale), `audit` falls back to the same hypothetical next-launch derivation
it has always used: it reuses the same construction logic `open`/`Launch`
uses to assemble mounts, env, and network flags, and classifies the result
— no running container, built image, or container runtime is required, and
the daemon is never started. `forwardedEnv` and `reseedCreds` have no
inspect source (they only apply to the *next* exec into a container), so a
running-basis text report labels them `(next-exec — not observed from the
running container)` rather than presenting them as observed facts.

If a container IS running but its actual posture could **not** be verified
(a `ps`/daemon failure, or an unreadable/malformed `inspect` response),
`audit` still renders a full report — `basis:"planned"` plus a prominent,
non-empty `inspectWarning` — rather than either failing hard or silently
printing the reassuring default-safe baseline line. Exit stays 0 in this
case: `audit` degrades to a visible warning, never a hard failure, and never
a false "nothing to see here."

This is also the difference from `diagnose` (above): `diagnose` reports an
already-running session's raw inspected state (logs, exit code, mount paths)
as a diagnostic tool, while `audit` reports the *security posture* — running
when a container exists, otherwise the same "what would a launch apply
right now" planned answer it has always given — which is also what
[`cenci security explain`](#explain-the-security-posture-cenci-security-explain)
builds on.

Nested Docker (`--dind`, or a repo's `sandbox.dind` config) gets its own
"Nested Docker (sysbox-isolated)" section, separate from boundary
weakenings — it runs under its own sysbox-isolated OCI runtime, never
touching the host's own container runtime, so it is not an equivalent risk.
On a host that can never register `sysbox-runc` (macOS), that section
reports `source: platform-unsupported` with a note explaining why — an off
state deliberately distinct from the plain `off` a repo that never asked for
dind reports.
Only `--host-network` is reported as a **boundary weakening**, and the text
report visually marks it (`⚠`) instead of burying it in a flat list; the
default-safe baseline (no weakenings active) is reported explicitly, not
just omitted.

`cenci audit --json` emits the same data with stable field names
(`basis`, `agent`, `scope`, `image`, `workspace`, `network`, `dind`,
`mounts`, `volumes`, `env`, `forwardedEnv`, `credentialSources`,
`boundaryWeakenings`, `reseedCreds`, `inspectWarning`) suitable for
scripting or CI checks. As with the text report, no field ever carries a
secret or credential *value* — only names, paths, and presence/status
booleans. `basis` is always present (`"running"` or `"planned"`, never a
third value). `inspectWarning` is present with a non-empty string only
when a running container's actual posture could not be verified (see
above); it is omitted entirely otherwise, so `jq 'select(.inspectWarning)'`
is a reliable "flag me if inspection failed" check. Each `credentialSources`
entry carries `type`, `hostPath`, `present` (host file is a readable regular
file), `probe` (`"present"` | `"missing"` | `"error"` — distinguishes a
missing file from an unreadable/stat-error one), `applicable` (whether this
credential type applies to the selected `--agent`), and `staged` (whether it
is actually staged into this session by the real mount plan). Each `env`
entry carries `name` and `secret` (whether the name is classified
secret-bearing, e.g. `CONTEXT7_API_KEY`), matching how `forwardedEnv` is
already classified.

**Migration notes (additive, backward compatible):** `basis` and
`inspectWarning` are new top-level fields (ticket #627) — existing
consumers that don't check them keep working unchanged, since a
runtime-less environment (no container runtime installed) always reports
`basis:"planned"` with `inspectWarning` omitted, matching `audit`'s
behavior before this change. Scripts that want to distinguish an inspected
running container's real posture from a hypothetical plan should start
checking `basis`. `present` keeps its original meaning and does not by
itself tell you whether a credential is mounted — a credential can be
`present:true` but `staged:false` (e.g. Codex auth is present on host but
the selected agent is Claude). Scripts and tools that need "is this
credential actually mounted" must check `staged`, not `present`. `probe`
distinguishes a genuinely missing file (`"missing"`) from an
unreadable/stat-error one (`"error"`); both previously collapsed into
`present:false`. Create-time `env` entries now mark secret-bearing names
with `secret:true`, the same classification forwarded exec env already had.

Like `diagnose`, `audit` is read-only and exits 0 on a successful render;
only usage errors (e.g. `--dind` with `--no-dind`, or `--dind` outside a
repo-scoped session) exit 2.

## Explain the security posture (`cenci security explain`)

```bash
cenci security explain                      # plain-language report for the current repo/session context
cenci security explain --agent codex        # explain as codex instead of claude
cenci security explain --dind               # explain as if launched with nested Docker (sysbox-runc) enabled
cenci security explain --host-network       # explain as if launched with host network mode
```

`cenci security explain [--agent claude|codex|opencode] [--name NAME]
[--dind] [--no-dind] [--host-network] [--reseed-creds]` renders the same
posture `cenci audit` derives as a plain-language "why this is/isn't safe"
narrative instead of a tabular report: it reuses `audit`'s posture-detection
logic verbatim, adding no new detection, only prose. It explicitly states
whether it is narrating the scoped container's **observed running state**
(inspected directly) or a **plan** for the next launch (no running scoped
container was found, or its state could not be verified) — the same
`basis`/`inspectWarning` distinction `cenci audit` reports as JSON fields,
narrated instead of tabulated. The narrative opens with the threat-model
framing from [SECURITY.md](../SECURITY.md) — the container, not the agent's
own permissions, is the isolation boundary, since the agent runs unattended
with no per-command approval — then walks through one paragraph per posture
element (workspace/home mounts, other mounts such as the cenci
socket/binary/gitconfig and named volumes, network mode, credential
sources, and forwarded env var names — labeled `(next-exec — not observed
from the running container)` when the basis is running, since forwarded env
only applies to the *next* exec), a dedicated "Nested Docker
(sysbox-isolated)" section explaining why dind is not a boundary weakening,
and a closing boundary-weakenings block that visually marks any opt-in
weakening (`⚠`) or states the default-safe baseline explicitly when none
apply (suppressed in favor of a prominent inspect-failure warning when the
running container's actual posture could not be verified).

The network paragraph states the same guarantee [SECURITY.md](../SECURITY.md)
does: the default bridge network uses a separate network namespace and
publishes no inbound ports, but outbound connections initiated from inside
the container may still reach routable host, LAN, or internet services,
depending on your container runtime and firewall configuration — this is
*not* a claim of complete host isolation or universal outbound-only
enforcement. `--host-network` additionally states what changes: a *shared*
host network namespace, loss of network namespace separation, and
host-`localhost` exposure, with the blast-radius warning kept front and
center.

```
cenci security explain: agent=claude scope=my-repo basis=planned

This report describes a plan for the next launch: no running scoped
container was found (or its state could not be verified), so nothing below
reflects observed state — it is what the launcher WOULD apply.

Threat model:
The container is the security boundary, not the agent's own permissions.
The agent runs unattended, with no per-command approval prompts, so isolation
relies entirely on the container standing between the agent and the host — see
SECURITY.md for the full threat model this narrative paraphrases.

Workspace and home:
Only the current repo (/home/me/my-repo) is bind-mounted into the container at
/workspace — not your whole host filesystem. ...

...

Boundary weakenings: none (default-safe baseline)
```

Like `audit`, `explain` is read-only — it never launches, attaches, or wires
the daemon — and it never prints a credential or secret *value*, only names,
paths, and presence/status, the same as `audit`'s text and JSON reports.

## Support bundle (`cenci support-bundle`)

```bash
cenci support-bundle                        # write ./cenci-support-bundle-<UTCstamp>.tar.gz
cenci support-bundle -o /tmp/bundle.tar.gz  # write to a specific path
cenci support-bundle --yes                  # skip the confirmation prompt
```

`cenci support-bundle [--output|-o PATH] [--yes|-y]` collects a single
sanitized diagnostic archive covering the whole host + sandbox fleet, for
attaching to a bug report: cenci and container-runtime versions, host
environment variable NAMES only (never values), daemon reachability,
config.json (or a "(not found ...)" placeholder), and, for every known
sandbox container, a read-only `cenci diagnose`-style report plus its tailed
boot log (`<runtime> logs --tail 500`).

Before writing anything, it prints a manifest to stdout — every entry's name
and byte size, the container count, and a review-before-sharing caveat — then
asks for confirmation (`Write bundle to <path>? [y/N]`, prompted on stderr;
default-deny unless `--yes`). Collection is entirely in-memory: nothing
touches disk until the archive itself, which refuses to clobber an existing
file at the target path and defaults to
`./cenci-support-bundle-<UTCstamp>.tar.gz` in the current directory.

Only the environment variable NAMES collected in `environment.txt` are a hard
sanitization guarantee. Everything else — `logs/boot-*.log`, `config.json`,
and each container's `diagnose-*.txt` — is collected verbatim and may contain
secrets (API keys, tokens echoed at container boot, credentials stored in
config) or host paths. Review the archive before sharing it.

## Installer integration (`cenci doctor`, `cenci update`, `cenci uninstall`)

```bash
cenci doctor      # check prerequisites and installed stack components, change nothing
cenci update      # update installed plugins and restart the daemon
cenci uninstall   # remove installed plugins, PATH links, daemon, and config
```

All three shell out to the `cenci-installer` wrapper script installed on `PATH` (see
the [root README](../README.md)), forwarding the mode — `cenci doctor` runs
`cenci-installer doctor`, `cenci update` runs `cenci-installer update`, `cenci
uninstall` runs `cenci-installer uninstall` — with stdio inherited and the wrapper's
exit code propagated. This gives you a single entry point (`cenci
doctor`/`update`/`uninstall`) that reaches the same installer logic as running
`cenci-installer doctor`/`update`/`uninstall` directly. `cenci doctor` and `cenci
uninstall` take no flags or extra arguments (exit 2 if given any); `cenci update`
forwards a fixed set of flags (`--yes`/`-y`, `--build`/`--no-build`,
`--lazyboards`/`--no-lazyboards`). Destructive flags like `--yes` and `--lazyboards`
are not forwarded by `cenci uninstall` — invoke `cenci-installer uninstall` directly
if you need them. If `cenci-installer` isn't on `PATH`, all three exit 1 with a clear
error instead of silently doing nothing — re-run the [installer](#installation) to
create it.

`cenci update` also best-effort refreshes plugins in every currently running
sandbox container (`cenci sandbox update-plugins --all`, after the image
build/daemon-restart steps), so an already-running sandbox doesn't stay on a
stale plugin cache until you manually run `cenci sandbox update-plugins`. A
per-container refresh failure only warns — it never fails the update.

Right after that plugin refresh, `cenci update` also best-effort runs `cenci
sandbox update-agent --all` (via `step_sandbox_update_agents`), refreshing
every shared agent-CLI volume that already exists so it doesn't stay on a
stale version until you manually run `cenci sandbox update-agent --all`. Same
shape as the plugin refresh above: warn-not-fail, never blocks the rest of
the update.

## Advanced / development

The marketplace install above provisions the binary and daemon automatically. You
only need this section to install the binary by hand (e.g. Codex-only setups),
hack on cenci-watch, or run against a local plugin directory.

### Install the binary manually

```bash
go install github.com/matteobortolazzo/cenci/watch/v2@latest
```

Or build from source:

```bash
git clone https://github.com/matteobortolazzo/cenci.git
cd cenci/watch
make build
```

### Verifying release provenance

Every `watch/vX.Y.Z` release publishes a [SLSA build provenance
attestation](https://slsa.dev/spec/v1.0/provenance) covering all release
tarballs, generated in CI by `actions/attest-build-provenance` from
`.github/workflows/watch-release.yml`. This lets you confirm a downloaded
tarball was actually built by this repo's CI from the tagged commit, not
tampered with or substituted post-build. It complements — and is independent
of — the installer's existing `checksums.txt` verification, which only proves
the file matches what CI uploaded, not who built it.

Verify a downloaded tarball with the [GitHub CLI](https://cli.github.com):

```bash
gh attestation verify cenci_<ver>_<os>_<arch>.tar.gz --owner matteobortolazzo
```

For example, for the `1.6.0` Linux amd64 tarball:

```bash
gh attestation verify cenci_1.6.0_linux_amd64.tar.gz --owner matteobortolazzo
```

For a tighter guarantee that binds verification to this exact repo (rather
than any repo owned by `matteobortolazzo`), pass `--repo` instead:

```bash
gh attestation verify cenci_1.6.0_linux_amd64.tar.gz --repo matteobortolazzo/cenci
```

A successful verification reports the workflow run and commit that produced
the artifact.

### Run against a local plugin directory

`make plugin-bin` builds the current source into `plugin/bin/cenci` and stamps
the version marker, so `claude --plugin-dir ./plugin` uses your local build instead
of downloading a released artifact:

```bash
make plugin-bin
claude --plugin-dir /path/to/watch/plugin
```

### Daemon lifecycle (`cenci daemon start|stop|restart|status`)

When you install the binary by hand, start the daemon once (the marketplace plugin
does this for you via `EnsureRunning`, which spawns `cenci daemon start` detached
on demand):

```bash
cenci daemon start            # foreground; run in background or a dedicated pane
cenci daemon start -v         # verbose logging
cenci daemon start -v --json  # verbose logging, structured JSON lines instead of plain text
cenci daemon                  # bare "daemon" acts as "start"
```

**BREAKING**: bare `cenci` (no subcommand) and unrecognized top-level
flags/subcommands used to fall through to running the daemon in the foreground. They
now print usage and exit 2 instead — the daemon only starts via the explicit `daemon`
subcommand group. Update any script or shortcut that ran bare `cenci` to run
`cenci daemon start` (or `cenci daemon`) instead.

`daemon start` writes a PID file at `<socket dir>/cenci.pid`
once it has become the one live daemon (never on the "already running" no-op path
below), and removes it on clean shutdown (SIGINT/SIGTERM). A second `cenci
daemon start` against a socket that's already bound is a safe no-op — it detects the
running daemon, logs "daemon already running", and exits without disturbing it or
touching the PID file.

The socket dir resolves through a three-tier chain: `$CENCI_SOCKET_DIR` (used
verbatim, if set), then `$XDG_STATE_HOME/cenci/run` (default
`~/.local/state/cenci/run`), and only when that state tier is itself
unresolvable does it fall back to `/tmp/cenci-<uid>/cenci`. Run `cenci
socket-dir` to print the resolved path for the current environment.

```bash
cenci daemon stop      # SIGTERM, then SIGKILL if still alive after a few seconds; exits 0 whether or not anything was running
cenci daemon restart   # stop (if running), then spawn a fresh detached daemon and wait for it to come up
cenci daemon status    # running/not-running + PID; exits 1 when not running
```

`daemon stop` determines liveness via the same event-socket dial `EnsureRunning`
uses, then reads the PID file, sends `SIGTERM`, and polls (bounded, a few seconds)
before escalating to `SIGKILL`. If the PID file is missing or stale but the socket
reports a live daemon, it falls back to a `pgrep -f` process-table scan. The PID
file is always removed once it's known stale.

| Flag | Default | Description |
|------|---------|-------------|
| `-v` | `false` | Verbose logging |
| `-json` | `false` (or `CENCI_LOG_JSON`, see below) | Emit `-v` start/signal lines as structured JSON (`{timestamp, severity, code, message}`) instead of plain text |
| `-event-socket` | `<socket dir>/cenci-events.sock` (see the socket-dir chain above) | Event socket for hook notifications |
| `-socket` | `<socket dir>/cenci.sock` (see the socket-dir chain above) | Broadcast socket for widget clients |
| `-sweep` | `1` | Stale session reconciliation interval in seconds |
| `-session-ttl` | `2h` | Idle TTL for paneless sessions (Go duration); sessions without a pane are expired after this duration if no `SessionEnd` fires |
| `-style-running` | `fg=blue,dim` | tmux style for running state (inactive windows) |
| `-style-done` | `fg=green,dim` | tmux style for done state (inactive windows) |
| `-style-input` | `fg=red,dim` | tmux style for need-input state (inactive windows) |
| `-style-idle` | `dim` | tmux style for idle state (inactive windows) |
| `-symbol-running` | `▶` | Symbol shown in status bar indicator |
| `-symbol-done` | `✓` | Symbol shown in status bar indicator |
| `-symbol-input` | `!` | Symbol shown in status bar indicator |
| `-symbol-idle` | `~` | Symbol shown in status bar indicator |

(Flags above apply to `daemon start`; `stop`/`restart`/`status` take no flags — they
always resolve the default socket/PID paths.)

`CENCI_LOG_JSON=1` sets `-json`'s default without passing the flag; an
explicit `-json`/`-json=false` on the command line always overrides the
environment variable. Only `daemon_cmd.go`'s own
`-v` start/signal lines (the startup announcement, the shutdown-signal line,
and the PID-file warning) go through the JSON seam — the daemon's own
internal event/sweep/attention `-v` logging is unaffected and stays plain
text.

### Human status overview (`cenci status`)

`cenci status` prints a human-readable overview: whether the daemon is running
(with its PID), the active sessions from the daemon's broadcast state snapshot (the
same data `widget-json` reads), and the embedded fleet dispatch loop's state (the
same renderer `cenci dispatch loop status` uses). It degrades gracefully when
the daemon is down — it still prints a report and always exits 0. This is distinct
from `cenci daemon status` above, which is a narrower running/not-running + PID
check that exits 1 when not running (for scripting).

```console
$ cenci status
daemon: running (pid 12345)
sessions (1):
  main:0 - implement thing (claude) (running)
Dispatch loop: enabled
  daemon:   running
  interval: 5m
  pass_running: false
  last_dispatched: 2
  last_skipped: 0
```

### Machine-readable status for widgets (`cenci widget-json`)

`cenci widget-json` connects to the daemon's broadcast socket, reads the current state, prints a single line of JSON in the [Waybar custom module protocol](https://github.com/Alexays/Waybar/wiki/Module:-Custom), and exits. This is the hidden plumbing subcommand every bar widget (Waybar itself, noctalia, DMS, GNOME Shell, KDE Plasma, macOS/SwiftBar) polls — it used to be named `cenci status` before `status` became the human-readable overview above. (`cenci waybar` remains a backwards-compatible hidden alias for `widget-json`.)

```bash
cenci widget-json
```

| Flag | Default | Description |
|------|---------|-------------|
| `-socket` | `<socket dir>/cenci.sock` (see the socket-dir chain in `daemon start` above) | Broadcast socket path |
| `-symbol-running` | `▶` | Symbol for running count |
| `-symbol-done` | `✓` | Symbol for done count |
| `-symbol-input` | `!` | Symbol for need-input count |
| `-symbol-escalated` | `?` | Symbol for a planner-escalated (`Input Needed` label) ticket count (ticket #826) — a different concept from need-input above (a live session waiting mid-turn) and deliberately a different glyph from both `-symbol-input`'s `!` and the failed-count symbol `✗` |
| `-symbol-dispatch` | `⟳` | Symbol for the fleet dispatch loop indicator (idle/enabled state) |
| `-symbol-dispatch-running` | `⚙` | Symbol for a fleet dispatch pass actively running |

#### Waybar config

```jsonc
"custom/cenci": {
    "exec": "cenci widget-json",
    "return-type": "json",
    "interval": 1
}
```

Then add `"custom/cenci"` to your bar's modules.

#### Waybar styling

The module sets a `class` based on the highest-priority status: `failed` >
`escalated` > `need-input` > `running` > `done` > `stopped` > `idle` (ticket
#826 inserted `escalated` between `failed` and `need-input` — a
planner-escalated ticket outranks a live need-input session but never a
genuine dispatch failure).

```css
#custom-cenci {
    padding: 0 8px;
}

#custom-cenci.escalated {
    color: #f9e2af;
}

#custom-cenci.need-input {
    color: #f38ba8;
}

#custom-cenci.running {
    color: #89b4fa;
}

#custom-cenci.done {
    color: #a6e3a1;
}

#custom-cenci.idle {
    color: #6c7086;
}
```

#### Budget headroom

When per-agent-type budget headroom is available (see [Usage budgets](#usage-budgets)),
the module appends a compact percentage per agent to `text` and `tooltip`, sorted by
agent name (e.g. `▶ 1  claude 73%  codex 15%`). This is plain text with no Pango
markup — Waybar has no built-in way to color a substring within `text`, so per-agent
threshold coloring is left to frontends that read the raw numeric `headroom` field
directly (see [macOS menu bar](#macos-menu-bar-swiftbar) below). The thresholds
(`>25%` normal, `10–25%` warning, `<10%` critical) are consistent across frontends
even though only SwiftBar currently renders them as color. When no headroom data is
available (budget loop disabled/unconfigured), the `headroom` field and the
percentage text are both omitted entirely — output is unchanged from before this
field existed.

#### Fleet dispatch indicator

When the daemon's fleet dispatch loop is enabled, the module surfaces a `dispatch`
object passed through verbatim from the broadcast snapshot:

```jsonc
{
    "enabled": true,
    "daemon_running": true,
    "interval": "5m",
    "pass_running": false,
    "last_run_at": "2026-07-13T12:04:00Z",
    "last_dispatched": 2,
    "last_skipped": 3
    // "last_error": "..." — present instead of last_run_at/last_dispatched/
    // last_skipped context on a failed run
}
```

At most one dispatch glyph is ever appended to `text`, chosen by priority: while a
pass is actively running (`pass_running: true`) the module shows a distinct
`⚙` glyph (default; override with `-symbol-dispatch-running`); otherwise, if the
loop is enabled but idle, it falls back to the `⟳` glyph (default; override with
`-symbol-dispatch`); if the loop isn't enabled, no dispatch glyph is shown at all.
`tooltip` gets a summary line regardless of which glyph is chosen, e.g. `dispatch:
on (5m) — last run 12:04, 2 dispatched / 3 skipped`, or `dispatch: on (5m) — last
run failed: <err>` after a failed pass — tooltip wording is unaffected by the
running/idle glyph choice. Both `text` and `tooltip` are omitted entirely when the
loop is disabled/absent — byte-compatible with output from before this field
existed.

The `enabled` and `pass_running` fields in the raw `dispatch` JSON object remain
independently available regardless of the max-one-icon rule above — that rule only
governs which single glyph the default Waybar `text`/`tooltip` rendering picks. Any
consumer (GNOME/Plasma/DMS/noctalia widgets) that wants to render both states
independently (e.g. a spinning icon plus a separate enabled indicator) can do so
from the raw fields directly. `class` is unaffected (session-status
priority is unchanged; it stays `"none"` when there are zero live sessions,
dispatch-enabled or not), but `alt` becomes `"dispatch-only"` instead of `"none"`
when there are zero live sessions and the loop is enabled — **`alt`, not `class`, is
what determines whether the module is hidden** (`cenci widget-json`'s exit code
follows `alt == "none"`), so the indicator still appears even with no active
sessions. Every non-waybar frontend (noctalia/DMS/GNOME/Plasma/macOS) reads this
same `alt` field to decide visibility.

Since `alt` isn't a stylable CSS class in Waybar (only `class` is — Waybar exposes
`alt` as the `{alt}` text substitution for `format-alt`, not as a `#custom-cenci.<alt>`
selector), the dispatch-only glyph inherits whatever `.none` is already styled as:

```css
#custom-cenci.none {
    color: #6c7086;
}
```

If you want the dispatch glyph to stand out from the plain "no sessions" case, key
off `format-alt` / `{alt}` instead of CSS — e.g. `"format-alt": "{alt}"` with
`"format-alt-click": "click"` toggles between the compact glyph and the literal
`dispatch-only` alt text on click.

#### macOS menu bar (SwiftBar)

macOS users get the same status surface via a [SwiftBar](https://swiftbar.app)
plugin that consumes the identical `cenci widget-json` JSON — no daemon changes. It
shows the counts in the menu bar (loud red on `need-input`) and a per-session
dropdown, and hides when no sessions are live. See
[`plugin/macos/README.md`](plugin/macos/README.md) for install and settings.

#### GNOME Shell (Ubuntu)

Ubuntu's default desktop gets the same status surface via a GNOME Shell 45+
extension that polls `cenci widget-json` and adds a top-bar indicator — an icon +
counts colored by the highest-priority status, with a click-through menu listing
each session. It hides when no sessions are live. See
[`plugin/gnome/README.md`](plugin/gnome/README.md) for install and settings.

#### KDE Plasma (Kubuntu)

KDE Plasma 6 users get a native panel widget that consumes the identical
`cenci widget-json` JSON — a compact icon + counts with an expandable per-session
list, hidden from the panel when no sessions are live. See
[`plugin/plasma/README.md`](plugin/plasma/README.md) for install and settings.

#### Remote host over SSH

Nothing above needs any change to work over SSH: the daemon, sockets, tmux, and
the agent hooks all run on whichever host you're actually working on, so
`ssh`ing in and running `tmux` + `claude`/`codex` there behaves exactly like a
local session — window renaming/coloring shows up the moment you attach.

The one piece that doesn't cross the SSH boundary on its own is a **desktop bar
widget on your local machine** pointed at a **daemon on a remote host**: every
widget above reads the broadcast socket via `cenci widget-json -socket <path>`,
and a Unix socket is host-local IPC, not something reachable over the network.
Bridge it explicitly with OpenSSH's Unix-socket forwarding (OpenSSH 6.7+):

```bash
# Find the remote socket path:
ssh myhost cenci socket-dir
# /home/you/.local/state/cenci/run

# Forward it to a local path and leave the tunnel running:
ssh -N -L "$HOME/.cenci-myhost.sock:/home/you/.local/state/cenci/run/cenci.sock" myhost &

# Read it locally exactly like a local daemon:
cenci widget-json -socket "$HOME/.cenci-myhost.sock"
```

Then point your widget at the forwarded socket:

- **Waybar** manages its own config, so just add the flag to the module's `exec`:
  `"exec": "cenci widget-json -socket /home/you/.cenci-myhost.sock"`.
- **DMS, KDE Plasma, GNOME Shell, macOS (SwiftBar)** invoke `<cenciPath>
  widget-json` (or `waybar`) without room for extra flags in their `cenciPath`/
  `cenci-path`/`CENCI_BIN` setting, so point it at a small wrapper instead:
  ```bash
  #!/bin/sh
  # ~/.local/bin/cenci-myhost — chmod +x, then set cenciPath/CENCI_BIN to this path
  exec cenci "$1" -socket "$HOME/.cenci-myhost.sock" "${@:2}"
  ```

The forwarded socket reflects one remote host at a time; to watch sessions on
more than one host from the same local bar, run one tunnel per host to a
distinct local path and switch which wrapper the widget's `cenciPath` points
at (or run separate widget instances where the surface supports more than one).

## Consuming status from your own tool

The daemon broadcasts live status as newline-delimited JSON over a Unix socket.
The public `pkg/watch` package lets any Go tool subscribe to that stream — for
example to badge kanban cards or dashboards with per-window agent status.

```bash
go get github.com/matteobortolazzo/cenci/watch/v2
```

It versions via the existing `watch/v*` submodule tags.

```go
import "github.com/matteobortolazzo/cenci/watch/v2/pkg/watch"

c, err := watch.Dial(watch.DefaultSocketPath())
// ... handle err; defer c.Close()
for {
    snap, err := c.ReadSnapshot()
    if err != nil {
        break // net.ErrClosed on daemon shutdown
    }
    for _, w := range snap.Windows {
        fmt.Printf("%s: %s\n", w.WindowName, w.Status) // join your cards on WindowName
    }
}
```

The JSON schema is a stable, additive-only contract: fields are only ever added,
never renamed, removed, or repurposed, and unknown fields must be ignored (Go's
`encoding/json` does this by default).

## How it works

### Hook-to-status mapping

#### Claude Code

| Hook Event | Status | Notes |
|------------|--------|-------|
| `SessionStart` | Idle | Fresh session, no task yet |
| `UserPromptSubmit` | Running | User just submitted a prompt |
| `Notification` (permission_prompt) | NeedInput | Permission dialog shown |
| `PreToolUse` (when NeedInput) | Running | Permission was granted |
| `Stop` | Done | Claude finished responding |
| `Stop` (background work in flight) | Running | Turn ended, but a backgrounded task or a pending wakeup can still resume it |
| `SessionEnd` | Remove | Restore window, clean up |

A `Stop` that reports in-flight background work — a backgrounded subagent or
shell task, a `ScheduleWakeup`/`/loop` timer — holds the window at `running`
rather than `done`, since the session is paused waiting to be woken rather than
finished. Any event, including the background work's own, re-arms that hold.
Work that never wakes the session (a backgrounded server, a `tail`) would
otherwise pin the window at `running` until your next prompt, so the hold
expires after two minutes of complete event silence and the window falls back
to `done`. If the work does wake the session later, its next event moves the
window straight back to `running`.

#### OpenAI Codex

| Hook Event | Status | Notes |
|------------|--------|-------|
| `SessionStart` | Idle | Fresh session, no task yet |
| `UserPromptSubmit` | Running | User just submitted a prompt |
| `PermissionRequest` | NeedInput | Approval prompt shown |
| `PreToolUse` | Running | Codex is about to run a tool |
| `PostToolUse` | Running | Codex completed a tool call and is still working |
| `Stop` | Done | Codex finished responding |

Codex does not currently document a `SessionEnd` hook. cenci restores tracked Codex windows during the stale sweep once the pane returns to a non-Codex command after a completed/idle turn.

For Codex, the first non-empty line of the first submitted prompt becomes the
session's task label. Control characters are removed, whitespace is collapsed,
and the label is capped at 30 characters. The first label stays pinned across
later prompts and native pane-title changes; manually named windows, including
dispatched `<number>-<skill>` windows, remain unchanged.

Only that compact `task_name` is sent over cenci's internal hook-event IPC.
The raw prompt and its remaining lines are never transmitted or persisted by
cenci.

Codex currently does not emit `PreToolUse` for non-shell/non-MCP tools such as
`request_user_input`. During reconciliation, cenci therefore recognizes
Codex's native `[ ! ] Action Required | project` pane title as `need-input`,
keeps the pinned prompt label (or falls back to `project` after a daemon
restart), and recognizes a later braille-spinner title as `running` again.

`need-input` is a cenci-owned status: the window receives the configured red/dim
foreground style and a literal `!` symbol. A red tmux background without `!` is normally
tmux's native bell style, often triggered by Codex terminal notifications using BEL; it
is not evidence that a cenci hook classified a question. `cenci doctor` reports the
effective notification settings it can inspect and recommends `notification_method =
"osc9"` when BEL overlays are unwanted, but never rewrites them.

With daemon verbose logging enabled, attention transitions identify
`permission-request`, `input-tool:<name>`, `notification:<type>`, or
`action-required-title`. A bell with none of those records is an unclassified native
alert owned by tmux/terminal notification behavior.

### Stale session sweep

The daemon has two sweep mechanisms:

**Pane-based sweep (tmux-backed sessions)**: Every 1s (configurable with `-sweep`), the tmux frontend reconciles native agent titles and checks if tracked pane IDs still exist in tmux. If a pane is gone (e.g. an agent crashed without firing a cleanup hook), the window is restored. For Codex, the sweep also detects native input prompts and restores the window after a completed session exits back to the user's shell.

**Paneless TTL sweep**: Sessions without a tmux pane (plain terminals, cenci-sandbox without a pane) are tracked by session id only. They are removed on `SessionEnd`; if no `SessionEnd` fires (e.g. a crash or a Codex session), the daemon expires them after the idle TTL (default `2h`, configurable with `-session-ttl`).

**Sandbox orphan reap**: When the pane-based sweep detects one or more tmux-backed sessions whose pane no longer exists, the daemon triggers a single `cenci sandbox reap-orphans` pass (coalesced — not one per stale window, self-exec'd via the daemon's own binary) to kill any orphaned container-side agent processes for those sessions. The daemon also runs one reap pass at startup, covering panes that closed while it was down or restarting. The reap is fire-and-forget, non-blocking for the event loop, and self-no-ops when there's nothing to reap. Liveness is matched on the `(socket, pane)` pair — each container process carries the host tmux socket it belongs to (`CENCI_TMUX_SOCKET`) alongside its pane id (`TMUX_PANE`), so a process is only ever classified live against its own tmux server's pane set, not a bare pane-id union across servers.

### Paneless sessions

`cenci notify` accepts events even when `$TMUX_PANE` is unset. Sessions running in plain terminals or cenci-sandbox without a tmux pane appear in `cenci widget-json` output with empty `session` and `window_index` fields; their tooltip line reads `(no session) - name (agent) (status)` rather than the tmux-backed `session:index - name (agent) (status)`.

**Caveat**: for paneless sessions the task name comes only from the hook payload's `task_name` field — there is no pane title to read. Codex `UserPromptSubmit` hooks provide the compact first-prompt label, but native action-required title detection is only available for tmux-backed sessions.

### Custom status-format integration

cenci exposes two per-window user variables for custom `status-format` configs:

- `@cenci-symbol` — the status symbol (`~`, `▶`, `✓`, `!`)
- `@cenci-style` — the status style (e.g. `fg=blue,dim`)

Use them in your `status-format` to replace the default indicator and color:

```
# Replace ● with cenci symbol when active, keep ● otherwise
#{?#{@cenci-symbol},#{@cenci-symbol},●}

# Use cenci style when active, fall back to default color
#{?#{@cenci-style},#[#{@cenci-style}],#[fg=brightblack]}
```

cenci-watch intentionally does not ship a tmux theme: it supplies live state while
your existing theme remains responsible for layout, spacing, separators, and the
active-window treatment. For users with the default tmux status format, cenci-watch
automatically prepends `#{@cenci-symbol}` to `window-status-format` and
`window-status-current-format` during tracking, and restores them on cleanup.
Themes that fully replace those formats should reference the variables above.

### Budget headroom in status-line

For agent-types with budget tracking configured (see [Usage budgets](#usage-budgets)), cenci sets a session-wide (not per-window) tmux user variable per agent-type carrying the remaining budget headroom as an integer percent:

- `@cenci-headroom-<agent>` — remaining headroom, `0`–`100` (e.g. `@cenci-headroom-claude` → `73`)

Unlike `@cenci-symbol`/`@cenci-style`, this is a global option (`set-option -g`), not scoped to any one window, since headroom is a per-agent-type fact rather than a per-session/window one. Reference it once in your own `status-line`:

```
set -g status-right "claude: #{@cenci-headroom-claude}% | codex: #{@cenci-headroom-codex}%"
```

The variable is cleared (absent) when the daemon has no headroom data for that agent-type (budget tracking disabled or unconfigured).

### Manual window names

cenci respects manually set window names:

- If a window has `automatic-rename` set to `off` (i.e. you renamed it with `Ctrl+b ,`), cenci will show status indicators but keep your window name.
- If you rename a window while an agent is running, cenci detects the change and stops overriding your name.
- When the agent exits, manually-named windows keep their name (indicators are removed).

### Daemon restart

If the daemon is absent after a login or restart, the next installed Claude or Codex
hook starts it on demand (spawning `cenci daemon start` detached), waits briefly,
and retries that same event. The daemon then re-discovers the session — a `ListPanes`
call maps the `$TMUX_PANE` to the correct window — and status consumers such as DMS
see it on their next poll. Custom `-event-socket` instances are never started
automatically. To restart deliberately (e.g. after a config change), use `cenci
daemon restart` instead of waiting for the next hook — see
[Daemon lifecycle](#daemon-lifecycle-cenci-daemon-startstoprestartstatus) above.

**Upgrading past the socket-directory nesting change**: sockets moved from
`$XDG_RUNTIME_DIR/cenci*.sock` to `$XDG_RUNTIME_DIR/cenci/cenci*.sock`
(nested under a dedicated `cenci/` subdirectory — see `cenci socket-dir`). An
already-running pre-upgrade daemon stays bound to its old path and keeps running there,
harmlessly orphaned. A client on the upgraded binary computes the new nested path,
can't reach that old daemon, and the existing `EnsureRunning()` self-heal spawns a fresh
daemon at the new location on the next call — the same self-heal already documented
above for any other daemon-absent case. No special migration steps are needed.

**Upgrading past the socket-dir chain change (#1143)**: the socket dir moved off
`$XDG_RUNTIME_DIR` entirely, onto the three-tier `$CENCI_SOCKET_DIR` →
`$XDG_STATE_HOME/cenci/run` → `/tmp/cenci-<uid>/cenci` chain described above. The
cutover happens at the daemon's next start — automatically via `cenci update`, or
by hand via `cenci daemon restart` — the same `EnsureRunning()` self-heal moves any
still-connecting client onto the new path with no separate migration step. One
exception: a long-lived sandbox container created before this change has its
`CENCI_SOCKET_DIR` mount baked in for its whole lifetime (mounts can't be
repointed on a running container — see `cenci open`'s stale-socket-mount
warning), so it must be stopped and relaunched, not just have the host daemon
restarted. If you previously ran `loginctl enable-linger` as a workaround to keep
`$XDG_RUNTIME_DIR` (and its socket) alive across logout — needed because that
directory was tied to your login session — it is no longer required for cenci:
the state-tier default survives logout. Treat it as a pre-fix mitigation you can
remove, not a supported configuration to keep maintaining.

## Verifying release artifacts

Every `watch/dist/checksums.txt` published with a `cenci-watch` release is signed
keylessly by the `watch-release.yml` GitHub Actions workflow via
[cosign](https://docs.sigstore.dev/cosign/overview/) and [Sigstore](https://www.sigstore.dev/):
a self-contained Sigstore bundle (`checksums.txt.bundle`, carrying both the signature
and the signing certificate) is uploaded as a release asset alongside the tarballs,
and the signing event is recorded
in the public [Rekor](https://docs.sigstore.dev/logging/overview/) transparency log.
This lets you confirm a downloaded checksums file — and therefore every tarball it
covers — really was produced by this repository's release workflow, not tampered
with in transit or on a mirror.

Download `checksums.txt` and `checksums.txt.bundle` from the
release, then verify the signature. This workflow runs on both a `watch/v*` tag push
and a `workflow_dispatch` from `plugin-version-bump.yml` (dispatched with `--ref main`),
and the Fulcio-issued certificate's SAN identity differs between those two trigger
paths — a tag push binds `refs/tags/watch/v<version>`, while the dispatch path binds
`refs/heads/main` (the ref that *triggered* the run, not the release tag the job
resolves and publishes to later) — so verification matches the workflow file by an
identity regexp naming exactly those two alternatives, rather than a single fixed
identity:

```bash
cosign verify-blob \
  --certificate-identity-regexp '^https://github\.com/matteobortolazzo/cenci/\.github/workflows/watch-release\.yml@refs/(heads/main|tags/watch/v[0-9]+\.[0-9]+\.[0-9]+)$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --bundle checksums.txt.bundle \
  checksums.txt
```

Once `checksums.txt` itself is verified, use it to verify the downloaded tarball(s):

```bash
sha256sum -c checksums.txt
```

## Troubleshooting

For meaning, common causes, diagnostic commands, a recovery procedure, and
known platform-specific issues per registered error code (the
`CENCI-<AREA>-<SUBAREA>-<NNN>` identifiers `cenci diagnose` attaches to its
findings), see the [failure atlas](../docs/failure-atlas.md). After running a
suggested recovery command, `cenci diagnose --name <session> --verify` re-runs the
same read-only probe and reports `[pass]`/`[fail]` to confirm it worked.

**No status updates**: Ensure the hook/plugin is loaded (`claude plugin list`, `claude --plugin-dir ./plugin`, or Codex `/hooks`). Check `cenci daemon status` (running/not-running + PID) and that `cenci notify` can reach the event socket (`cenci daemon start -v` shows the socket path).

**Binary/daemon didn't bootstrap**: The SessionStart bootstrap fails silently so it
never blocks the agent. Check the bootstrap log at
`${TMPDIR:-/tmp}/cenci-bootstrap.log` — it records download, checksum, arch,
and network failures (e.g. no release published yet, or an unsupported OS/arch), and
two more outcomes: a fallback binary was adopted from another known location
(`/usr/local/bin/cenci`, a sibling plugin-cache version, `$PATH`, …) when the release
download failed, naming the source path; or no binary could be resolved anywhere,
naming that too. Either line still means the download itself failed — the log
records it as the reason regardless of whether a fallback was found. If bootstrap
can't run, install the binary manually and start the daemon (see
[Advanced / development](#advanced--development)).

**Names not restoring**: cenci restores names on clean exit (Ctrl+C / SIGTERM) and via the stale sweep. If it was killed with SIGKILL, manually rename windows or restart tmux.

**Daemon not running**: for the default event socket, `cenci notify` starts the
daemon and retries once. Recovery failures remain silent (exit 0), so the agent is
never blocked. Custom event sockets fail silently without starting another instance.

### Verbose mode

When running with `-v`, cenci logs compact task names derived from prompt labels or pane titles to stderr. Pane titles may reflect file paths, command output, or other workspace context. Raw prompts are never logged, transmitted, or persisted; task names and window names are truncated to 50 characters in log output to limit exposure.

If verbose logs are persisted (e.g. by a process supervisor), direct output to a user-owned file with restricted permissions:

```bash
cenci -v 2>~/.local/state/cenci.log
```

## License

MIT
