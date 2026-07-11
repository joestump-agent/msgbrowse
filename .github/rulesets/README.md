# Branch protection rulesets

Ruleset definitions for this repository, kept in-repo so the protection policy
is reviewable and versioned like everything else. GitHub does not read this
directory automatically — a repo admin applies it once (and re-applies it after
any change here).

## What `main.json` enforces on `main`

- **No direct pushes** — all changes land through a pull request.
- **Squash merges only** — matches the SDD merge strategy in CLAUDE.md and
  keeps history linear (the `required_linear_history` rule backstops this).
- **Required status checks** — the two CI jobs that run on *every* PR must
  pass before merging:
  - `gofmt + vet + tests` (ci.yml)
  - `docker image builds` (ci.yml)

  The CSS, Desktop, and Security workflows are deliberately **not** required:
  they are path-filtered and only run when their files change, so requiring
  them would deadlock every PR that doesn't touch those paths (a required
  check that never reports never turns green).
- **Review-thread resolution required** — every PR conversation must be
  resolved before merge. The approving-review count is 0 because this repo is
  developed by a single owner + agent pair; bump
  `required_approving_review_count` to 1 if a second reviewer account should
  gate merges.
- **No force pushes, no branch deletion** on `main`.
- **Branches need not be up to date with `main` to merge**
  (`strict_required_status_checks_policy: false`) — squash merges keep history
  clean without forcing a rebase-and-rerun on every landed PR. Flip it to
  `true` for stricter pre-merge testing at the cost of serializing merges.

## How to apply

### Option A — GitHub UI (import)

Settings → Rules → Rulesets → New ruleset → **Import a ruleset** → upload
`main.json`.

### Option B — `gh` CLI

```sh
gh api repos/joestump-agent/msgbrowse/rulesets --input .github/rulesets/main.json
```

To update an existing ruleset instead of creating a duplicate, find its id and
`PUT` it:

```sh
gh api repos/joestump-agent/msgbrowse/rulesets --jq '.[] | {id, name}'
gh api -X PUT repos/joestump-agent/msgbrowse/rulesets/<id> --input .github/rulesets/main.json
```

> **Plan note:** rulesets (like classic branch protection) are free on public
> repositories; private repositories need GitHub Pro/Team for them to be
> enforced.
