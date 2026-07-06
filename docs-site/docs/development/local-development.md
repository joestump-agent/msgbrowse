---
title: Local development
sidebar_position: 1
description: Everything you need to hack on msgbrowse — Go toolchain, make targets, the no-Node CSS pipeline and its drift guard, desktop-shell builds, fixtures, and docs-site development.
---

# Local development

msgbrowse is deliberately easy to build: the core is a **pure-Go** binary with
**no C toolchain, no cgo, and no Node** anywhere in the runtime path. This page
covers the full contributor loop — building, testing, the CSS pipeline, the
desktop shell, and the docs site itself.

## Toolchain requirements

- **Go 1.25 or newer** — the only hard requirement for the core.
  `go.mod` declares `go 1.25.0` and CI builds with Go 1.25.
- **No C toolchain.** The SQLite driver is pure Go
  ([`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) with FTS5
  built in — [ADR-0013](/decisions/0013-pure-go-sqlite-driver)), so the root
  module builds with `CGO_ENABLED=0`. The Makefile exports that pin and CI
  enforces it, so the binary provably stays cgo-free.
- **No Node for the app.** Node (≥ 20) is needed only if you work on the
  [docs site](#docs-site-development). The web UI's CSS is built by a
  standalone binary — see [the CSS pipeline](#the-css-pipeline-no-node)
  below.
- The only cgo in the repository is the [desktop shell](#desktop-shell-development),
  quarantined in its own nested Go module.

## Clone, build, test

```sh
git clone https://github.com/joestump/msgbrowse.git
cd msgbrowse
make build   # build ./bin/msgbrowse (pure Go, CGO_ENABLED=0)
make test    # go test ./...
make check   # gofmt + go vet + tests — the exact CI gate
```

`make check` is what CI runs
([`ci.yml`](https://github.com/joestump/msgbrowse/blob/main/.github/workflows/ci.yml)),
so a green `make check` locally means the core gate passes. Other useful
targets: `make cover` (coverage summary), `make fmt` (gofmt the tree),
`make install` (`go install` into `$GOBIN`/`$GOPATH/bin`).

## Run the server against the synthetic fixtures

The repo ships small, **hand-written synthetic archives** under `testdata/` —
a signal-export style archive (`testdata/archive`, with its `export/` tree and
journal) and an imessage-exporter style one (`testdata/imessage`). They are
enough to click around the UI:

```sh
make build
./bin/msgbrowse --data-dir ./data \
  --archive-root          testdata/archive \
  --imessage-archive-root testdata/imessage \
  import
./bin/msgbrowse --data-dir ./data serve   # auto-opens http://127.0.0.1:8787
```

`make run` is the shortcut for build-then-`serve` (using whatever roots your
config/env provide). The data dir is the only thing msgbrowse writes;
`./data/` is gitignored.

:::warning Real archives never go in the repo
The `testdata/` fixtures are synthetic by design. Never import, copy, or
commit a **real** exported archive (yours or anyone else's) inside the repo
checkout — point `--archive-root` and friends at a directory outside it. The
archive roots are treated as strictly read-only either way (see the
[security model](../reference/security-model.md)).
:::

## The CSS pipeline (no Node)

The web UI uses Tailwind CSS v4 + daisyUI with the bespoke slate themes
([ADR-0007](/decisions/0007-frontend-styling-tailwind-daisyui),
[ADR-0012](/decisions/0012-slate-redesign-design-system)). The built
`internal/web/static/app.css` is **committed** and served via `go:embed`, so
the runtime needs no toolchain, no CDN, and the strict CSP holds.

When you change classes in templates, rebuild it:

```sh
make css
```

That downloads the **Tailwind v4 standalone CLI** (a single binary — no npm,
no `node_modules`) and the daisyUI package into the gitignored `.tools/`
directory, versions pinned in the Makefile, then rebuilds `app.css` from
`internal/web/tailwind/input.css`.

**The drift guard.** CI
([`css.yml`](https://github.com/joestump/msgbrowse/blob/main/.github/workflows/css.yml),
job "app.css is up to date") runs `make css` from a fresh download and **fails
the PR** if the committed `app.css` differs from the rebuild. Two gotchas will
trip you here:

1. **Stale `.tools/` cache.** A rebuild against an old cached toolchain can
   differ from CI's clean download even when consecutive local builds look
   deterministic. Before committing a CSS change, always rebuild from a clean
   cache:

   ```sh
   rm -rf .tools && make css
   ```

2. **Tailwind scans `.go` files too.** Tailwind v4's automatic content
   detection walks every non-gitignored text file from the repo root —
   *including Go source* — in addition to the explicit
   `@source "../templates"` in `input.css`. Class-shaped strings in `.go`
   files therefore end up compiled into `app.css` (and removing them changes
   it), so even a pure-Go change can require a `make css` rebuild. Classes
   composed at runtime in Go (never appearing literally anywhere) must be
   safelisted via `@source inline(...)` in `input.css`; the docs site is
   excluded from the scan with `@source not "../../../docs-site"`.

## Desktop shell development

The desktop app ([ADR-0017](/decisions/0017-desktop-shell-wails)) is the
repository's **only cgo code**, quarantined in a nested Go module
(`cmd/msgbrowse-desktop/go.mod`, with a `replace` back to the root) behind the
`desktop` build tag — so the core stays `CGO_ENABLED=0` and the desktop
targets are never prerequisites of any core target.

```sh
make desktop-test    # the module's headless tests — pure Go, no webview toolchain needed
make desktop-linux   # build the Linux desktop app (cgo) to cmd/msgbrowse-desktop/build/bin/msgbrowse
```

`make desktop-linux` links the system webview, so it needs the GTK3/WebKit2GTK
dev headers:

```sh
sudo apt-get install -y libgtk-3-dev libwebkit2gtk-4.1-dev pkg-config
```

The default tags target webkit2gtk-**4.1** (Ubuntu 24.04+); on distros still
shipping webkit2gtk-4.0 build with
`make desktop-linux DESKTOP_TAGS=desktop,production`.

**macOS `.app` builds happen in CI**, not locally: the
[`desktop.yml`](https://github.com/joestump/msgbrowse/blob/main/.github/workflows/desktop.yml)
matrix builds the darwin/universal `.app` (including the bundled exporter
toolchain and Syncthing) on `v*` tags, manual dispatch, and PRs that touch the
desktop shell. You only need the Wails CLI for local shell development on a
Mac — install the pin that matches the `wails/v2` library version in the
module's `go.mod`:

```sh
go install github.com/wailsapp/wails/v2/cmd/wails@v2.12.0
```

The signing story for those CI-built `.app` bundles is its own runbook:
[macOS signing & notarization](release-signing.md).

## Docs site development

The documentation site (this site) is a Docusaurus app in `docs-site/` — the
one place Node (≥ 20) is required:

```sh
cd docs-site
npm install
npm start        # rebuild generated content + dev server
npm run build    # full production build (fails on broken links)
```

Hand-authored pages live under `docs-site/docs/<section>/`; the ADR and spec
sections are **generated** from the repo sources on every build — never edit
`docs-generated/`. See
[`docs-site/CONTENT-PLAN.md`](https://github.com/joestump/msgbrowse/blob/main/docs-site/CONTENT-PLAN.md)
for the ground rules (verify every command against the code before writing
it).

## ADRs, specs, and pull requests

- **Architecture Decision Records** live in
  [`docs/adr/`](https://github.com/joestump/msgbrowse/tree/main/docs/adr)
  (MADR format) and **specifications** in
  [`docs/openspec/specs/`](https://github.com/joestump/msgbrowse/tree/main/docs/openspec/specs)
  (each a `spec.md` + `design.md` pair). Both render here under
  [Architecture](/architecture).
- PRs should keep `make check` green, add tests for new
  ingest/search/MCP behavior, and — when templates or classes change — commit
  a fresh `rm -rf .tools && make css` rebuild so the drift guard passes.
