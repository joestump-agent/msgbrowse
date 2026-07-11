---
status: draft
date: 2026-07-11
implements: [ADR-0022]
requires: [SPEC-0001]
---

# SPEC-0015: Contact merging & de-duplication

- **Capability:** contact-merge
- **Target packages:** `internal/contacts` (new), `internal/store` (`schema.go`
  v11, merge/reconcile methods), `internal/web` (`SetContactResolver`, settings
  merge section), desktop/cli wiring
- **Related ADRs:** [ADR-0022 (contact merging & address-book abstraction)](../../../adr/0022-contact-merging-and-address-book-abstraction.md),
  [ADR-0003 (dual-source archive)](../../../adr/0003-dual-source-archive.md),
  [ADR-0011 (contact facts extraction)](../../../adr/0011-contact-facts-extraction.md),
  [ADR-0010 (security & privacy posture)](../../../adr/0010-security-privacy-posture.md)
- **Tracking:** epic #8 (#9 interface + no-op, #10 macOS provider, #11 merge
  engine, #12 settings UI; ADR/spec: #13)

## Overview

msgbrowse lets the user merge the same real person's identities across
providers (Signal, iMessage, WhatsApp, …) into one canonical contact, split
wrongly-merged ones, and control how candidates are detected — optionally
assisted by the native macOS address book through a pluggable, injected resolver
(the `contacts.Resolver` interface, wired via `SetContactResolver`). Merging MUST default to user-confirmed suggestions (never
silent heuristics, per [ADR-0003](../../../adr/0003-dual-source-archive.md)),
manual decisions MUST survive re-ingest and source disable/re-enable, the
address book MUST be optional on every platform (a no-op on Linux), and nothing
in this capability may perform network egress.

## Requirements

### REQ-0015-001: Pluggable address-book resolver with a safe absent state

There MUST be a resolver interface — the Go identifier is `contacts.Resolver`
(package `contacts`), wired into the web layer via `SetContactResolver` — in a
pure-Go package (no cgo) exposing: a tri-state availability (absent / needs-permission / available),
enumeration of address-book people with their identifiers, and lookup of the
people matching one normalized identifier. A default no-op implementation MUST
report absent, return empty results, and never return an error, so that every
consumer works unchanged without an address book. Resolver implementations MUST
be read-only over the address book and MUST NOT perform network I/O.

#### Scenario: No address book, merge still works
- **Given** a build with only the no-op resolver
- **When** merge candidates are computed and a manual merge is performed
- **Then** matching runs on stored identifiers alone, no error is raised, and the address-book hint surface reports itself absent.

### REQ-0015-002: Injection seam mirroring the existing `Set…` contract

The resolver MUST be wired into the web layer via a `Set…` method on
`web.Server` (the `SetDetector`/`SetEnabler`/`SetPairingSource` contract:
called after `NewServer` and before serving begins; handlers read the field
without locking; unset renders a documented absent state). The merge engine
MUST receive the same resolver instance at construction in the cli/desktop
wiring. The web layer MUST NOT import any cgo or platform-specific package to
consume the resolver.

#### Scenario: Unwired resolver renders the absent state
- **Given** a server with no resolver wired
- **When** the merge settings section renders
- **Then** the address-book option appears in its disabled/absent state and every other merge control still functions.

### REQ-0015-003: macOS provider is build-gated and permission-graceful

The macOS Contacts provider MUST be excluded from default builds by a build tag
(with a paired stub supplying the no-op constructor, following the
`devicesync` gating precedent), so `CGO_ENABLED=0` builds and CI never link the
Contacts framework. When Contacts access is denied or undetermined (TCC), the
provider MUST behave like the no-op for results while reporting
needs-permission, consistent with the `internal/setup` permission-probe model;
a permission failure MUST NOT error the merge path.

#### Scenario: Permission denied degrades to hints-off
- **Given** the macOS provider with Contacts access denied
- **When** candidates are computed and settings render
- **Then** matching proceeds without address-book hints, no operation errors, and the UI shows a needs-permission state for the address-book option.

### REQ-0015-004: Normalized identifier matching produces explained candidates

Candidate detection MUST group contacts whose identifiers are equal after
normalization — phone numbers to E.164, emails lowercased — across sources, and
MAY add candidates from address-book grouping (two stored identifiers on one
address-book person) when a resolver is available and enabled. Normalization
MUST be pure Go, shared between matcher and providers, and unit-testable
without any framework. Every candidate MUST carry a machine-readable reason
(which identifier matched, or which address-book person grouped them).
Candidates MUST NOT be silently merged by default.

#### Scenario: Cross-source phone match is suggested, not applied
- **Given** `signal:+1 (555) 123-4567` and `imessage:+15551234567` on two contacts, default rules
- **When** candidates are computed
- **Then** the pair is listed as a candidate with the matching E.164 value as its reason, and the contacts remain unmerged.

### REQ-0015-005: Manual merge unions the person

A manual merge of two contacts MUST repoint the loser's
`contact_identifiers`, `conversations.contact_id`, and `contact_facts` rows to
the winner (facts deduplicating via the existing `UNIQUE(contact_id,
fact_hash)`), delete the loser's `contacts` row, and record the decision (see
REQ-0015-007). After the merge, every conversation of either former contact
MUST render the same person and one deduplicated fact set
([ADR-0011](../../../adr/0011-contact-facts-extraction.md)).

#### Scenario: Merged threads share one fact set
- **Given** two contacts each holding a conversation and an identical extracted fact
- **When** the user merges them
- **Then** both conversations link to the surviving contact and the fact appears once.

### REQ-0015-006: Manual split separates identifiers

The user MUST be able to split chosen identifiers off a contact onto a new
contact. The split MUST record that the separated identifier pairs stay apart
(see REQ-0015-007), and conversations MUST follow their identifiers to the new
contact. A pair holds one current decision: manually merging a previously-split
pair MUST replace the split record, and splitting a previously-merged pair MUST
replace the merge record — the latest manual action wins.

#### Scenario: Split pair is not re-suggested by auto-match
- **Given** a contact split into two, whose identifiers share a normalized phone value, with auto-merge enabled
- **When** reconcile and candidate detection run
- **Then** the pair is neither auto-merged nor re-applied by any stored decision, because the split record takes precedence.

### REQ-0015-007: Overrides persist across re-ingest, keyed by stable identifiers

Merge and split decisions MUST be persisted keyed by canonical-ordered
`(source, identifier)` pairs — never by contact rowids — with a uniqueness
constraint on the pair, an origin (`manual` | `auto`), and no foreign key to
`contact_identifiers` (a link whose identifier is absent is inert, not
invalid). A merge MUST record the full bipartite pairing of the two contacts'
identifiers. An idempotent reconcile pass MUST re-apply decisions after every
import and on demand, with precedence **manual split > manual merge > auto
rules**, and a deterministic winner chosen by an explicit ordered rule: (1) if
exactly one contact has a user-meaningful `display_name` (differs from all its
identifiers) that contact wins; (2) otherwise (both or neither user-meaningful)
the lower `id` wins. Disabling a source and
re-importing it MUST converge back to the merged state without user action.

#### Scenario: Re-ingest cannot clobber a manual merge
- **Given** a manual merge of `signal:MJ` and `imessage:+15551234567`, then the iMessage source disabled (its identifiers and orphaned contacts pruned) and re-imported
- **When** the post-import reconcile runs
- **Then** the re-created iMessage identity is folded back onto the surviving contact, and no duplicate person remains.

### REQ-0015-008: Merge rules are persisted settings with safe defaults

Merge rules MUST persist in the store (a single-row settings table owned by
migration v11) and MUST cover at least: auto-merge on/off (default **off** —
suggestions only), which identifier kinds to auto-match (phone, email), and
whether to use the address book as a hint source. Enabled auto-merge MUST apply
only to exact normalized-identifier equality on the trusted kinds and MUST
record applied merges as `auto`-origin decisions; address-book hints MUST NOT
auto-merge under any setting. With default rules, behavior differs from
pre-merge msgbrowse only by the presence of suggestions.

#### Scenario: Opt-in auto-merge applies and persists
- **Given** auto-merge enabled for phone identifiers and two contacts sharing an E.164 value
- **When** reconcile runs
- **Then** the contacts merge, the decision is recorded with origin `auto`, and a later re-ingest converges to the same merged state.

### REQ-0015-009: Settings UI for candidates, merge/split, and rules

The settings surface MUST let the user: review candidates with their reasons,
merge a candidate pair, split a contact, and edit the merge rules. It MUST be
rendered via the established web patterns — boosted partial with the
`*_content` template owning its `<title>` (SPEC-0008), CSP-safe with no inline
JS, fixed-enum banner states for POST results (the `PairResult`/`UnpairResult`
pattern), and state-changing actions as same-origin POSTs. The address-book
option MUST reflect resolver availability (absent / needs-permission /
available). Merge and split MUST be presented as explicit, confirmable actions.

#### Scenario: Merge action confirms and reports through a banner
- **Given** a listed candidate pair
- **When** the user confirms a merge
- **Then** the POST performs the merge, the page re-renders with a fixed-enum success banner naming the surviving contact, and the candidate disappears from the list.

### REQ-0015-010: Local-only, no egress, no persisted address book

No operation in this capability may perform network I/O
([ADR-0010](../../../adr/0010-security-privacy-posture.md)): matching,
reconcile, merge, split, and address-book lookups MUST all be local.
Address-book data MUST be consulted live and MUST NOT be persisted into the
msgbrowse database beyond user-confirmed effects (e.g. a chosen display name).

#### Scenario: Reconcile makes no network calls
- **Given** any combination of rules, decisions, and resolver availability
- **When** import-time reconcile and candidate detection run
- **Then** no network connection is opened, and no address-book records are written to the database.
