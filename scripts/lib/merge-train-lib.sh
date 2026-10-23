#!/usr/bin/env bash
# scripts/lib/merge-train-lib.sh
#
# Pure decision helpers for the Dependabot merge train. Sourced by
# scripts/dependabot-merge-train.sh. Has no GitHub/network side effects --
# every input is supplied through arguments, and the output of each function
# is one of a small set of literal strings. Tests source this lib directly
# (see internal/mergetrain/merge_train_lib_test.go) so each rule is
# exercised in isolation, with a deterministic clock injected.
set -euo pipefail

# classify_check_runs <checkruns-json-array>
#   Emit exactly one of: green | failed | running | none.
#
# Rules, in priority order:
#   * empty array                                  -> none
#     (treat as "no signal" -- skip, next cycle will re-evaluate)
#   * any run with .status != "completed"          -> running
#     (skip -- wait for completion)
#   * every .conclusion in {success, neutral, skipped} -> green
#   * any other completed .conclusion (failure,
#     timed_out, cancelled, action_required,
#     startup_failure, stale, or anything unknown)   -> failed
#
# The allow-list comes from the PR-merge-train policy; explicit denials
# include anything that did not finish the CI pipeline successfully. A
# null or missing .conclusion on a completed run is also treated as failed
# (it should not happen, but defends against GitHub adding new states).
classify_check_runs() {
  local runs_json="$1"
  # Fail-safe guard: classify_check_runs must NEVER fail open to "green"
  # on malformed input. If the value does not parse as a JSON array (or
  # jq errored out under `set -euo pipefail`), treat the call as "no
  # signal" -- emit "none" and return. The downstream jq calls below
  # can then assume the input is a well-formed array. This guard makes
  # the function monotonically safe: an unexpected payload (e.g. an
  # error string returned by the upstream `gh api` invocation) will
  # always classify as "no signal", never as "green".
  # Require EXACTLY one JSON array. Slurping with -s turns N concatenated
  # values into an N-element array, so a multi-value payload (e.g. two
  # un-slurped `gh api --paginate` pages) fails this check and classifies as
  # "no signal" -- it can never fall through to "green".
  if ! jq -s -e 'length == 1 and (.[0] | type == "array")' <<<"$runs_json" >/dev/null 2>&1; then
    echo "none"
    return
  fi
  local length
  length="$(jq 'length' <<<"$runs_json")"
  if [[ "$length" -eq 0 ]]; then
    echo "none"
    return
  fi
  # Defence in depth: if jq errors here despite the array guard
  # (e.g. a future jq bug or an upstream payload shape change), coerce
  # non_completed to 0 so the function moves on to the bad-conclusion
  # check. 0 is the safe default for the `running` decision -- would
  # be set only if there is positive proof of a non-completed run.
  local non_completed=0
  non_completed="$(jq '[.[] | select(.status != "completed")] | length' <<<"$runs_json" 2>/dev/null)" \
    || non_completed=0
  if (( non_completed > 0 )); then
    echo "running"
    return
  fi
  # Defence in depth: if jq errors here, coerce bad to 1 so we FAIL
  # CLOSED to "failed" rather than failing open to "green". Only
  # positive proof that every completed run is allow-listed unlocks
  # "green". This is the most important guard in the function.
  local bad=0
  bad="$(jq '
      ["success","neutral","skipped"] as $allow
      | [.[] | select((.conclusion // "") as $c | ($allow | index($c)) == null)] | length
    ' <<<"$runs_json" 2>/dev/null)" \
    || bad=1
  if (( bad > 0 )); then
    echo "failed"
    return
  fi
  echo "green"
}

# cooldown_allows <age_minutes> <failed_run_count> <now_epoch>
#   Emit "allow" or "deny", using FAIL_COOLDOWN_MINUTES (default 120) and
#   MIN_FAILED_RUNS (default 2) injected via environment. The third
#   argument is accepted so callers/tests can inject a deterministic clock;
#   the decision itself uses only age_minutes and is independent of when
#   the script started.
#
# Algorithm:
#   1. if age < cooldown                          -> deny
#      (we have not waited long enough; next cycle may still recover)
#   2. if failed_runs >= MIN_FAILED_RUNS          -> allow
#      (enough strikes -- this PR is a repeat offender)
#   3. if age < 2 * cooldown                      -> deny
#      (covers the "no Actions run history" case: with zero failed_runs
#      the script waits the double cooldown to avoid flagging a PR that
#      simply has no CI workflow configured)
#   4. otherwise                                  -> allow
cooldown_allows() {
  local age_min="$1" failed_runs="$2" _now_epoch="$3"
  local cooldown="${FAIL_COOLDOWN_MINUTES:-120}"
  local min_fails="${MIN_FAILED_RUNS:-2}"
  if (( age_min < cooldown )); then echo "deny"; return; fi
  if (( failed_runs >= min_fails )); then echo "allow"; return; fi
  if (( age_min < cooldown * 2 )); then echo "deny"; return; fi
  echo "allow"
}

# pr_age_minutes <created_at> <now_epoch>
#   Whole minutes between <created_at> (GitHub's ISO-8601 UTC timestamp,
#   e.g. 2020-01-01T00:00:00Z) and <now_epoch>. A timestamp that cannot be
#   read yields 0, so the cooldown treats it as "just opened" -- an
#   under-count, never an over-count.
#
#   The timestamp is parsed with jq's fromdateiso8601 rather than `date -d`.
#   The -d flag exists only in GNU coreutils date; a BSD date (macOS)
#   rejects it, which previously made every PR read as 0 minutes old and
#   kept the FAILED path behind the cooldown. jq is already a hard
#   dependency of the merge train, so this costs nothing.
pr_age_minutes() {
  local created_at="$1" now_epoch="$2" created_epoch=0
  if [[ -n "$created_at" ]]; then
    created_epoch="$(jq -rn --arg d "$created_at" '$d | fromdateiso8601' 2>/dev/null)" \
      || created_epoch=0
  fi
  if ! [[ "$created_epoch" =~ ^[0-9]+$ ]] || (( created_epoch <= 0 )); then
    echo 0
    return
  fi
  echo $(( (now_epoch - created_epoch) / 60 ))
}

# rebase_marker <sha>
#   Emit the literal marker we embed in an @dependabot rebase nudge so
#   subsequent runs can detect an already-requested rebase for the same
#   HEAD SHA without scanning comment history. Returns the exact
#   substring (no trailing newline) so callers can use the result inside
#   a `[[ "$blob" == *"$marker"* ]]` assertion.
rebase_marker() {
  local sha="$1"
  printf 'merge-train head: %s' "$sha"
}

# is_github_actions_pr <head_ref>
#   Emit "yes" when the PR's head branch is a Dependabot *github-actions* bump.
#   Dependabot names those branches dependabot/github_actions/<...>; every other
#   ecosystem uses a different token, so this is the ecosystem signal without an
#   extra API call. The hyphenated form is accepted defensively.
is_github_actions_pr() {
  local head_ref="$1"
  case "$head_ref" in
    dependabot/github_actions/*|dependabot/github-actions/*) echo "yes" ;;
    *) echo "no" ;;
  esac
}

# is_major_bump <title>
#   Emit "yes" when a Dependabot bump title ("... from <from> to <to>") crosses a
#   major-version boundary, else "no". Both sides may carry a leading "v". The
#   conventional-commit prefix Dependabot prepends (e.g. "chore(deps): ") is
#   irrelevant because the versions are located by the " from ... to ..."
#   fragment. Anything unparseable -- a grouped "Bump the ... group" title, a
#   non-numeric version such as "20-slim", a missing from/to -- is "no": the
#   train only withholds auto-merge from a bump it can positively read as a major
#   crossing.
is_major_bump() {
  local title="$1" from to from_major to_major
  from="$(printf '%s' "$title" | sed -n 's/.* from \([^ ]*\) to \([^ ]*\).*$/\1/p')"
  to="$(printf '%s' "$title" | sed -n 's/.* from \([^ ]*\) to \([^ ]*\).*$/\2/p')"
  from="${from#v}"
  to="${to#v}"
  from_major="${from%%.*}"
  to_major="${to%%.*}"
  if [[ ! "$from_major" =~ ^[0-9]+$ || ! "$to_major" =~ ^[0-9]+$ ]]; then
    echo "no"
    return
  fi
  if (( to_major > from_major )); then echo "yes"; else echo "no"; fi
}
