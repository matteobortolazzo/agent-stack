package babysit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const fixCap = 3

// stateSchemaVersion is State's persisted schema version. Version 2 (#885)
// adds LaunchedKeys, split out of AddressedKeys' old dual role as both
// resolution truth and launch-dedup marker -- a dedup key must never double
// as merge authorization. load()'s migration seeds LaunchedKeys from
// PendingKeys for any state below this version, so a supervisor upgraded
// mid-episode does not fire one spurious address-review relaunch for work
// already dispatched under the old schema.
//
// #924 adds ChecksAbsentSince/ChecksAbsentHeadSHA without bumping this: both
// are purely additive `omitempty` fields with no migration to run (their
// zero value already means "clock not running" / "already elapsed"), and a
// bump would re-run the LaunchedKeys migration above on every already-healthy
// v2 state file, which can suppress an address-review dispatch (#885).
const stateSchemaVersion = 2

type Options struct {
	PR, Agent, StateDir string
	Interval            time.Duration
	Once                bool
	// Session, when set, is the tmux session babysit's launch() calls
	// target explicitly (#975) -- flag precedence over the arm-time local
	// tmux resolution, mirroring run.Opts.Session's own "flag > current
	// tmux session" precedence. The arming parent threads this onto the
	// detached supervisor child's argv (`cenci babysit ... --session
	// <name>`); the daemon-spawned path (#977) will set it directly.
	Session string
	// Dir, when set, is the working directory babysit's launch() calls
	// start their windows in (#975), mirroring run.Opts.Dir.
	Dir string
}
type State struct {
	SchemaVersion       int      `json:"schemaVersion"`
	PR                  string   `json:"pr"`
	Repo                string   `json:"repo"`
	Agent               string   `json:"agent"`
	IntervalSeconds     int64    `json:"intervalSeconds"`
	CurrentDelaySeconds int64    `json:"currentDelaySeconds"`
	LastHeadSHA         string   `json:"lastCiHeadSha,omitempty"`
	FixAttempts         int      `json:"ciFixAttempts"`
	RepairPending       bool     `json:"ciRepairPending"`
	LastCommentAt       string   `json:"lastCommentTimestamp,omitempty"`
	AddressedKeys       []string `json:"addressedCommentKeys,omitempty"`
	PendingKeys         []string `json:"pendingCommentKeys,omitempty"`
	// LaunchedKeys records which currently-pending keys already had an
	// address-review workflow dispatched for their *current* resolution
	// episode (#885). It is launch-dedup bookkeeping ONLY -- a key's
	// presence here means nothing more than "don't relaunch this episode's
	// workflow again"; it is never merge authorization, and resolution truth
	// stays entirely AddressedKeys/PendingKeys' job. A key drops out of
	// LaunchedKeys the moment reconcileFeedback resolves it, so a later
	// reopen of the same key starts its new episode unlaunched and is picked
	// up by tick's PendingKeys-\-LaunchedKeys launch trigger again.
	LaunchedKeys     []string `json:"launchedFeedbackKeys,omitempty"`
	PendingCommentAt string   `json:"pendingCommentTimestamp,omitempty"`
	// PendingHeadSHA is repair-attempt/deduplication metadata only -- it
	// records the head commit SHA at the moment new feedback was detected --
	// and is never proof of resolution (#850). A push landing after this SHA
	// (repair or otherwise) does not, by itself, mean a reviewer accepted
	// anything; only reconcileFeedback's GitHub-authoritative check (resolved
	// review thread, or a dismissed/superseded CHANGES_REQUESTED review)
	// clears a PendingKeys entry. Still written at detection time (see the
	// new-key append below), just no longer read to decide resolution.
	PendingHeadSHA string    `json:"pendingCommentHeadSha,omitempty"`
	PID            int       `json:"pid,omitempty"`
	Status         string    `json:"status"`
	UpdatedAt      time.Time `json:"updatedAt"`

	// RepoRoot is the supervised repository's local checkout root, resolved
	// once at startup. It is the repo half of BlocksClose's join key; a
	// network-free `git rev-parse` rather than repository()'s `gh` call,
	// because the close path must make no network calls (#787).
	RepoRoot string `json:"repoRoot,omitempty"`
	// LaunchSession is the tmux session every launch() call targets,
	// resolved once at arm time (#975) rather than inherited from whatever
	// $TMUX_PANE happens to be live when a much-later tick actually
	// launches a repair/attention/address-review workflow -- the arming
	// pane can be long gone by then. Additive; re-resolved on every arm, no
	// stateSchemaVersion bump needed (contrast #885's LaunchedKeys
	// migration).
	LaunchSession string `json:"launchSession,omitempty"`
	// LaunchDir is the working directory every launch() call's spawned
	// window starts in, resolved alongside LaunchSession at arm time
	// (#975).
	LaunchDir string `json:"launchDir,omitempty"`
	// ClosingIssues are the issue numbers the supervised PR closes — the
	// ticket half of BlocksClose's join key (#787).
	ClosingIssues []int `json:"closingIssues,omitempty"`
	// CIStatus is the collapsed CI verdict for the supervised PR:
	// ciStatusGreen, ciStatusFailing, ciStatusPending, ciStatusUnknown, or ""
	// when no tick has completed yet. ciStatusUnknown covers three distinct
	// causes -- a PR with zero checks configured, a genuine gh pr view/gh pr
	// checks read failure, and an unusable check bucket (cancel, an empty
	// bucket string, or an unrecognized value, #1129) -- all bound by the
	// same ChecksAbsentSince/ChecksAbsentHeadSHA settle clock (#924).
	// ciStatusFailing, ciStatusPending, and ciStatusUnknown all hold a
	// window open (#787, #923), but ciStatusUnknown's hold is now bounded to
	// up to checksSettleGrace (10 minutes) rather than unbounded while the
	// supervisor lives (#924's deliberate narrowing of #923 -- a lost
	// network or bad gh auth must not wedge every babysat window on the
	// board open forever with no self-heal); `cenci close --force` and
	// `cenci babysit stop <pr>` remain the documented remedies.
	CIStatus string `json:"ciStatus,omitempty"`
	// ChecksAbsentSince records when ciStatusUnknown was first observed for
	// the current ChecksAbsentHeadSHA (#924): set/maintained by
	// noteChecksUnknown at every tick site that publishes ciStatusUnknown
	// (zero checks or a genuine gh read failure), cleared by
	// clearChecksClock once real checks show up again. BlocksClose reads it
	// at decision time -- not tick -- to decide whether checksSettleGrace has
	// elapsed. A zero, missing, or future-dated value is treated as "already
	// elapsed" (fail open), covering legacy pre-#924 state files and
	// anomalous hand-edited timestamps alike.
	ChecksAbsentSince time.Time `json:"checksAbsentSince,omitempty"`
	// ChecksAbsentHeadSHA is the HeadRefOID the current ChecksAbsentSince
	// observation window is scoped to (#924): a new head commit invalidates
	// the previous window entirely, even if checks are still absent,
	// restarting the clock rather than letting a stale observation from a
	// since-superseded commit count toward the grace.
	ChecksAbsentHeadSHA string `json:"checksAbsentHeadSha,omitempty"`

	// Automerge fields (#824). The supervisor's detached mode sets
	// cmd.Stdout = nil, so the automerge decision log line only reaches a
	// terminal under --once -- these persist the decision into the state
	// file so it survives that, for both `cenci babysit status` and
	// debugging.
	AutomergeDecision string `json:"automergeDecision,omitempty"`
	AutomergeReason   string `json:"automergeReason,omitempty"`
	// AutomergeDetail is optional, purely diagnostic context layered onto
	// AutomergeReason -- e.g. a rejected merge's captured `gh` output, or a
	// wrapped policy/labels/allowed-methods fetch error's message. It never
	// replaces AutomergeReason's stable reason-constant contract.
	AutomergeDetail     string            `json:"automergeDetail,omitempty"`
	AutomergeConditions []conditionResult `json:"automergeConditions,omitempty"`
	AutomergeCheckedAt  time.Time         `json:"automergeCheckedAt,omitempty"`
	// AutomergeFailureClass is the orthogonal "cause" axis to
	// AutomergeReason's "site" axis (#886): AutomergeReason says which stage
	// of the condition chain (or which upstream read) held or failed,
	// AutomergeFailureClass says what kind of underlying `gh` failure
	// produced it (command/timeout/cancelled/truncated/parse), when the hold
	// stemmed from a `gh` failure at all. recordDecision assigns it
	// unconditionally on every call so a stale class from a previous failed
	// tick never survives into a later clean tick's persisted state.
	AutomergeFailureClass string `json:"automergeFailureClass,omitempty"`
}
type prFile struct {
	Path string `json:"path"`
}
type prView struct {
	Number                  int                    `json:"number"`
	Title                   string                 `json:"title"`
	State                   string                 `json:"state"`
	HeadRefName             string                 `json:"headRefName"`
	HeadRefOID              string                 `json:"headRefOid"`
	BaseRefName             string                 `json:"baseRefName"`
	URL                     string                 `json:"url"`
	MergedAt                *time.Time             `json:"mergedAt"`
	ClosingIssuesReferences []struct{ Number int } `json:"closingIssuesReferences"`
	Mergeable               string                 `json:"mergeable"`
	IsDraft                 bool                   `json:"isDraft"`
	ChangedFiles            int                    `json:"changedFiles"`
	Additions               int                    `json:"additions"`
	Deletions               int                    `json:"deletions"`
	Files                   []prFile               `json:"files"`
}

// prViewFields is the --json field set every `gh pr view` call in this
// package requests -- shared by tick's own fetch and merge.go's post-merge
// verification refetch so both decode the identical prView shape.
const prViewFields = "number,title,state,headRefName,headRefOid,mergedAt,closingIssuesReferences,url,baseRefName,mergeable,isDraft,changedFiles,additions,deletions,files"

type check struct{ Bucket, Name, State string }
type comment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	User      struct{ Login string }
}

type review struct {
	ID          int64  `json:"id"`
	State       string `json:"state"`
	SubmittedAt string `json:"submitted_at"`
	User        struct{ Login string }
}

var errNeedsInput = errors.New("human input required")

var command = func(name string, args ...string) ([]byte, error) { return exec.Command(name, args...).CombinedOutput() }

// startSupervisor is a test seam over the detached supervisor child's
// process start (#975), mirroring the package's existing command/
// processOwned seam shape (a var pointing at a default func, restorable via
// t.Cleanup).
var startSupervisor = defaultStartSupervisor

func defaultStartSupervisor(cmd *exec.Cmd) error {
	return cmd.Start()
}

// resolveLaunchTarget resolves the tmux session and start directory every
// launch() call will target, at arm time (#975): an explicit Options field
// wins over ambient resolution, mirroring run.Opts's own "flag > current
// tmux session" precedence -- so a detached child or a `--once` invocation
// that already carries --session/--dir never re-resolves. Dir falls back
// from an explicit flag to `git rev-parse --show-toplevel` to os.Getwd(),
// computed independently of (and stored separately from) RepoRoot. When no
// session can be resolved at all (armed outside tmux), arming must still
// succeed -- this only warns to stderr and returns an empty session, which
// launch() later gates on (AC 6).
func resolveLaunchTarget(o Options) (session, dir string) {
	session = strings.TrimSpace(o.Session)
	if session == "" {
		if s, err := currentTmuxSession(); err == nil {
			session = s
		} else {
			fmt.Fprintf(os.Stderr, "cenci babysit: %v -- no repair window can be opened until you re-arm from inside a tmux pane\n", err)
		}
	}
	dir = strings.TrimSpace(o.Dir)
	if dir == "" {
		dir = gitToplevel()
		if dir == "" {
			if wd, err := os.Getwd(); err == nil {
				dir = wd
			}
		}
	}
	return session, dir
}

// gitToplevel runs `git rev-parse --show-toplevel` via the package's command
// seam, returning "" on any failure -- the shared implementation behind both
// localRepoRoot and resolveLaunchTarget's dir fallback, which both need the
// checkout root and both already tolerate a "" result (localRepoRoot's own
// documented fail-open contract, and resolveLaunchTarget's further
// os.Getwd() fallback).
func gitToplevel() string {
	out, err := command("git", "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// logPath is the detached supervisor's per-repo/PR stdout/stderr log path
// (#975), sharing statePath's hash-prefix convention: one per repo/PR,
// keeping the repo name off disk, and staying outside BlocksClose's *.json
// glob (auto-adopted answer #6).
func logPath(dir, repo, pr string) string {
	sum := sha256.Sum256([]byte(repo))
	return filepath.Join(dir, hex.EncodeToString(sum[:6])+"-"+pr+".log")
}

// openSupervisorLog opens (creating if needed) the detached supervisor's
// append-mode, 0600 log file. O_CREATE's mode argument only applies at
// creation, so an explicit Chmod normalizes a pre-existing looser-permission
// file to 0600 too (AC 5).
func openSupervisorLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0600); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func Run(o Options) error {
	if o.Interval < time.Minute {
		o.Interval = time.Minute
	}
	if o.Interval > time.Hour {
		o.Interval = time.Hour
	}
	if _, err := strconv.Atoi(o.PR); err != nil {
		return fmt.Errorf("PR must be a number")
	}
	repo, err := repository()
	if err != nil {
		return err
	}
	// Forward to the host daemon over the event socket instead of spawning a
	// local supervisor (#1094): CENCI_SANDBOX=1 marks a container-side
	// invocation, CENCI_BABYSIT_SUPERVISOR is only ever set on the detached
	// supervisor child itself (never the container's outer invocation), and
	// --once never forwards -- unchanged in and out of the container.
	if !o.Once && os.Getenv("CENCI_BABYSIT_SUPERVISOR") == "" && os.Getenv("CENCI_SANDBOX") == "1" {
		return armOnHost(o, repo)
	}
	dir, err := stateDir(o.StateDir)
	if err != nil {
		return err
	}
	path := statePath(dir, repo, o.PR)
	lockPath := path + ".lock"
	if !o.Once && os.Getenv("CENCI_BABYSIT_SUPERVISOR") == "" {
		if owner, err := os.ReadFile(lockPath); err == nil {
			return fmt.Errorf("supervisor already running for PR #%s (%s)", o.PR, strings.TrimSpace(string(owner)))
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0700); err != nil {
			return err
		}
		// Resolve the PR's closing issues and publish the close guard's
		// join key *before* forking the child (#924 D3): `cenci babysit`
		// returns as soon as it has detached its child, so the child's own
		// eager save (below) gives Phase 9 no happens-before edge. A failed
		// gh pr view here is non-fatal (AC 9) -- arming must still succeed
		// even when gh/the network is unavailable; the child's own first
		// tick will retry the same read.
		var pr prView
		ghErr := ghJSON(&pr, "pr", "view", o.PR, "--repo", repo, "--json", prViewFields)
		_, statErr := os.Stat(path)
		preexisting := statErr == nil
		s := load(path)
		if s.Repo != "" && (s.Repo != repo || s.Agent != o.Agent) {
			return errors.New("existing supervisor state belongs to different repository or agent")
		}
		s.PR = o.PR
		s.SchemaVersion = stateSchemaVersion
		s.Repo = repo
		s.Agent = o.Agent
		s.Status = "arming"
		s.PID = 0
		if ghErr == nil {
			s.ClosingIssues = nil
			for _, i := range pr.ClosingIssuesReferences {
				s.ClosingIssues = append(s.ClosingIssues, i.Number)
			}
			// Never downgrade an already-recorded verdict on a re-arm
			// (Decision, AC 12/13) -- only fill it in when nothing has
			// published one yet.
			if s.CIStatus == "" {
				s.CIStatus = ciStatusUnknown
			}
			// The settle clock must be maintained whenever the verdict being
			// published is ciStatusUnknown, whether freshly set above or
			// inherited as-is from the loaded state (#924 Q&A 16) -- a re-arm
			// over a state whose CIStatus was already "unknown" from a prior
			// episode still needs its clock restarted against the fresh
			// HeadRefOID just read, or a stale/already-elapsed clock would
			// carry forward untouched and BlocksClose would fail open
			// immediately after arming.
			if s.CIStatus == ciStatusUnknown {
				noteChecksUnknown(&s, pr.HeadRefOID)
			}
		} else {
			// Best-effort only (AC 9): the arm must still proceed even when
			// gh/the network is unavailable, but a silent failure here would
			// leave ClosingIssues/CIStatus unpublished until the detached
			// child's own first tick succeeds -- with zero operator-visible
			// signal in the exact window #924 exists to close (mirrors
			// resolveLaunchTarget's own best-effort stderr convention below).
			fmt.Fprintf(os.Stderr, "cenci babysit: could not resolve closing issues yet (%v); the guard will pick them up on the first tick\n", ghErr)
		}
		s.UpdatedAt = time.Now().UTC()
		if err := save(path, s); err != nil {
			return err
		}
		session, launchDir := resolveLaunchTarget(o)
		lp := logPath(dir, repo, o.PR)
		logFile, err := openSupervisorLog(lp)
		if err != nil {
			return fmt.Errorf("open supervisor log %s: %w", lp, err)
		}
		cmd := exec.Command(os.Args[0], "babysit", o.PR, "--agent", o.Agent, "--interval", o.Interval.String(), "--state-dir", dir, "--session", session, "--dir", launchDir)
		cmd.Env = append(os.Environ(), "CENCI_BABYSIT_SUPERVISOR=1")
		cmd.Stdin = nil
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := startSupervisor(cmd); err != nil {
			_ = logFile.Close()
			if !preexisting {
				// Only a state file the parent itself created fresh is
				// removed on failure -- a pre-existing supervisor state
				// must never be wiped out by a failed re-arm (#924). The
				// `preexisting` stat, however, was taken before this
				// parent's own save/Start ran, so a concurrent `cenci
				// babysit` for the same PR can race in between: re-load the
				// file now and only remove it if it still looks like this
				// parent's own untouched arming write (Status "arming", PID
				// 0, same repo/PR) -- if some other arm's child has already
				// taken it over (e.g. a nonzero PID), leave it alone rather
				// than clobbering a supervisor that now owns it.
				if reloaded := load(path); reloaded.Status == "arming" && reloaded.PID == 0 && reloaded.Repo == repo && reloaded.PR == o.PR {
					_ = os.Remove(path)
				}
			}
			return fmt.Errorf("start supervisor: %w", err)
		}
		// The parent's own fd is no longer needed once the child inherits it
		// across Start(); the child keeps writing to the same underlying file
		// via its own inherited descriptor.
		_ = logFile.Close()
		pid := 0
		if cmd.Process != nil {
			pid = cmd.Process.Pid
		}
		fmt.Printf("Babysitting PR #%s in the background (pid %d). Supervisor log: %s\n", o.PR, pid, lp)
		return nil
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("supervisor already owns PR #%s; stop it before using --once", o.PR)
		}
		return err
	}
	_, _ = fmt.Fprintf(lock, "%d\n", os.Getpid())
	_ = lock.Close()
	defer func() {
		_ = os.Remove(lockPath)
	}()
	s := load(path)
	if s.Repo != "" && (s.Repo != repo || s.Agent != o.Agent) {
		return errors.New("existing supervisor state belongs to different repository or agent")
	}
	s.PR = o.PR
	s.SchemaVersion = stateSchemaVersion
	s.Repo = repo
	s.Agent = o.Agent
	s.IntervalSeconds = int64(o.Interval.Seconds())
	if s.CurrentDelaySeconds == 0 {
		s.CurrentDelaySeconds = s.IntervalSeconds
	}
	s.PID = os.Getpid()
	s.Status = "running"
	s.RepoRoot = localRepoRoot()
	s.LaunchSession, s.LaunchDir = resolveLaunchTarget(o)
	// Fill in a blocking verdict + settle clock only when nothing has
	// published one yet (#924 AC 13): a re-arm over an existing state file
	// (e.g. one the detaching parent already wrote, or a prior episode's
	// verdict) must never be downgraded, but a truly fresh state (no parent,
	// e.g. `--once`) needs *some* verdict recorded before the first tick, or
	// this eager save (below) publishes ClosingIssues with no CIStatus to
	// pair it with, and BlocksClose's closesIssue match finds an empty
	// verdict that (pre-#924) allowed the close outright.
	if s.CIStatus == "" {
		s.CIStatus = ciStatusUnknown
		noteChecksUnknown(&s, "")
	}
	// Persist once *before* the first poll: `cenci close` reads this file to
	// decide whether a supervisor still owns the ticket, and without an eager
	// save there is an arm-to-first-poll window (a full interval wide) in
	// which the supervisor is live but invisible to the guard (#787).
	s.UpdatedAt = time.Now().UTC()
	if err := save(path, s); err != nil {
		return err
	}
	for {
		terminal, delay, err := tick(&s)
		s.UpdatedAt = time.Now().UTC()
		if errors.Is(err, errNeedsInput) {
			s.PID = 0
			return save(path, s)
		}
		if err != nil {
			s.Status = "retrying"
			s.CurrentDelaySeconds *= 2
			if s.CurrentDelaySeconds < 60 {
				s.CurrentDelaySeconds = 60
			}
			if s.CurrentDelaySeconds > 3600 {
				s.CurrentDelaySeconds = 3600
			}
			_ = save(path, s)
			if o.Once {
				s.PID = 0
				return err
			}
			time.Sleep(time.Duration(s.CurrentDelaySeconds) * time.Second)
			continue
		}
		if terminal {
			_ = os.Remove(path)
			return nil
		}
		if err := save(path, s); err != nil {
			return err
		}
		if o.Once {
			return nil
		}
		time.Sleep(delay)
	}
}

func tick(s *State) (bool, time.Duration, error) {
	var pr prView
	if err := ghJSON(&pr, "pr", "view", s.PR, "--repo", s.Repo, "--json", prViewFields); err != nil {
		s.CIStatus = ciStatusUnknown
		// The PR body never decoded, so there is no fresh HeadRefOID to bind
		// the clock to; noteChecksUnknown with a blank headSHA neither
		// restarts nor adopts anything -- it only starts the clock if none
		// is running yet (#924 Q1: a genuine read failure shares the same
		// settle clock as the zero-checks cause).
		noteChecksUnknown(s, "")
		recordUpstreamReadFailure(s, reasonUpstreamPRUnreadable, err)
		return false, 0, err
	}
	if pr.State == "MERGED" || pr.State == "CLOSED" {
		if pr.State == "MERGED" {
			_, _, _ = execGh("label", "create", "Implemented", "--repo", s.Repo, "--color", "6F42C1", "--description", "PR merged — done")
			var childErrs []error
			var closedChildren []int
			for _, i := range pr.ClosingIssuesReferences {
				if _, stderr, err := execGh("issue", "edit", strconv.Itoa(i.Number), "--repo", s.Repo, "--add-label", "Implemented", "--remove-label", "In Review"); err != nil {
					childErrs = append(childErrs, fmt.Errorf("label issue #%d: %s: %w", i.Number, strings.TrimSpace(stderr), err))
					continue
				}
				closedChildren = append(closedChildren, i.Number)
			}
			// #811: reconcile every native split-parent this tick's closed
			// children reach, before the terminal report below -- a held
			// parent (an unresolved gap report) still prints its own
			// distinguishable line first, and any graph/comment-read failure
			// joins alongside childErrs as a non-terminal error rather than
			// silently discarding the child relabel work that already
			// succeeded above.
			outcomes, parentErr := reconcileParents(s.Repo, closedChildren)
			for _, o := range outcomes {
				if o.Kind == parentOutcomeHeld {
					fmt.Printf("Parent #%d held: unresolved acceptance-criteria gap report on its comment thread; not auto-closing\n", o.Parent)
				}
			}
			if err := errors.Join(append(childErrs, parentErr)...); err != nil {
				return false, 0, err
			}
		}
		fmt.Printf("PR #%s %s: %s %s\n", s.PR, strings.ToLower(pr.State), pr.Title, pr.URL)
		return true, 0, nil
	}
	// Publish the close guard's join key from data this tick already fetched
	// — no extra API calls (#787) -- before the checks fetch runs, so a
	// checks failure still leaves it populated (#923).
	s.ClosingIssues = nil
	for _, i := range pr.ClosingIssuesReferences {
		s.ClosingIssues = append(s.ClosingIssues, i.Number)
	}
	checks, err := fetchChecks(s.PR, s.Repo)
	if err != nil {
		s.CIStatus = ciStatusUnknown
		noteChecksUnknown(s, pr.HeadRefOID)
		recordUpstreamReadFailure(s, reasonUpstreamChecksUnreadable, err)
		return false, 0, err
	}
	s.CIStatus = ciStatus(checks)
	if s.CIStatus == ciStatusUnknown {
		// Zero checks reported (#924 D2): start/maintain the same settle
		// clock a genuine read failure above uses.
		noteChecksUnknown(s, pr.HeadRefOID)
	} else {
		// Real checks are present again -- wipe any clock a prior tick's
		// zero-checks or read-failure observation started, so a later gap
		// starts its own fresh window (#924).
		clearChecksClock(s)
	}
	actionable := s.CIStatus == ciStatusPending || (s.CIStatus == ciStatusFailing && s.RepairPending)
	var failing []string
	for _, c := range checks {
		if c.Bucket == "fail" {
			failing = append(failing, c.Name)
		}
	}
	if len(failing) > 0 && pr.HeadRefOID != s.LastHeadSHA {
		if s.FixAttempts >= fixCap {
			s.Status = "needs-input"
			if err := launch(s, "babysit-attention", s.PR+" CI retry cap reached; decide whether to retry, pause, or stop"); err != nil {
				// One-decision-per-tick (Decision 7, #854): without this, a
				// failed workflow dispatch on an enabled automerge tick
				// returned tick's error with no automerge decision recorded
				// at all, leaving a stale decision from the previous tick
				// displayed.
				recordUpstreamReadFailure(s, reasonWorkflowLaunchFailed, err)
				return false, 0, err
			}
			return false, 0, errNeedsInput
		} else {
			prompt := fmt.Sprintf("PR #%s (%s) has failing CI checks: %s. Diagnose, fix, test, commit, and push without force-pushing.", s.PR, pr.HeadRefName, strings.Join(failing, ", "))
			if err := launch(s, "ci-repair", prompt); err != nil {
				recordUpstreamReadFailure(s, reasonWorkflowLaunchFailed, err)
				return false, 0, err
			}
			s.FixAttempts++
			s.RepairPending = true
		}
		s.LastHeadSHA = pr.HeadRefOID
		actionable = true
	} else if pr.HeadRefOID != s.LastHeadSHA {
		s.FixAttempts = 0
		s.RepairPending = false
		s.LastHeadSHA = pr.HeadRefOID
	}
	// Fully paginated (#854): fetchPaged follows every page up to
	// maxFeedbackPages, so a PR with more comments than one page no longer
	// silently misses them; commentsComplete records whether the traversal
	// actually proved completeness (a short/empty terminating page) or hit
	// the page cap while still full-sized.
	comments, commentsComplete, err := fetchPaged[comment]("repos/" + s.Repo + "/pulls/" + s.PR + "/comments")
	if err != nil {
		recordUpstreamReadFailure(s, reasonUpstreamCommentsUnreadable, err)
		return false, 0, err
	}
	// Fully paginated (#854): reviewsComplete feeds feedbackState.ReviewsComplete
	// (feedback.go), replacing #850's reviewsPageSize length tripwire with the
	// actual completeness signal from the traversal.
	reviews, reviewsComplete, err := fetchPaged[review]("repos/" + s.Repo + "/pulls/" + s.PR + "/reviews")
	if err != nil {
		recordUpstreamReadFailure(s, reasonUpstreamReviewsUnreadable, err)
		return false, 0, err
	}
	// #897: snapshot the entry-time watermark before reconcileFeedback runs
	// below -- reconcileFeedback can advance s.LastCommentAt, so
	// detectNewFeedbackKeys compares this tick's new comments against this
	// snapshot rather than the (possibly already-advanced) live value. See
	// detectNewFeedbackKeys' own doc comment for why (AC 8's same-second
	// non-narrowing case).
	since := s.LastCommentAt
	// #850/#885/#897: re-fetch authoritative review-feedback state before
	// this tick's own new-feedback detection runs (reordered from "after,"
	// #897 AC 6/7/8) -- a brand-new comment on a thread GitHub already
	// reports resolved must not be classified away by this same tick's own
	// reconcile pass before the PendingKeys \ LaunchedKeys launch trigger
	// below ever sees it. Runs unconditionally, regardless of whether
	// automerge itself is enabled, since the launch-dedup/AddressedKeys
	// bookkeeping it performs matters independent of automerge.
	// reconcileFeedback also reclassifies every previously-addressed key
	// against fresh GitHub state (#885), so a reopened thread or review is
	// caught here even though its key already lives in AddressedKeys;
	// verdict.Reopened names any such key.
	verdict := reconcileFeedback(s, reviews, reviewsComplete)
	if len(verdict.Reopened) > 0 {
		actionable = true
	}
	keys, newest := detectNewFeedbackKeys(s, comments, reviews, since)
	if len(keys) > 0 {
		s.PendingKeys = append(s.PendingKeys, keys...)
		s.PendingCommentAt = newest
		s.PendingHeadSHA = pr.HeadRefOID
		actionable = true
	}
	// #897 (guards a #920 chain-suite regression): a newly-detected
	// CHANGES_REQUESTED review already superseded by a later effective
	// review from the same reviewer resolves immediately, in this same
	// tick -- unlike a comment on an already-resolved thread, a review's
	// resolution is fully determined by this tick's own already-fetched
	// reviews list (latestEffectiveReview), with no separate async fetch
	// whose result could go stale between detection and classification, so
	// there is no reason to defer it to the next tick the way the
	// comment-on-resolved-thread fix above must. Scoped to only the review
	// keys detectNewFeedbackKeys just returned -- never touches any
	// already-tracked pending/addressed key -- so it can never reintroduce
	// the comment-swallow bug the reorder above exists to fix. Gated on
	// reviewsComplete (#854): an unproven-complete reviews read must never
	// be trusted to declare a review already superseded.
	if reviewsComplete {
		resolvedAny := false
		for _, key := range keys {
			id, ok := strings.CutPrefix(key, "review:")
			if !ok {
				continue
			}
			reviewID, err := strconv.ParseInt(id, 10, 64)
			if err != nil {
				continue
			}
			target := findReview(reviews, reviewID)
			if target == nil {
				continue
			}
			latest, ok := latestEffectiveReview(reviews, target.User.Login)
			if !ok || latest.ID == target.ID {
				continue
			}
			if latest.State != "APPROVED" && latest.State != "DISMISSED" {
				continue
			}
			s.PendingKeys = removeKeys(s.PendingKeys, []string{key})
			s.AddressedKeys = append(s.AddressedKeys, key)
			resolvedAny = true
		}
		if resolvedAny && len(s.PendingKeys) == 0 && s.PendingCommentAt != "" {
			s.LastCommentAt = s.PendingCommentAt
			s.PendingCommentAt, s.PendingHeadSHA = "", ""
		}
	}
	// #885/#897: the single per-tick address-review launch trigger, driven by
	// PendingKeys \ LaunchedKeys -- covers both this tick's brand-new keys
	// (detected above, after reconcileFeedback) and any key reconcileFeedback
	// just reopened above, exactly once per resolution episode (Decision 3).
	// Runs last, after both reconcileFeedback and detectNewFeedbackKeys, so
	// it always sees this tick's complete PendingKeys. A launch failure
	// still leaves the keys recorded as pending (merge safety must not
	// depend on launch success) but out of LaunchedKeys, so the very next
	// tick retries -- matching today's effectively-unbounded retry behavior,
	// no fixCap-style cap.
	if toLaunch := removeKeys(s.PendingKeys, s.LaunchedKeys); len(toLaunch) > 0 {
		if err := launch(s, "address-review", s.PR); err != nil {
			recordUpstreamReadFailure(s, reasonWorkflowLaunchFailed, err)
			return false, 0, err
		}
		s.LaunchedKeys = append(s.LaunchedKeys, toLaunch...)
		actionable = true
	}
	if runAutomerge(s, pr, checks, verdict, commentsComplete, reviewsComplete) {
		actionable = true
	}
	if actionable {
		s.CurrentDelaySeconds = s.IntervalSeconds
	} else {
		s.CurrentDelaySeconds *= 2
		if s.CurrentDelaySeconds > 3600 {
			s.CurrentDelaySeconds = 3600
		}
		fmt.Printf("PR #%s quiet — no new actionable work. Next check in ~%dm (backing off).\n", s.PR, s.CurrentDelaySeconds/60)
	}
	return false, time.Duration(s.CurrentDelaySeconds) * time.Second, nil
}

// launch dispatches workflow via a self-exec of `cenci run`, targeting the
// tmux session recorded at arm time (#975) rather than inheriting whatever
// $TMUX_PANE/cwd happen to be live when this tick's launch actually fires --
// the arming pane can be long gone by then. An empty recorded session (armed
// outside tmux) fails immediately with no probe and no `cenci run` call; a
// recorded session that no longer exists fails with an error naming it,
// again issuing zero `cenci run` calls -- never falling back to another
// session, never creating one (ticket Decision). The probe-error and
// session-absent branches are kept as separate returns
// (watch/docs/error-handling.md's rule against collapsing "probe errored"
// into "condition false").
func launch(s *State, workflow, arg string) error {
	if s.LaunchSession == "" {
		return fmt.Errorf("launch %s: no tmux session was recorded at arm time; re-arm from a host tmux pane", workflow)
	}
	exists, err := tmuxHasSession(s.LaunchSession)
	if err != nil {
		return fmt.Errorf("launch %s: checking tmux session %q: %w", workflow, s.LaunchSession, err)
	}
	if !exists {
		return fmt.Errorf("launch %s: recorded tmux session %q no longer exists", workflow, s.LaunchSession)
	}
	args := []string{"run", workflow, arg, "--agent", s.Agent, "--session", s.LaunchSession}
	if s.LaunchDir != "" {
		args = append(args, "--dir", s.LaunchDir)
	}
	out, err := command(os.Args[0], args...)
	if err != nil {
		return fmt.Errorf("launch %s: %s: %w", workflow, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// detectNewFeedbackKeys is the pure, read-only new-feedback detection
// predicate, extracted out of tick's own inline loop so both tick and the
// pre-merge re-check (merge.go's recheckAutomergeInputs) share exactly one
// implementation (#854's rejected "reuse reconcileFeedback" alternative:
// that function mutates State and re-fetches GraphQL thread
// resolution, risking double-bookkeeping and unable to detect feedback that
// landed strictly between this tick's own fetch and the merge attempt).
// Given s's already-recorded AddressedKeys/PendingKeys and an explicit since
// watermark, it reports every comment/review key in comments/reviews not
// already seen, timestamped strictly after since, ignoring bot authors and
// cenci's own banner-first-line replies (#897 -- see commentBannerPrefix's
// doc comment in attribution.go), plus the newest timestamp found (seeded
// from the live s.LastCommentAt, never from since, so the watermark this
// returns can never move backward).
//
// since (#897, Decision/Q2) is an explicit parameter rather than always
// reading s.LastCommentAt directly: tick's caller snapshots s.LastCommentAt
// *before* reconcileFeedback runs and passes that snapshot here, since
// reconcileFeedback can advance s.LastCommentAt earlier in the same tick.
// Without this, a same-second comment landing after a prior tick's fetch
// would be permanently dropped once reconcile advances the watermark to that
// same second -- narrowing detection, which AC 8 forbids. The pre-merge
// recheck (merge.go's recheckAutomergeInputs) passes s.LastCommentAt
// directly, preserving its existing behavior byte-for-byte.
func detectNewFeedbackKeys(s *State, comments []comment, reviews []review, since string) (keys []string, newest string) {
	seen := map[string]bool{}
	for _, key := range append(append([]string{}, s.AddressedKeys...), s.PendingKeys...) {
		seen[key] = true
	}
	newest = s.LastCommentAt
	for _, c := range comments {
		ts := c.UpdatedAt
		if ts == "" {
			ts = c.CreatedAt
		}
		key := "comment:" + strconv.FormatInt(c.ID, 10)
		if !seen[key] && ts > since && !strings.HasSuffix(c.User.Login, "[bot]") && !isCommentBannerFirstLine(c.Body) {
			keys = append(keys, key)
			if ts > newest {
				newest = ts
			}
		}
	}
	for _, r := range reviews {
		key := "review:" + strconv.FormatInt(r.ID, 10)
		if r.State == "CHANGES_REQUESTED" && !seen[key] && r.SubmittedAt > since && !strings.HasSuffix(r.User.Login, "[bot]") {
			keys = append(keys, key)
			if r.SubmittedAt > newest {
				newest = r.SubmittedAt
			}
		}
	}
	return keys, newest
}

// ghJSON decodes dst from `gh <args...>`'s stdout. Strict (#886): any
// non-nil execGh error -- a nonzero exit, a bounded-output truncation
// (errGhOutputTruncated), a timeout, a cancellation -- fails closed
// unconditionally, before dst is ever decoded, even when stdout still
// happens to be complete, valid JSON. This removes the previous "gh pr
// checks exits 8 while checks are pending, but stdout still decodes, so
// treat it as success" carve-out entirely: a caller that wants to
// distinguish a genuine `gh` failure from a checks-pending exit must do so
// itself, not rely on ghJSON to paper over it (watch/docs/error-handling.md's
// default-deny rule). Only once execGh itself reported success does ghJSON
// attempt to decode; a decode failure there is wrapped with errGhDecode so
// classifyGhFailure resolves it to failureClassParse.
func ghJSON(dst any, args ...string) error {
	stdout, stderr, err := execGh(args...)
	if err != nil {
		return fmt.Errorf("gh %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr), err)
	}
	if decodeErr := json.Unmarshal([]byte(stdout), dst); decodeErr != nil {
		return fmt.Errorf("gh %s: decode: %w", strings.Join(args, " "), errors.Join(decodeErr, errGhDecode))
	}
	return nil
}
func repository() (string, error) {
	stdout, stderr, err := execGh("repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
	if err != nil {
		return "", fmt.Errorf("resolve repository: %s: %w", strings.TrimSpace(stderr), err)
	}
	r := strings.TrimSpace(stdout)
	if !strings.Contains(r, "/") {
		return "", errors.New("could not resolve owner/repository")
	}
	return r, nil
}
func stateDir(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		h, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		base = filepath.Join(h, ".local", "state")
	}
	return filepath.Join(base, "cenci", "babysit"), nil
}
func statePath(dir, repo, pr string) string {
	sum := sha256.Sum256([]byte(repo))
	return filepath.Join(dir, hex.EncodeToString(sum[:6])+"-"+pr+".json")
}
func load(path string) State {
	var s State
	b, e := os.ReadFile(path)
	if e == nil {
		_ = json.Unmarshal(b, &s)
	}
	// Schema migration (#885): a state persisted below stateSchemaVersion
	// predates LaunchedKeys -- its PendingKeys already represents in-flight
	// feedback dispatched under the old AddressedKeys-as-dedup scheme, so
	// seed LaunchedKeys from it here, before Run() (or any other caller)
	// ever overwrites s.SchemaVersion, so an upgraded supervisor with
	// in-flight feedback does not fire one spurious address-review relaunch
	// for work already dispatched. Both the standalone `load()` callers
	// (BlocksClose, `cenci babysit status`) and Run()'s own startup load see
	// the migrated value.
	if s.SchemaVersion < stateSchemaVersion {
		if len(s.PendingKeys) > 0 {
			s.LaunchedKeys = append(append([]string{}, s.LaunchedKeys...), s.PendingKeys...)
		}
		s.SchemaVersion = stateSchemaVersion
	}
	return s
}
func save(path string, s State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err = os.WriteFile(tmp, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
func Stop(pr, explicit string) error {
	// The supervisor this would stop runs on the host, not inside this
	// sandbox (#1094 AC5); `babysit stop` sends no disarm message, it only
	// reports the host-supervision fact and exits non-zero. Checked before
	// stateDir/repository so this never needs a state directory or a gh
	// call.
	if os.Getenv("CENCI_SANDBOX") == "1" {
		cleanPR := strings.TrimPrefix(pr, "#")
		return fmt.Errorf("the supervisor for PR #%s runs on the host, not inside this sandbox; run `cenci babysit stop %s` from a host tmux pane", cleanPR, cleanPR)
	}
	dir, err := stateDir(explicit)
	if err != nil {
		return err
	}
	repo, err := repository()
	if err != nil {
		return err
	}
	cleanPR := strings.TrimPrefix(pr, "#")
	path := statePath(dir, repo, cleanPR)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return fmt.Errorf("no supervisor found for PR #%s", pr)
	}
	s := load(path)
	if s.PID > 0 {
		if !processOwned(s.PID, cleanPR) {
			return fmt.Errorf("refusing to signal pid %d: it is not the recorded cenci babysit process", s.PID)
		}
		proc, e := os.FindProcess(s.PID)
		if e != nil {
			return e
		}
		if e = proc.Signal(syscall.SIGTERM); e != nil {
			return fmt.Errorf("stop pid %d: %w", s.PID, e)
		}
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Printf("Stopped babysitting PR #%s.\n", cleanPR)
	return nil
}

// processOwned is a test seam over defaultProcessOwned, mirroring the
// package's existing `command` seam: BlocksClose's decision matrix must be
// testable without spawning real supervisor processes.
var processOwned = defaultProcessOwned

func defaultProcessOwned(pid int, pr string) bool {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		// Where procfs is readable (Linux), an unreadable cmdline means the
		// pid is gone or hidden — stay strict, since Stop signals on this
		// answer and a recycled pid must never be signalled. Where procfs is
		// unavailable *entirely* (no /proc mount, non-Linux), ownership
		// cannot be established at all and an unconditional false would make
		// BlocksClose a silent no-op, so fall back to a liveness-only probe
		// there (#787).
		if procfsReadable() {
			return false
		}
		return syscall.Kill(pid, 0) == nil
	}
	cmdline := strings.ReplaceAll(string(b), "\x00", " ")
	return strings.Contains(cmdline, "babysit") && strings.Contains(cmdline, pr)
}

// procfsReadable reports whether this process can read its own procfs cmdline
// — i.e. whether /proc is mounted and readable for the caller at all.
func procfsReadable() bool {
	_, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(os.Getpid()), "cmdline"))
	return err == nil
}

// localRepoRoot resolves the supervised checkout's root without touching the
// network. "" when it cannot be resolved, which makes the guard's repo
// comparison fail open rather than block on an unknown repo (#787).
func localRepoRoot() string {
	return gitToplevel()
}

// ciStatusUnknown records that either this tick's own gh pr view or gh pr
// checks read genuinely failed (#923), the PR legitimately has zero checks
// configured at all (#924 D2), or the check set's only non-pass/pending/fail
// buckets are unusable -- cancel, an empty bucket string, or an unrecognized
// value (#1129) -- "not started" is not the same as "passed", so none of
// these must ever read as green. All three causes hold the close guard open,
// bound by the same ChecksAbsentSince/ChecksAbsentHeadSHA settle clock (see
// checksSettleGrace below).
const ciStatusUnknown = "unknown"

// ciStatusGreen, ciStatusFailing, and ciStatusPending are the guard's other
// three verdicts, collapsed by ciStatus below (#924: named alongside
// ciStatusUnknown instead of inline string literals, so every verdict site
// in this file references one constant set).
const (
	ciStatusGreen   = "green"
	ciStatusFailing = "failing"
	ciStatusPending = "pending"
)

// checksSettleGrace bounds how long BlocksClose keeps a ciStatusUnknown
// verdict blocking a close, evaluated read-side at BlocksClose's call time
// from ChecksAbsentSince (#924): a supervisor whose every poll fails (bad gh
// auth, no network) never republishes anything, so bounding this at tick's
// write side instead would leave its window blocked forever with no
// self-heal. Kept a package const, not a `now` seam (no test needs to fake
// the clock -- grace boundaries are computed relative to time.Now()).
const checksSettleGrace = 10 * time.Minute

// noteChecksUnknown records/maintains the checks-absent settle clock on s
// whenever a tick (or the arming parent) publishes ciStatusUnknown, from
// either cause (#924 Q&A 11/16): starts ChecksAbsentSince the first time the
// clock is unset, restarts both fields when a new headSHA invalidates the
// previous observation window, and adopts headSHA once the recorded SHA is
// still empty. A blank headSHA (the gh pr view failure site, where the PR
// body never decoded) neither restarts nor adopts anything -- it just
// leaves an already-running clock alone.
func noteChecksUnknown(s *State, headSHA string) {
	if s.ChecksAbsentHeadSHA != "" && headSHA != "" && headSHA != s.ChecksAbsentHeadSHA {
		s.ChecksAbsentSince = time.Now().UTC()
		s.ChecksAbsentHeadSHA = headSHA
		return
	}
	if s.ChecksAbsentSince.IsZero() {
		s.ChecksAbsentSince = time.Now().UTC()
	}
	if s.ChecksAbsentHeadSHA == "" {
		s.ChecksAbsentHeadSHA = headSHA
	}
}

// clearChecksClock wipes the checks-absent settle clock once real checks
// show up again (#924): a later gap (e.g. a genuine read failure) must start
// its own fresh window rather than inherit a stale one.
func clearChecksClock(s *State) {
	s.ChecksAbsentSince = time.Time{}
	s.ChecksAbsentHeadSHA = ""
}

// settleGraceElapsed reports whether s's checks-absent clock has crossed
// checksSettleGrace, treating a missing (zero) or future-dated
// ChecksAbsentSince as "already elapsed" (#924 Q&A 2/15): a legacy state
// file predating this ticket, or one bearing an anomalous hand-edited
// timestamp, must fail open rather than wedge a window forever.
func settleGraceElapsed(s State) bool {
	if s.ChecksAbsentSince.IsZero() {
		return true
	}
	elapsed := time.Since(s.ChecksAbsentSince)
	if elapsed < 0 {
		return true
	}
	return elapsed >= checksSettleGrace
}

// ciStatus collapses a PR's check buckets into the guard's verdict, in
// precedence order fail > pending > unknown > green (#1129 Decision): a
// still-pending check is in-flight work and must outrank the settle-clock-
// bounded unknown verdict, while a genuine failure keeps today's top
// position. A PR with no checks at all, or whose only non-pass/pending/fail
// buckets are unusable (cancel, an empty bucket string, or an unrecognized
// value), reports ciStatusUnknown rather than green -- "not started" or
// "unusable" is not the same as "passed" (#787, #924 D2, #1129's fail-open
// close). skipping stays green-compatible, including a skipping-only,
// zero-pass set, so `cenci close` behavior is unchanged on path-filtered
// PRs. Counts come from the shared countBuckets tally; ciStatusUnknown's
// hold is bounded by the settle clock (checksSettleGrace), maintained by the
// caller via noteChecksUnknown/clearChecksClock, not by ciStatus itself (a
// pure classifier).
func ciStatus(checks []check) string {
	t := countBuckets(checks)
	switch {
	case t.Total == 0:
		return ciStatusUnknown
	case t.Fail > 0:
		return ciStatusFailing
	case t.Pending > 0:
		return ciStatusPending
	case t.Cancel > 0 || t.Empty > 0 || t.Unknown > 0:
		return ciStatusUnknown
	default:
		return ciStatusGreen
	}
}

// BlocksClose reports whether a live supervisor owns a PR that closes the
// given ticket with CI not yet green, plus a human-readable reason for the
// skip line. It is the read side of the `cenci close` × `cenci babysit` join
// (#787): closing an agent's window while its PR is still being supervised
// destroys work in progress, since the ticket is "In Review", not
// "Implemented", until babysit says so.
//
// repoRoot scopes the answer to one checkout; an empty repoRoot on either
// side (caller or state file) skips the comparison rather than rejecting the
// match, so an unresolvable repo root degrades to a ticket-only match instead
// of silently disabling the guard. stateDirOverride mirrors babysit's
// --state-dir; "" uses the standard location.
//
// Every failure — missing directory, unreadable or corrupt state file, no
// procfs — fails *open* (returns false), and nothing is ever written to
// stdout: this runs on every lazyboards board refresh, and a guard that
// errored into "never close anything" would be worse than the bug it fixes.
// It makes no network calls for the same reason.
func BlocksClose(ticket, repoRoot, stateDirOverride string) (bool, string) {
	number, err := strconv.Atoi(strings.TrimPrefix(ticket, "#"))
	if err != nil {
		return false, ""
	}
	dir, err := stateDir(stateDirOverride)
	if err != nil {
		return false, ""
	}
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return false, ""
	}
	for _, p := range paths {
		s := load(p) // corrupt/unreadable decodes to the zero State: no match
		if !closesIssue(s, number) {
			continue
		}
		if repoRoot != "" && s.RepoRoot != "" && repoRoot != s.RepoRoot {
			continue
		}
		// Default-deny (#924 D1): only an explicit ciStatusGreen verdict
		// allows the close unconditionally. ciStatusFailing/ciStatusPending
		// block unconditionally too (unchanged from #787/#923). Every other
		// value -- ciStatusUnknown, "", or anything unrecognized -- blocks
		// only subject to the settle-grace escape below (Q&A 15; never a
		// silent catch-all allow, watch/AGENTS.md's Critical Rule).
		switch s.CIStatus {
		case ciStatusGreen:
			continue
		case ciStatusFailing, ciStatusPending:
			// falls through to the liveness check below: blocks
			// unconditionally.
		default:
			if settleGraceElapsed(s) {
				continue
			}
		}
		if !supervisorLive(s) {
			continue
		}
		return true, fmt.Sprintf("babysit supervising PR #%s, CI not green", s.PR)
	}
	return false, ""
}

// closesIssue reports whether s's supervised PR closes issue number.
func closesIssue(s State, number int) bool {
	for _, i := range s.ClosingIssues {
		if i == number {
			return true
		}
	}
	return false
}

// armingLivenessGrace bounds how long a "arming" (Status "arming", PID 0)
// state is treated as live with no process to check yet (#924): it covers
// normal fork-to-first-tick latency between the parent's pre-Start save and
// the detached child's own first save (the parent's own gh pr view is itself
// bounded by ghTimeout, so 2 minutes safely exceeds that latency). Without a
// bound, a child that crashes or exits before its first save leaves an
// orphaned "arming" state that supervisorLive would treat as live forever,
// blocking a close unconditionally with no grace escape -- the unbounded-hold
// pattern watch/AGENTS.md's #1079 convention forbids.
const armingLivenessGrace = 2 * time.Minute

// supervisorLive reports whether the supervisor described by s is still on
// the hook for its PR. A running supervisor is identified by its own pid; a
// supervisor paused for human input deliberately zeroes its pid (see Run's
// errNeedsInput branch) but has *not* finished the work, so its window must
// stay open too (#787). A supervisor mid-arm (Status "arming", PID 0 -- the
// detaching parent's pre-Start write, #924 D3) is live too: there is no
// process to check yet, but the guard must still hold before the child's
// first tick ever runs -- bounded by armingLivenessGrace so an orphaned arm
// (child crashed before its own first save) eventually stops counting as
// live.
func supervisorLive(s State) bool {
	if s.PID > 0 && processOwned(s.PID, s.PR) {
		return true
	}
	if s.Status == "needs-input" {
		return true
	}
	return s.Status == "arming" && time.Since(s.UpdatedAt) < armingLivenessGrace
}
