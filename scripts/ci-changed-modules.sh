#!/usr/bin/env bash
# Core never imports plugins and plugins never import core
# (check-provider-boundary.sh enforces this), and both sides depend only on
# contracts/*, so contracts/** changes are treated as "run everything"
# rather than scoped to individual contracts/* submodules — the map does
# not attempt a finer edge than the enforced boundary actually gives it.
#
# A plugin's shipped config/testdata is read directly off disk by app-side
# golden tests (app/internal/config, app/internal/task) that walk
# plugins/catalog.toml and each plugin's manifest — a reverse edge from
# plugin config back to the app build-test entry that a plain Go import
# graph would not surface.
#
# The docs/language/**/*.md and testdata/config-language/** paths are the
# same kind of reverse edge, not prose: app/internal/lang's conformance
# tests (docs_conformance_test.go, diag_test.go,
# native_conformance_test.go) read these files directly off disk as the
# executable specification.
#
# web/** (the TypeSpec API contract and the React Web UI shell) and the Go
# packages that consume their generated output — app/internal/webapi/** and
# app/internal/webui/** — are production frontend/API paths: they gate the
# web-api-contract and web-app-build workflow jobs (WEB_CI below), which run
# pnpm and so stay out of the app/contracts/plugins build-test matrix.
# docs/design/web-ui/prototype/** is a frozen visual reference, not
# production source; it falls under the docs-only prose rule below and so
# never sets WEB_CI, precisely so touching it alone cannot stand in for
# validating the production paths.
#
# The "docs-only, no job needed" list below is a whitelist of locations
# individually confirmed (by grep, not by extension) to hold prose no Go
# code reads — docs/** other than docs/language/, the root CLAUDE.md and
# AGENTS.md (read only by check-agent-config.sh, already unconditional),
# and each plugin's own README.md. It is deliberately not "any *.md
# anywhere": a bare extension match would silently wave through a future
# fixture suite that happens to use .md files somewhere new (e.g. a
# testdata/new-suite/case.md), the exact false-green this reverse edge was
# added to close. Anything not on this list, or one of the reverse edges
# above, falls back to a full run: skipping a job is only safe when its
# rationale traces to an enforced boundary or an explicit reverse edge, not
# by default.
set -euo pipefail

# The plugins/legacy-migration module has no src/ subdirectory of its
# own — its module root doubles as what every other plugin calls src/.
BUILD_TEST_ALL='["app","contracts/atomicfile","contracts/channel-protocol","contracts/event","contracts/state","plugins/claude/src/channel-server","plugins/github/src","plugins/legacy-migration","plugins/okf/src","plugins/slack/src/slack-adapter"]'
ALL_PLUGIN_DIRS='["claude","codex","github","legacy-migration","okf","slack","tmux"]'

if [ "${FORCE_FULL_RUN:-false}" = true ]; then
  echo "FULL_RUN=true"
  echo "BUILD_TEST_MATRIX=$BUILD_TEST_ALL"
  echo "INTEGRATION_TEST=true"
  echo "README_VERIFY=true"
  echo "AFFECTED_PLUGINS=$ALL_PLUGIN_DIRS"
  echo "WEB_CI=true"
  exit 0
fi

files="$(cat)"

# A plain `echo "$files" | grep ...` pipe is unsafe here: under pipefail, a
# `grep -q` that exits after its first match can SIGPIPE a still-writing
# `echo`, and pipefail then reports that SIGPIPE, not the match, turning a
# real hit into a false miss. A here-string has no concurrent writer to
# race, and it also sidesteps `echo` treating a literal filename like "-n"
# as its own flag instead of data.
any_match() {
  [ -n "$files" ] && grep -Eq "$1" <<< "$files"
}

CONTRACTS_RE='^contracts/'
APP_RE='^app/'
CLAUDE_SRC_RE='^plugins/claude/src/'
GITHUB_SRC_RE='^plugins/github/src/'
LEGACY_MIGRATION_RE='^plugins/legacy-migration/'
OKF_SRC_RE='^plugins/okf/src/'
SLACK_SRC_RE='^plugins/slack/src/slack-adapter/'
CLAUDE_CFG_RE='^plugins/claude/(config|testdata)/'
GITHUB_CFG_RE='^plugins/github/(config|testdata)/'
OKF_CFG_RE='^plugins/okf/(config|testdata)/'
SLACK_CFG_RE='^plugins/slack/(config|testdata)/'
CODEX_CFG_RE='^plugins/codex/(config|testdata)/'
TMUX_CFG_RE='^plugins/tmux/(config|testdata)/'
LANG_DOCS_RE='^docs/language/'
LANG_TESTDATA_RE='^testdata/config-language/'
README_RE='^README\.md$'
FULL_RUN_TRIGGER_RE='^\.github/workflows/|^scripts/[^/]+\.sh$'
DOCS_PROSE_RE='^docs/'
ROOT_PROSE_RE='^(CLAUDE|AGENTS)\.md$'
PLUGIN_README_RE='^plugins/[^/]+/README\.md$'
WEB_RE='^web/'
WEBAPI_GO_RE='^app/internal/webapi/'
WEBUI_GO_RE='^app/internal/webui/'

KNOWN_RE="$CONTRACTS_RE|$APP_RE|$CLAUDE_SRC_RE|$GITHUB_SRC_RE|$LEGACY_MIGRATION_RE|$OKF_SRC_RE|$SLACK_SRC_RE|$CLAUDE_CFG_RE|$GITHUB_CFG_RE|$OKF_CFG_RE|$SLACK_CFG_RE|$CODEX_CFG_RE|$TMUX_CFG_RE|$LANG_DOCS_RE|$LANG_TESTDATA_RE|$FULL_RUN_TRIGGER_RE|$README_RE|$DOCS_PROSE_RE|$ROOT_PROSE_RE|$PLUGIN_README_RE|$WEB_RE"

unknown=false
if [ -n "$files" ] && grep -Eqv "$KNOWN_RE" <<< "$files"; then
  unknown=true
fi

full_run=false
if any_match "$FULL_RUN_TRIGGER_RE" || any_match "$CONTRACTS_RE" || [ "$unknown" = true ]; then
  full_run=true
fi

if [ "$full_run" = true ]; then
  matrix="$BUILD_TEST_ALL"
  affected_plugins="$ALL_PLUGIN_DIRS"
else
  mods=()
  any_match "$APP_RE" && mods+=("app")
  any_match "$CLAUDE_CFG_RE" && mods+=("app")
  any_match "$GITHUB_CFG_RE" && mods+=("app")
  any_match "$OKF_CFG_RE" && mods+=("app")
  any_match "$SLACK_CFG_RE" && mods+=("app")
  any_match "$CODEX_CFG_RE" && mods+=("app")
  any_match "$TMUX_CFG_RE" && mods+=("app")
  any_match "$LANG_DOCS_RE" && mods+=("app")
  any_match "$LANG_TESTDATA_RE" && mods+=("app")
  any_match "$CLAUDE_SRC_RE" && mods+=("plugins/claude/src/channel-server")
  any_match "$GITHUB_SRC_RE" && mods+=("plugins/github/src")
  any_match "$LEGACY_MIGRATION_RE" && mods+=("plugins/legacy-migration")
  any_match "$OKF_SRC_RE" && mods+=("plugins/okf/src")
  any_match "$SLACK_SRC_RE" && mods+=("plugins/slack/src/slack-adapter")
  if [ "${#mods[@]}" -eq 0 ]; then
    matrix="[]"
  else
    matrix="$(printf '%s\n' "${mods[@]}" | sort -u | jq -R . | jq -s -c .)"
  fi

  # One plugin's change must not run another plugin's selftests: only the
  # plugins whose own src/config/testdata changed go in this list.
  plugins=()
  { any_match "$CLAUDE_SRC_RE" || any_match "$CLAUDE_CFG_RE"; } && plugins+=("claude")
  { any_match "$GITHUB_SRC_RE" || any_match "$GITHUB_CFG_RE"; } && plugins+=("github")
  { any_match "$OKF_SRC_RE" || any_match "$OKF_CFG_RE"; } && plugins+=("okf")
  { any_match "$SLACK_SRC_RE" || any_match "$SLACK_CFG_RE"; } && plugins+=("slack")
  any_match "$CODEX_CFG_RE" && plugins+=("codex")
  any_match "$TMUX_CFG_RE" && plugins+=("tmux")
  any_match "$LEGACY_MIGRATION_RE" && plugins+=("legacy-migration")
  if [ "${#plugins[@]}" -eq 0 ]; then
    affected_plugins="[]"
  else
    affected_plugins="$(printf '%s\n' "${plugins[@]}" | sort -u | jq -R . | jq -s -c .)"
  fi
fi

integration_test=false
if [ "$full_run" = true ] || any_match "$APP_RE" || any_match "$CLAUDE_SRC_RE" \
  || any_match "$GITHUB_SRC_RE" || any_match "$LEGACY_MIGRATION_RE" \
  || any_match "$OKF_SRC_RE" || any_match "$SLACK_SRC_RE"; then
  integration_test=true
fi

readme_verify=false
if [ "$full_run" = true ] || any_match "$README_RE"; then
  readme_verify=true
fi

web_ci=false
if [ "$full_run" = true ] || any_match "$WEB_RE" || any_match "$WEBAPI_GO_RE" || any_match "$WEBUI_GO_RE"; then
  web_ci=true
fi

echo "FULL_RUN=$full_run"
echo "BUILD_TEST_MATRIX=$matrix"
echo "INTEGRATION_TEST=$integration_test"
echo "README_VERIFY=$readme_verify"
echo "AFFECTED_PLUGINS=$affected_plugins"
echo "WEB_CI=$web_ci"
