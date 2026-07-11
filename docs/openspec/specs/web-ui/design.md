# SPEC-0004 Design: Web UI

- **Capability:** web-ui
- **Related ADRs:** [ADR-0006 (web stack)](../../../adr/0006-web-stack-htmx.md), [ADR-0007 (frontend styling)](../../../adr/0007-frontend-styling-tailwind-daisyui.md), [ADR-0010 (security posture)](../../../adr/0010-security-privacy-posture.md), [ADR-0003](../../../adr/0003-dual-source-archive.md), [ADR-0005](../../../adr/0005-imessage-txt-parser.md)

## Architecture

```
internal/cli/serve.go ──▶ web.NewServer(store, cfg, log)
  routes() ── securityHeaders(mux)
    GET /{$}              handleIndex
    GET /search          handleSearch        ─┐  keyword search (SPEC-0002)
    GET /search/results  handleSearchResults  │  HTMX live partial
    GET /gallery         handleGallery        │  images / files / links tabs
    GET /c/{id}          handleConversation   │  transcript + infinite scroll
    GET /c/{id}/messages handleMessages       │  keyset next-page partial
    GET /c/{id}/at/{mid} handleConversationAt  │  jump-to-context (ownership check)
    GET /status          handleStatus         │  freshness + ingest
    GET /backups         handleBackups        │  encrypted DB snapshot inventory
    GET /media/{id}/...  handleMedia          ─┘  source-aware, traversal-safe
    GET /static/...      embedded assets (htmx, theme.js, app.css)
```

The server is intentionally minimal (ADR-0006): `net/http` with Go 1.22 pattern
routing, `html/template` (which auto-escapes), HTMX for partials, daisyUI/Tailwind
for styling (ADR-0007). No SPA, no runtime build step; templates and static assets
are `go:embed`-ed into the binary.

## Key design decisions

### Server-rendered HTMX, no SPA (ADR-0006)

Pages render fully server-side and degrade without JavaScript (the search form
submits via GET and renders results server-side; HTMX upgrades it to live partial
updates). Infinite scroll is HTMX `hx-trigger="revealed"` on a load-more sentinel
that fetches the next keyset page and swaps itself out. Keyset pagination on
`(ts_unix, id)` (rather than OFFSET) avoids duplicates/skips and stays cheap on
large transcripts.

### Defense-in-depth for untrusted content (ADR-0010)

Message bodies are untrusted. Three layers protect rendering: (1) `html/template`
auto-escapes everything by default; (2) `renderBody` is the only producer of
`template.HTML` for bodies and itself escapes all plain text, linkifies only
`http(s)` URLs, drops image/media Markdown, and adds `rel="noopener noreferrer
nofollow"` to anchors; (3) `highlightSnippet` (SPEC-0002) sanitizes search snippets
escape-first. The strict CSP (`default-src 'none'`, same-origin scripts/styles,
`img-src 'self' data:`, `frame-ancestors 'none'`) plus `nosniff` is the backstop:
even if escaping were bypassed, content could not load or run external resources or
be framed. All assets (HTMX, theme.js, app.css) are self-hosted so they satisfy
`script-src 'self'` / `style-src 'self'` (ADR-0006).

### Loopback-only, unauthenticated by design (ADR-0010)

The UI has no authentication (it is a single-user personal tool), so it binds
`127.0.0.1` by default and `serve` warns when bound to a non-loopback interface —
exposing it elsewhere is an explicit operator decision behind their own access
control. This mirrors the MCP HTTP transport (SPEC-0003).

### Source-aware, traversal-safe media (ADR-0003, ADR-0005, ADR-0010)

`mediaFilePath` resolves an attachment under the correct base for the
conversation's source: Signal media is per-conversation
(`<archive>/export/<conv>/<rel>`) and iMessage media is a flat tree
(`<imessage_archive>/<rel>`). Both go through `containWithin`, which cleans the
relative path (anchored at `/` so `..` is neutralized) and verifies via
`filepath.Rel` that the result does not escape the base — rejecting traversal with
a 400. Images are served inline; everything else forces download; SVG is forced to
download even though it is absent from the inline-image map, with an explicit guard
so a future "add svg to the map" change cannot re-enable inline script-capable SVG.
`http.ServeContent` provides correct content-type and range support. The encode
step (`mediaURL`) URL-path-escapes each segment so conversation/media names with
spaces and punctuation are safe in URLs.

### Render-to-buffer

`render` executes each template into a buffer before writing the response, so a
template error yields a clean 500 rather than a half-written page.

### daisyUI theming with no FOUC (ADR-0007)

The page sets `data-theme` and loads a self-hosted `theme.js` synchronously in
`<head>` to apply the persisted theme before paint, avoiding a flash of unstyled
content while staying inside `script-src 'self'`. Monogram avatars are rendered
with deterministic per-name colors (FNV-1a then palette), and dynamically-selected
Tailwind classes are force-included via `@source inline(...)` so the content scan
doesn't drop them.

## Trade-offs

- No authentication: acceptable for a loopback single-user tool; documented and
  warned-on when bound wider (ADR-0010).
- Lexical containment trusts that a symlinked media dir's target is intended
  (the check is against the base, not the symlink target) — accepted because the
  archive is operator-owned and read-only.
- Per-conversation enrichment queries (last message, media counts) are run per row
  rather than in one mega-join; simple and indexed at the ~hundreds-of-conversations
  scale.
