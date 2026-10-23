package mergetrain

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRootFromHere returns the absolute path of the repository root from
// this test's working directory (internal/mergetrain/...).
func repoRootFromHere(t *testing.T) string {
	t.Helper()
	here, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(here, "..", ".."))
}

func libPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRootFromHere(t), "scripts", "lib", "merge-train-lib.sh")
}

// skipIfNoShellTools skips the test (t.Skip) when bash or jq is unavailable
// on PATH. Both are required by the lib and by the test harness, so we
// surface that constraint early with a clear message rather than a less
// obvious exec failure later.
func skipIfNoShellTools(t *testing.T) {
	t.Helper()
	if _, err := osexec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH; bash-driven lib test skipped")
	}
	if _, err := osexec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH; merge-train lib depends on jq")
	}
}

// runLib invokes <fn> <args...> in a fresh bash process with the merge
// train lib sourced and returns the trimmed combined output plus the
// exit code (nil → 0). extraEnv is appended last so tests can override
// FAIL_COOLDOWN_MINUTES/MIN_FAILED_RUNS for the call.
func runLib(t *testing.T, fn string, args []string, extraEnv map[string]string) (string, error) {
	t.Helper()
	skipIfNoShellTools(t)
	lib := libPath(t)
	// bash -c "<script>" <bash> <lib> <argv...>; the script shifts the
	// <lib> entry out of "$@" and forwards the rest to <fn>. This keeps
	// each argv element byte-identical through bash (no shell-quoting
	// artefacts on the JSON / number arguments).
	script := `LIB_PATH="$1"; shift; . "$LIB_PATH"; ` + fn + ` "$@"`
	argv := []string{"bash", "-c", script, "bash", lib}
	argv = append(argv, args...)
	cmd := osexec.Command(argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), " \t\n"), err
}

// ----- classify_check_runs ----------------------------------------------

func TestClassifyCheckRunsGreen(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"only success",
			`[{"status":"completed","conclusion":"success"}]`},
		{"success-plus-neutral-plus-skipped",
			`[{"status":"completed","conclusion":"success"},` +
				`{"status":"completed","conclusion":"neutral"},` +
				`{"status":"completed","conclusion":"skipped"}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := runLib(t, "classify_check_runs", []string{c.in}, nil)
			if got != "green" {
				t.Errorf("classify_check_runs()=%q want green (input=%s)", got, c.in)
			}
		})
	}
}

func TestClassifyCheckRunsFailed(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"mixed-success-plus-failure",
			`[{"status":"completed","conclusion":"success"},` +
				`{"status":"completed","conclusion":"failure"}]`},
		{"timed_out",
			`[{"status":"completed","conclusion":"success"},` +
				`{"status":"completed","conclusion":"timed_out"}]`},
		{"cancelled",
			`[{"status":"completed","conclusion":"cancelled"}]`},
		{"action_required",
			`[{"status":"completed","conclusion":"action_required"}]`},
		{"startup_failure",
			`[{"status":"completed","conclusion":"startup_failure"}]`},
		{"stale",
			`[{"status":"completed","conclusion":"stale"}]`},
		{"unknown-conclusion-not-in-allowlist",
			`[{"status":"completed","conclusion":"banana"}]`},
		{"all-completed-with-startup_failure-mixed",
			`[{"status":"completed","conclusion":"success"},` +
				`{"status":"completed","conclusion":"startup_failure"}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := runLib(t, "classify_check_runs", []string{c.in}, nil)
			if got != "failed" {
				t.Errorf("classify_check_runs()=%q want failed (input=%s)", got, c.in)
			}
		})
	}
}

func TestClassifyCheckRunsRunning(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"queued",
			`[{"status":"queued","conclusion":null}]`},
		{"in_progress",
			`[{"status":"in_progress","conclusion":null}]`},
		{"mixed-completed-plus-running",
			`[{"status":"completed","conclusion":"success"},` +
				`{"status":"queued","conclusion":null}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := runLib(t, "classify_check_runs", []string{c.in}, nil)
			if got != "running" {
				t.Errorf("classify_check_runs()=%q want running (input=%s)", got, c.in)
			}
		})
	}
}

func TestClassifyCheckRunsNone(t *testing.T) {
	got, _ := runLib(t, "classify_check_runs", []string{"[]"}, nil)
	if got != "none" {
		t.Errorf("classify_check_runs([])=%q want none", got)
	}
}

// ----- cooldown_allows --------------------------------------------------

func TestCooldownAllows(t *testing.T) {
	// Defaults under test: FAIL_COOLDOWN_MINUTES=120, MIN_FAILED_RUNS=2.
	cases := []struct {
		name              string
		age, fails, epoch string
		want              string
	}{
		{"age 119/cooldown 120 -> deny (cooling down)", "119", "2", "1700000000", "deny"},
		{"age 120 / fails=2 -> allow (enough strikes)", "120", "2", "1700000000", "allow"},
		{"age 120 / fails=1 -> deny (one strike < two)", "120", "1", "1700000000", "deny"},
		{"fails=0 at age 239 -> deny (no history, 2x-cooldown)", "239", "0", "1700000000", "deny"},
		{"fails=0 at age 240 -> allow (no history past 2x-cooldown)", "240", "0", "1700000000", "allow"},
		{"fails=2 at age 30 -> deny (cooldown first)", "30", "2", "1700000000", "deny"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := runLib(t, "cooldown_allows",
				[]string{c.age, c.fails, c.epoch},
				map[string]string{
					"FAIL_COOLDOWN_MINUTES": "120",
					"MIN_FAILED_RUNS":       "2",
				})
			if got != c.want {
				t.Errorf("cooldown_allows(%s,%s,%s)=%q want %q",
					c.age, c.fails, c.epoch, got, c.want)
			}
		})
	}
}

// ----- pr_age_minutes ---------------------------------------------------

// pr_age_minutes must read a GitHub timestamp the same way on every platform
// the script runs on. An earlier implementation used `date -d`, which BSD
// date (macOS) rejects, so every PR aged 0 minutes and the FAILED path never
// cleared the cooldown.
func TestPRAgeMinutes(t *testing.T) {
	// 1700000000 == 2023-11-14T22:13:20Z.
	cases := []struct {
		name, createdAt, now, want string
	}{
		{"same instant", "2023-11-14T22:13:20Z", "1700000000", "0"},
		{"two hours earlier", "2023-11-14T20:13:20Z", "1700000000", "120"},
		{"sub-minute remainder truncated", "2023-11-14T22:11:21Z", "1700000000", "1"},
		{"years old", "2020-01-01T00:00:00Z", "1700000000", "2036053"},
		{"empty is treated as just opened", "", "1700000000", "0"},
		{"unparseable is treated as just opened", "not-a-date", "1700000000", "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := runLib(t, "pr_age_minutes", []string{c.createdAt, c.now}, nil)
			if err != nil {
				t.Fatalf("pr_age_minutes(%q,%s) error: %v; output %q", c.createdAt, c.now, err, got)
			}
			if got != c.want {
				t.Errorf("pr_age_minutes(%q,%s)=%q want %q", c.createdAt, c.now, got, c.want)
			}
		})
	}
}

// ----- rebase_marker ----------------------------------------------------

func TestRebaseMarker(t *testing.T) {
	got, _ := runLib(t, "rebase_marker", []string{"abc123sha=="}, nil)
	want := "merge-train head: abc123sha=="
	if got != want {
		t.Errorf("rebase_marker(abc123sha==) = %q want %q", got, want)
	}
}

// ----- is_github_actions_pr ---------------------------------------------

func TestIsGithubActionsPR(t *testing.T) {
	cases := []struct {
		name, ref, want string
	}{
		{"underscore ecosystem token", "dependabot/github_actions/actions-b43e7c6df8", "yes"},
		{"hyphenated form accepted defensively", "dependabot/github-actions/actions-b43e7c6df8", "yes"},
		{"gomod branch", "dependabot/go_modules/github.com/aws/aws-sdk-go-v2-1.78.0", "no"},
		{"npm branch", "dependabot/npm_and_yarn/web/vite-8.3.0", "no"},
		{"empty", "", "no"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := runLib(t, "is_github_actions_pr", []string{c.ref}, nil)
			if got != c.want {
				t.Errorf("is_github_actions_pr(%q)=%q want %q", c.ref, got, c.want)
			}
		})
	}
}

// ----- is_major_bump ----------------------------------------------------

func TestIsMajorBump(t *testing.T) {
	cases := []struct {
		name, title, want string
	}{
		{"major", "chore(deps): bump actions/checkout from 7.0.1 to 8.0.0", "yes"},
		{"minor", "chore(deps): bump actions/checkout from 7.0.1 to 7.1.0", "no"},
		{"patch", "chore(deps): bump actions/checkout from 7.0.1 to 7.0.2", "no"},
		{"bare major tags with v prefix", "chore(deps): bump actions/checkout from v6 to v7", "yes"},
		{"bare major tags without v prefix", "Bump aws-actions/amazon-ecr-login from 1 to 2", "yes"},
		{"grouped title unparseable -> no", "chore(deps): bump the actions group with 2 updates", "no"},
		{"non-semver token -> no", "chore(deps): bump node from 20-slim to 26-slim in the docker-deps group", "no"},
		{"missing from/to -> no", "chore(deps): bump actions/checkout", "no"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _ := runLib(t, "is_major_bump", []string{c.title}, nil)
			if got != c.want {
				t.Errorf("is_major_bump(%q)=%q want %q", c.title, got, c.want)
			}
		})
	}
}
