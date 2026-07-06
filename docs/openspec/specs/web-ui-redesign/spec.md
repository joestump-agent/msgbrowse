# SPEC-0006: Web UI redesign (slate)

- **Status:** Accepted
- **Date:** 2026-06-28
- **Capability:** web-ui-redesign
- **Source packages:** `internal/web` (templates, render, server), `internal/store` (small additions), `internal/web/tailwind/input.css`
- **Related ADRs:** [ADR-0012 (slate design system)](../../../adr/0012-slate-redesign-design-system.md), [ADR-0007 (Tailwind + daisyUI)](../../../adr/0007-frontend-styling-tailwind-daisyui.md), [ADR-0006 (web stack/CSP)](../../../adr/0006-web-stack-htmx.md), [ADR-0010 (privacy posture)](../../../adr/0010-security-privacy-posture.md)
- **Design source:** [docs/design/redesign-handoff.md](../../../design/redesign-handoff.md)
- **Extends / supersedes (visually):** [SPEC-0004 (web-ui)](../web-ui/spec.md)

## Overview

This spec defines the "slate" redesign of the msgbrowse web UI: a bespoke dark
design system implemented as a custom daisyUI theme (+ a derived light variant),
a dense-log transcript replacing chat bubbles, and redesigned Home, Search,
Media, and Journal screens. All existing constraints hold: server-rendered
`html/template` + HTMX, no Node at runtime, strict CSP, system fonts, Heroicons.
The design handoff is the pixel-level source of truth; this spec captures the
testable requirements and the slate tokens.

## Design tokens (normative)

- **Surfaces:** base `#0f1216`, raised panel `#13171c`, navbar/sidebar `#161a20`,
  hover/selected tint `#1b2330` / `rgba(111,147,214,.05–.12)`.
- **Borders:** primary `#232a33`; subtle `#1c2128`/`#1c222a`/`rgba(35,42,51,.55)`.
- **Text:** primary `#dbe2ea` (headings up to `#eaf1fb`), secondary `.5–.7`
  alpha, dim/meta `.32–.42` alpha.
- **Accent:** slate blue `#6f93d6`; light accent (the "Me" name) `#9bb6e6`; text
  on accent `#0f1216`. Search `<mark>`: bg `rgba(232,184,75,.28)`, text `#f0d28a`.
- **Source colors:** Signal `#3b82f6` (pill text `#6f93d6` / border `rgba(111,147,214,.35)`),
  iMessage `#34c759` (pill text `#7bd88f` / border `rgba(52,199,89,.3)`).
- **Avatar palette (hash→index):** `#f43f5e #0ea5e9 #f59e0b #14b8a6 #d946ef #f97316 #6366f1 #10b981`.
- **Type:** system sans (UI); system mono for timestamps/filenames/counts;
  `tabular-nums` on all counts. Scale (px): meta 10.5–12 · body 13–14.5 · row
  titles 14 · H1 21–26 · home hero 38. Heading tracking `-.01em`…`-.025em`.
- **Shape:** radius rows/cards 8–14, inputs 8–11, avatars 50% (or 6px dense),
  pills 5–8, frame 14–16. In-app elevation via borders, not shadows.
- **Layout:** full-width unified header ~52px (amended by #190; originally a
  54px navbar), sidebar 320px, transcript column max-width 640px
  (680px editorial). Centered screen columns: Home ~780px, Journal ~880px. Use
  flex/grid `gap`, not margins.

## Requirements

### REQ-0006-001: Slate theme foundation
The UI MUST render in the slate tokens via a custom daisyUI theme set as the
default (dark). A derived light variant MUST exist and the header toggle MUST
switch between them, persisted to `localStorage` and applied before paint (no
FOUC). Numerals in counts/timestamps/filenames MUST use a mono face and
`tabular-nums`. No web fonts or external assets may be introduced (CSP holds).

#### Scenario: Theme applied without FOUC
- **Given** a saved theme preference
- **When** any page loads
- **Then** the slate (or light) tokens apply before first paint and counts render in tabular mono.

### REQ-0006-002: App shell — header

*Amended by #190 (shell redesign). Originally: a 54px navbar with the accent
home glyph + **msgbrowse** wordmark, right-aligned global counts in dim mono,
and a 34px circular settings button. The global counts moved to Status
(Settings → Diagnostics) in #152; #190 promoted the toolbar to a full-width
header owning the primary navigation.*

The app MUST render one full-width unified header (~52px) as a direct child of
`<body>`, spanning the window above both the sidebar and the content column.
It MUST show: LEFT — the below-md sidebar toggle and a contextual title
(**msgbrowse** on home/global surfaces, the active conversation name on a
transcript) that links home; CENTER — a Messages/Media segmented tab pair (the
primary nav), kept in normal flow and centered within the header's available
space so it can never overlap the side clusters at any viewport width; RIGHT —
a search pill (an icon-only `/search` link below sm), the theme toggle, and a
settings gear (the sole Settings entry). The header MUST NOT show global
counts.

#### Scenario: Header tabs and contextual title
- **Given** an open conversation
- **When** the page renders
- **Then** the header shows the conversation name on the left (linking home), the centered Messages/Media tabs with Messages active, and the search/theme/settings cluster on the right — with no global counts.

### REQ-0006-003: App shell — sidebar

*Amended by #190 (shell redesign). Originally the sidebar also carried nav
links with leading icons (Search · Media · Journal · Status & backups); those
surfaces moved to the header — the centered Messages/Media tabs and the search
pill — leaving the sidebar list-only.*

The 320px sidebar MUST be list-only: a filter input ("Filter conversations"),
a PINNED section (when any conversation is pinned), and a CONVERSATIONS header
with a right-aligned total — no nav links. Conversation rows MUST be two lines
(avatar+presence · name · right-aligned mono count / last-message preview with
`Me:` prefix when sent by the owner, truncated). The selected row MUST show the
`#1b2330` tint and a left accent rail. Typing in the filter MUST narrow the
list. Below md the sidebar MUST present as an overlay drawer toggled from the
header; at md and above it MUST be pinned open.

#### Scenario: Selected conversation row
- **Given** a conversation is open
- **When** the sidebar renders
- **Then** its row shows the accent left rail + selected tint, and the filter narrows the list as typed.

### REQ-0006-004: Avatars, presence dots, source pills
Avatars MUST be a colored circle (6px rounded square in dense lists) with white
600-weight initials, colored deterministically from the avatar palette by a hash
of the name. A presence dot MUST sit bottom-right, colored by source (Signal
blue / iMessage green) with a 2px border matching the surface behind it. Source
MUST also be expressible as a colored pill (Signal/iMessage).

#### Scenario: Source-derived presence
- **Given** a Signal conversation and an iMessage conversation
- **When** their rows/headers render
- **Then** each shows the correct presence-dot color and, where shown, the matching source pill.

### REQ-0006-005: Conversation reader — dense-log transcript
The transcript MUST use the dense-log layout (NOT chat bubbles): a left mono
timestamp gutter (~76px, `HH:MM:SS`, dim), a 3px sender-colored rail (accent for
"Me"), and a content column (max-width 640px) with the sender name (12.5px/600;
"Me" in `#9bb6e6`) above the body (14px, line-height ~1.55). It MUST render: a
faint accent wash + accent edge on "Me" rows; centered day separators; centered
italic/mono system events; attachment chips (paperclip + mono filename + size);
link-preview pills; image placeholders (real decoded media in production); and
quoted blocks. Consecutive messages from the same sender within a short window
MUST be grouped. Large histories MUST scroll without loading all rows at once
(keep the existing HTMX keyset paging; virtualization optional).

#### Scenario: Dense-log row
- **Given** an open conversation
- **When** the transcript renders
- **Then** each message shows the timestamp gutter, sender-colored rail, name, and body; "Me" rows carry the accent wash; day separators and system events render as non-message rows; and no `chat-bubble` markup remains.

### REQ-0006-006: Conversation header
The header MUST show the avatar + name (22–26px/700), inline pill chips for
phone/email identifiers, and a meta line (`N messages · <date range>`) with
icon+count chips for images / links / attachments.

#### Scenario: Identifier chips
- **Given** a contact with a merged phone and email
- **When** the conversation header renders
- **Then** the phone and email render as inline pill chips alongside the message/range/media meta.

### REQ-0006-007: Home landing
Home MUST render a centered column with: the hero wordmark + tagline; a 3-cell
stat strip (Conversations / Messages / Newest message) with mono tabular values;
an "On this day" card surfacing a resurfaced message from a prior year (when one
exists); a "Jump back in" card listing recent conversations with relative times;
and a quick-links grid (Search / Media / Journal / Status).

#### Scenario: On this day
- **Given** messages exist from the same calendar day in a prior year
- **When** Home renders
- **Then** the "On this day" card shows one such message with a link to that day; absent any, the card is omitted or shows an empty state.

### REQ-0006-008: Search redesign
Search MUST show a large field, a wrapping filter row (conversation, source,
from/to date, sender, Has-attachment / Has-link toggles, accent Search button),
a results meta line with count and elapsed time, and result cards (avatar ·
conversation · sender · timestamp · source pill · snippet with `<mark>`). The
top/active result MAY carry an accent border. Live filtering MUST refine results
as the query/filters change.

#### Scenario: Results with timing
- **Given** a query that matches messages
- **When** results render
- **Then** the meta line shows `<count> results · <elapsed>` and each card shows a source pill and a highlighted snippet.

### REQ-0006-009: Media redesign
Media MUST show tabs (Images / Files / Links) with count badges and an accent
underline on the active tab, a filter row, and for Images a multi-column grid of
square cover tiles with a bottom scrim + mono filename; clicking a tile MUST open
a lightbox navigable within the current tab/filter set.

#### Scenario: Image grid + lightbox
- **Given** images in the archive
- **When** the Images tab renders
- **Then** square tiles show filenames over a scrim and clicking one opens a lightbox.

### REQ-0006-010: Pinned conversations
A conversation MUST be pinnable; pinned conversations MUST appear in the sidebar
PINNED section. Pin state MUST persist (schema) and a pin/unpin affordance MUST
exist.

#### Scenario: Pin persists
- **Given** a pinned conversation
- **When** the app reloads
- **Then** it still appears under PINNED.

### REQ-0006-011: Status & backups re-skin
The Status & backups screen MUST adopt the slate system (surfaces, borders, mono
tabular values, source-grouped runs) with no behavioral change.

#### Scenario: Status in slate
- **Given** ingest runs and snapshots
- **When** Status renders
- **Then** it uses slate surfaces/borders and mono tabular values.

### REQ-0006-012a: Journal screen shell (testable now)
The Journal screen MUST render the slate layout — daily editorial card (accent
eyebrow, prose region, People/Themes/Mood meta row), month strip, and per-day
cards — from a **stubbed/fixture** day summary, independent of the LLM backend.
This is verifiable at merge with a fixture and MUST NOT depend on live digest
generation.

#### Scenario: Editorial card renders from a fixture
- **Given** a fixture day summary (prose + people/themes/mood)
- **When** the Journal screen renders
- **Then** it shows the eyebrow, prose, and People/Themes/Mood meta in the slate layout.

### REQ-0006-012b: Journal wired to real digests — BLOCKED on backend
Once the journal digest backend exists, the Journal screen MUST render real
generated summaries (per-day, cached) over the raw messages. This requirement is
BLOCKED on the journal digest backend (roadmap Slice 6) and is deferred — it is
NOT counted against this epic's mergeable slices.

#### Scenario: Real editorial day
- **Given** a generated editorial summary for a day
- **When** the Journal screen renders that day
- **Then** it shows the generated prose and People/Themes/Mood derived from that day's threads.
