package mergetrain

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ghCall mirrors the JSON shape the testdata/fake-gh/gh stub writes to
// $FAKE_GH_LOG (one object per line): argv (the tokenised gh args),
// matched_pattern (the response-map key that matched, or null when no
// key matched), and exit (the exit code the stub returned).
type ghCall struct {
	MatchedPattern *string  `json:"matched_pattern"`
	Argv           []string `json:"argv"`
	Exit           int      `json:"exit"`
}

// scriptPath returns the absolute path of scripts/dependabot-merge-train.sh
// from this test's working directory.
func scriptPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRootFromHere(t), "scripts", "dependabot-merge-train.sh")
}

// fakeGhPath returns the absolute path of the bundled gh stub used to
// drive the e2e harness (testdata is invisible to go tooling, but the
// file is on disk and executable thanks to the chmod below).
func fakeGhPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRootFromHere(t),
		"internal", "mergetrain", "testdata", "fake-gh", "gh")
}

// readGhLog parses the JSONL log emitted by the fake-gh stub into a slice
// of ghCall records. The file is written under $FAKE_GH_LOG during the
// test run; the harness deletes the scratch dir via t.TempDir().
func readGhLog(t *testing.T, logPath string) []ghCall {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read gh log: %v", err)
	}
	var calls []ghCall
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var c ghCall
		if err := dec.Decode(&c); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("decode gh log line: %v", err)
		}
		calls = append(calls, c)
	}
	return calls
}

// e2eBundle is a per-test scratch dir wired up with:
//
//   - a copy of testdata/fake-gh/gh on $PATH (chmod 0755)
//   - an empty $FAKE_GH_LOG JSONL file
//   - a $FAKE_GH_RESPONSES_FILE with the JSON map the test wrote
//
// run() spawns scripts/dependabot-merge-train.sh with the harness PATH
// and returns (gh-jsonl path, exit code, combined output).
type e2eBundle struct {
	t        *testing.T
	logPath  string
	respPath string
	binDir   string
}

func newE2E(t *testing.T, responsesJSON string) *e2eBundle {
	t.Helper()
	skipIfNoShellTools(t)
	scratch := t.TempDir()
	b := &e2eBundle{
		t:        t,
		logPath:  filepath.Join(scratch, "gh.jsonl"),
		respPath: filepath.Join(scratch, "responses.json"),
		binDir:   filepath.Join(scratch, "bin"),
	}
	if err := os.MkdirAll(b.binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin dir: %v", err)
	}
	src := fakeGhPath(t)
	dst := filepath.Join(b.binDir, "gh")
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fake-gh: %v", err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatalf("write fake-gh to scratch: %v", err)
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		t.Fatalf("chmod fake-gh: %v", err)
	}
	if err := os.WriteFile(b.respPath, []byte(responsesJSON), 0o644); err != nil {
		t.Fatalf("write responses: %v", err)
	}
	return b
}

// run invokes the script with the harness env and returns (gh-jsonl
// path, exit code, combined stdout+stderr). extraEnv entries are appended
// last so tests can override defaults (DRY_RUN, MAX_MERGES_PER_RUN...).
func (b *e2eBundle) run(extraEnv ...string) (string, int, string) {
	b.t.Helper()
	cmd := osexec.Command("bash", scriptPath(b.t))
	env := append(os.Environ(),
		"PATH="+b.binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_GH_LOG="+b.logPath,
		"FAKE_GH_RESPONSES_FILE="+b.respPath,
		"REPO=acme/widgets",
		"GH_TOKEN=fake",
		"MAX_MERGES_PER_RUN=1",
		"FAIL_COOLDOWN_MINUTES=120",
		"MIN_FAILED_RUNS=2",
		"CI_FAILED_LABEL=ci-failed",
	)
	env = append(env, extraEnv...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	rc := 0
	if exitErr, ok := err.(*osexec.ExitError); ok {
		rc = exitErr.ExitCode()
	} else if err != nil {
		b.t.Fatalf("run script: %v", err)
	}
	return b.logPath, rc, string(out)
}

// argvHasToken reports whether one of the .argv elements is exactly
// "needle" (matching token-for-token keeps the assertion sharp; a
// substring search would let a sha=ABC value pass when looking for
// sha=ABCD, for instance).
func argvHasToken(argv []string, needle string) bool {
	for _, a := range argv {
		if a == needle {
			return true
		}
	}
	return false
}

// argvHasSubstring reports whether any argv element contains substr.
func argvHasSubstring(argv []string, substr string) bool {
	for _, a := range argv {
		if strings.Contains(a, substr) {
			return true
		}
	}
	return false
}

// findFirstCall returns the first gh log call whose argv contains every
// supplied substring; used to pluck a specific call (e.g., the merge
// PUT) for further assertion.
func findFirstCall(calls []ghCall, needles ...string) *ghCall {
	for i, c := range calls {
		hit := true
		for _, n := range needles {
			found := false
			for _, a := range c.Argv {
				if strings.Contains(a, n) {
					found = true
					break
				}
			}
			if !found {
				hit = false
				break
			}
		}
		if hit {
			return &calls[i]
		}
	}
	return nil
}

// findFirstCallByMatchedPattern returns the first gh log call whose
// matched_pattern contains every supplied substring. Use when args are
// dynamic and you need to identify the call by its fixture key.
func findFirstCallByMatchedPattern(calls []ghCall, needles ...string) *ghCall {
	for i, c := range calls {
		if c.MatchedPattern == nil {
			continue
		}
		hit := true
		for _, n := range needles {
			if !strings.Contains(*c.MatchedPattern, n) {
				hit = false
				break
			}
		}
		if hit {
			return &calls[i]
		}
	}
	return nil
}

// ----- e2e cases --------------------------------------------------------

// With two green PRs and MAX_MERGES_PER_RUN=1, the train must merge
// EXACTLY one (the older PR), the PUT must carry merge_method=squash
// and sha=<HEAD_OID> for that PR, and no PUT to the second PR is
// permitted (the cap short-circuits the loop).
//
// Pattern keys are substrings the fake-gh matches against the
// space-joined argv. Pick substrings that uniquely identify each call
// type without depending on the "--repo" arg that sits between the
// subcommand tokens and the rest of the line.
func TestE2ETwoGreenPRs(t *testing.T) {
	responses := `{
    "pr list --repo": [
      {"number":1,"title":"dep: bump foo","headRefName":"dependabot/foo","headRefOid":"aaa111","createdAt":"2020-01-01T00:00:00Z","labels":[],"isDraft":false},
      {"number":2,"title":"dep: bump bar","headRefName":"dependabot/bar","headRefOid":"bbb222","createdAt":"2020-01-02T00:00:00Z","labels":[],"isDraft":false}
    ],
    "mergeable,mergeStateStatus": {"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":""},
    "--paginate": {"check_runs":[{"status":"completed","conclusion":"success"}]},
    "pulls/1/merge": {"merged":true,"sha":"aaa111"}
  }`
	s := newE2E(t, responses)
	_, rc, out := s.run()
	if rc != 0 {
		t.Fatalf("script exit=%d want 0; output:\n%s", rc, out)
	}

	calls := readGhLog(t, s.logPath)

	// Exactly one merge PUT: the older PR (sha=aaa111). The script puts
	// "PUT" as a discrete argv token; the path element contains "/merge".
	// Together they uniquely identify the merge REST call.
	var mergePut *ghCall
	mergePutCount := 0
	for i := range calls {
		if argvHasToken(calls[i].Argv, "PUT") && argvHasSubstring(calls[i].Argv, "/merge") {
			mergePutCount++
			if mergePut == nil {
				mergePut = &calls[i]
			}
		}
	}
	if mergePutCount != 1 {
		t.Fatalf("expected exactly 1 merge PUT, got %d\nlog:\n%s",
			mergePutCount, dumpCalls(calls))
	}
	if mergePut == nil {
		t.Fatalf("merge PUT not located")
	}
	if !argvHasToken(mergePut.Argv, "sha=aaa111") {
		t.Errorf("merge PUT argv missing sha=aaa111 (HEAD OID of merged PR); got %v",
			mergePut.Argv)
	}
	if !argvHasToken(mergePut.Argv, "merge_method=squash") {
		t.Errorf("merge PUT argv missing merge_method=squash; got %v",
			mergePut.Argv)
	}
	if argvHasToken(mergePut.Argv, "sha=bbb222") {
		t.Errorf("merge PUT must NOT carry sha=bbb222 (second PR's head); got %v",
			mergePut.Argv)
	}

	// Cap stopped iteration before the second PR's merge PUT was attempted.
	if c := findFirstCall(calls, "PUT", "pulls/2/merge"); c != nil {
		t.Errorf("second PR merge PUT was attempted; cap should short-circuit\nargv=%v",
			c.Argv)
	}
	// The script may still pr.view the second PR while iterating (cheap
	// read), but it must not approve or PUT against it.
	if c := findFirstCall(calls, "pr", "view", "2"); c != nil {
		for _, a := range c.Argv {
			switch a {
			case "--approve":
				t.Errorf("second PR was approved; cap should have stopped the loop before approve")
			case "PUT":
				t.Errorf("second PR saw a merge PUT; cap should have stopped the loop")
			}
		}
	}
}

// A green PR whose merge PUT returns a 409 must end the cycle with a
// successful exit (0) and a non-fatal WARNING block naming the PR, so the
// run stays green while the condition remains visible to a human.
func TestE2EMergeFails409(t *testing.T) {
	responses := `{
    "pr list --repo": [
      {"number":7,"title":"dep: bump baz","headRefName":"dependabot/baz","headRefOid":"sha007xxx","createdAt":"2020-01-01T00:00:00Z","labels":[],"isDraft":false}
    ],
    "mergeable,mergeStateStatus": {"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":""},
    "--paginate": {"check_runs":[{"status":"completed","conclusion":"success"}]},
    "pulls/7/merge": {"exit":1,"stderr":"Head SHA was not passed to the API. (status 409)","stdout":""}
  }`
	s := newE2E(t, responses)
	_, rc, out := s.run()
	if rc != 0 {
		t.Fatalf("script exit=%d want 0 (merge failure is non-fatal); output:\n%s", rc, out)
	}

	// The structured log block must include the PR number and the SHA, and
	// must mention the underlying gh error message verbatim.
	wantNeedles := []string{
		"WARNING", "::warning::", "decision=merge", "pr=7", "sha=sha007xxx",
		"Head SHA was not passed", "next-cycle-will-retry",
	}
	for _, n := range wantNeedles {
		if !strings.Contains(out, n) {
			t.Errorf("output missing %q\n---\n%s\n---", n, out)
		}
	}
}

// An approve failure (`gh pr review --approve` exits non-zero) must end
// the cycle with a successful exit (0), a non-fatal WARNING block naming
// the PR, and NO merge attempt: the approve leg is retried next cycle
// rather than being bypassed. This pins the approve-failure leg as
// non-fatal; pulls/7/merge is deliberately configured to succeed so a
// regression that falls through to the merge PUT would visibly merge.
func TestE2EApproveFails(t *testing.T) {
	responses := `{
    "pr list --repo": [
      {"number":7,"title":"dep: bump baz","headRefName":"dependabot/baz","headRefOid":"sha007xxx","createdAt":"2020-01-01T00:00:00Z","labels":[],"isDraft":false}
    ],
    "mergeable,mergeStateStatus": {"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":""},
    "--paginate": {"check_runs":[{"status":"completed","conclusion":"success"}]},
    "pr review": {"exit":1,"stderr":"approve rejected (status 403)","stdout":""},
    "pulls/7/merge": {"merged":true,"sha":"sha007xxx"}
  }`
	s := newE2E(t, responses)
	_, rc, out := s.run()
	if rc != 0 {
		t.Fatalf("script exit=%d want 0 (approve failure is non-fatal); output:\n%s", rc, out)
	}

	wantNeedles := []string{
		"::warning::", "WARNING", "decision=approve", "pr=7",
		"next-cycle-will-retry",
	}
	for _, n := range wantNeedles {
		if !strings.Contains(out, n) {
			t.Errorf("output missing %q\n---\n%s\n---", n, out)
		}
	}

	// The approve leg failed, so the cycle must end before the merge PUT.
	calls := readGhLog(t, s.logPath)
	if c := findFirstCall(calls, "PUT", "pulls/7/merge"); c != nil {
		t.Errorf("merge PUT must not be attempted after approve failure; argv=%v",
			c.Argv)
	}
}

// A merge failure on the OLDER of two green PRs must STOP the train
// (break), not continue to the younger PR. A single-PR fixture cannot
// distinguish break from continue, so this fixture carries two PRs: if
// the break regressed into a continue, #2 would be approved and its
// explicitly-successful pulls/2/merge PUT would land. Assert instead
// that #1's merge was attempted, and #2 saw neither an approve nor a PUT.
func TestE2EMergeFailureStopsTrain(t *testing.T) {
	responses := `{
    "pr list --repo": [
      {"number":1,"title":"dep: bump foo","headRefName":"dependabot/foo","headRefOid":"aaa111","createdAt":"2020-01-01T00:00:00Z","labels":[],"isDraft":false},
      {"number":2,"title":"dep: bump bar","headRefName":"dependabot/bar","headRefOid":"bbb222","createdAt":"2020-01-02T00:00:00Z","labels":[],"isDraft":false}
    ],
    "mergeable,mergeStateStatus": {"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":""},
    "--paginate": {"check_runs":[{"status":"completed","conclusion":"success"}]},
    "pulls/1/merge": {"exit":1,"stderr":"Head SHA was not passed to the API. (status 409)","stdout":""},
    "pulls/2/merge": {"merged":true,"sha":"bbb222"}
  }`
	s := newE2E(t, responses)
	_, rc, out := s.run()
	if rc != 0 {
		t.Fatalf("script exit=%d want 0 (merge failure is non-fatal); output:\n%s", rc, out)
	}

	calls := readGhLog(t, s.logPath)

	// The older PR must have reached its merge PUT (proving the failure
	// came from the merge, not an earlier gate).
	if c := findFirstCall(calls, "PUT", "pulls/1/merge"); c == nil {
		t.Fatalf("first PR merge PUT was not attempted; log:\n%s", dumpCalls(calls))
	}
	// The younger PR must see no approve...
	for _, c := range calls {
		if argvHasToken(c.Argv, "--approve") && argvHasToken(c.Argv, "2") {
			t.Errorf("second PR was approved after the first PR's merge failure; argv=%v",
				c.Argv)
		}
	}
	// ...and no merge PUT: a continue regression would have merged it.
	if c := findFirstCall(calls, "PUT", "pulls/2/merge"); c != nil {
		t.Errorf("second PR merge PUT was attempted; the train must break on the first merge failure; argv=%v",
			c.Argv)
	}
}

// DRY_RUN=true replaces every mutating gh call with a [DRY_RUN] would
// log line and returns 0. The log must contain NO merge PUT and NO
// review POST -- both are the load-bearing side-effects of the GREEN
// path.
func TestE2EDryRunGreenPR(t *testing.T) {
	responses := `{
    "pr list --repo": [
      {"number":99,"title":"dep: bump qux","headRefName":"dependabot/qux","headRefOid":"sha099xxx","createdAt":"2020-01-01T00:00:00Z","labels":[],"isDraft":false}
    ],
    "mergeable,mergeStateStatus": {"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":""},
    "--paginate": {"check_runs":[{"status":"completed","conclusion":"success"}]},
    "pulls/99/merge": {"merged":true,"sha":"sha099xxx"}
  }`
	s := newE2E(t, responses)
	_, rc, out := s.run("DRY_RUN=true")
	if rc != 0 {
		t.Fatalf("script exit=%d want 0 (DRY_RUN never exits non-zero); output:\n%s",
			rc, out)
	}

	// Reads still ran: pr list, check-runs, pr view. Mutating calls (label
	// create, pr review POST, merge PUT, pr comment, pr edit) were all
	// replaced with the [DRY_RUN] would: line, so the gh log MUST NOT
	// contain them.
	calls := readGhLog(t, s.logPath)
	forbidden := []struct {
		label string
		token string
	}{
		{"label create", "label"},
		{"pr review POST (approve)", "--approve"},
		{"merge PUT", "PUT"},
		{"pr comment", "comment"},
		{"pr edit (add-label)", "--add-label"},
	}
	for _, f := range forbidden {
		for _, c := range calls {
			for _, a := range c.Argv {
				if a == f.token {
					t.Errorf("DRY_RUN must not call %s; saw argv token %q in call %v",
						f.label, a, c.Argv)
				}
			}
		}
	}

	if !strings.Contains(out, "[DRY_RUN] would:") {
		t.Errorf("DRY_RUN output should surface [DRY_RUN] would: lines; got:\n%s", out)
	}
}

// A green PR with mergeable=CONFLICTING must yield an @dependabot
// rebase nudge containing the rebase_marker for the HEAD OID. Re-
// running the train for the same HEAD must NOT post a second nudge:
// the marker check should find it and skip silently.
func TestE2EConflictedRebase(t *testing.T) {
	t.Run("first run with no prior marker posts the nudge", func(t *testing.T) {
		responses := `{
      "pr list --repo": [
        {"number":31,"title":"dep: bump zip","headRefName":"dependabot/zip","headRefOid":"sha31abc==","createdAt":"2020-01-01T00:00:00Z","labels":[],"isDraft":false}
      ],
      "mergeable,mergeStateStatus": {"mergeable":"CONFLICTING","mergeStateStatus":"DIRTY","reviewDecision":""},
      "--paginate": {"check_runs":[{"status":"completed","conclusion":"success"}]},
      "--json comments": {"comments":[]}
    }`
		s := newE2E(t, responses)
		logPath, rc, out := s.run()
		if rc != 0 {
			t.Fatalf("script exit=%d want 0; output:\n%s", rc, out)
		}

		calls := readGhLog(t, logPath)

		// Exact-token match: avoid the false positive where "comment"
		// substring-matches the "comments" argv token from
		// `gh pr view --json comments`. The pr comment call has a
		// literal "comment" argv element; the pr view call has
		// "comments" only.
		var commentCall *ghCall
		for i, c := range calls {
			for _, a := range c.Argv {
				if a == "comment" {
					commentCall = &calls[i]
					break
				}
			}
			if commentCall != nil {
				break
			}
		}
		if commentCall == nil {
			t.Fatalf("no pr comment token in any log entry; log:\n%s", dumpCalls(calls))
		}
		var body string
		for i, a := range commentCall.Argv {
			if a == "--body" && i+1 < len(commentCall.Argv) {
				body = commentCall.Argv[i+1]
				break
			}
		}
		if !strings.Contains(body, "@dependabot rebase (merge-train head: sha31abc==)") {
			t.Errorf("rebase nudge body missing the marker for HEAD OID; body=%q", body)
		}
		if !strings.Contains(body, "merge-train") {
			t.Errorf("rebase nudge body missing context line; body=%q", body)
		}
		_ = out
	})

	t.Run("second run with same head skips (marker present in comments)", func(t *testing.T) {
		// On this run the pr view comments fixture contains the marker we
		// would have posted on the first run. The script must therefore
		// detect the marker and skip silently -- no `pr comment` call.
		responses := `{
      "pr list --repo": [
        {"number":31,"title":"dep: bump zip","headRefName":"dependabot/zip","headRefOid":"sha31abc==","createdAt":"2020-01-01T00:00:00Z","labels":[],"isDraft":false}
      ],
      "mergeable,mergeStateStatus": {"mergeable":"CONFLICTING","mergeStateStatus":"DIRTY","reviewDecision":""},
      "--paginate": {"check_runs":[{"status":"completed","conclusion":"success"}]},
      "--json comments": {"comments":[{"body":"some prior comment\n\nmerge-train head: sha31abc==\n"}]}
    }`
		s := newE2E(t, responses)
		logPath, rc, _ := s.run()
		if rc != 0 {
			t.Fatalf("script exit=%d want 0", rc)
		}
		calls := readGhLog(t, logPath)
		for _, c := range calls {
			// The rebase-nudge comment is posted via `gh pr comment ... --body`;
			// we must not see a gh pr comment call here because the marker
			// check should have caught it.
			isCommentCall := false
			for _, a := range c.Argv {
				if a == "comment" {
					isCommentCall = true
					break
				}
			}
			if isCommentCall {
				t.Errorf("second run posted pr comment despite marker being present; call=%v",
					c.Argv)
			}
		}
	})
}

// Failed-CI past cooldown with strikes >= MIN_FAILED_RUNS must trigger
// the FAILED path, posting the diagnostic comment BEFORE applying the
// $CI_FAILED_LABEL. This is the contract a future regression would
// most easily break (GH API ordering), so we pin it explicitly.
func TestE2ECommentBeforeLabel(t *testing.T) {
	// Pattern keys are substrings picked so they uniquely identify each
	// call. The check-runs key is HEAD-OID-specific ("commits/<sha042>/")
	// rather than a generic flag like "--paginate" so the canned fail
	// payload cannot be served to a different HEAD's check-runs call
	// within the same run.
	responses := `{
    "pr list --repo": [
      {"number":42,"title":"dep: bump wibble","headRefName":"dependabot/wibble","headRefOid":"sha042","createdAt":"2020-01-01T00:00:00Z","labels":[],"isDraft":false}
    ],
    "commits/sha042/check-runs?per_page=100": {"check_runs":[{"status":"completed","conclusion":"failure","name":"build","html_url":"https://example.invalid/checks/build"}]},
    "branch dependabot/wibble": [{"conclusion":"failure"},{"conclusion":"failure"}]
  }`
	s := newE2E(t, responses)
	_, rc, out := s.run()
	if rc != 0 {
		t.Fatalf("script exit=%d want 0 (label path exits 0); output:\n%s", rc, out)
	}
	calls := readGhLog(t, s.logPath)

	// Locate the comment POST and the label-add calls.
	var commentIdx, editIdx = -1, -1
	for i, c := range calls {
		for _, a := range c.Argv {
			if a == "comment" {
				if commentIdx == -1 {
					commentIdx = i
				}
			}
			if a == "--add-label" {
				editIdx = i
			}
		}
	}
	if commentIdx == -1 {
		t.Fatalf("no pr comment call recorded; log:\n%s", dumpCalls(calls))
	}
	if editIdx == -1 {
		t.Fatalf("no --add-label call recorded; log:\n%s", dumpCalls(calls))
	}
	if commentIdx > editIdx {
		t.Errorf("label call appeared BEFORE comment call; comment idx=%d edit idx=%d\nlog:\n%s",
			commentIdx, editIdx, dumpCalls(calls))
	}

	// The comment body must include the failing check name and html_url.
	commentCall := calls[commentIdx]
	var body string
	for i, a := range commentCall.Argv {
		if a == "--body" && i+1 < len(commentCall.Argv) {
			body = commentCall.Argv[i+1]
			break
		}
	}
	if !strings.Contains(body, "build") || !strings.Contains(body, "https://example.invalid/checks/build") {
		t.Errorf("comment body missing failing-check name+html_url; body=%q", body)
	}
	_ = out
}

// A green Dependabot *github-actions* PR whose title crosses a major version
// boundary must NOT be approved or merged. The train posts an explanatory
// comment, applies $UPGRADE_REVIEW_LABEL, and moves on. pulls/88/merge is
// deliberately configured to succeed so a regression that falls through to the
// merge PUT would visibly merge.
func TestE2EMajorActionBumpFlagged(t *testing.T) {
	responses := `{
    "pr list --repo": [
      {"number":88,"title":"chore(deps): bump actions/checkout from 7.0.1 to 8.0.0","headRefName":"dependabot/github_actions/actions-b43e7c6df8","headRefOid":"sha088xxx","createdAt":"2020-01-01T00:00:00Z","labels":[],"isDraft":false}
    ],
    "mergeable,mergeStateStatus": {"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":""},
    "--paginate": {"check_runs":[{"status":"completed","conclusion":"success"}]},
    "pulls/88/merge": {"merged":true,"sha":"sha088xxx"}
  }`
	s := newE2E(t, responses)
	_, rc, out := s.run()
	if rc != 0 {
		t.Fatalf("script exit=%d want 0; output:\n%s", rc, out)
	}
	calls := readGhLog(t, s.logPath)

	// No approve and no merge PUT for the flagged PR.
	for _, c := range calls {
		if argvHasToken(c.Argv, "PUT") && argvHasSubstring(c.Argv, "pulls/88/merge") {
			t.Errorf("major action bump must not be merged; argv=%v", c.Argv)
		}
		if argvHasToken(c.Argv, "--approve") {
			t.Errorf("major action bump must not be approved; argv=%v", c.Argv)
		}
	}

	// The explanatory comment must be posted, and the upgrade label applied.
	var commentCall, labelCall *ghCall
	for i := range calls {
		if argvHasToken(calls[i].Argv, "comment") {
			commentCall = &calls[i]
		}
		if argvHasToken(calls[i].Argv, "--add-label") {
			labelCall = &calls[i]
		}
	}
	if commentCall == nil {
		t.Fatalf("no upgrade-review comment posted; log:\n%s", dumpCalls(calls))
	}
	if labelCall == nil {
		t.Fatalf("no --add-label call recorded; log:\n%s", dumpCalls(calls))
	}
	if !argvHasToken(labelCall.Argv, "needs-upgrade-review") {
		t.Errorf("upgrade label not applied; argv=%v", labelCall.Argv)
	}
	var body string
	for i, a := range commentCall.Argv {
		if a == "--body" && i+1 < len(commentCall.Argv) {
			body = commentCall.Argv[i+1]
			break
		}
	}
	if !strings.Contains(body, "major") {
		t.Errorf("comment body should explain the major bump; body=%q", body)
	}
	_ = out
}

// A green Dependabot *github-actions* PR with a non-major (minor) bump must
// still be approved and merged -- the gate withholds only major crossings.
func TestE2EMinorActionBumpMerges(t *testing.T) {
	responses := `{
    "pr list --repo": [
      {"number":89,"title":"chore(deps): bump actions/checkout from 7.0.1 to 7.1.0","headRefName":"dependabot/github_actions/actions-b43e7c6df8","headRefOid":"sha089xxx","createdAt":"2020-01-01T00:00:00Z","labels":[],"isDraft":false}
    ],
    "mergeable,mergeStateStatus": {"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":""},
    "--paginate": {"check_runs":[{"status":"completed","conclusion":"success"}]},
    "pulls/89/merge": {"merged":true,"sha":"sha089xxx"}
  }`
	s := newE2E(t, responses)
	_, rc, out := s.run()
	if rc != 0 {
		t.Fatalf("script exit=%d want 0; output:\n%s", rc, out)
	}
	calls := readGhLog(t, s.logPath)
	if c := findFirstCall(calls, "PUT", "pulls/89/merge"); c == nil {
		t.Fatalf("minor action bump should have been merged; log:\n%s", dumpCalls(calls))
	} else if !argvHasToken(c.Argv, "sha=sha089xxx") {
		t.Errorf("merge PUT missing sha=sha089xxx; argv=%v", c.Argv)
	}
	_ = out
}

// A PR that already carries $UPGRADE_REVIEW_LABEL must be skipped early:
// no comment, no label re-add, no approve, no merge PUT. This pins the
// already-labelled early-skip branch.
func TestE2EUpgradeLabeledSkipped(t *testing.T) {
	responses := `{
    "pr list --repo": [
      {"number":90,"title":"chore(deps): bump actions/checkout from 7.0.1 to 8.0.0","headRefName":"dependabot/github_actions/actions-b43e7c6df8","headRefOid":"sha090xxx","createdAt":"2020-01-01T00:00:00Z","labels":[{"name":"needs-upgrade-review"}],"isDraft":false}
    ],
    "mergeable,mergeStateStatus": {"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":""},
    "--paginate": {"check_runs":[{"status":"completed","conclusion":"success"}]},
    "pulls/90/merge": {"merged":true,"sha":"sha090xxx"}
  }`
	s := newE2E(t, responses)
	_, rc, out := s.run()
	if rc != 0 {
		t.Fatalf("script exit=%d want 0; output:\n%s", rc, out)
	}
	calls := readGhLog(t, s.logPath)
	for _, c := range calls {
		if argvHasToken(c.Argv, "comment") {
			t.Errorf("already-labelled PR must not be re-commented; argv=%v", c.Argv)
		}
		if argvHasToken(c.Argv, "--add-label") {
			t.Errorf("already-labelled PR must not be re-labelled; argv=%v", c.Argv)
		}
		if argvHasToken(c.Argv, "--approve") {
			t.Errorf("already-labelled PR must not be approved; argv=%v", c.Argv)
		}
		if argvHasToken(c.Argv, "PUT") && argvHasSubstring(c.Argv, "pulls/90/merge") {
			t.Errorf("already-labelled PR must not be merged; argv=%v", c.Argv)
		}
	}
	_ = out
}

// A PR whose check-runs span more than one page must never be classified green.
// `gh api --paginate --jq` emits one array per page; concatenated pages used to
// mis-parse into a `green` decision, auto-approving and merging a PR that has
// failures. Here page two carries the failure.
func TestE2EMultiPageCheckRunsNeverGreen(t *testing.T) {
	responses := `{
    "pr list --repo": [
      {"number":77,"title":"dep: bump multi","headRefName":"dependabot/multi","headRefOid":"sha077xxx","createdAt":"2020-01-01T00:00:00Z","labels":[],"isDraft":false}
    ],
    "mergeable,mergeStateStatus": {"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":""},
    "--paginate": {"pages":[
      {"check_runs":[{"status":"completed","conclusion":"success"}]},
      {"check_runs":[{"status":"completed","conclusion":"failure"}]}
    ]},
    "pulls/77/merge": {"merged":true,"sha":"sha077xxx"}
  }`
	s := newE2E(t, responses)
	_, rc, out := s.run()
	if rc != 0 {
		t.Fatalf("script exit=%d want 0; output:\n%s", rc, out)
	}
	calls := readGhLog(t, s.logPath)
	if c := findFirstCall(calls, "PUT", "pulls/77/merge"); c != nil {
		t.Errorf("multi-page check-runs must not be merged; argv=%v", c.Argv)
	}
	for _, c := range calls {
		if argvHasToken(c.Argv, "--approve") {
			t.Errorf("multi-page check-runs must not be approved; argv=%v", c.Argv)
		}
	}
	_ = out
}

// ----- helpers ----------------------------------------------------------

func dumpCalls(calls []ghCall) string {
	var b strings.Builder
	for i, c := range calls {
		pat := "<nil>"
		if c.MatchedPattern != nil {
			pat = *c.MatchedPattern
		}
		b.WriteString(pat)
		b.WriteString("\n  argv=")
		b.WriteString(strings.Join(c.Argv, " | "))
		b.WriteString("\n  exit=")
		b.WriteString(strings.Join([]string{itoa(c.Exit)}, " "))
		if i < len(calls)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
