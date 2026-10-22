# merge-train-action

Drives a one-merge-per-cycle train over open Dependabot PRs. On each run it
approves and SHA-pinned squash-merges green Dependabot PRs (oldest first, up to
a configurable cap), quarantines failing ones by posting a diagnostic comment
and applying the `ci-failed` label, requests `@dependabot rebase` on PRs that
conflict with the base branch, and withholds auto-merge from major GitHub
Actions version bumps by flagging them with `needs-upgrade-review` for human
review.

## Usage

`GH_TOKEN` must be supplied by the caller's step (for example from an
`octo-sts` step that mints a short-lived, scoped token); the caller also owns
the trigger, `runs-on`, `permissions` and `concurrency` of the job.

```yaml
name: Dependabot Merge Train
on:
  schedule:
    - cron: '*/15 * * * *'
  workflow_dispatch:
concurrency:
  group: dependabot-merge-train
  cancel-in-progress: false
permissions:
  id-token: write
  contents: read
jobs:
  merge-train:
    runs-on: self-hosted-arm64
    timeout-minutes: 15
    steps:
      - uses: actions/checkout@v7
      - name: Generate short-lived GitHub token
        uses: octo-sts/action@v1.1.1
        id: octo-sts
        with:
          scope: acme/widgets
          identity: dependabot-merge-train
      - uses: southernlights-tech/merge-train-action@v1
        with:
          repo: acme/widgets
        env:
          GH_TOKEN: ${{ steps.octo-sts.outputs.token }}
```

## Inputs

| Input | Default | Description |
| ----- | ------- | ----------- |
| `repo` | (required) | `owner/name` of the repository whose Dependabot PRs to process. |
| `max-merges-per-run` | `1` | Maximum merges per cycle. |
| `fail-cooldown-minutes` | `120` | Minutes a failing PR must age before it is quarantined. |
| `min-failed-runs` | `2` | Failed Actions runs required on the head branch before quarantining. |
| `ci-failed-label` | `ci-failed` | Label applied to PRs quarantined for failing checks. |
| `upgrade-review-label` | `needs-upgrade-review` | Label applied to major GitHub Actions bumps withheld for human review. |
| `dry-run` | `false` | When true, log decisions without approving, commenting, labelling or merging. |

## Why a composite action

This is a composite action, not a reusable workflow: it runs inside the
caller's job. That keeps `runs-on`, `permissions`, `concurrency` and the
octo-sts identity in the consuming repository, and lets the caller's job use
its own self-hosted runners. A called reusable workflow owned by another org
could not use the caller's self-hosted runners.

## Development

Run the tests and lint the shell scripts:

```sh
go test ./...
shellcheck scripts/dependabot-merge-train.sh scripts/lib/merge-train-lib.sh
```
