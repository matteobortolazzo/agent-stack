package babysit

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// noChecksReportedPattern matches `gh pr checks`' stderr text when a PR
// genuinely has no checks configured yet -- fetchChecks' narrow exit-1-only
// fallback below (#923).
var noChecksReportedPattern = regexp.MustCompile(`(?i)no checks reported`)

// fetchChecks fetches a PR's checks, tolerating the two documented `gh pr
// checks` nonzero-exit shapes that are not genuine read failures (#923):
// exit 8 (checks pending) and exit 1 (a failing check, or -- narrowly --
// zero checks reported at all) both still print valid JSON to stdout in the
// success sub-case. ghJSON itself stays strictly fail-closed (its own doc
// comment); this helper is the caller that distinguishes these exits
// itself, calling execGh directly rather than ghJSON so stdout and stderr
// stay separated.
//
// The tolerance is gated on classifyGhFailure(err) == failureClassCommand
// (a plain nonzero exit, nothing else joined in) AND errors.As succeeding
// against *ghExitError specifically -- not a bare `interface{ ExitCode()
// int }`, which would also match a raw *exec.ExitError that never went
// through execGh's wrapping. Every other failure class (timeout, cancelled,
// truncated, parse) stays a hard failure even when the wrapped exit code
// would otherwise qualify.
func fetchChecks(pr, repo string) ([]check, error) {
	args := []string{"pr", "checks", pr, "--repo", repo, "--json", "bucket,name,state"}
	stdout, stderr, err := execGh(args...)
	if err == nil {
		var checks []check
		if decodeErr := json.Unmarshal([]byte(stdout), &checks); decodeErr != nil {
			return nil, fmt.Errorf("gh %s: decode: %w", strings.Join(args, " "), errors.Join(decodeErr, errGhDecode))
		}
		return checks, nil
	}

	wrapped := fmt.Errorf("gh %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr), err)

	if classifyGhFailure(err) != failureClassCommand {
		return nil, wrapped
	}
	var exitErr *ghExitError
	if !errors.As(err, &exitErr) {
		return nil, wrapped
	}

	exitCode := exitErr.ExitCode()
	if exitCode != 8 && exitCode != 1 {
		return nil, wrapped
	}
	var checks []check
	if decodeErr := json.Unmarshal([]byte(stdout), &checks); decodeErr == nil {
		return checks, nil
	}
	if exitCode == 1 && noChecksReportedPattern.MatchString(stderr) {
		return nil, nil
	}
	return nil, wrapped
}

// bucketTally is a closed-set-enum tally of a check slice's buckets (#1129):
// named counters, not a map[string]int, so both call sites (automergeCIHold
// and ciStatus) stay compile-checked against the fixed bucket set per
// watch/docs/go-gotchas.md's closed-set-enum discipline. Empty and Unknown
// cover the malformed bucket string and any future/unrecognized bucket
// value, respectively. Total is every check counted, regardless of bucket.
type bucketTally struct {
	Pass     int
	Fail     int
	Pending  int
	Cancel   int
	Skipping int
	Empty    int
	Unknown  int
	Total    int
}

// countBuckets tallies checks' buckets into a bucketTally -- the single
// shared counting helper both automergeCIHold and ciStatus draw their counts
// from (#1129), so neither classifier's own precedence logic duplicates the
// bucket-to-counter mapping.
func countBuckets(checks []check) bucketTally {
	var t bucketTally
	for _, c := range checks {
		switch c.Bucket {
		case "pass":
			t.Pass++
		case "fail":
			t.Fail++
		case "pending":
			t.Pending++
		case "cancel":
			t.Cancel++
		case "skipping":
			t.Skipping++
		case "":
			t.Empty++
		default:
			t.Unknown++
		}
		t.Total++
	}
	return t
}
