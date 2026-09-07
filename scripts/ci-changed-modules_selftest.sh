#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
mapper="$root/scripts/ci-changed-modules.sh"

ALL_PLUGINS='["claude","codex","github","legacy-migration","okf","slack","tmux"]'
ALL_MODULES='["app","contracts/atomicfile","contracts/channel-protocol","contracts/event","contracts/state","plugins/claude/src/channel-server","plugins/github/src","plugins/legacy-migration","plugins/okf/src","plugins/slack/src/slack-adapter"]'

fail=0

check() {
  local name="$1" input="$2" expected="$3"
  local actual
  actual="$(printf '%s' "$input" | "$mapper")"
  if [ "$actual" != "$expected" ]; then
    echo "FAIL: $name" >&2
    echo "  input:" >&2
    printf '%s\n' "$input" | sed 's/^/    /' >&2
    echo "  expected:" >&2
    printf '%s\n' "$expected" | sed 's/^/    /' >&2
    echo "  actual:" >&2
    printf '%s\n' "$actual" | sed 's/^/    /' >&2
    fail=1
    return
  fi
  echo "ok: $name"
}

full_run_expected="FULL_RUN=true"$'\n'"BUILD_TEST_MATRIX=$ALL_MODULES"$'\n'"INTEGRATION_TEST=true"$'\n'"README_VERIFY=true"$'\n'"AFFECTED_PLUGINS=$ALL_PLUGINS"$'\n'"WEB_CI=true"

check "docs-only diff -> lint set only, nothing else" \
  $'docs/design/foo.md\nCLAUDE.md' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=[]\nINTEGRATION_TEST=false\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=false'

check "app/** -> app build-test + integration-test, not plugin build-tests or selftests" \
  'app/internal/task/executor.go' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=["app"]\nINTEGRATION_TEST=true\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=false'

check "plugins/X/src/** -> that plugin's build-test + integration-test + selftests, not app" \
  'plugins/github/src/internal/watcher/poll.go' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=["plugins/github/src"]\nINTEGRATION_TEST=true\nREADME_VERIFY=false\nAFFECTED_PLUGINS=["github"]\nWEB_CI=false'

check "legacy-migration has no src/ subdir of its own" \
  'plugins/legacy-migration/internal/migrate/migrate.go' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=["plugins/legacy-migration"]\nINTEGRATION_TEST=true\nREADME_VERIFY=false\nAFFECTED_PLUGINS=["legacy-migration"]\nWEB_CI=false'

check "plugins/X/config|testdata/** -> app build-test (reverse edge) + that plugin's selftests, not integration-test" \
  'plugins/okf/config/tasks/pursue_goal.toml' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=["app"]\nINTEGRATION_TEST=false\nREADME_VERIFY=false\nAFFECTED_PLUGINS=["okf"]\nWEB_CI=false'

check "a config-only plugin's config/testdata still reaches app build-test and its own selftests" \
  'plugins/codex/testdata/effects/enqueue.json' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=["app"]\nINTEGRATION_TEST=false\nREADME_VERIFY=false\nAFFECTED_PLUGINS=["codex"]\nWEB_CI=false'

check "two plugins' src changed together -> only those two build-test entries and selftests" \
  $'plugins/okf/src/internal/foo.go\nplugins/slack/src/slack-adapter/cmd/main.go' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=["plugins/okf/src","plugins/slack/src/slack-adapter"]\nINTEGRATION_TEST=true\nREADME_VERIFY=false\nAFFECTED_PLUGINS=["okf","slack"]\nWEB_CI=false'

check "a plugin's src and another plugin's config together -> each plugin only, not each other's" \
  $'plugins/claude/src/channel-server/main.go\nplugins/tmux/testdata/effects/pane.json' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=["app","plugins/claude/src/channel-server"]\nINTEGRATION_TEST=true\nREADME_VERIFY=false\nAFFECTED_PLUGINS=["claude","tmux"]\nWEB_CI=false'

check "contracts/** -> full run (no scoping finer than the enforced boundary)" \
  'contracts/state/store.go' \
  "$full_run_expected"

check ".github/workflows/** -> full run (the checks themselves changed)" \
  '.github/workflows/ci.yml' \
  "$full_run_expected"

check "scripts/*.sh -> full run" \
  'scripts/gh-guard.sh' \
  "$full_run_expected"

check "README.md -> readme-verify only, no build-test/integration-test/selftests" \
  'README.md' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=[]\nINTEGRATION_TEST=false\nREADME_VERIFY=true\nAFFECTED_PLUGINS=[]\nWEB_CI=false'

check "an unmapped path (e.g. go.work) is not silently skipped -> full run" \
  'go.work' \
  "$full_run_expected"

# Regression for a real false-green bug: testdata/config-language/**/*.md
# and docs/language/**/*.md are executable fixtures that app/internal/lang's
# conformance tests read directly off disk, not prose — the generic *.md
# docs-only pattern must not swallow them into "no job needed".
check "an app-conformance-test fixture under testdata/config-language/ reaches app build-test, not just lint" \
  'testdata/config-language/tasks/document.md' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=["app"]\nINTEGRATION_TEST=false\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=false'

check "a non-.md fixture under testdata/config-language/ also reaches app build-test, not a full run" \
  'testdata/config-language/foo/case.toml' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=["app"]\nINTEGRATION_TEST=false\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=false'

check "a docs/language/ chapter is a conformance-test fixture, not prose -> app build-test" \
  'docs/language/README.md' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=["app"]\nINTEGRATION_TEST=false\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=false'

check "a genuinely prose doc (docs/adr) stays lint-set-only" \
  'docs/adr/2026-01-01-example.md' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=[]\nINTEGRATION_TEST=false\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=false'

check "a plugin's own plugin.toml is unmapped -> full run, not silently skipped" \
  'plugins/github/plugin.toml' \
  "$full_run_expected"

# Regression for the fail-open the whitelist design closes: a bare *.md
# extension match would have waved through a brand new, never-audited
# fixture suite just because it happens to use .md files.
check "an unrecognized new .md path outside every audited prose location -> full run, not docs-only" \
  'testdata/new-suite/case.md' \
  "$full_run_expected"

check "root AGENTS.md is audited prose (read only by check-agent-config.sh, already unconditional)" \
  'AGENTS.md' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=[]\nINTEGRATION_TEST=false\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=false'

check "a plugin's own README.md is audited prose, not read by any test" \
  'plugins/github/README.md' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=[]\nINTEGRATION_TEST=false\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=false'

check "a loose docs/ file outside any subdirectory is still audited prose" \
  'docs/naming.md' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=[]\nINTEGRATION_TEST=false\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=false'

check "no changed files -> nothing runs beyond the unconditional lint set" \
  '' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=[]\nINTEGRATION_TEST=false\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=false'

check "a file literally named -n is data, not an echo flag, and is not silently skipped" \
  '-n' \
  "$full_run_expected"

# Regression for a real bug: under pipefail, `echo "$files" | grep -Eq` can
# have `echo` SIGPIPE'd by a `grep -q` that exits after its first match,
# turning a genuine hit into a reported miss. A large input makes the race
# reliably observable instead of incidental.
large_input="$(printf 'app/main.go\n'; seq 1 50000 | sed 's#^#docs/file#; s#$#.md#')"
check "a real match survives even with 50000 lines of unrelated input after it" \
  "$large_input" \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=["app"]\nINTEGRATION_TEST=true\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=false'

# web/** is the TypeSpec API contract and the React Web UI shell: neither
# has a Go module of its own, so it must not fall through to "unmapped ->
# full run" and must not force any Go build-test/integration-test entry.
check "web/api/** -> web_ci only, no Go module affected" \
  'web/api/routes/sessions.tsp' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=[]\nINTEGRATION_TEST=false\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=true'

check "web/app/** -> web_ci only, no Go module affected" \
  'web/app/src/components/session-tree.tsx' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=[]\nINTEGRATION_TEST=false\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=true'

check "a web/ workspace-root file (e.g. the shared lockfile) -> web_ci only" \
  'web/pnpm-lock.yaml' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=[]\nINTEGRATION_TEST=false\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=true'

# app/internal/webapi/** and app/internal/webui/** are already app/** for
# build-test/integration-test purposes; the reverse edge these two cases
# guard is that they additionally set WEB_CI, since they consume web/api's
# generated contract and web/app's Vite build respectively.
check "app/internal/webapi/** -> app build-test + integration-test + web_ci" \
  'app/internal/webapi/handler.go' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=["app"]\nINTEGRATION_TEST=true\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=true'

check "app/internal/webui/** -> app build-test + integration-test + web_ci" \
  'app/internal/webui/webapp/embed.go' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=["app"]\nINTEGRATION_TEST=true\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=true'

# The frozen visual reference is prose under docs/, not production source:
# touching it alone must leave WEB_CI false so it cannot stand in for
# validating web/** or its Go consumers.
check "docs/design/web-ui/prototype/**-only does not set web_ci (frozen reference, not production)" \
  'docs/design/web-ui/prototype/components/session-tree.tsx' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=[]\nINTEGRATION_TEST=false\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=false'

check "a production web/** change alongside a frozen-reference-only edit still sets web_ci" \
  $'web/app/src/components/session-tree.tsx\ndocs/design/web-ui/prototype/components/session-tree.tsx' \
  $'FULL_RUN=false\nBUILD_TEST_MATRIX=[]\nINTEGRATION_TEST=false\nREADME_VERIFY=false\nAFFECTED_PLUGINS=[]\nWEB_CI=true'

force_full_run_actual="$(FORCE_FULL_RUN=true "$mapper" < /dev/null)"
if [ "$force_full_run_actual" != "$full_run_expected" ]; then
  echo "FAIL: FORCE_FULL_RUN=true (used for push-to-main, which has no PR base to diff)" >&2
  echo "  expected:" >&2
  printf '%s\n' "$full_run_expected" | sed 's/^/    /' >&2
  echo "  actual:" >&2
  printf '%s\n' "$force_full_run_actual" | sed 's/^/    /' >&2
  fail=1
else
  echo "ok: FORCE_FULL_RUN=true short-circuits to a full run"
fi

if [ "$fail" -ne 0 ]; then
  echo
  echo "ci-changed-modules.sh does not reproduce the documented trigger map." >&2
  exit 1
fi

echo "all ci-changed-modules.sh trigger-map cases passed"
