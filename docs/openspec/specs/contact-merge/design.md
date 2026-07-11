---
status: draft
date: 2026-07-11
implements: [ADR-0022]
---

# SPEC-0015 Design: Contact merging & de-duplication

- **Capability:** contact-merge
- **Related ADRs:** [ADR-0022](../../../adr/0022-contact-merging-and-address-book-abstraction.md),
  [ADR-0003](../../../adr/0003-dual-source-archive.md),
  [ADR-0011](../../../adr/0011-contact-facts-extraction.md),
  [ADR-0010](../../../adr/0010-security-privacy-posture.md)
- **Tracking:** epic #8; built by #9 (interface + no-op), #10 (macOS provider),
  #11 (merge engine), #12 (settings UI)

## Architecture

The abstraction and matching logic are pure Go in `internal/contacts`;
persistence and the merge/reconcile transactions are store methods; the web
layer consumes both through the existing seam style. Only the macOS provider
touches cgo, and only tagged builds contain it.

```
cli / desktop wiring ──▶ contacts.Resolver (no-op | macOS provider, build-tagged)
        │                      │
        │                      ├──▶ web.Server.SetContactResolver (availability,
        │                      │        hint state for settings)
        │                      └──▶ merge engine construction
        │
internal/contacts ── Identifier normalization (E.164, lowercase email)
        │            Candidates(storedIdentifiers, resolverPeople, rules)
        │
internal/store ───── schema v11: contact_links, contact_merge_rules
        │            MergeContacts / SplitContact (transactional)
        │            ReconcileContacts (idempotent, post-import + on demand)
        │            MergeCandidates, GetMergeRules / SetMergeRules
        │
internal/web ─────── settings merge section (boosted partial, CSP-safe,
                     fixed-enum result banners) → store methods
```

## Key design decisions

### Decisions are identifier-keyed rows, not a pointer graph

`contact_links` stores each merge/split decision against canonical-ordered
`(source_a, identifier_a, source_b, identifier_b)` tuples with
`UNIQUE` on the pair. This is the codebase's standard answer to rowid churn:
embeddings (v3), contact facts (v4), and reactions (v6) all key durable state
by stable message hashes because `ReplaceConversationMessages` reassigns
rowids on every import; contact rowids churn the same way under
`DeleteSourceData` + re-enable, while `(source, identifier)` is the stable
identity (`UNIQUE(source, identifier)` in `contact_identifiers` since v2).
There is deliberately no FK from links to `contact_identifiers` — an absent
identifier makes a link inert, and it re-activates when its source is
re-imported, exactly like a fact whose message hash has vanished simply loses
its jump link.

Canonical pair ordering (`(source_a, identifier_a) < (source_b,
identifier_b)`) makes symmetric decisions collide on the UNIQUE constraint,
and "one current decision per pair" (a manual merge of a split pair replaces
the split row and vice versa) keeps the table free of contradictions without
timestamp arbitration. A merge records the full bipartite pairing of both
contacts' identifier sets so that any surviving pair re-links the group after
a partial source deletion.

### `contacts` stays the canonical person

Merging repoints `contact_identifiers.contact_id`,
`conversations.contact_id`, and `contact_facts.contact_id` to the winner and
deletes the loser — the mechanics [ADR-0003](../../../adr/0003-dual-source-archive.md)
§Consequences anticipated. A `canonical_id` alias column or a second person
table was rejected (ADR-0022 Considered Options): every existing query joins
`contacts(id)` directly, and an alias hop would tax all of them to solve a
problem the identifier-keyed journal solves better. Fact repointing rides the
existing dedup: `UPDATE OR IGNORE contact_facts SET contact_id = :winner WHERE
contact_id = :loser` moves unique facts, and deleting the loser contact
cascades away the duplicates the IGNORE left behind (`ON DELETE CASCADE` from
v4). All of this happens in one transaction per merge.

### Reconcile is an idempotent post-import pass

`UpsertConversation`'s get-or-create stays untouched: mid-import it may
resurrect an unmerged contact for a re-imported identifier, and the reconcile
pass folds it back immediately afterwards. Reconcile:

1. applies every `merge` link whose two identifiers currently sit on
   different contacts (union with the deterministic winner rule, applied as an
   explicit ordered rule: (1) if exactly one contact has a user-meaningful
   `display_name` — differs from all of its identifiers, i.e. was user-edited
   or address-book-derived rather than auto-created — that contact wins;
   (2) otherwise, when both or neither is user-meaningful, the lower `id` wins);
2. if rules enable auto-merge, merges exact normalized matches on trusted
   kinds, skipping any pair carrying a `split` link (precedence: manual split
   > manual merge > auto rules) and recording each applied merge as an
   `origin='auto'` link so it, too, survives the next churn.

Running it twice is a no-op by construction (all decisions applied → no
identifiers on different contacts remain for any link). It runs after every
import and on demand from settings; it performs no I/O beyond SQLite and the
optional in-memory resolver snapshot, so hooking it to the import path adds no
egress and needs no opt-in ceremony (unlike `facts`/`embed`, whose
deliberate-step rule exists because they call the LLM).

### The resolver seam and the no-op default

`contacts.Resolver` is minimal by demand of the matcher: availability,
`People` (batch matching), `LookupIdentifier` (spot checks / UI hints). The
no-op returns absent + empty + nil-error, so call sites carry no platform
conditionals; the merge engine treats "no resolver" and "resolver with no
matches" identically. Web wiring is `SetContactResolver` with the established
contract (wire after `NewServer`, before serving; unset renders the absent
state) because the web layer cannot import the cgo desktop module — the same
constraint that produced `SetDetector`/`SetEnabler`/`SetPairingSource`
(`internal/web/enable.go`, `internal/web/settings.go`).

### Platform gating follows the `devicesync` precedent

The macOS provider compiles under `//go:build darwin && macontacts` with a
paired stub for every other build, mirroring
`internal/cli/serve_devicesync.go` / `serve_nodevicesync.go` (#20): the
default `CGO_ENABLED=0 make check` build never links the Contacts framework,
and the desktop shell builds with the tag and wires the real provider.
TCC permission state maps onto the `internal/setup` detector model:
needs-permission is a rendered state, never an error. The framework call is a
thin adapter; normalization and mapping logic live in pure Go and are the
CI-tested surface (native Contacts cannot run in CI).

### Rules live in the store, not the config file

`contact_merge_rules` is a single-row table because the settings UI is the
owner of these values and the web layer has no config-file write path
(inventing one for three booleans would be a new, worse seam). Config keys
stay the domain of operator-set, restart-scoped concerns. Defaults are
conservative: auto-merge off, phone+email trusted once enabled, address-book
hints on (they only ever *suggest*, and the provider is permission-gated
anyway).

## Schema (migration v11)

```sql
CREATE TABLE contact_links (
    id           INTEGER PRIMARY KEY,
    kind         TEXT    NOT NULL,          -- 'merge' | 'split'
    origin       TEXT    NOT NULL,          -- 'manual' | 'auto'
    source_a     TEXT    NOT NULL,
    identifier_a TEXT    NOT NULL,
    source_b     TEXT    NOT NULL,
    identifier_b TEXT    NOT NULL,
    created_at   TEXT    NOT NULL,
    UNIQUE(source_a, identifier_a, source_b, identifier_b)
);
CREATE INDEX idx_contact_links_a ON contact_links(source_a, identifier_a);
CREATE INDEX idx_contact_links_b ON contact_links(source_b, identifier_b);

CREATE TABLE contact_merge_rules (
    id               INTEGER PRIMARY KEY CHECK (id = 1),
    auto_merge       INTEGER NOT NULL DEFAULT 0,
    match_phone      INTEGER NOT NULL DEFAULT 1,
    match_email      INTEGER NOT NULL DEFAULT 1,
    use_address_book INTEGER NOT NULL DEFAULT 1,
    updated_at       TEXT    NOT NULL
);
```

Both tables are node-local derived-decision state; on device-sync replicas
([ADR-0021](../../../adr/0021-syncthing-sync-engine.md)) each node keeps its
own (the database never syncs), and reconcile makes any node converge from its
own decisions.

## Risks / trade-offs

- **Deleted loser ids can dangle in external references** (e.g. a bookmark to
  a hypothetical contact-by-id URL — no such route exists today). Accepted: the
  winner rule keeps the longest-lived/user-curated row, and contact ids are not
  a stability contract today.
- **Bipartite link fan-out** is O(n·m) per merge. At address-book scale
  (hundreds of contacts, a handful of identifiers each) this is trivial, and
  it buys re-linking from any surviving pair.
- **Group conversations are out of scope**: `conversations.contact_id` is NULL
  for groups (ADR-0003); merging affects 1:1 threads and per-contact data
  only. Group-member identity mapping is a future capability.
- **Auto-merge, even opt-in, can still be wrong** (shared/recycled numbers).
  Mitigated by: off by default, split-link precedence, and split being a
  first-class recorded undo.

## Testing

- `internal/contacts`: normalization tables (E.164 variants, casing, junk),
  candidate grouping with and without resolver people, reason payloads; a fake
  `Resolver` covering absent / needs-permission / available.
- `internal/store`: merge transaction (identifier/conversation/fact repoint,
  fact dedup on collision, loser deleted), split, decision replacement
  (merge↔split on the same pair), reconcile idempotency (run twice → no-op),
  precedence (split blocks auto and stored merge), re-ingest survival
  (`DeleteSourceData` + re-import + reconcile converges), winner determinism,
  rules round-trip and defaults.
- `internal/web`: seam contract with a fake resolver (absent /
  needs-permission / available renders), settings section render, POST
  merge/split happy path + fixed-enum banners, CSP compliance via the existing
  template test style.
- Build gating: the tagged provider compiles only under
  `darwin && macontacts`; `CGO_ENABLED=0 go build ./...` stays green with the
  stub (CI already enforces this shape for `devicesync`).
