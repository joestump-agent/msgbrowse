# ADR-0022: Contact merging and the address-book abstraction

- **Status:** Proposed
- **Date:** 2026-07-11
- **Deciders:** Joe Stump
- **Related:**
  - [ADR-0003 (dual-source archive with unified contacts)](0003-dual-source-archive.md) — established
    `contacts` + `contact_identifiers`, auto-creation on import, and the rule
    that cross-source merging is a **manual confirmation, never a heuristic**;
    this ADR builds the machinery that ADR-0003 deferred to "the contacts page".
  - [ADR-0011 (contact facts extraction)](0011-contact-facts-extraction.md) — prior art for
    per-contact data that must survive merges and re-ingest: facts are keyed to
    `contacts(id)` with idempotent, hash-deduplicated writes; merged threads
    accumulate a single fact set.
  - [ADR-0010 (security & privacy posture)](0010-security-privacy-posture.md) — the address-book
    integration is local-only, read-only, and adds **no** network egress.
  - [ADR-0020 (bundled exporters + guided setup)](0020-bundled-exporters-guided-setup.md) /
    [ADR-0021 (Syncthing sync engine)](0021-syncthing-sync-engine.md) — the platform-gating
    precedents: permission-probed macOS integrations behind seams, and the
    `devicesync` build tag that keeps an unfinished/platform-specific feature
    out of the default build (#20).
- **Tracking:** epic #8; children #9 (interface + no-op), #10 (macOS provider),
  #11 (merge engine), #12 (settings UI); this ADR is #13.
- **Requirements:** [SPEC-0015 (contact merging & de-duplication)](../openspec/specs/contact-merge/spec.md)

## Context and Problem Statement

msgbrowse ingests identities from multiple providers (Signal, iMessage,
WhatsApp, later Telegram) into the unified contacts layer of
[ADR-0003](0003-dual-source-archive.md): `contacts(id, display_name, notes)` is
the canonical person, `contact_identifiers(contact_id, source, identifier,
UNIQUE(source, identifier))` is the source-side handle, and
`conversations.contact_id` points a 1:1 thread at its person. On import,
`UpsertConversation` (internal/store/store.go) does a transactional
get-or-create: an unseen `(source, identifier)` auto-creates a contact named
after the identifier and links it. Per-contact features already lean on this —
contact facts ([ADR-0011](0011-contact-facts-extraction.md)) key to
`contacts(id)` precisely so merged threads share one fact set.

What is missing is the merging itself. Today the same real person shows up as
`signal:MJ`, `imessage:+15551234567`, and `whatsapp:MJ 🎸` — three contacts,
three fact sets, three journal identities. There is no engine to detect that
they match, no user-facing control to merge or split them, and no persistence
model that keeps a manual decision alive across re-ingest: `DeleteSourceData`
prunes a source's identifiers and orphaned contacts, and the next import's
get-or-create would resurrect a fresh, unmerged contact.

On macOS, the native Contacts app is the obvious *suggestion* source — it
already maps phone numbers and emails to named people. But it is Mac-only,
permission-gated (TCC), and requires cgo bindings, so it must not become a hard
dependency of the store, the web layer, or the Linux build.

Three decisions need capturing: the shape and injection seam of the
address-book abstraction, the platform split, and the merge/override
persistence model.

## Decision Drivers

- **A wrong merge corrupts derived history.** Journal digests and contact facts
  propagate a bad merge forward; ADR-0003 already ruled that merging is
  user-confirmed, never heuristic. The engine must default to *suggesting*.
- **Overrides must survive re-ingest.** `ReplaceConversationMessages` churns
  message rowids on every import, and `DeleteSourceData` + re-enable churns
  contact rowids too. The codebase's answer to rowid churn is to key durable
  state by **stable identity** (message hashes for embeddings, facts,
  reactions); merge decisions need the same treatment.
- **Platform integrations stay behind seams.** The web layer cannot import cgo
  modules; every privileged or platform-specific capability so far is injected
  (`SetDetector`, `SetEnabler`, `SetPairingSource` in internal/web) with a
  documented nil/absent state. The address book must follow suit.
- **No new egress.** The Contacts lookup is a local framework call; nothing
  about merging may talk to the network ([ADR-0010](0010-security-privacy-posture.md)).
- **CI and Linux build clean with `CGO_ENABLED=0`.** The default `make check`
  build must not link the Contacts framework, mirroring how the `devicesync`
  tag keeps Syncthing wiring out of release binaries (#20).

## Considered Options

### Address-book abstraction

1. **`ContactResolver` interface in a pure-Go package, injected via a `Set…`
   seam; macOS provider behind a build tag; default no-op (CHOSEN).**
2. Call the macOS Contacts framework directly from the merge engine behind
   `runtime.GOOS` checks. **Rejected:** links cgo into every build, makes the
   store/web layers platform-aware, and is untestable without a Mac — the exact
   problems the existing seams were built to avoid.
3. Import the user's address book into the database at setup time. **Rejected:**
   copies sensitive third-party PII into msgbrowse's store when a read-at-match
   lookup suffices; goes stale; and turns a *hint* source into persisted state
   that would then need its own sync/cleanup story.

### Canonical-person persistence

1. **Keep `contacts` as the canonical person; persist merge/split *decisions*
   in a new identifier-keyed `contact_links` table and re-apply them in an
   idempotent reconcile pass (CHOSEN).**
2. Add a `canonical_id` self-reference column on `contacts` (merge = pointing a
   loser at a winner, rows never deleted). **Rejected:** every existing query
   (`conversations.contact_id`, `contact_facts.contact_id`,
   `ContactFactsByConversation`, sidebar joins) would need a resolve-the-alias
   hop or a view; orphan cleanup in `DeleteSourceData` gets subtle; and the
   alias chain still dies with the row on source delete, so it does not even
   solve re-ingest survival by itself.
3. A separate `canonical_persons` table above `contacts`. **Rejected:** a second
   person concept when `contacts` already *is* the canonical person
   (ADR-0003); it would fork every per-contact feature (facts, journal,
   conversation linking) into "which person table?".

### Auto-matching posture

1. **Suggest by default; exact-normalized-identifier auto-merge only as an
   explicit opt-in; address book is hints-only (CHOSEN).**
2. Auto-merge on identifier equality out of the box. **Rejected:** shared family
   phone numbers, recycled numbers, and stale address-book entries make exact
   matches wrong often enough that ADR-0003's "manual confirmation" rule stands.

## Decision Outcome

### 1. `ContactResolver`: a pure-Go seam, wired like the existing ones

A new pure-Go package `internal/contacts` defines the abstraction:

```go
// Identifier is a normalized source-side handle.
type Identifier struct {
    Kind  string // "phone" | "email" | "handle"
    Value string // E.164 for phones, lowercased for emails
}

// Person is one address-book entry: a display name plus its identifiers.
type Person struct {
    Name        string
    Identifiers []Identifier
}

// Availability is the resolver's tri-state, mirroring the setup detector's
// permission model: absent (no provider on this platform/build), needs
// permission (provider present, OS grant missing), available.
type Availability int

// Resolver is the address-book seam. Implementations MUST be read-only and
// MUST NOT perform network I/O.
type Resolver interface {
    Availability(ctx context.Context) Availability
    // People enumerates address-book entries for batch matching. An absent or
    // permission-denied book returns an empty slice and no error.
    People(ctx context.Context) ([]Person, error)
    // LookupIdentifier returns the people matching one normalized identifier.
    LookupIdentifier(ctx context.Context, id Identifier) ([]Person, error)
}
```

The default implementation is a **no-op resolver** (`Availability` = absent,
empty results, never an error), so the merge path degrades to
stored-identifier matching with zero conditional logic at call sites.

Injection mirrors the established seams — and for the same reason. The web
layer cannot import the cgo desktop module, so `internal/web/enable.go` and
`internal/web/settings.go` take their privileged/platform capabilities through
`SetDetector` / `SetEnabler` / `SetPairingSource`: a `Set…` method called after
`NewServer` and **before serving begins** (handlers read the field without
locking; late wiring would race), with a documented rendered state when nothing
is wired. `ContactResolver` gets the identical contract:
`Server.SetContactResolver(contacts.Resolver)`; unset or no-op means the
settings UI renders the address-book hint option in its disabled/absent state
and the merge engine runs on stored identifiers alone. The merge engine
receives the same `Resolver` instance at construction in the cli/desktop wiring
(where `wireDeviceSync` and the Enabler are wired today).

### 2. Platform split: macOS provider behind a build tag, Linux no-op

The macOS Contacts provider is the only cgo consumer, so it is doubly gated,
following the `devicesync` precedent that just landed in #20
(`internal/cli/serve_devicesync.go` / `serve_nodevicesync.go`):

- The provider lives behind `//go:build darwin && macontacts`; a paired stub
  file (`!darwin || !macontacts`) supplies a constructor returning the no-op.
  The default build — and CI's `CGO_ENABLED=0 make check` — never links the
  Contacts framework; the desktop shell (already cgo, [ADR-0017](0017-desktop-shell-wails.md))
  builds with the tag and wires the real provider.
- Contacts access is TCC-gated. A denied or undetermined grant makes the
  provider behave exactly like the no-op for results while reporting
  `Availability` = needs-permission, so the settings UI can render the same
  "needs permission" guidance the setup detectors use (`internal/setup`). A
  permission failure never errors the merge path.
- Identifier normalization (E.164 phones, lowercased emails) is pure Go in
  `internal/contacts`, shared by the provider and the matcher, and unit-tested
  in CI where the framework itself cannot run.

### 3. Identifier matching + manual merge/split, with suggestions as the default

The merge engine (`internal/store` methods plus matching logic in
`internal/contacts`) produces **candidates**: groups of contacts sharing a
normalized identifier value across sources, optionally augmented by
address-book grouping (two stored identifiers appearing on one `Person`) —
each candidate carrying its reason. Per ADR-0003, the address book is a
*suggestion* source, never a *decision* source:

- **Default:** candidates are surfaced in settings for manual confirmation.
  Nothing merges silently.
- **Opt-in auto-merge:** the user may enable auto-merge for exact normalized
  equality on chosen identifier kinds (phone and/or email). Address-book hints
  never auto-merge regardless of settings.
- **Manual merge** unions two contacts: repoint `contact_identifiers`,
  `conversations.contact_id`, and `contact_facts` (dedup via the existing
  `UNIQUE(contact_id, fact_hash)`) to the winner, delete the loser — the
  mechanics ADR-0003 §Consequences already sketched.
- **Manual split** moves chosen identifiers off a contact onto a fresh one and
  records that the affected pairs must stay apart.

### 4. Overrides persist across re-ingest, keyed by stable identifiers

The persistence model is a decision journal, not a pointer graph — new tables
in migration v11, no changes to `contacts` / `contact_identifiers`:

```sql
CREATE TABLE contact_links (
    id          INTEGER PRIMARY KEY,
    kind        TEXT    NOT NULL,           -- 'merge' | 'split'
    origin      TEXT    NOT NULL,           -- 'manual' | 'auto'
    source_a    TEXT    NOT NULL,
    identifier_a TEXT   NOT NULL,
    source_b    TEXT    NOT NULL,
    identifier_b TEXT   NOT NULL,
    created_at  TEXT    NOT NULL,
    UNIQUE(source_a, identifier_a, source_b, identifier_b)
);

CREATE TABLE contact_merge_rules (          -- single-row settings
    id                 INTEGER PRIMARY KEY CHECK (id = 1),
    auto_merge         INTEGER NOT NULL DEFAULT 0,
    match_phone        INTEGER NOT NULL DEFAULT 1,
    match_email        INTEGER NOT NULL DEFAULT 1,
    use_address_book   INTEGER NOT NULL DEFAULT 1,
    updated_at         TEXT    NOT NULL
);
```

Links are keyed by **`(source, identifier)` pairs, not contact rowids** — the
same reasoning that keys embeddings, facts, and reactions by stable message
hashes instead of rowids: contact rowids churn (`DeleteSourceData` + re-enable
recreates them), identifiers are stable. Pairs are stored in canonical order
(`(source_a, identifier_a) < (source_b, identifier_b)`) so the `UNIQUE`
constraint dedups symmetric records, and a pair holds exactly **one current
decision**: manually merging a previously-split pair replaces the split row,
and vice versa — the latest manual action wins, and the table never contradicts
itself. A merge records the full bipartite pairing of the two contacts'
identifiers so a partially-deleted group still re-links from any surviving
pair. There is deliberately **no foreign key** from links to
`contact_identifiers`: a link whose identifier is currently absent is inert,
not invalid — it re-activates when that source is re-imported.

An idempotent **reconcile pass** re-applies decisions after every import (and
on demand from settings): for each `merge` link whose two identifiers both
exist on different contacts, union them (winner selection is a deterministic
ordered rule: (1) exactly one contact has a user-meaningful `display_name` —
i.e. differs from all of its identifiers — that contact wins; (2) otherwise
(both or neither user-meaningful) the lower `id` wins); then, if rules enable
auto-merge,
apply exact-match merges, skipping any pair with a `split` link (**precedence:
manual split > manual merge > auto rules**), recording applied auto-merges as
`origin='auto'` links so they too survive re-ingest. Reconcile runs entirely
locally after the store write path — it is not an import side effect that adds
egress (there is none to add) and it never touches the LLM. The get-or-create
in `UpsertConversation` is untouched: it may briefly resurrect an unmerged
contact mid-import, and reconcile immediately folds it back.

## Consequences

### Good

- Merge and split become first-class, reversible, durable user decisions; the
  journal, facts, and transcripts address one person per human.
- Re-ingest, source disable/re-enable, and device-sync replicas
  ([ADR-0021](0021-syncthing-sync-engine.md) replicas run their own ingest)
  all converge to the same merged state, because decisions are identifier-keyed
  and reconcile is idempotent.
- Linux, CI, and release builds carry zero cgo/Contacts surface; the seam keeps
  the web layer testable with fakes exactly like the existing seams.
- Facts and future per-contact features inherit merging for free — they already
  key to `contacts(id)`, and fact dedup already tolerates the union
  ([ADR-0011](0011-contact-facts-extraction.md)).

### Bad

- A reconcile pass now runs after imports: more work in the import path
  (bounded — contacts number in the hundreds, not millions) and one more
  invariant ("reconcile converges, in one pass, regardless of decision order")
  to test carefully.
- Merging deletes the loser's `contacts` row; if any future feature keys an
  external reference to a contact id (a bookmark, or a hypothetical
  contact-by-id URL — no such route exists today), that reference can dangle.
  The winner-selection rule keeps ids stable where possible but not always.
- `contact_links` grows with the bipartite pairing of merged groups; a person
  with many identifiers across many sources produces O(n·m) rows. Acceptable at
  address-book scale, but it is bookkeeping the UI never shows directly.
- Two decisions (`macontacts` tag name, exact settings surface) are delegated to
  the implementing issues and could drift; SPEC-0015 pins the behavior, not the
  spellings.

### Neutral

- The address book is consulted live and never persisted; msgbrowse's database
  gains no third-party PII beyond what imports already contain.
- Auto-merge stays off by default; users who never open settings get exactly
  today's behavior plus suggestions.

## Requirements

Normative requirements live in
[SPEC-0015 (contact merging & de-duplication)](../openspec/specs/contact-merge/spec.md)
with design rationale in its paired
[design.md](../openspec/specs/contact-merge/design.md). Implementation is
tracked by epic #8 (children: #9 interface + no-op, #10 macOS provider, #11
merge engine, #12 settings UI).
