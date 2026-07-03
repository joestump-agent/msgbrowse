# SPEC-0008: Web performance

- **Status:** Accepted
- **Date:** 2026-07-03
- **Capability:** web-performance
- **Source packages:** `internal/store` (`query.go`, `schema.go`, `gallery.go`), `internal/web` (`server.go`, `handlers.go`, `media.go`, `gallery.go`, `templates/`, `static/`, `tailwind/`)
- **Related ADRs:** [ADR-0006 (web stack)](../../../adr/0006-web-stack-htmx.md), [ADR-0007 (styling)](../../../adr/0007-frontend-styling-tailwind-daisyui.md), [ADR-0012 (slate design system)](../../../adr/0012-slate-redesign-design-system.md), [ADR-0013 (pure-Go SQLite)](../../../adr/0013-pure-go-sqlite-driver.md)

## Overview

Every page render was measured at 1.8–2.9 s server-side TTFB against the
reference archive (405,241 messages / 2,271 conversations), dominated by a
~6,800-query sidebar N+1, an uncompressed 1.9 MB HTML document (98% of which is
the sidebar), and full-page work repeated on every HTMX-boosted navigation.
A Chrome trace attributes 1,966 ms of the 2,161 ms LCP to TTFB and estimates
1,861 ms of savings from text compression alone.

This spec makes the web UI fast and keeps it correct: set-based queries,
partial rendering for boosted requests, compression, cacheable statics,
filter-aware gallery SQL, and contained client-side rendering.

**Headline acceptance targets** (warm, on the reference archive):

| Metric | Baseline | Target |
|---|---|---|
| `/` TTFB | 2.3–2.9 s | **< 300 ms** |
| Boosted nav server time | 1.8–2.0 s | **< 100 ms** |
| Page bytes on the wire (`/`) | 1.87 MB | **< 200 KB** (gzip) |
| Sidebar DB cost | 1.0–1.6 s, 6,810 queries | **≤ 450 ms, ≤ 5 queries** (≤ 150 ms with denormalization) |

## Requirements

### REQ-0008-001: Set-based conversation listing

`ListConversations` MUST produce every sidebar summary using a bounded number
of set-based statements (no per-conversation queries). Output MUST be
column-identical to the current summaries. On the reference archive the
listing MUST complete in ≤ 450 ms warm (measured rewrite: 346–388 ms vs
1.0–1.6 s + 6,810 queries today).

#### Scenario: One listing, no N+1
- **Given** the reference archive
- **When** any page renders the sidebar
- **Then** conversation summaries are produced by ≤ 5 SQL statements total, with no statement executed once per conversation.

### REQ-0008-002: Chronologically correct summary timestamps

First/last/newest message timestamps in summaries MUST be selected via
`ts_unix` ordering (e.g. the row at `MAX(ts_unix)`), never lexicographic
`MIN(ts)`/`MAX(ts)` string aggregation. This applies to `ListConversations`,
`GetConversationByID`, and `NewestMessageTS`.

#### Scenario: iMessage date ranges are true
- **Given** an iMessage conversation whose `ts` strings are month-name-first ("Nov 13, 2015 5:53:29 AM")
- **When** its summary renders
- **Then** first/last shown match the chronologically first/last messages by `ts_unix` (today `MIN(ts)` returns the alphabetically-first month — e.g. "Apr 01, 2017" for a thread that started Nov 2015).

### REQ-0008-003: Conversation-scoped attachment and link rows

The schema MUST denormalize `conversation_id` onto `attachments` and `links`
(schema v7): migration backfills existing rows from `messages`, ingest writes
it going forward, and indexes `attachments(conversation_id, kind)` /
`links(conversation_id)` support per-conversation counting without joining
`messages` (measured 44–112× faster counts; sidebar DB cost → ~0.10–0.13 s).

#### Scenario: Counting without the messages join
- **Given** schema v7
- **When** per-conversation image/file/link counts are computed
- **Then** the query plans touch only `attachments`/`links` via the new indexes, and a re-ingested archive yields identical counts to v6.

### REQ-0008-004: No redundant global aggregates per render

Full-page renders MUST NOT issue standalone global aggregate queries whose
answer is derivable from data already fetched. `CountMessages` (measured
133 ms full-index scan per render) MUST be derived from the conversation
listing; `NewestMessageTS` (measured 430 ms full table scan, lexicographically
wrong) MUST be a single `ORDER BY ts_unix DESC LIMIT 1` probe.

#### Scenario: Home page aggregate cost
- **Given** the reference archive
- **When** `/` renders
- **Then** no `COUNT(*)` over all messages and no `MAX(ts)` string aggregation executes.

### REQ-0008-005: Lightweight conversation lookup on hot paths

Handlers that only need a conversation's `name`/`source` (media serving,
infinite-scroll pages, pin toggling) MUST use a minimal single-row lookup, not
`GetConversationByID` (which also aggregates counts, identifiers, and AI facts
— measured 105 ms). Pin toggling MUST be a direct `UPDATE` (no
read-modify-write) and its form MUST navigate via the boosted partial-render
path. Store errors on these paths MUST be logged, not silently swallowed.

#### Scenario: Serving one image
- **Given** a transcript with an image attachment
- **When** the browser requests `/media/{conv}/{path}`
- **Then** conversation resolution is a single `SELECT name, source FROM conversations WHERE id = ?`.

### REQ-0008-006: Partial rendering for boosted navigation

When a request carries `HX-Request: true` (and is not a history restore), the
server MUST render only the `#main-content` region (plus `<title>`), MUST NOT
execute the sidebar listing, and MUST keep byte-identical full-page rendering
for non-HTMX requests. Server time for a boosted transcript navigation MUST be
< 100 ms warm on the reference archive (payload ~35 KB vs 1.9 MB today).

#### Scenario: Boosted click skips the sidebar
- **Given** a user on `/` clicking a conversation in the boosted sidebar
- **When** the request arrives with `HX-Request: true`
- **Then** the response contains only the `#main-content` element (+ title), no sidebar markup, and no conversation-listing SQL ran.

#### Scenario: Direct load still full
- **Given** a bookmarked `/c/{id}` opened in a fresh tab
- **When** the request has no HTMX headers
- **Then** the full document (shell + sidebar + content) renders as today.

### REQ-0008-007: Compressed text responses

The server MUST gzip text responses (HTML, CSS, JS, JSON, SVG) when the client
advertises `Accept-Encoding: gzip`, and MUST NOT double-compress media
responses. Measured: `/` 1,868,371 → ~152 KB (12.4×); `app.css` 137 KB → 21 KB.

#### Scenario: Compressed page
- **Given** a browser requesting `/` with `Accept-Encoding: gzip`
- **When** the response is written
- **Then** it carries `Content-Encoding: gzip` and decodes to the identical document.

### REQ-0008-008: Cacheable static assets

Embedded static assets MUST support conditional revalidation: an `ETag`
derived from content (embedded files have no modtime) with `304 Not Modified`
on `If-None-Match`, or content-hashed names with immutable caching.

#### Scenario: Revisit does not re-download app.css
- **Given** a client that has `app.css` cached with its ETag
- **When** it revalidates
- **Then** the server answers `304` with no body.

### REQ-0008-009: Filter-aware gallery queries

Gallery counting/listing MUST NOT join `messages` when no message-scoped
filter (conversation/source/date) is active (measured: unfiltered counts
576 ms → 16 ms without the join). The links tab MUST be paginated (today it
renders ~20k anchors in one response), and attachment listing MUST avoid
whole-table sorts via an appropriate index or keyset pagination.

#### Scenario: Unfiltered gallery counts
- **Given** `/gallery` with no filters
- **When** tab counts are computed
- **Then** the query plans touch only `attachments` / `links`.

### REQ-0008-010: Lazy lightbox media

Lightbox `<img>` elements MUST be lazy (`loading="lazy"`); hidden lightboxes
MUST NOT trigger downloads on page load (measured: 179 eager full-size
downloads today, defeating the grid's lazy loading).

#### Scenario: Gallery opens without downloading originals
- **Given** `/gallery` with 179 images
- **When** the page loads
- **Then** only viewport-visible grid thumbnails are fetched; a lightbox image is fetched when its lightbox is opened.

### REQ-0008-011: Contained row rendering

Sidebar conversation rows and transcript message rows MUST be render-contained
(`content-visibility: auto` with `contain-intrinsic-size`), so off-screen rows
are skipped for style/layout/paint; a theme switch MUST recalc only on-screen
content (~hundreds of elements, not the measured ~23k), and hand-written
transitions MUST be gated behind `prefers-reduced-motion: no-preference`.

#### Scenario: Theme toggle on a large archive
- **Given** the reference archive's 2,271-row sidebar plus a loaded transcript
- **When** the user toggles the theme
- **Then** the synchronous style recalculation is limited to on-screen rows and completes without a perceptible hang.

### REQ-0008-012: Efficient sidebar filter

The sidebar filter MUST precompute lowercased names once, MUST only write
`hidden` when a row's visibility changes, and MUST coalesce input events
(rAF or ~100 ms debounce) so typing does not force full-list layout per
keystroke over 2,271 rows.

#### Scenario: Typing stays responsive
- **Given** the reference archive sidebar
- **When** the user types a 5-character filter quickly
- **Then** visibility writes happen at most once per coalesced frame and only on rows whose match state changed.
