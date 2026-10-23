#!/usr/bin/env bash
# scripts/dependabot-merge-train.sh
#
# Drives a one-merge-per-cycle train over open Dependabot PRs in $REPO.
#
# Per PR (oldest first):
#   * skip when PR is a draft or already labeled $CI_FAILED_LABEL or $UPGRADE_REVIEW_LABEL
#   * fetch check-runs for the PR's HEAD SHA (per_page=100, paginated)
#   * classify via scripts/lib/merge-train-lib.sh::classify_check_runs:
#       none / running       -> skip (no signal yet / try again next cycle)
#       failed               -> cooldown/strikes gate -> comment THEN label
#       green                -> branch on merge-conflict / review / merge
#
# GREEN path:
#   * CONFLICTING or DIRTY:
#       search PR comments for rebase_marker <HEAD_OID>
#         not found -> post "@dependabot rebase (merge-train head: <OID>)"
#                       plus one short sentence of context
#         found     -> skip silently (a request for THIS head is already
#                      pending; Dependabot is on it)
#   * CHANGES_REQUESTED -> skip (human precedence)
#   * else              -> approve + SHA-pinned squash merge via the
#                         Pulls REST PUT endpoint (gh api -X PUT
#                         repos/.../pulls/<N>/merge) -- the SHA parameter
#                         makes the merge fail-closed on a stale head
#                         rather than racing the train onto a moved base.
#                         Any failure (409 head-moved, 405, 5xx, network)
#                         is logged as a warning block and the loop ends
#                         for this cycle; the run stays GREEN (exit 0) and
#                         the next cycle retries.
#
# FAILED path (after gate):
#   * post diagnostic comment with failing-check list (one per check,
#     html_url), THEN
#   * add $CI_FAILED_LABEL   (label application comes AFTER the comment
#     so removing the label does not erase the human-visible failure
#     evidence)
#
# MAJOR ACTION BUMP path (in the GREEN path, before approve/merge):
#   * a Dependabot github-actions PR whose title crosses a major version
#     boundary is NEVER auto-merged. The train posts a review comment,
#     adds $UPGRADE_REVIEW_LABEL, records the row and moves on; a labelled
#     PR is skipped early on every later cycle. The train holds the PR
#     indefinitely; a major bump merges only if a human does it manually.
#
# The pure decision helpers live in scripts/lib/merge-train-lib.sh so
# each rule can be exercised through a fake-`gh` harness in Go.
#
# Required env:
#   REPO (e.g. owner/name); GH_TOKEN (so gh can act as the workflow).
# Optional env:
#   MAX_MERGES_PER_RUN (default 1), FAIL_COOLDOWN_MINUTES (default 120),
#   MIN_FAILED_RUNS (default 2), CI_FAILED_LABEL (default ci-failed),
#   UPGRADE_REVIEW_LABEL (default needs-upgrade-review),
#   DRY_RUN (true|1) -- when set, every mutating gh call is replaced with
#     a [DRY_RUN] would: log line and returns 0; reads still run.
set -euo pipefail

LIB_PATH="$(dirname "$0")/lib/merge-train-lib.sh"
# shellcheck source=scripts/lib/merge-train-lib.sh
source "$LIB_PATH"

MAX_MERGES_PER_RUN="${MAX_MERGES_PER_RUN:-1}"
FAIL_COOLDOWN_MINUTES="${FAIL_COOLDOWN_MINUTES:-120}"
MIN_FAILED_RUNS="${MIN_FAILED_RUNS:-2}"
CI_FAILED_LABEL="${CI_FAILED_LABEL:-ci-failed}"
UPGRADE_REVIEW_LABEL="${UPGRADE_REVIEW_LABEL:-needs-upgrade-review}"
REPO="${REPO:?REPO must be set, e.g. owner/name}"

DRY_RUN="${DRY_RUN:-}"
if [[ "$DRY_RUN" == "true" || "$DRY_RUN" == "1" ]]; then
  DRY_RUN_ENABLED=1
else
  DRY_RUN_ENABLED=0
fi

SUMMARY_FILE="${GITHUB_STEP_SUMMARY:-/dev/null}"
NOW_EPOCH="$(date +%s)"

# mutating <desc> <command...>
#   Runs <command...> unless DRY_RUN is set, in which case prints
#   "[DRY_RUN] would: <desc>" on stderr and returns 0. In the real run
#   path the function propagates the inner command's exit code, so
#   callers can use `if mutating ...; then ... else ... fi` to gate the
#   train on failure exactly like a direct gh call would.
mutating() {
  local desc="$1"; shift
  if (( DRY_RUN_ENABLED )); then
    printf '[DRY_RUN] would: %s\n' "$desc" >&2
    return 0
  fi
  "$@"
}

# Count failing GitHub Actions runs for the HEAD branch. Returns 0 if no
# run history exists (or the query itself fails). Used only by the FAILED
# path's strikes gate.
failed_runs_on_branch() {
  local branch="$1" raw total=0
  if raw="$(gh run list --repo "$REPO" --branch "$branch" \
              --json conclusion --limit 20 2>/dev/null)"; then
    if [[ -n "$raw" ]]; then
      total="$(printf '%s' "$raw" | \
        jq '[.[] | select(.conclusion == "failure")] | length' 2>/dev/null || echo 0)"
    fi
  fi
  echo "${total:-0}"
}

# Is the lib's rebase marker for <sha> already present in the PR's
# issue comments? Empty result means "not present". A transport failure
# is treated as "not present" so we still post the nudge when data is
# missing -- silent loss of rebase prompts breaks the dedup contract.
rebase_marker_present() {
  local number="$1" sha="$2"
  local marker
  marker="$(rebase_marker "$sha")"
  local blob=""
  blob="$(gh pr view "$number" --repo "$REPO" --json comments \
            --jq '[.comments[].body] | join("\n")' 2>/dev/null || true)"
  [[ "$blob" == *"$marker"* ]]
}

# Safe inclusion in a Markdown table cell.
md_escape() {
  printf '%s' "$1" | tr '|' '`' | tr -d '\r'
}

# Append one PR row to $SUMMARY_FILE (markdown table).
record_summary() {
  local number="$1" title="$2" decision="$3" detail="$4"
  printf '| #%-6s | %s | %s | %s |\n' \
    "$number" "$(md_escape "$title")" "$decision" "$(md_escape "$detail")" \
    >> "$SUMMARY_FILE"
}

# ----- Ensure the train's labels exist (idempotent) -----------------------
echo "::group::Ensure train labels exist"
# Idempotent create; an existing label is not an error. mutating() emits
# "[DRY_RUN] would:" instead of running the gh call when DRY_RUN is set.
mutating "create label $CI_FAILED_LABEL" gh label create "$CI_FAILED_LABEL" \
  --repo "$REPO" \
  --description "Auto-flagged: CI did not go green; excluded from the merge train" \
  --color D93F0B || true
mutating "create label $UPGRADE_REVIEW_LABEL" gh label create "$UPGRADE_REVIEW_LABEL" \
  --repo "$REPO" \
  --description "Auto-flagged: major GitHub Actions bump; excluded from the merge train pending human review" \
  --color FBCA04 || true
echo "::endgroup::"

# ----- Step summary scaffold ----------------------------------------------
{
  echo "## Dependabot Merge Train"
  echo
  echo "| PR      | Title | Decision | Detail |"
  echo "| ------- | ----- | -------- | ------ |"
} > "$SUMMARY_FILE"

# ----- List open dependabot PRs (oldest first) ----------------------------
echo "::group::Fetch open Dependabot PRs"
PR_JSON="$(gh pr list --repo "$REPO" --author "app/dependabot" \
             --state open --json number,title,headRefName,headRefOid,createdAt,labels,isDraft \
             --jq 'sort_by(.createdAt)')"
PR_COUNT=$(printf '%s' "$PR_JSON" | jq 'length')
echo "Found $PR_COUNT open Dependabot PR(s)."
echo "::endgroup::"

MERGES_DONE=0

for ((i = 0; i < PR_COUNT; i++)); do
  # Cap is checked at the top of every iteration BEFORE doing any work,
  # so any PR that would attempt a merge is rejected once MAX is hit.
  if (( MERGES_DONE >= MAX_MERGES_PER_RUN )); then
    echo "Reached MAX_MERGES_PER_RUN=$MAX_MERGES_PER_RUN; ending the train."
    break
  fi

  NUMBER=$(printf '%s' "$PR_JSON" | jq -r ".[$i].number")
  TITLE=$(printf '%s' "$PR_JSON" | jq -r ".[$i].title")
  HEAD_REF=$(printf '%s' "$PR_JSON" | jq -r ".[$i].headRefName")
  HEAD_OID=$(printf '%s' "$PR_JSON" | jq -r ".[$i].headRefOid")
  CREATED_AT=$(printf '%s' "$PR_JSON" | jq -r ".[$i].createdAt")
  IS_DRAFT=$(printf '%s' "$PR_JSON" | jq -r ".[$i].isDraft")
  LABELED_FAILED=$(printf '%s' "$PR_JSON" | \
    jq --arg lbl "$CI_FAILED_LABEL" -r \
      ".[$i].labels | map(.name) | (index(\$lbl) // null) | tostring")
  LABELED_UPGRADE=$(printf '%s' "$PR_JSON" | \
    jq --arg lbl "$UPGRADE_REVIEW_LABEL" -r \
      ".[$i].labels | map(.name) | (index(\$lbl) // null) | tostring")

  echo
  echo "::group::PR #$NUMBER: $TITLE"

  if [[ "$LABELED_FAILED" != "null" ]]; then
    echo "skip: labeled $CI_FAILED_LABEL; excluded from train"
    record_summary "$NUMBER" "$TITLE" "skipped-ci-failed" "already labeled"
    echo "::endgroup::"
    continue
  fi

  if [[ "$LABELED_UPGRADE" != "null" ]]; then
    echo "skip: labeled $UPGRADE_REVIEW_LABEL; awaiting human upgrade review"
    record_summary "$NUMBER" "$TITLE" "skipped-upgrade-review" "already labeled"
    echo "::endgroup::"
    continue
  fi

  if [[ "$IS_DRAFT" == "true" ]]; then
    echo "skip: PR is a draft"
    record_summary "$NUMBER" "$TITLE" "skipped-draft" "draft PR"
    echo "::endgroup::"
    continue
  fi

  # ----- Inspect check-runs against HEAD_OID (per_page=100, paginated)
  echo "Checking CI for $HEAD_OID..."
  CHECKS_JSON="$(gh api "repos/$REPO/commits/$HEAD_OID/check-runs?per_page=100" \
                   --paginate --jq '.check_runs // []')"
  CLASSIFICATION="$(classify_check_runs "$CHECKS_JSON")"

  case "$CLASSIFICATION" in
    none)
      echo "skip: zero check-runs for HEAD; no CI signal yet"
      record_summary "$NUMBER" "$TITLE" "skipped-no-ci" "no check-runs on $HEAD_OID"
      echo "::endgroup::"
      continue
      ;;
    running)
      echo "skip: a check is still running"
      record_summary "$NUMBER" "$TITLE" "skipped-running" "CI still running on $HEAD_OID"
      echo "::endgroup::"
      continue
      ;;
    failed)
      AGE_MIN="$(pr_age_minutes "$CREATED_AT" "$NOW_EPOCH")"
      FAIL_COUNT="$(failed_runs_on_branch "$HEAD_REF")"
      if [[ "$(cooldown_allows "$AGE_MIN" "$FAIL_COUNT" "$NOW_EPOCH")" == "deny" ]]; then
        DOUBLE_COOLDOWN=$(( FAIL_COOLDOWN_MINUTES * 2 ))
        if (( FAIL_COUNT == 0 )); then
          echo "skip: failed PR with no Actions run history (need ${DOUBLE_COOLDOWN}m, age ${AGE_MIN}m)"
          record_summary "$NUMBER" "$TITLE" "skipped-no-history" \
            "${AGE_MIN}m elapsed on $HEAD_REF, no run history"
        elif (( AGE_MIN < FAIL_COOLDOWN_MINUTES )); then
          echo "skip: failed PR within cooldown (${AGE_MIN}m < ${FAIL_COOLDOWN_MINUTES}m)"
          record_summary "$NUMBER" "$TITLE" "skipped-cooldown" \
            "${AGE_MIN}m elapsed (cooldown ${FAIL_COOLDOWN_MINUTES}m)"
        else
          echo "skip: only $FAIL_COUNT failed run(s) on $HEAD_REF (need $MIN_FAILED_RUNS; age ${AGE_MIN}m < ${DOUBLE_COOLDOWN}m)"
          record_summary "$NUMBER" "$TITLE" "skipped-strikes" \
            "${FAIL_COUNT} failed runs on $HEAD_REF"
        fi
        echo "::endgroup::"
        continue
      fi

      # ----- FAILED path: comment THEN apply label ------------------------
      FAILING=$(printf '%s' "$CHECKS_JSON" | jq -r '
        [ .[]
          | select(.conclusion == "failure"
                   or .conclusion == "timed_out"
                   or .conclusion == "cancelled"
                   or .conclusion == "action_required"
                   or .conclusion == "startup_failure"
                   or .conclusion == "stale") ]
        | sort_by(.name)
        | .[] | "- " + .name + " \u2014 " + .html_url')

      COMMENT_BODY="$(cat <<EOF
This PR has failing checks and has been excluded from the merge train.

Failing checks:
$FAILING

This PR will remain OPEN. Remove the \`$CI_FAILED_LABEL\` label to put it back in line.
EOF
)"

      echo "Posting diagnostic comment and applying $CI_FAILED_LABEL..."
      # comment BEFORE label (unchanged ordering) so removing the label
      # does not erase the human-visible evidence of the failure.
      # IMPORTANT: --body must receive the WHOLE heredoc-built string as
      # ONE single quoted argument. The body contains spaces, newlines
      # and backticks; never inline-unquote "$COMMENT_BODY" or pass an
      # unquoted expansion -- that would word-split it into multiple
      # argv tokens and "gh pr comment" would receive a one-word body.
      mutating "post ci-failed comment on #${NUMBER}" gh pr comment "$NUMBER" \
        --repo "$REPO" --body "$COMMENT_BODY"
      mutating "add label $CI_FAILED_LABEL to #${NUMBER}" gh pr edit "$NUMBER" \
        --repo "$REPO" --add-label "$CI_FAILED_LABEL"
      record_summary "$NUMBER" "$TITLE" "labeled-ci-failed" \
        "comment posted, label applied"
      echo "::endgroup::"
      continue
      ;;
    green)
      # ----- GREEN path: conflicts, review, then approve + merge ----------
      echo "All CI checks passed; evaluating merge-readiness..."
      PR_STATE_JSON="$(gh pr view "$NUMBER" --repo "$REPO" \
                         --json mergeable,mergeStateStatus,reviewDecision)"
      MERGEABLE="$(printf '%s' "$PR_STATE_JSON" | jq -r '.mergeable // ""')"
      MERGE_STATE="$(printf '%s' "$PR_STATE_JSON" | jq -r '.mergeStateStatus // ""')"
      REVIEW="$(printf '%s' "$PR_STATE_JSON" | jq -r '.reviewDecision // ""')"

      if [[ "$MERGEABLE" == "CONFLICTING" || "$MERGE_STATE" == "DIRTY" ]]; then
        if rebase_marker_present "$NUMBER" "$HEAD_OID"; then
          echo "skip: rebase marker for $HEAD_OID already present; mergeable=$MERGEABLE state=$MERGE_STATE"
          record_summary "$NUMBER" "$TITLE" "skipped-conflict" \
            "rebase already requested for $HEAD_OID"
          echo "::endgroup::"
          continue
        fi
        MARKER="$(rebase_marker "$HEAD_OID")"
        # Build the rebase nudge body in a local var so the entire
        # multi-line string -- with spaces, parentheses, and the
        # embedded HEAD OID -- is forwarded as ONE single quoted
        # argument through mutating(). If "$NUDGE_BODY" were expanded
        # unquoted (or the body inlined), the spaces would word-split
        # the body into separate argv tokens and "gh pr comment" would
        # receive only the first word ("@dependabot"). Build locally,
        # pass double-quoted, every time.
        NUDGE_BODY="$(printf '@dependabot rebase (merge-train head: %s)\n\n%s' \
          "$HEAD_OID" \
          'This is the Dependabot merge train asking for a rebase because a merge into the base branch conflicts on this HEAD.')"
        mutating "post @dependabot rebase nudge #${NUMBER} (marker $MARKER)" \
          gh pr comment "$NUMBER" --repo "$REPO" --body "$NUDGE_BODY"
        record_summary "$NUMBER" "$TITLE" "rebased" \
          "marker $MARKER posted (mergeable=$MERGEABLE, state=$MERGE_STATE)"
        echo "::endgroup::"
        continue
      fi

      if [[ "$REVIEW" == "CHANGES_REQUESTED" ]]; then
        echo "skip: reviewDecision=CHANGES_REQUESTED (human precedence)"
        record_summary "$NUMBER" "$TITLE" "skipped-changes-requested" "human precedence"
        echo "::endgroup::"
        continue
      fi

      # ----- Major GitHub Actions bump: human review, never auto-merge -----
      # Actions do not guarantee SemVer, and a major bump can rename
      # inputs/outputs or change defaults. A green CI run may not even exercise
      # the workflow that uses the action (build/deploy workflows run on push,
      # not on PRs), so a major bump is withheld from the train and flagged for
      # a human -- the same shape as the failing-checks ($CI_FAILED_LABEL) path.
      if [[ "$(is_github_actions_pr "$HEAD_REF")" == "yes" \
            && "$(is_major_bump "$TITLE")" == "yes" ]]; then
        UPGRADE_BODY="$(printf '%s\n\n%s\n\n%s' \
          "This PR is a **major** version bump of a GitHub Action, so the Dependabot merge train will not auto-merge it." \
          "Actions do not guarantee SemVer, and a major bump can rename inputs/outputs or change defaults. A green CI run may not exercise the workflow that uses this action (build/deploy workflows run on push, not on PRs). Please review the action's release notes and merge this PR manually." \
          "Bump: \`$TITLE\`")"
        echo "Flagging #$NUMBER for human upgrade review..."
        mutating "post upgrade-review comment on #${NUMBER}" gh pr comment "$NUMBER" \
          --repo "$REPO" --body "$UPGRADE_BODY"
        mutating "add label $UPGRADE_REVIEW_LABEL to #${NUMBER}" gh pr edit "$NUMBER" \
          --repo "$REPO" --add-label "$UPGRADE_REVIEW_LABEL"
        record_summary "$NUMBER" "$TITLE" "upgrade-review" \
          "major action bump; human review required"
        echo "::endgroup::"
        continue
      fi

      # ----- Approve -----------------------------------------------------
      # --body here is a literal quoted string (no variables, no
      # expansions); the closing " ... --body \"...\"" pattern is
      # identical to the two other --body sites above so a future
      # reader sees the same single-quoted-arg invariant everywhere.
      echo "Approving #$NUMBER..."
      if ! mutating "approve #${NUMBER}" gh pr review "$NUMBER" --repo "$REPO" \
            --approve --body "All CI checks passed; auto-merge via merge train."; then
        echo "::warning::approve failed for #$NUMBER; ending this cycle, next cycle will retry"
        echo "WARNING: decision=approve pr=$NUMBER" >&2
        echo "WARNING: action=next-cycle-will-retry" >&2
        record_summary "$NUMBER" "$TITLE" "approve-failed" "gh pr review failed"
        echo "::endgroup::"
        break
      fi

      # Cap is checked again before the merge PUT -- a parallel evaluation
      # could (in theory, on a future change) have raised MERGES_DONE.
      if (( MERGES_DONE >= MAX_MERGES_PER_RUN )); then
        echo "Reached MAX_MERGES_PER_RUN=$MAX_MERGES_PER_RUN between approve and merge; ending the train."
        echo "::endgroup::"
        break
      fi

      # ----- SHA-pinned merge via REST PUT --------------------------------
      # The PUT must capture BOTH stdout and stderr so we can echo the
      # API failure message verbatim in the structured WARNING block; we
      # therefore inline the call here instead of going through mutating()
      # (whose "$@" expansion quotes "2>&1" as a literal arg, breaking
      # the redirect). DRY_RUN still skips the actual invocation.
      COMMIT_TITLE="$(printf '%s (#%s)' "$TITLE" "$NUMBER")"
      COMMIT_MESSAGE="$(printf 'Auto-merged by Dependabot merge train (PR #%s).' "$NUMBER")"
      echo "Squash-merging #$NUMBER (sha=$HEAD_OID)..."
      MERGE_OUT=""
      if (( DRY_RUN_ENABLED )); then
        printf '[DRY_RUN] would: merge PUT #%s sha=%s\n' \
          "$NUMBER" "$HEAD_OID" >&2
      elif ! MERGE_OUT="$(gh api -X PUT "repos/$REPO/pulls/$NUMBER/merge" \
            -f merge_method=squash \
            -f sha="$HEAD_OID" \
            -f commit_title="$COMMIT_TITLE" \
            -f commit_message="$COMMIT_MESSAGE" 2>&1)"; then
        echo "::warning::merge failed for #$NUMBER (sha=$HEAD_OID); ending this cycle, next cycle will retry"
        echo "WARNING: decision=merge pr=$NUMBER sha=$HEAD_OID" >&2
        echo "WARNING: gh output: $MERGE_OUT" >&2
        echo "WARNING: action=next-cycle-will-retry-after-base-stabilises" >&2
        record_summary "$NUMBER" "$TITLE" "merge-failed" \
          "merge PUT failed (sha=$HEAD_OID, see run log)"
        echo "::endgroup::"
        break
      fi

      MERGES_DONE=$((MERGES_DONE + 1))
      record_summary "$NUMBER" "$TITLE" "merged" \
        "squash-merged by merge train (sha=$HEAD_OID)"
      echo "Merged #$NUMBER (count: $MERGES_DONE/$MAX_MERGES_PER_RUN)"
      echo "::endgroup::"
      ;;
  esac
done

echo
echo "Merge train finished. Merged ${MERGES_DONE}/${MAX_MERGES_PER_RUN} PR(s)."
exit 0
