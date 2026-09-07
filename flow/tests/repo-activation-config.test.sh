#!/usr/bin/env bash
# Dogfood contract for issue #661. The repository must state its planning
# autonomy explicitly and authorize bounded per-project automerge while
# root-owned files stay fail-closed through the intentional absence of a
# top-level policy.
#
# The autonomy assertion pins explicitness, not a particular mode: which of
# the two documented values this repo commits is a per-repo operational
# choice (it moved from "lean" to "interactive" in #1161), but an absent
# `planning` block or an unrecognized value must never pass -- those are the
# shapes that silently resolve to a default nobody chose.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)" || {
  echo "repo-activation-config.test.sh: failed to resolve script directory." >&2
  exit 2
}
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)" || {
  echo "repo-activation-config.test.sh: failed to resolve repository root." >&2
  exit 2
}
CONFIG="${REPO_ROOT}/.cenci/config.json"
FLOW_WORKFLOW="${REPO_ROOT}/.github/workflows/flow-ci.yml"
WATCH_WORKFLOW="${REPO_ROOT}/.github/workflows/watch-ci.yml"
failures=0

fail() {
  echo "FAIL: $1" >&2
  failures=$((failures + 1))
}

if [[ ! -f "${CONFIG}" ]]; then
  fail "repository config is missing: ${CONFIG}"
elif ! jq empty "${CONFIG}" >/dev/null 2>&1; then
  fail "repository config is not valid JSON: ${CONFIG}"
else
  jq -e '.planning.autonomy == "lean" or .planning.autonomy == "interactive"' "${CONFIG}" >/dev/null 2>&1 ||
    fail "planning.autonomy must be explicitly set to one of the documented values (\"lean\" or \"interactive\")"

  jq -e 'has("automerge") | not' "${CONFIG}" >/dev/null 2>&1 ||
    fail "top-level automerge must stay absent so root-owned files fail closed"

  jq -e '
    [.projects[] | select(.slug == "flow" or .slug == "watch" or .slug == "sandbox")] as $projects
    | ($projects | length) == 3
      and all($projects[];
        (.automerge | type) == "object"
        and (.automerge.protectedPaths | type) == "array"
        and (.automerge.protectedPaths | length) > 0
        and all(.automerge.protectedPaths[]; type == "string" and length > 0)
        and (.automerge.maxChangedFiles | type) == "number"
        and .automerge.maxChangedFiles > 0
        and (.automerge.maxDiffLines | type) == "number"
        and .automerge.maxDiffLines > 0
        and .automerge.mergeMethod == "squash")
  ' "${CONFIG}" >/dev/null 2>&1 ||
    fail "flow, watch, and sandbox must each have a valid bounded squash policy"

  jq -e '
    (.projects[] | select(.slug == "flow") | .automerge
      | .maxChangedFiles == 8 and .maxDiffLines == 400)
    and
    (.projects[] | select(.slug == "watch") | .automerge
      | .maxChangedFiles == 10 and .maxDiffLines == 500)
    and
    (.projects[] | select(.slug == "sandbox") | .automerge
      | .maxChangedFiles == 8 and .maxDiffLines == 400)
  ' "${CONFIG}" >/dev/null 2>&1 ||
    fail "project policy caps must match the reviewed conservative limits"
fi

assert_protected_path() {
  local slug="$1" path="$2"
  jq -e --arg slug "${slug}" --arg path "${path}" '
    .projects[]
    | select(.slug == $slug)
    | .automerge.protectedPaths
    | index($path) != null
  ' "${CONFIG}" >/dev/null 2>&1 || fail "${slug} policy must protect ${path}"
}

if [[ -f "${CONFIG}" ]] && jq empty "${CONFIG}" >/dev/null 2>&1; then
  for path in \
    flow/agents/ \
    flow/skills/ \
    flow/hooks/ \
    flow/codex/ \
    flow/opencode/ \
    flow/scripts/ \
    flow/templates/ \
    flow/.claude-plugin/ \
    flow/.codex-plugin/ \
    flow/AGENTS.md \
    flow/CLAUDE.md \
    '*credential*' \
    '*secret*'
  do
    assert_protected_path flow "${path}"
  done

  for path in \
    watch/internal/ \
    watch/pkg/ \
    watch/plugin/ \
    watch/plugin/.claude-plugin/ \
    watch/plugin/.codex-plugin/ \
    watch/main.go \
    'watch/*_cmd.go' \
    watch/cli_helpers.go \
    watch/go.mod \
    watch/go.sum \
    watch/Makefile \
    watch/.goreleaser.yml \
    watch/AGENTS.md \
    watch/CLAUDE.md \
    '*credential*' \
    '*secret*'
  do
    assert_protected_path watch "${path}"
  done

  for path in \
    'sandbox/Dockerfile*' \
    sandbox/entrypoint.sh \
    sandbox/fragments/ \
    sandbox/lib/ \
    sandbox/skills/ \
    sandbox/.claude-plugin/ \
    sandbox/.codex-plugin/ \
    sandbox/AGENTS.md \
    sandbox/CLAUDE.md \
    '*credential*' \
    '*secret*'
  do
    assert_protected_path sandbox "${path}"
  done

fi

grep -qF -- "- '.cenci/config.json' # Run the repository activation contract on config-only changes." "${FLOW_WORKFLOW}" 2>/dev/null ||
  fail "flow CI must run the activation contract for config-only changes"
grep -qF -- "- '.cenci/config.json' # Run the real policy-resolver dogfood test on config-only changes." "${WATCH_WORKFLOW}" 2>/dev/null ||
  fail "watch CI must run the real policy resolver for config-only changes"

echo "repo-activation-config.test.sh: failures=${failures}"
[[ "${failures}" -eq 0 ]]
