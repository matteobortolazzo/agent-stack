package watch

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureLog redirects the standard logger's output into a buffer for the
// duration of the test and restores the original output in t.Cleanup. Not
// safe to run in parallel with other tests that also mutate log output (this
// package already forces serial execution via t.Setenv elsewhere).
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(orig) })
	return &buf
}

// -- shared tier fixtures ----------------------------------------------------

// socketDirTier configures the process environment so ResolveSocketDir()
// lands on one specific tier of the $CENCI_SOCKET_DIR -> $XDG_STATE_HOME ->
// /tmp chain, and reports the exact leaf directory that tier will resolve
// to. The leaf is always built from primitives (t.TempDir(), fmt.Sprintf),
// never from a live ResolveSocketDir()/SocketDir() call, so a test comparing
// against it stays red until the resolver genuinely produces that path.
type socketDirTier struct {
	name string
	// baseHardened reports whether this tier's base (filepath.Dir(leaf)) is
	// routed through hardenDir like the leaf is. True for state/tmp (both
	// create an intermediate directory cenci itself names); false for
	// override (CENCI_SOCKET_DIR's parent is not a directory cenci manages).
	baseHardened bool
	configure    func(t *testing.T) (leaf string)
}

// socketDirTiers returns the three tier fixtures shared by every hardening
// test below (AC #2), so each behavior gets an explicit test case per tier
// rather than one shared assertion across a table that only covers a subset.
func socketDirTiers() []socketDirTier {
	return []socketDirTier{
		{
			name:         "override",
			baseHardened: false,
			configure: func(t *testing.T) string {
				t.Setenv("XDG_STATE_HOME", "") // irrelevant: override always wins outright
				base := t.TempDir()
				// Short leaf name deliberately: t.TempDir()'s own path already
				// embeds this (sub)test's full name, and the longest test name
				// in this file combined with a verbose leaf name can land
				// exactly on sunPathMax (#1142 Phase 6+7 review Fix 2 tightened
				// the bound from "<= sunPathMax" to "< sunPathMax"), making the
				// override tier spuriously hard-error. "sock" keeps every
				// caller safely under the bound regardless of test name length.
				leaf := filepath.Join(base, "sock")
				t.Setenv("CENCI_SOCKET_DIR", leaf)
				return leaf
			},
		},
		{
			name:         "state",
			baseHardened: true,
			configure: func(t *testing.T) string {
				t.Setenv("CENCI_SOCKET_DIR", "")
				base := t.TempDir()
				t.Setenv("XDG_STATE_HOME", base)
				return filepath.Join(base, "cenci", "run")
			},
		},
		{
			name:         "tmp",
			baseHardened: true,
			configure: func(t *testing.T) string {
				t.Setenv("CENCI_SOCKET_DIR", "")
				t.Setenv("XDG_STATE_HOME", "")
				t.Setenv("HOME", "") // force $XDG_STATE_HOME's default-derivation to fail too
				tmpRoot := t.TempDir()
				t.Setenv("TMPDIR", tmpRoot)
				return filepath.Join(tmpRoot, fmt.Sprintf("cenci-%d", os.Getuid()), "cenci")
			},
		},
	}
}

// ensureParentDir pre-creates leaf's parent directory so a test can plant a
// fixture (symlink, plain file, pre-existing dir) directly at the leaf path
// before calling the resolver, even for tiers whose leaf is nested two
// levels below a not-yet-existing base (e.g. the state tier's
// <XDG_STATE_HOME>/cenci/run).
func ensureParentDir(t *testing.T, leaf string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(leaf), 0700); err != nil {
		t.Fatalf("pre-creating parent of %q: %v", leaf, err)
	}
}

// -- AC #1: three-tier chain resolution --------------------------------------

// TestResolveSocketDir_OverrideTierWins covers the top of the chain:
// $CENCI_SOCKET_DIR, when set, wins verbatim (no appended segment) over
// every lower tier.
func TestResolveSocketDir_OverrideTierWins(t *testing.T) {
	base := t.TempDir()
	leaf := filepath.Join(base, "sock")
	t.Setenv("CENCI_SOCKET_DIR", leaf)
	t.Setenv("XDG_STATE_HOME", t.TempDir()) // present but must lose to the override

	res, err := ResolveSocketDir()
	if err != nil {
		t.Fatalf("ResolveSocketDir() error: %v", err)
	}
	if res.Dir != leaf {
		t.Errorf("Dir = %q, want %q (verbatim override, no appended segment)", res.Dir, leaf)
	}
	if res.Tier != TierOverride {
		t.Errorf("Tier = %q, want %q", res.Tier, TierOverride)
	}
}

// TestResolveSocketDir_StateTierWins covers the middle tier: with no
// override set, $XDG_STATE_HOME/cenci/run wins.
func TestResolveSocketDir_StateTierWins(t *testing.T) {
	t.Setenv("CENCI_SOCKET_DIR", "")
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)

	want := filepath.Join(base, "cenci", "run")
	res, err := ResolveSocketDir()
	if err != nil {
		t.Fatalf("ResolveSocketDir() error: %v", err)
	}
	if res.Dir != want {
		t.Errorf("Dir = %q, want %q", res.Dir, want)
	}
	if res.Tier != TierState {
		t.Errorf("Tier = %q, want %q", res.Tier, TierState)
	}
}

// TestResolveSocketDir_TmpTierWins covers the bottom tier: with no override
// and no usable state root, /tmp/cenci-<uid>/cenci wins.
func TestResolveSocketDir_TmpTierWins(t *testing.T) {
	t.Setenv("CENCI_SOCKET_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)

	want := filepath.Join(tmpRoot, fmt.Sprintf("cenci-%d", os.Getuid()), "cenci")
	res, err := ResolveSocketDir()
	if err != nil {
		t.Fatalf("ResolveSocketDir() error: %v", err)
	}
	if res.Dir != want {
		t.Errorf("Dir = %q, want %q", res.Dir, want)
	}
	if res.Tier != TierTmp {
		t.Errorf("Tier = %q, want %q", res.Tier, TierTmp)
	}
}

// TestResolveSocketDir_BogusXDGRuntimeDirHasNoEffect covers the removal half
// of AC #1: no socket-resolution code path reads $XDG_RUNTIME_DIR anymore, so
// a set-but-bogus value must never influence the resolved directory.
func TestResolveSocketDir_BogusXDGRuntimeDirHasNoEffect(t *testing.T) {
	t.Setenv("CENCI_SOCKET_DIR", "")
	base := t.TempDir()
	t.Setenv("XDG_STATE_HOME", base)
	t.Setenv("XDG_RUNTIME_DIR", "/nonexistent/bogus/xdg-runtime-dir-must-be-ignored")

	want := filepath.Join(base, "cenci", "run")
	res, err := ResolveSocketDir()
	if err != nil {
		t.Fatalf("ResolveSocketDir() error: %v", err)
	}
	if res.Dir != want {
		t.Errorf("a bogus XDG_RUNTIME_DIR affected resolution: Dir = %q, want %q", res.Dir, want)
	}
	if res.Tier != TierState {
		t.Errorf("Tier = %q, want %q", res.Tier, TierState)
	}
}

// -- AC #2: leaf hardening per tier (12 cases: 4 behaviors x 3 tiers, plus a
// 0700 no-warn counter-case per tier) ----------------------------------------

// TestSocketDir_HardensFreshLeafTo0700 covers the "created 0700 when absent"
// behavior of AC #2, for each of the three tiers.
func TestSocketDir_HardensFreshLeafTo0700(t *testing.T) {
	for _, tier := range socketDirTiers() {
		t.Run(tier.name, func(t *testing.T) {
			leaf := tier.configure(t)

			res, err := ResolveSocketDir()
			if err != nil {
				t.Fatalf("ResolveSocketDir() error: %v", err)
			}
			if res.Dir != leaf {
				t.Errorf("Dir = %q, want %q", res.Dir, leaf)
			}
			info, err := os.Stat(leaf)
			if err != nil {
				t.Fatalf("stat(%q): %v", leaf, err)
			}
			if !info.IsDir() {
				t.Fatalf("%q is not a directory", leaf)
			}
			if perm := info.Mode().Perm(); perm != 0700 {
				t.Errorf("leaf permissions = %04o, want 0700", perm)
			}
		})
	}
}

// TestSocketDir_RejectsSymlinkAtLeaf covers the "symlink path rejected"
// behavior of AC #2, for each of the three tiers: the resolver must refuse
// to follow the symlink and must leave it untouched.
func TestSocketDir_RejectsSymlinkAtLeaf(t *testing.T) {
	for _, tier := range socketDirTiers() {
		t.Run(tier.name, func(t *testing.T) {
			leaf := tier.configure(t)
			ensureParentDir(t, leaf)

			target := t.TempDir()
			if err := os.Symlink(target, leaf); err != nil {
				t.Fatalf("planting symlink %q -> %q: %v", leaf, target, err)
			}

			_, err := ResolveSocketDir()
			if err == nil {
				t.Fatalf("ResolveSocketDir() error = nil, want error when the leaf is a symlink")
			}
			if !strings.Contains(err.Error(), "symlink") {
				t.Errorf("error = %q, want it to mention %q", err.Error(), "symlink")
			}

			linkInfo, lerr := os.Lstat(leaf)
			if lerr != nil {
				t.Fatalf("Lstat(%q): %v", leaf, lerr)
			}
			if linkInfo.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("expected %q to still be a symlink, got mode %v", leaf, linkInfo.Mode())
			}
			gotTarget, rerr := os.Readlink(leaf)
			if rerr != nil {
				t.Fatalf("Readlink(%q): %v", leaf, rerr)
			}
			if gotTarget != target {
				t.Errorf("symlink target changed: got %q, want %q", gotTarget, target)
			}
		})
	}
}

// TestSocketDir_RejectsPlainFileAtLeaf covers the "non-directory path
// rejected" behavior of AC #2, for each of the three tiers.
func TestSocketDir_RejectsPlainFileAtLeaf(t *testing.T) {
	for _, tier := range socketDirTiers() {
		t.Run(tier.name, func(t *testing.T) {
			leaf := tier.configure(t)
			ensureParentDir(t, leaf)

			if err := os.WriteFile(leaf, []byte("not a directory"), 0600); err != nil {
				t.Fatalf("planting plain file at %q: %v", leaf, err)
			}

			_, err := ResolveSocketDir()
			if err == nil {
				t.Fatalf("ResolveSocketDir() error = nil, want error when the leaf is a plain file")
			}
			if !strings.Contains(err.Error(), "not a directory") {
				t.Errorf("error = %q, want it to mention %q", err.Error(), "not a directory")
			}
		})
	}
}

// TestSocketDir_WarnsOnLoosePermissionsWithoutChmod covers the "loose
// permission (0077) warning without chmod on a pre-existing directory"
// behavior of AC #2, for each of the three tiers: the resolver must still
// succeed (no chmod fight with an already-mounted dir) but must log a
// non-fatal warning.
func TestSocketDir_WarnsOnLoosePermissionsWithoutChmod(t *testing.T) {
	for _, tier := range socketDirTiers() {
		t.Run(tier.name, func(t *testing.T) {
			leaf := tier.configure(t)
			ensureParentDir(t, leaf)

			if err := os.MkdirAll(leaf, 0700); err != nil {
				t.Fatalf("pre-creating leaf %q: %v", leaf, err)
			}
			// Chmod bypasses umask, ensuring the dir is actually group/world
			// accessible regardless of the process umask.
			if err := os.Chmod(leaf, 0755); err != nil {
				t.Fatalf("chmod %q: %v", leaf, err)
			}

			logBuf := captureLog(t)
			res, err := ResolveSocketDir()
			if err != nil {
				t.Fatalf("ResolveSocketDir() error: %v", err)
			}
			if res.Dir != leaf {
				t.Errorf("Dir = %q, want %q", res.Dir, leaf)
			}
			if !strings.Contains(strings.ToLower(logBuf.String()), "warning") {
				t.Errorf("expected a warning to be logged for loose permissions on %q, got log output: %q", leaf, logBuf.String())
			}

			info, err := os.Stat(leaf)
			if err != nil {
				t.Fatalf("stat(%q): %v", leaf, err)
			}
			if perm := info.Mode().Perm(); perm != 0755 {
				t.Errorf("leaf permissions changed to %04o, want unchanged 0755 (no chmod fight)", perm)
			}
		})
	}
}

// TestSocketDir_NoWarnOnStrictPermissions is the counter-case of the
// preceding test: a pre-existing leaf that is already 0700 or stricter must
// not trigger any warning, for each of the three tiers.
func TestSocketDir_NoWarnOnStrictPermissions(t *testing.T) {
	for _, tier := range socketDirTiers() {
		t.Run(tier.name, func(t *testing.T) {
			leaf := tier.configure(t)
			ensureParentDir(t, leaf)

			if err := os.MkdirAll(leaf, 0700); err != nil {
				t.Fatalf("pre-creating leaf %q: %v", leaf, err)
			}
			// Chmod explicitly (bypassing umask) so the dir is provably 0700
			// regardless of the process umask.
			if err := os.Chmod(leaf, 0700); err != nil {
				t.Fatalf("chmod %q: %v", leaf, err)
			}

			logBuf := captureLog(t)
			res, err := ResolveSocketDir()
			if err != nil {
				t.Fatalf("ResolveSocketDir() error: %v", err)
			}
			if res.Dir != leaf {
				t.Errorf("Dir = %q, want %q", res.Dir, leaf)
			}
			if strings.Contains(strings.ToLower(logBuf.String()), "warning") {
				t.Errorf("expected no warning for strict permissions on %q, got log output: %q", leaf, logBuf.String())
			}
		})
	}
}

// TestSocketDir_RejectsWritableLeaf covers the "group/other-writable
// pre-existing directory hard-rejected" behavior restored by the #1142
// Phase 6+7 review (Fix 1): unlike the read/execute-only 0755 case above
// (which only warns), a leaf that is group- or other-**writable** must hard
// error instead of merely warning, for each of the three tiers.
func TestSocketDir_RejectsWritableLeaf(t *testing.T) {
	for _, tier := range socketDirTiers() {
		t.Run(tier.name, func(t *testing.T) {
			leaf := tier.configure(t)
			ensureParentDir(t, leaf)

			if err := os.MkdirAll(leaf, 0700); err != nil {
				t.Fatalf("pre-creating leaf %q: %v", leaf, err)
			}
			// Chmod bypasses umask, ensuring the dir is actually
			// group/other writable regardless of the process umask.
			if err := os.Chmod(leaf, 0777); err != nil {
				t.Fatalf("chmod %q: %v", leaf, err)
			}

			_, err := ResolveSocketDir()
			if err == nil {
				t.Fatalf("ResolveSocketDir() error = nil, want error when the leaf is group/other writable")
			}
			lower := strings.ToLower(err.Error())
			if !strings.Contains(lower, "writable") && !strings.Contains(lower, "insecure permissions") {
				t.Errorf("error = %q, want it to mention %q or %q", err.Error(), "writable", "insecure permissions")
			}

			info, statErr := os.Stat(leaf)
			if statErr != nil {
				t.Fatalf("stat(%q): %v", leaf, statErr)
			}
			if perm := info.Mode().Perm(); perm != 0777 {
				t.Errorf("leaf permissions changed to %04o, want unchanged 0777 (no chmod fight)", perm)
			}
		})
	}
}

// -- AC #3: sun_path length validation ---------------------------------------

// TestResolveSocketDir_TierTwoOverBound_LogsLengthAndBoundThenFallsToTierThree
// shrinks sunPathMax via the package test seam so a tier-2 path that would
// otherwise win is provably over the bound, and asserts the resolver logs
// both the computed length and the bound before falling back to tier 3.
func TestResolveSocketDir_TierTwoOverBound_LogsLengthAndBoundThenFallsToTierThree(t *testing.T) {
	orig := sunPathMax
	sunPathMax = 40
	t.Cleanup(func() { sunPathMax = orig })

	t.Setenv("CENCI_SOCKET_DIR", "")
	base := t.TempDir() // embeds the full (long) test name -- easily over 40 bytes alone
	t.Setenv("XDG_STATE_HOME", base)
	tier2Leaf := filepath.Join(base, "cenci", "run")
	computedLen := len(tier2Leaf) + 1 + len(EventSocketBasename)
	if computedLen <= sunPathMax {
		t.Fatalf("test fixture invalid: computed length %d does not exceed the shrunk bound %d", computedLen, sunPathMax)
	}

	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)
	wantTier3 := filepath.Join(tmpRoot, fmt.Sprintf("cenci-%d", os.Getuid()), "cenci")

	logBuf := captureLog(t)
	res, err := ResolveSocketDir()
	if err != nil {
		t.Fatalf("ResolveSocketDir() error: %v", err)
	}
	if res.Tier != TierTmp {
		t.Errorf("Tier = %q, want %q (an over-bound tier-2 path must fall through to tier 3)", res.Tier, TierTmp)
	}
	if res.Dir != wantTier3 {
		t.Errorf("Dir = %q, want %q", res.Dir, wantTier3)
	}

	logOut := logBuf.String()
	if !strings.Contains(logOut, fmt.Sprintf("%d", computedLen)) {
		t.Errorf("log output missing the computed length %d, got: %q", computedLen, logOut)
	}
	if !strings.Contains(logOut, fmt.Sprintf("%d", sunPathMax)) {
		t.Errorf("log output missing the sun_path bound %d, got: %q", sunPathMax, logOut)
	}
}

// TestResolveSocketDir_TierTwoOverBound_RealBoundWithoutShrinkingSeam
// exercises the same fallback WITHOUT touching the sunPathMax test seam: a
// genuinely deep $XDG_STATE_HOME naturally exceeds the platform's real
// default bound, proving the bound isn't only enforced when a test shrinks
// it artificially.
func TestResolveSocketDir_TierTwoOverBound_RealBoundWithoutShrinkingSeam(t *testing.T) {
	t.Setenv("CENCI_SOCKET_DIR", "")
	deepBase := filepath.Join(t.TempDir(), strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40))
	t.Setenv("XDG_STATE_HOME", deepBase)
	tier2Leaf := filepath.Join(deepBase, "cenci", "run")
	if computedLen := len(tier2Leaf) + 1 + len(EventSocketBasename); computedLen <= sunPathMax {
		t.Fatalf("test fixture invalid: computed length %d does not exceed the real bound %d", computedLen, sunPathMax)
	}

	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)
	wantTier3 := filepath.Join(tmpRoot, fmt.Sprintf("cenci-%d", os.Getuid()), "cenci")

	res, err := ResolveSocketDir()
	if err != nil {
		t.Fatalf("ResolveSocketDir() error: %v", err)
	}
	if res.Tier != TierTmp {
		t.Errorf("Tier = %q, want %q", res.Tier, TierTmp)
	}
	if res.Dir != wantTier3 {
		t.Errorf("Dir = %q, want %q", res.Dir, wantTier3)
	}
}

// -- AC #4: an unusable $CENCI_SOCKET_DIR hard-errors, never falls through ---

// TestResolveSocketDir_OverrideUnusable_HardErrorsWithNoFallThrough covers
// five distinct tier-1 failure classes. Each asserts a content-specific
// error substring (never just non-nil, watch/docs/error-handling.md #446)
// and that the result is not the tier-2 or tier-3 path that would have won
// had tier 1 fallen through.
func TestResolveSocketDir_OverrideUnusable_HardErrorsWithNoFallThrough(t *testing.T) {
	// nonWinners configures tier 2/3 to genuinely resolvable paths (so a
	// silent fall-through would be observable) and returns what they'd be.
	nonWinners := func(t *testing.T) (tier2, tier3 string) {
		t.Helper()
		stateBase := t.TempDir()
		t.Setenv("XDG_STATE_HOME", stateBase)
		tmpRoot := t.TempDir()
		t.Setenv("TMPDIR", tmpRoot)
		return filepath.Join(stateBase, "cenci", "run"), filepath.Join(tmpRoot, fmt.Sprintf("cenci-%d", os.Getuid()), "cenci")
	}

	assertHardError := func(t *testing.T, wantSubstr, tier2, tier3 string) {
		t.Helper()
		res, err := ResolveSocketDir()
		if err == nil {
			t.Fatalf("ResolveSocketDir() error = nil, want a hard error naming %q", wantSubstr)
		}
		if !strings.Contains(err.Error(), wantSubstr) {
			t.Errorf("error = %q, want it to mention %q", err.Error(), wantSubstr)
		}
		if res.Dir == tier2 {
			t.Errorf("silently fell through to the tier-2 path %q instead of hard-erroring", tier2)
		}
		if res.Dir == tier3 {
			t.Errorf("silently fell through to the tier-3 path %q instead of hard-erroring", tier3)
		}
	}

	t.Run("relative_path", func(t *testing.T) {
		tier2, tier3 := nonWinners(t)
		t.Setenv("CENCI_SOCKET_DIR", "relative/socket-dir")
		assertHardError(t, "absolute", tier2, tier3)
	})

	t.Run("over_sun_path_bound", func(t *testing.T) {
		tier2, tier3 := nonWinners(t)
		orig := sunPathMax
		sunPathMax = 20
		t.Cleanup(func() { sunPathMax = orig })
		base := t.TempDir()
		long := filepath.Join(base, strings.Repeat("x", 40))
		t.Setenv("CENCI_SOCKET_DIR", long)
		assertHardError(t, fmt.Sprintf("%d", sunPathMax), tier2, tier3)
	})

	t.Run("symlink", func(t *testing.T) {
		tier2, tier3 := nonWinners(t)
		base := t.TempDir()
		target := t.TempDir()
		leaf := filepath.Join(base, "sock")
		if err := os.Symlink(target, leaf); err != nil {
			t.Fatalf("planting symlink: %v", err)
		}
		t.Setenv("CENCI_SOCKET_DIR", leaf)
		assertHardError(t, "symlink", tier2, tier3)
	})

	t.Run("plain_file", func(t *testing.T) {
		tier2, tier3 := nonWinners(t)
		base := t.TempDir()
		leaf := filepath.Join(base, "sock")
		if err := os.WriteFile(leaf, []byte("nope"), 0600); err != nil {
			t.Fatalf("planting plain file: %v", err)
		}
		t.Setenv("CENCI_SOCKET_DIR", leaf)
		assertHardError(t, "not a directory", tier2, tier3)
	})

	t.Run("uncreatable_parent", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root bypasses Unix directory permission checks; cannot simulate an uncreatable parent")
		}
		tier2, tier3 := nonWinners(t)
		parent := t.TempDir()
		if err := os.Chmod(parent, 0500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
		leaf := filepath.Join(parent, "sock")
		t.Setenv("CENCI_SOCKET_DIR", leaf)
		assertHardError(t, "permission denied", tier2, tier3)
	})
}

// -- AC #5: unresolvable/uncreatable state root falls back to tier 3 --------

// TestResolveSocketDir_HomeAndStateHomeBothEmpty_LogsReasonAndFallsToTierThree
// covers the case where neither $HOME nor $XDG_STATE_HOME can produce a
// state root at all.
func TestResolveSocketDir_HomeAndStateHomeBothEmpty_LogsReasonAndFallsToTierThree(t *testing.T) {
	t.Setenv("CENCI_SOCKET_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)
	want := filepath.Join(tmpRoot, fmt.Sprintf("cenci-%d", os.Getuid()), "cenci")

	logBuf := captureLog(t)
	res, err := ResolveSocketDir()
	if err != nil {
		t.Fatalf("ResolveSocketDir() error: %v", err)
	}
	if res.Tier != TierTmp {
		t.Errorf("Tier = %q, want %q", res.Tier, TierTmp)
	}
	if res.Dir != want {
		t.Errorf("Dir = %q, want %q", res.Dir, want)
	}
	logOut := strings.ToLower(logBuf.String())
	if !strings.Contains(logOut, "home") {
		t.Errorf("expected a logged reason naming HOME/XDG_STATE_HOME resolution failure, got: %q", logBuf.String())
	}
}

// TestResolveSocketDir_StateDirUncreatable_LogsReasonAndFallsToTierThree
// covers the case where $XDG_STATE_HOME resolves but the directory itself
// cannot be created (e.g. a read-only parent).
func TestResolveSocketDir_StateDirUncreatable_LogsReasonAndFallsToTierThree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses Unix directory permission checks; cannot simulate an uncreatable state dir")
	}
	t.Setenv("CENCI_SOCKET_DIR", "")
	parent := t.TempDir()
	if err := os.Chmod(parent, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
	t.Setenv("XDG_STATE_HOME", filepath.Join(parent, "state"))
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)
	want := filepath.Join(tmpRoot, fmt.Sprintf("cenci-%d", os.Getuid()), "cenci")

	logBuf := captureLog(t)
	res, err := ResolveSocketDir()
	if err != nil {
		t.Fatalf("ResolveSocketDir() error: %v", err)
	}
	if res.Tier != TierTmp {
		t.Errorf("Tier = %q, want %q", res.Tier, TierTmp)
	}
	if res.Dir != want {
		t.Errorf("Dir = %q, want %q", res.Dir, want)
	}
	if !strings.Contains(strings.ToLower(logBuf.String()), "warning") {
		t.Errorf("expected a logged reason for the uncreatable state dir, got: %q", logBuf.String())
	}
}

// -- Regression: container bind-mount idempotency and shared socket parent --

// TestSocketDir_IdempotentAgainstPreCreatedDir covers the container
// bind-mount case called out in the plan's Risks section: a container may
// pre-create the leaf mountpoint (with host ownership) before cenci itself
// ever runs. SocketDir() must not error against that, and repeated calls
// must keep resolving to the same path without error.
func TestSocketDir_IdempotentAgainstPreCreatedDir(t *testing.T) {
	for _, tier := range socketDirTiers() {
		t.Run(tier.name, func(t *testing.T) {
			leaf := tier.configure(t)

			// Simulate a container bind-mounting the leaf before cenci ever
			// runs (host pre-creates the mountpoint).
			if err := os.MkdirAll(leaf, 0700); err != nil {
				t.Fatalf("pre-creating %q: %v", leaf, err)
			}

			got, err := SocketDir()
			if err != nil {
				t.Fatalf("SocketDir() must not error against a pre-existing dir: %v", err)
			}
			if got != leaf {
				t.Errorf("SocketDir() = %q, want %q", got, leaf)
			}

			second, err := SocketDir()
			if err != nil {
				t.Fatalf("second SocketDir() call error (must be idempotent): %v", err)
			}
			if second != leaf {
				t.Errorf("second SocketDir() call = %q, want %q", second, leaf)
			}
		})
	}
}

// TestSocketNames_ShareSocketDirParent guards the risk called out in the
// plan: if defaultSocketPath isn't rerouted through SocketDir(), the
// broadcast and events sockets would split across two different
// directories. Both must resolve inside whichever directory SocketDir()
// returns, across all three tiers.
func TestSocketNames_ShareSocketDirParent(t *testing.T) {
	for _, tier := range socketDirTiers() {
		t.Run(tier.name, func(t *testing.T) {
			// wantParent is built from primitives (not a live SocketDir()
			// call): pins the actual leaf-path shape from the AC.
			wantParent := tier.configure(t)

			broadcast := DefaultSocketPath()
			events := defaultSocketPath("cenci-events")

			if filepath.Dir(broadcast) != wantParent {
				t.Errorf("cenci.sock parent = %q, want %q", filepath.Dir(broadcast), wantParent)
			}
			if filepath.Dir(events) != wantParent {
				t.Errorf("cenci-events.sock parent = %q, want %q", filepath.Dir(events), wantParent)
			}
			if filepath.Dir(broadcast) != filepath.Dir(events) {
				t.Errorf("cenci.sock and cenci-events.sock must share a parent dir; got %q vs %q", filepath.Dir(broadcast), filepath.Dir(events))
			}
		})
	}
}

// -- #1147: hardenDir sentinel classification --------------------------------

// TestHardenDir_ClassifiesInsecureVsUncreatable directly exercises hardenDir
// (in-package call, no resolver involved) to pin the errInsecureDir sentinel
// classification per watch/docs/error-handling.md #412: errors.Is must be
// true for each of the three security rejections (symlink, plain file, 0777
// dir) and false — but still non-nil — for a plain creation (MkdirAll)
// failure, so callers can tell "proven hostile" apart from "cannot
// determine".
func TestHardenDir_ClassifiesInsecureVsUncreatable(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		base := t.TempDir()
		target := t.TempDir()
		dir := filepath.Join(base, "insecure")
		if err := os.Symlink(target, dir); err != nil {
			t.Fatalf("planting symlink %q -> %q: %v", dir, target, err)
		}

		err := hardenDir(dir)
		if err == nil {
			t.Fatal("hardenDir() error = nil, want error for a symlink")
		}
		if !errors.Is(err, errInsecureDir) {
			t.Errorf("errors.Is(err, errInsecureDir) = false, want true for a symlink; err = %v", err)
		}
	})

	t.Run("plain_file", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, "insecure")
		if err := os.WriteFile(dir, []byte("not a directory"), 0600); err != nil {
			t.Fatalf("planting plain file at %q: %v", dir, err)
		}

		err := hardenDir(dir)
		if err == nil {
			t.Fatal("hardenDir() error = nil, want error for a plain file")
		}
		if !errors.Is(err, errInsecureDir) {
			t.Errorf("errors.Is(err, errInsecureDir) = false, want true for a plain file; err = %v", err)
		}
	})

	t.Run("writable_dir", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, "insecure")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatalf("pre-creating %q: %v", dir, err)
		}
		if err := os.Chmod(dir, 0777); err != nil {
			t.Fatalf("chmod %q: %v", dir, err)
		}

		err := hardenDir(dir)
		if err == nil {
			t.Fatal("hardenDir() error = nil, want error for a 0777 directory")
		}
		if !errors.Is(err, errInsecureDir) {
			t.Errorf("errors.Is(err, errInsecureDir) = false, want true for a 0777 directory; err = %v", err)
		}
	})

	t.Run("uncreatable", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root bypasses Unix directory permission checks; cannot simulate an uncreatable directory")
		}
		parent := t.TempDir()
		if err := os.Chmod(parent, 0500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(parent, 0700) })
		dir := filepath.Join(parent, "unusable")

		err := hardenDir(dir)
		if err == nil {
			t.Fatal("hardenDir() error = nil, want error for an uncreatable directory")
		}
		if errors.Is(err, errInsecureDir) {
			t.Errorf("errors.Is(err, errInsecureDir) = true, want false for an MkdirAll failure (this is availability, not security); err = %v", err)
		}
	})
}

// TestHardenDir_RejectsForeignOwnedDirectory covers #1147 Fix 2: a
// pre-existing directory at an otherwise-acceptable permission (0755, so
// perm&0022==0 and the writable check alone would pass) but owned by a
// different uid must still be hard-rejected as errInsecureDir. This mirrors
// the local-attacker scenario of pre-creating a predictable base (e.g.
// /tmp/cenci-<uid>) they own before the victim ever runs cenci: without an
// ownership check, the victim's own MkdirAll on the leaf inside it fails
// with a plain (unclassified) EACCES, which callers would otherwise treat
// as a mere availability failure and fall back to the unhardened flat /tmp
// path.
//
// Constructing a genuinely foreign-owned directory needs root or a second
// real system user, so this stubs dirOwnerUID -- the same seam pattern
// internal/daemon/processctl.go's pidFileOwner uses for
// TestReadPIDFile_RefusesForeignOwnedFile -- rather than requiring a second
// real uid. Verified to behave the same under root: the stub changes what
// hardenDir treats as "the expected owner", not the directory's actual
// (real) owning uid, so the mismatch is provable regardless of Geteuid().
func TestHardenDir_RejectsForeignOwnedDirectory(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "insecure")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("pre-creating %q: %v", dir, err)
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatalf("chmod %q: %v", dir, err)
	}

	restore := dirOwnerUID
	dirOwnerUID = func() int { return os.Getuid() + 1 }
	t.Cleanup(func() { dirOwnerUID = restore })

	err := hardenDir(dir)
	if err == nil {
		t.Fatal("hardenDir() error = nil, want error for a foreign-owned directory")
	}
	if !errors.Is(err, errInsecureDir) {
		t.Errorf("errors.Is(err, errInsecureDir) = false, want true for a foreign-owned directory; err = %v", err)
	}
	if !strings.Contains(err.Error(), "not owned") {
		t.Errorf("error = %q, want it to mention %q", err.Error(), "not owned")
	}
}

// -- #1147: base hardening for the state and tmp tiers -----------------------

// TestSocketDir_RejectsSymlinkAtBase covers a symlink planted at the tier's
// *base* (filepath.Dir(leaf): /tmp/cenci-<uid> or $XDG_STATE_HOME/cenci) for
// the two hardened tiers, mirroring TestSocketDir_RejectsSymlinkAtLeaf: the
// resolver must refuse to follow the symlink and must leave it untouched.
func TestSocketDir_RejectsSymlinkAtBase(t *testing.T) {
	for _, tier := range socketDirTiers() {
		if !tier.baseHardened {
			continue
		}
		t.Run(tier.name, func(t *testing.T) {
			leaf := tier.configure(t)
			base := filepath.Dir(leaf)
			ensureParentDir(t, base)

			target := t.TempDir()
			if err := os.Symlink(target, base); err != nil {
				t.Fatalf("planting symlink %q -> %q: %v", base, target, err)
			}

			_, err := ResolveSocketDir()
			if err == nil {
				t.Fatalf("ResolveSocketDir() error = nil, want error when the base is a symlink")
			}
			if !strings.Contains(err.Error(), "symlink") {
				t.Errorf("error = %q, want it to mention %q", err.Error(), "symlink")
			}

			linkInfo, lerr := os.Lstat(base)
			if lerr != nil {
				t.Fatalf("Lstat(%q): %v", base, lerr)
			}
			if linkInfo.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("expected %q to still be a symlink, got mode %v", base, linkInfo.Mode())
			}
			gotTarget, rerr := os.Readlink(base)
			if rerr != nil {
				t.Fatalf("Readlink(%q): %v", base, rerr)
			}
			if gotTarget != target {
				t.Errorf("symlink target changed: got %q, want %q", gotTarget, target)
			}
		})
	}
}

// TestSocketDir_RejectsPlainFileAtBase covers a plain file planted at the
// tier's base for the two hardened tiers.
func TestSocketDir_RejectsPlainFileAtBase(t *testing.T) {
	for _, tier := range socketDirTiers() {
		if !tier.baseHardened {
			continue
		}
		t.Run(tier.name, func(t *testing.T) {
			leaf := tier.configure(t)
			base := filepath.Dir(leaf)
			ensureParentDir(t, base)

			if err := os.WriteFile(base, []byte("not a directory"), 0600); err != nil {
				t.Fatalf("planting plain file at %q: %v", base, err)
			}

			_, err := ResolveSocketDir()
			if err == nil {
				t.Fatalf("ResolveSocketDir() error = nil, want error when the base is a plain file")
			}
			if !strings.Contains(err.Error(), "not a directory") {
				t.Errorf("error = %q, want it to mention %q", err.Error(), "not a directory")
			}
		})
	}
}

// TestSocketDir_RejectsWritableBase covers a group/other-writable (0777)
// pre-existing base for the two hardened tiers: this must hard-error, not
// merely warn, and the removal of the old unconditional os.Chmod(base, 0700)
// on the tmp tier must not silently repair it.
func TestSocketDir_RejectsWritableBase(t *testing.T) {
	for _, tier := range socketDirTiers() {
		if !tier.baseHardened {
			continue
		}
		t.Run(tier.name, func(t *testing.T) {
			leaf := tier.configure(t)
			base := filepath.Dir(leaf)
			ensureParentDir(t, base)

			if err := os.MkdirAll(base, 0700); err != nil {
				t.Fatalf("pre-creating base %q: %v", base, err)
			}
			// Chmod bypasses umask, ensuring the base is actually
			// group/other writable regardless of the process umask.
			if err := os.Chmod(base, 0777); err != nil {
				t.Fatalf("chmod %q: %v", base, err)
			}

			_, err := ResolveSocketDir()
			if err == nil {
				t.Fatalf("ResolveSocketDir() error = nil, want error when the base is group/other writable")
			}
			lower := strings.ToLower(err.Error())
			if !strings.Contains(lower, "writable") && !strings.Contains(lower, "insecure permissions") {
				t.Errorf("error = %q, want it to mention %q or %q", err.Error(), "writable", "insecure permissions")
			}

			info, statErr := os.Stat(base)
			if statErr != nil {
				t.Fatalf("stat(%q): %v", base, statErr)
			}
			if perm := info.Mode().Perm(); perm != 0777 {
				t.Errorf("base permissions changed to %04o, want unchanged 0777 (no chmod fight)", perm)
			}
		})
	}
}

// TestSocketDir_WarnsOnLooseBasePermissionsWithoutChmod covers a pre-existing
// base at 0755 for the two hardened tiers: resolution must still succeed (no
// chmod fight with an already-mounted dir) but must log a non-fatal warning,
// and the base must be left at 0755 — this is the AC #3 replacement for the
// old unconditional os.Chmod(base, 0700) on the tmp tier.
func TestSocketDir_WarnsOnLooseBasePermissionsWithoutChmod(t *testing.T) {
	for _, tier := range socketDirTiers() {
		if !tier.baseHardened {
			continue
		}
		t.Run(tier.name, func(t *testing.T) {
			leaf := tier.configure(t)
			base := filepath.Dir(leaf)
			ensureParentDir(t, base)

			if err := os.MkdirAll(base, 0700); err != nil {
				t.Fatalf("pre-creating base %q: %v", base, err)
			}
			if err := os.Chmod(base, 0755); err != nil {
				t.Fatalf("chmod %q: %v", base, err)
			}

			logBuf := captureLog(t)
			res, err := ResolveSocketDir()
			if err != nil {
				t.Fatalf("ResolveSocketDir() error: %v", err)
			}
			if res.Dir != leaf {
				t.Errorf("Dir = %q, want %q", res.Dir, leaf)
			}
			if !strings.Contains(strings.ToLower(logBuf.String()), "warning") {
				t.Errorf("expected a warning to be logged for loose permissions on %q, got log output: %q", base, logBuf.String())
			}

			info, statErr := os.Stat(base)
			if statErr != nil {
				t.Fatalf("stat(%q): %v", base, statErr)
			}
			if perm := info.Mode().Perm(); perm != 0755 {
				t.Errorf("base permissions changed to %04o, want unchanged 0755 (no chmod fight)", perm)
			}
		})
	}
}

// TestSocketDir_FreshBaseIsCreated0700 covers the counter-case: a base that
// does not exist yet must still be created at 0700, for the two hardened
// tiers — proving the removal of the tmp tier's old unconditional
// os.Chmod(base, 0700) doesn't regress the fresh-creation case.
func TestSocketDir_FreshBaseIsCreated0700(t *testing.T) {
	for _, tier := range socketDirTiers() {
		if !tier.baseHardened {
			continue
		}
		t.Run(tier.name, func(t *testing.T) {
			leaf := tier.configure(t)
			base := filepath.Dir(leaf)

			res, err := ResolveSocketDir()
			if err != nil {
				t.Fatalf("ResolveSocketDir() error: %v", err)
			}
			if res.Dir != leaf {
				t.Errorf("Dir = %q, want %q", res.Dir, leaf)
			}

			info, err := os.Stat(base)
			if err != nil {
				t.Fatalf("stat(%q): %v", base, err)
			}
			if !info.IsDir() {
				t.Fatalf("%q is not a directory", base)
			}
			if perm := info.Mode().Perm(); perm != 0700 {
				t.Errorf("base permissions = %04o, want 0700", perm)
			}
		})
	}
}

// -- #1147: an insecure state root hard-errors with no fall-through ---------

// TestResolveSocketDir_InsecureStateRoot_HardErrorsWithNoFallThrough covers
// the state tier's new two-class split at the *base* ($XDG_STATE_HOME/cenci):
// a symlink, a plain file, or a group/other-writable (0777) state root must
// hard-error the whole resolution instead of falling through to the tmp
// tier, mirroring TestResolveSocketDir_OverrideUnusable_HardErrorsWithNoFallThrough.
// TMPDIR is isolated per watch/docs/test-isolation.md so a real fall-through
// would be observable rather than accidentally landing on a developer's
// ambient /tmp/cenci-<uid>.
func TestResolveSocketDir_InsecureStateRoot_HardErrorsWithNoFallThrough(t *testing.T) {
	// shortStateBase returns a fresh directory suitable for use as
	// $XDG_STATE_HOME in these subtests. Unlike t.TempDir(), it does not
	// embed this test's own (long) name into the path: that name, run
	// through the sun_path length check on <stateBase>/cenci/run, already
	// exceeds sunPathMax before hardenDir(stateRoot) ever runs -- a fixture
	// hazard unrelated to the security behavior under test, mirroring the
	// precaution socketDirTiers()'s "override" fixture documents for the
	// same reason.
	shortStateBase := func(t *testing.T) string {
		t.Helper()
		dir, err := os.MkdirTemp("", "state")
		if err != nil {
			t.Fatalf("MkdirTemp: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}

	setupTier3 := func(t *testing.T) string {
		t.Helper()
		tmpRoot := t.TempDir()
		t.Setenv("TMPDIR", tmpRoot)
		return filepath.Join(tmpRoot, fmt.Sprintf("cenci-%d", os.Getuid()), "cenci")
	}

	assertHardError := func(t *testing.T, wantSubstr, stateLeaf, tier3 string) {
		t.Helper()
		res, err := ResolveSocketDir()
		if err == nil {
			t.Fatalf("ResolveSocketDir() error = nil, want a hard error naming %q", wantSubstr)
		}
		lower := strings.ToLower(err.Error())
		if !strings.Contains(lower, strings.ToLower(wantSubstr)) {
			t.Errorf("error = %q, want it to mention %q", err.Error(), wantSubstr)
		}
		if res.Dir == stateLeaf {
			t.Errorf("resolved to the state-tier leaf %q despite a hard error", stateLeaf)
		}
		if res.Dir == tier3 {
			t.Errorf("silently fell through to the tier-3 path %q instead of hard-erroring", tier3)
		}
	}

	t.Run("symlink", func(t *testing.T) {
		t.Setenv("CENCI_SOCKET_DIR", "")
		stateBase := shortStateBase(t)
		t.Setenv("XDG_STATE_HOME", stateBase)
		stateRoot := filepath.Join(stateBase, "cenci")
		stateLeaf := filepath.Join(stateRoot, "run")
		tier3 := setupTier3(t)

		target := t.TempDir()
		if err := os.Symlink(target, stateRoot); err != nil {
			t.Fatalf("planting symlink %q -> %q: %v", stateRoot, target, err)
		}

		assertHardError(t, "symlink", stateLeaf, tier3)
	})

	t.Run("plain_file", func(t *testing.T) {
		t.Setenv("CENCI_SOCKET_DIR", "")
		stateBase := shortStateBase(t)
		t.Setenv("XDG_STATE_HOME", stateBase)
		stateRoot := filepath.Join(stateBase, "cenci")
		stateLeaf := filepath.Join(stateRoot, "run")
		tier3 := setupTier3(t)

		if err := os.WriteFile(stateRoot, []byte("nope"), 0600); err != nil {
			t.Fatalf("planting plain file at %q: %v", stateRoot, err)
		}

		assertHardError(t, "not a directory", stateLeaf, tier3)
	})

	t.Run("writable", func(t *testing.T) {
		t.Setenv("CENCI_SOCKET_DIR", "")
		stateBase := shortStateBase(t)
		t.Setenv("XDG_STATE_HOME", stateBase)
		stateRoot := filepath.Join(stateBase, "cenci")
		stateLeaf := filepath.Join(stateRoot, "run")
		tier3 := setupTier3(t)

		if err := os.MkdirAll(stateRoot, 0700); err != nil {
			t.Fatalf("pre-creating state root %q: %v", stateRoot, err)
		}
		// Chmod bypasses umask, ensuring the state root is actually
		// group/other writable regardless of the process umask.
		if err := os.Chmod(stateRoot, 0777); err != nil {
			t.Fatalf("chmod %q: %v", stateRoot, err)
		}

		res, err := ResolveSocketDir()
		if err == nil {
			t.Fatalf("ResolveSocketDir() error = nil, want a hard error for a 0777 state root")
		}
		lower := strings.ToLower(err.Error())
		if !strings.Contains(lower, "writable") && !strings.Contains(lower, "insecure permissions") {
			t.Errorf("error = %q, want it to mention %q or %q", err.Error(), "writable", "insecure permissions")
		}
		if res.Dir == stateLeaf {
			t.Errorf("resolved to the state-tier leaf %q despite a hard error", stateLeaf)
		}
		if res.Dir == tier3 {
			t.Errorf("silently fell through to the tier-3 path %q instead of hard-erroring", tier3)
		}
	})
}

// -- #1147: fallback helpers fail closed on security-relevant errors --------

// poisonTmpTierBase forces resolveTmpTier's own base
// (/tmp/cenci-<uid>/cenci) to be the winning (and only reachable) tier by
// disabling override and state, then plants a hostile object at that base so
// hardenDir's failure is security-relevant (errors.Is(err, errInsecureDir)).
func poisonTmpTierBase(t *testing.T) {
	t.Helper()
	t.Setenv("CENCI_SOCKET_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)

	base := filepath.Join(tmpRoot, fmt.Sprintf("cenci-%d", os.Getuid()))
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatalf("pre-creating base %q: %v", base, err)
	}
	// Chmod bypasses umask, ensuring the base is actually group/other
	// writable regardless of the process umask -- a proven-hostile base per
	// hardenDir, so SocketDir() must return an error classified via
	// errors.Is(err, errInsecureDir).
	if err := os.Chmod(base, 0777); err != nil {
		t.Fatalf("chmod %q: %v", base, err)
	}
}

// TestDefaultSocketPath_PoisonedBase_ReturnsEmpty covers Fix 1's
// security-relevant classification in defaultSocketPath: when SocketDir()
// fails because the tmp tier's base was proven hostile (0777, group/other
// writable), defaultSocketPath must return "" rather than falling back to
// the unhardened flat /tmp/cenci-<uid>-<name>.sock path.
func TestDefaultSocketPath_PoisonedBase_ReturnsEmpty(t *testing.T) {
	poisonTmpTierBase(t)

	got := DefaultSocketPath()
	if got != "" {
		t.Errorf("DefaultSocketPath() = %q, want \"\" (must not fall back to an unhardened /tmp path when the base is proven hostile)", got)
	}
}

// TestDefaultPIDPath_PoisonedBase_ReturnsEmpty covers Fix 1's
// security-relevant classification in DefaultPIDPath: when SocketDir() fails
// because the tmp tier's base was proven hostile (0777, group/other
// writable), DefaultPIDPath must return "" rather than falling back to the
// unhardened flat /tmp/cenci-<uid>.pid path.
func TestDefaultPIDPath_PoisonedBase_ReturnsEmpty(t *testing.T) {
	poisonTmpTierBase(t)

	got := DefaultPIDPath()
	if got != "" {
		t.Errorf("DefaultPIDPath() = %q, want \"\" (must not fall back to an unhardened /tmp path when the base is proven hostile)", got)
	}
}

// -- #1147: fallback helpers keep the availability fallback unchanged -------

// uncreatableTmpTierBase forces resolveTmpTier's base to be uncreatable (a
// read-only parent directory), so SocketDir() fails for a plain availability
// reason rather than a security-relevant one -- proving the fallback helpers'
// existing /tmp flat-path behavior is untouched by Fix 1. Skips under root,
// which bypasses Unix directory permission checks.
func uncreatableTmpTierBase(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses Unix directory permission checks; cannot simulate an uncreatable directory")
	}
	t.Setenv("CENCI_SOCKET_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	tmpRoot := t.TempDir()
	if err := os.Chmod(tmpRoot, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(tmpRoot, 0700) })
	t.Setenv("TMPDIR", tmpRoot)
}

// TestDefaultSocketPath_UncreatableDir_StillFallsBackToFlatTmpPath is the
// counter-case proving Fix 1 left the availability-failure path untouched: an
// uncreatable (not proven-hostile) socket dir must still produce today's flat
// /tmp/cenci-<name>-<uid>.sock fallback path, not "".
func TestDefaultSocketPath_UncreatableDir_StillFallsBackToFlatTmpPath(t *testing.T) {
	uncreatableTmpTierBase(t)

	want := filepath.Join(os.TempDir(), fmt.Sprintf("cenci-%d.sock", os.Getuid()))
	got := DefaultSocketPath()
	if got != want {
		t.Errorf("DefaultSocketPath() = %q, want %q (unchanged availability-failure fallback)", got, want)
	}
}

// TestDefaultPIDPath_UncreatableDir_StillFallsBackToFlatTmpPath is the
// counter-case proving Fix 1 left the availability-failure path untouched: an
// uncreatable (not proven-hostile) socket dir must still produce today's flat
// /tmp/cenci-<uid>.pid fallback path, not "".
func TestDefaultPIDPath_UncreatableDir_StillFallsBackToFlatTmpPath(t *testing.T) {
	uncreatableTmpTierBase(t)

	want := filepath.Join(os.TempDir(), fmt.Sprintf("cenci-%d.pid", os.Getuid()))
	got := DefaultPIDPath()
	if got != want {
		t.Errorf("DefaultPIDPath() = %q, want %q (unchanged availability-failure fallback)", got, want)
	}
}
