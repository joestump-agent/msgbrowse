# SPEC-0008 Design: Web performance

- **Capability:** web-performance
- **Related ADRs:** [ADR-0006](../../../adr/0006-web-stack-htmx.md), [ADR-0007](../../../adr/0007-frontend-styling-tailwind-daisyui.md), [ADR-0012](../../../adr/0012-slate-redesign-design-system.md), [ADR-0013](../../../adr/0013-pure-go-sqlite-driver.md)

## Context: what was measured

A multi-agent profile against the reference archive (405,241 messages, 2,271
conversations; modernc SQLite, warm) plus a headless-Chrome trace produced the
baselines this design is built around:

| Hotspot | Measured |
|---|---|
| `ListConversations` base `GROUP BY` (string `MIN/MAX(ts)` forces 405k row fetches) | 327–805 ms |
| Fill loop: 3 queries × 2,270 conversations (6,810 queries) | 639–802 ms |
| `CountMessages` per render | 133 ms |
| `NewestMessageTS` (`MAX(ts)` TEXT scan; lexicographically wrong) | 430 ms |
| `GetConversationByID` on media requests | 105 ms |
| Unfiltered gallery counts (needless `messages` join) | 576 ms |
| Page bytes (`/`), 98% sidebar; no gzip anywhere | 1.87 MB (152 KB gzipped) |
| Chrome: LCP 2,161 ms — TTFB 1,966 ms; DOM 22,794 elements | trace |
| Theme toggle: full-document recalc, 300 `color-mix()`, 112 `:has()` | ~37k nodes |

## Store layer

### Single-statement listing (REQ-0008-001/002)

One statement replaces the base query + fill loop. The shape that measured
346–388 ms (returning all 13 columns for 2,271 rows):

```sql
SELECT c.id, c.name, c.source, c.pinned,
       COALESCE(ms.msg_count, 0),
       COALESCE(fm.ts, ''), COALESCE(lm.ts, ''),
       COALESCE(ms.last_unix, 0),
       COALESCE(lm.sender, ''), COALESCE(substr(lm.body, 1, 320), ''),
       COALESCE(ac.image_count, 0), COALESCE(ac.file_count, 0),
       COALESCE(lc.link_count, 0)
  FROM conversations c
  LEFT JOIN (SELECT conversation_id, COUNT(*) msg_count, MAX(ts_unix) last_unix
               FROM messages GROUP BY conversation_id) ms ON ms.conversation_id = c.id
  LEFT JOIN messages fm ON fm.id = (SELECT m2.id FROM messages m2 WHERE m2.conversation_id = c.id
                                     ORDER BY m2.ts_unix ASC,  m2.id ASC  LIMIT 1)
  LEFT JOIN messages lm ON lm.id = (SELECT m2.id FROM messages m2 WHERE m2.conversation_id = c.id
                                     ORDER BY m2.ts_unix DESC, m2.id DESC LIMIT 1)
  LEFT JOIN (SELECT m.conversation_id,
                    SUM(a.kind = 'image') image_count, SUM(a.kind = 'file') file_count
               FROM attachments a JOIN messages m ON m.id = a.message_id
              GROUP BY m.conversation_id) ac ON ac.conversation_id = c.id
  LEFT JOIN (SELECT m.conversation_id, COUNT(*) link_count
               FROM links l JOIN messages m ON m.id = l.message_id
              GROUP BY m.conversation_id) lc ON lc.conversation_id = c.id
 ORDER BY COALESCE(ms.last_unix, 0) DESC, c.name ASC
```

First/last timestamps come from the rows *selected by* `ts_unix` ordering
(`fm`/`lm` rowid joins), which is what fixes the lexicographic
`MIN/MAX(ts)` wrongness for iMessage-format strings at zero extra cost.

**Measured rejects** (do not resurrect):
- `ROW_NUMBER()` window variant: 2.1 s.
- `CROSS JOIN` forcing attachments-outer: 0.30–0.91 s of random rowid I/O.
- `PRAGMA mmap_size`: helps the C CLI, **1.5–2× slower under modernc**.
- Covering index `(conversation_id, ts_unix, ts)`: 5× on the *old* query but
  superseded by the rewrite and doesn't fix REQ-0008-002.

### Schema v7 denormalization (REQ-0008-003)

After the rewrite, the `ac`/`lc` subqueries dominate (~0.28 s: they walk all
405k `idx_messages_conv_ts` entries probing per-message indexes). v7 adds
`conversation_id INTEGER NOT NULL` to `attachments` and `links`:

- Migration: `ALTER TABLE … ADD COLUMN` + backfill `UPDATE … FROM messages` +
  `CREATE INDEX idx_attachments_conv_kind ON attachments(conversation_id, kind)`
  and `idx_links_conv ON links(conversation_id)`.
- Ingest (`ReplaceConversationMessages`) writes it directly.
- The `ac`/`lc` subqueries become single-table `GROUP BY`s (measured
  simulation: 3–4 ms / 1 ms — 44–112×). Total sidebar DB cost ≈ 0.10–0.13 s.

v7 lands as its own migration so the query rewrite (correct at v6) and the
denormalization (fast at v7) are separately shippable and testable.

### Cheap lookups (REQ-0008-004/005)

- `TotalMessages` = sum of `MessageCount` over the listing (Go, free).
- `NewestMessageTS` = `SELECT ts FROM messages ORDER BY ts_unix DESC LIMIT 1`
  (sub-ms, chronologically correct).
- New `ConversationSourceName(ctx, id)` single-row probe for `handleMedia`,
  `handleMessages`, `handlePin`; errors logged via the server logger.
- `handlePin` becomes `UPDATE conversations SET pinned = 1 - pinned …` style
  direct write (or bind the desired state), no full summary fetch first.

## Web layer

### Partial rendering (REQ-0008-006)

Templates: each page template splits into a `*_content` define that renders
`<title>{{.Title}}</title>` + `<main id="main-content">…</main>`, and the full
page define wraps it with `page_start`/`page_end` (which lose/keep the shell
accordingly). `render()` branches:

```go
partial := r.Header.Get("HX-Request") == "true" &&
           r.Header.Get("HX-History-Restore-Request") != "true"
```

- Partial path executes `name+"_content"` and **skips baseData's sidebar
  listing entirely** (handlers fetch sidebar data only for full renders).
- htmx swaps `hx-select="#main-content"`; the emitted `<title>` rides along so
  history entries keep correct titles.
- History-restore requests get the full document (htmx replaces `body`).

### Middleware (REQ-0008-007/008)

`gzip` wrapper outermost (inside logging), applied to `text/html`, `text/css`,
`application/javascript`, `application/json`, `image/svg+xml`; skip when
`Content-Type` is image/video or response < ~1 KB. Static assets get
`ETag: "sha256-prefix"` computed once at startup from the embedded bytes, with
`If-None-Match` → `304`. (Embedded FS has zero modtimes, so `http.FileServer`
can never do time-based revalidation — hence ETags.)

### Gallery (REQ-0008-009/010)

`galleryWhere` builds the `messages` join only when a message-scoped filter is
set. Links tab pages with the same keyset/`LIMIT` pattern the transcript uses.
Lightbox `<img>` gains `loading="lazy"` (lazy images inside `display:none`
containers are not fetched until the `:target` lightbox opens; the grid tile
already warmed the cache for the same URL).

## Client layer (REQ-0008-011/012)

`input.css`:

```css
.conv-item { content-visibility: auto; contain-intrinsic-size: auto 52px; }
.msg-row, .sys-event { content-visibility: auto; contain-intrinsic-size: auto 2.2rem; }
@media (prefers-reduced-motion: no-preference) { /* the 13 hand-written transitions move here */ }
```

`theme.js` adds a `theme-switching` guard class that disables transitions
during the swap. `sidebar.js` filter: precompute lowercased names, write
`hidden` only on change, coalesce via `requestAnimationFrame`.

Off-screen containment shrinks both the theme-swap recalc set and the
deep-scroll cost of accumulated transcript pages; true windowing is explicitly
deferred until containment proves insufficient.

## Testing & verification

- Store: golden-equality test — rewrite output == legacy loop output on a
  fixture DB; migration test v6→v7 backfill correctness; `EXPLAIN QUERY PLAN`
  assertions that no `messages` join appears in unfiltered gallery counts.
- Web: httptest asserting (a) `HX-Request` responses contain `#main-content`
  and no `app-sidebar`, (b) full requests are byte-stable, (c) gzip round-trip,
  (d) `304` on matching `If-None-Match`.
- End-to-end: TTFB re-measured on the reference archive against the SPEC-0008
  targets table; headless-Chrome trace re-run for LCP/recalc.
