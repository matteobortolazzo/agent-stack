# Error Codes

Cenci uses a stable, structured error-identifier scheme so failures can be
referenced, documented, and matched programmatically instead of relying on
ad hoc free-text messages.

For meaning, common causes, diagnostic commands, a recovery procedure, and
known platform-specific issues per code, see the
[failure atlas](failure-atlas.md). This page owns the naming convention and
a short one-line meaning per code; the atlas is the single home for the
fuller recovery content, so the two never carry duplicated, driftable text.

## Naming convention

```
CENCI-<AREA>-<SUBAREA>-<NNN>
```

- `AREA` — the broad component (e.g. `SANDBOX`, `DAEMON`, `CREDENTIALS`).
- `SUBAREA` — narrows it further (e.g. `START`, `SOCKET`, `CODEX`).
- `NNN` — a zero-padded three-digit sequence number **scoped per
  `AREA-SUBAREA` pair**: numbering restarts at `001` for each distinct
  `AREA-SUBAREA` combination, not globally.

Examples: `CENCI-SANDBOX-START-001`, `CENCI-DAEMON-SOCKET-002`,
`CENCI-CREDENTIALS-CODEX-003`.

Codes are declared as exported `Code` constants in
`watch/internal/errcode/errcode.go` and registered in that package's
data-driven `registry`, each mapping to an `Entry` (`Message`, `Causes`,
`Hints`) retrievable via `Lookup`. `init()` validates every registered code
against the format above and panics at load time on a malformed or
duplicate code, and cross-checks that every declared constant has a
registry entry — so a drift between the two fails fast instead of silently
degrading to an unregistered code.

Consumers should always use the exported `Code` constants rather than bare
string literals.

## Registry validation (for implementers)

When adding new `Code` constants to the `errcode` package:

1. Always declare the new `Code` constant in the `const` block.
2. Always add it to the `allCodes` slice — this powers the bidirectional validation check.
3. Always add its entry to the `registry` map with diagnostic content.

The `init()` function enforces both directions:
- **Forward**: every entry in `registry` is well-formed (matches `CENCI-<AREA>-<SUBAREA>-<NNN>` format).
- **Backward**: every constant in `allCodes` has a corresponding `registry` entry.

If you declare a new `Code` constant but forget to add it to `allCodes` or `registry`, the `init()` function will panic at load time with a clear error. This prevents a silent drift where an unused constant or orphaned registry entry goes undetected and causes Lookup to silently fail in the future. Always check that both directions are covered, not just the forward direction.

### Registry-only codes (reserved for future work)

When adding a code that is registry-only — registered in the constant and registry but not yet wired into any detection path or command — clearly mark it as such:

- In the registry entry comment, add a `Registry-only:` clause explaining why it's deferred and which future work will wire it (e.g., `Registry-only: reserved for wiring by ticket #XYZ`).
- In the Go doc comment above the `Code` constant, note that it is reserved for future use and is not currently attached by any command.

This prevents readers and future implementers from assuming the code is actively produced and detected in the current implementation. Example: `CENCI-SANDBOX-START-003` (readiness timeout) is registry-only for #572; only after the next ticket's work will `waitUntilReady` or `diagnose` attach it.

## Registered codes

| Code | Area / Subarea | Meaning |
|---|---|---|
| `CENCI-SANDBOX-START-001` | Sandbox / Start | The sandboxed agent CLI path is missing or not executable. |
| `CENCI-SANDBOX-START-002` | Sandbox / Start | The sandbox entrypoint failed during container startup (boot log, generic entrypoint-trap marker, container runtime logs, or fully generic fallback). |
| `CENCI-SANDBOX-START-003` | Sandbox / Start | The sandbox container did not signal readiness within the readiness-poll budget. Registry-only: registered (message, causes, hints) but not yet wired into any detection path — neither `waitUntilReady` nor `cenci diagnose` attaches it today (see below). Reserved for future use. |
| `CENCI-SANDBOX-SESSION-001` | Sandbox / Session | No container exists for the requested sandbox session (never launched, launched under a different scope, or already auto-removed by `--rm`). Attached by `cenci diagnose`. |
| `CENCI-SANDBOX-DIND-001` | Sandbox / Dind | The nested Docker daemon (DinD) failed to start, or crashed/OOMed after starting, without an intentional-shutdown sentinel superseding the marker. Attached by the launcher's before-attach warning and by `cenci diagnose`. |
| `CENCI-SANDBOX-DIND-002` | Sandbox / Dind | Nested Docker was requested (`--dind` or `sandbox.dind`) on a host that can never register `sysbox-runc` — macOS — so the sandbox launched without it. Attached by the launcher's degrade warning and reported by `cenci audit` as the `platform-unsupported` dind source. |
| `CENCI-SANDBOX-DIND-003` | Sandbox / Dind | A dind launch's container create was rejected by the OCI runtime: `sysbox-runc` is registered with Docker (so `dindPreflight` passed) but could not create the container. Attached by the launcher's create-failure mapping. |
| `CENCI-SANDBOX-SOCKET-001` | Sandbox / Socket | A failed in-sandbox hook-event delivery is unrecoverable and container-scoped: the event-socket directory's bind mount is dangling or was never wired in (or the mount probe was inconclusive). Attached by `internal/daemon`'s delivery-failure classifier and surfaced by `cenci diagnose`'s "Event delivery:" section. |
| `CENCI-DAEMON-CONN-001` | Daemon / Conn | The cenci daemon's event socket exists but did not answer a read-only dial. Attached by `cenci diagnose`. |
| `CENCI-DAEMON-SOCKET-001` | Daemon / Socket | The cenci daemon's event socket does not exist at all. Attached by `cenci diagnose`. |

`CENCI-SANDBOX-START-001` and `CENCI-SANDBOX-START-002` are attached by
`watch/internal/sandbox/launcher/launch.go`'s `startupFailureDetail` /
`waitUntilReady`, alongside the existing verbatim failure detail (e.g.
`container '<name>' failed during startup (status ..., exit ...)
[CENCI-SANDBOX-START-001]: <verbatim detail>`).

`CENCI-SANDBOX-START-003` (readiness timeout), `CENCI-SANDBOX-SESSION-001`
(session/container not found), `CENCI-DAEMON-CONN-001` (daemon unreachable),
and `CENCI-DAEMON-SOCKET-001` (event socket missing) were added for `cenci
diagnose <session>` (#572), a read-only report command in
`watch/internal/sandbox/launcher/diagnose.go` / `watch/diagnose_cmd.go`. Each
finding it reports is classified fatal/degraded/warning (see that file's
`severityForCode`). Of the three codes `diagnose` actually attaches
(`CENCI-SANDBOX-SESSION-001`, `CENCI-DAEMON-CONN-001`,
`CENCI-DAEMON-SOCKET-001`), none is the readiness-timeout code:
`CENCI-SANDBOX-START-003` is registry-only — `severityForCode` maps it (so a
future caller gets the right severity for free) but neither `waitUntilReady`
nor `diagnose` currently detects a stuck/never-ready container and attaches
it. `waitUntilReady`'s own `did not become ready within 60 seconds` message
still does not attach `CENCI-SANDBOX-START-003` itself — wiring a
readiness-timeout detection path (in `launch.go`, `diagnose.go`, or both) to
attach it is left to a follow-up.

`CENCI-SANDBOX-DIND-001` (#630) is attached in two places: `launch.go`'s
`warnDockerdStartupFailure` prints a prominent, non-fatal warning with it
right before the first agent attach when the persistent
`.cenci-dockerd-startup-error` marker is present, and `diagnose.go`'s always-
on "Nested Docker:" section attaches a `SeverityDegraded` finding with it
whenever that marker is present for a dind session. Severity is `Degraded`
(not fatal) because the agent session still works — only nested Docker
inside the container is unavailable.

`CENCI-SANDBOX-DIND-002` (#962) is attached by `launch.go`'s
`warnDindPlatformUnsupported`, printed once during
`resolveLaunchContext` when a dind request is degraded away because the
host cannot register `sysbox-runc`. It is `Warning` tier — the only
registered code that is intentionally warning-tier (`severityForCode`'s
default branch is otherwise reserved for codeless findings, and
`diagnose_test.go`'s `warningByDesign` map records the exemption). It
describes a host capability rather than a failure: the session launched
and works, and no action on that host can enable nested Docker. This is
the macOS counterpart to `CENCI-SANDBOX-DIND-001`, which stays reserved
for an inner `dockerd` that should have started and didn't.

`CENCI-SANDBOX-DIND-003` (#1077) is attached by `dindcreate.go`'s
`createFailureError`, which `launch.go`'s create call site consults when
`docker run` fails. It fires only when dind was on for that launch *and* the
create's captured stderr carries the daemon's `OCI runtime create failed`
wrapper; every other create failure keeps the plain `<runtime> run: %w`
wording, so an ordinary failure (bad mount, name conflict) is never blamed on
sysbox. It is the only DinD code at `Fatal` tier: DIND-001 and DIND-002 both
describe a session that launched and works without nested Docker, whereas here
the container was never created, so there is no session at all.

The three DinD codes partition by *when* nested Docker failed —
DIND-002 before the launch (the host can never register `sysbox-runc`),
DIND-003 during container create (registered but non-functional),
DIND-001 after create (the container exists, the inner `dockerd` didn't
start or later died).

`CENCI-SANDBOX-SOCKET-001` (#1122) is attached by `internal/daemon`'s
`classifyDeliveryFailure`, called from `deliverEvent` when an in-sandbox
`cenci notify` fails to deliver a hook event to the default event socket and
its one retry (`EnsureRunning`'s in-sandbox no-op) is also exhausted. It
partitions against the pre-existing, host-scoped `CENCI-DAEMON-SOCKET-001`
by probing `/proc/self/mountinfo` for the socket directory's bind mount:
`CENCI-SANDBOX-SOCKET-001` covers the unrecoverable, container-scoped cases
— the mount is dangling (its source vanished from the mount namespace), was
never wired in at all, or the probe itself was inconclusive (unreadable or
malformed mountinfo, treated conservatively) — none of which can self-heal;
the only remediation is recreating the container. `CENCI-DAEMON-SOCKET-001`
covers the recoverable, host-scoped case: the mount is healthy, so the
failure is the host daemon not being reachable, fixed by `cenci daemon
start`. The classification is persisted to a home-volume marker
(`/home/dev/.cenci-events-undelivered`) and surfaced host-side by `cenci
diagnose`'s always-on "Event delivery:" section.
