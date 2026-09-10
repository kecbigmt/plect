Review the PR {{ resource.id }}.

**Completion condition — read this before anything else:** this review's
completion condition is recording a `plect judge` verdict for every judge id
the work session's `done_when` declares — `ac-met` and `solves` — at the
PR's current head revision. That record is the goal; anything you post to
GitHub afterward is supporting evidence, never a substitute for it.

**Decision rule (check first):** does this PR have a dispatched work session
behind it, or was it pushed directly with no work session (self-review)?
- Default to recording via `plect judge` (step 6 below). Try it first.
- Only fall back to a marker comment (also step 6) once `plect judge` has
  actually rejected your verdict as self-review — never choose the fallback
  speculatively, and never skip attempting `plect judge` because you assume
  it will reject.

Steps:

1. Understand the purpose and background of the PR — derive `<owner>/<repo>` and
   `<n>` from {{ resource.id }}'s URL and read
   `gh api repos/<owner>/<repo>/pulls/<n>` (REST; reserve `gh pr`/`gh issue`
   porcelain for writes, since porcelain reads consume GraphQL quota that
   write-side `gh` calls also share)
2. Review the diff (`gh api repos/<owner>/<repo>/pulls/<n> -H "Accept: application/vnd.github.diff"`)
3. Evaluate code quality, design, and security
4. Check CI results for this head (`gh api repos/<owner>/<repo>/commits/<head_sha>/check-runs`
   — REST, not the `gh pr checks` porcelain step 1 reserves for writes) instead of re-running
   tests/linters CI already covers — running them again duplicates CI and adds real memory/CPU
   cost to this session for no new signal. Run a test or linter yourself only to reproduce a
   failure CI reports, or to cover something CI
   does not run at all.
5. Identify risks, concerns, and open questions
6. Record one action per judge id (`ac-met`, `solves`) with a reason:
   `plect judge approve <work-session> <task-instance> <judge-id> --reason "<reason>"`
   Use `plect judge request-changes <work-session> <task-instance> <judge-id> --reason "<reason>"`
   for any unmet criterion and name the missing work in the reason.
   If `plect judge` rejects your verdict as self-review, the PR has no
   dispatched work session to record against (it was implemented directly) —
   only then record the verdict as a review comment with an explicit marker
   instead:
   `gh pr review {{ resource.id }} --comment --body "APPROVE: <reason>"` (or
   `"REQUEST_CHANGES: <reason>"`). A formal `--approve`/`--request-changes`
   is rejected by GitHub when your session shares the PR author's account.
   Sharing an account is a fact about GitHub transport identity, not about
   who the actor is — it does not make this review non-independent.
7. Post review comments for findings from steps 3-5 that don't rise to a
   judge verdict, or that back one (AI disclosure rule below is mandatory)
8. If review comments already exist, check their resolution status
9. Verify: read back this PR's judge state with
   `plect status <work-session> --json` and confirm both `ac-met` and
   `solves` show a recorded action at the head revision. If `plect judge`
   errored for any reason other than the expected self-review rejection,
   do not silently retry or fall back — report the exact error in your reply.
10. Record that you finished reviewing (this is what lets plect close out this
    review session — there is no third party to judge a reviewer, so
    completion is your own self-report): find this review's own current
    `revision` with `plect status "$PLECT_SESSION_NAME" --json` (the `revision`
    under `observed.state` on this session's own task instance, not the work
    session's — `$PLECT_SESSION_NAME` is this session), then run
    `plect state set "$PLECT_SESSION_NAME" --instance review#1 '{"verdict_revision":"<that revision>"}'`
    (adjust the instance id if `plect status "$PLECT_SESSION_NAME" --json` shows a different one).
    Do this whichever path you took above — judge action, marker comment, or
    pending review. If the reviewed resource gets a new revision later (another
    push), this review's done_when reopens automatically and you'll be asked
    to re-review; record a fresh `verdict_revision` again when you do.

**A standing-rule violation is never non-blocking.** A violation of the
target repository's own conventions (its contributing guide, house style, or
a practice a prior review already corrected) is not a `request-changes`
you can skip just because it feels minor. Either request the fix, or confirm
a fix commit before approving. Granting an exception to a standing rule is
the repository owner's call, not the reviewer's — if you believe an
exception is warranted, still verdict `request-changes` and say so in the
reason, leaving the decision to them. (A stylistic preference with no
grounding in the repository's own rules remains an ordinary suggestion, as
before.)

**Scope boundary:** this review's job ends at recording a verdict. Do not
merge or close the PR/issue, or edit labels, as part of this review — both
judges approving and CI looking green are not by themselves grounds to
merge. A human holding a merge for reasons outside judge state can exist and
not be visible from this session; merging is the orchestrator's call, not
the reviewer's.

**AI disclosure (mandatory):** every comment or review you post to GitHub must
open by identifying you as an AI agent. Start each body with:
`> 🤖 This review was written by an AI agent (Claude Code) under human supervision.`

To follow this PR's progress while you work, run `plect subscribe {{ resource.id }}` — its CI / review / merge events then arrive in this session (`plect event list`).

**Unattended session:** this session cannot ask the user interactive
questions. When a decision is needed, write it as a question in an issue or
PR comment and leave it as an unmet `done_when` criterion so it surfaces as
an escalation — don't proceed on an assumed answer.

Additional instructions from the dispatcher (may be empty): {{ inputs.instruction }}
