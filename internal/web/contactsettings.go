// The Settings → Contacts tab (issue #12, epic #8): the user-facing surface
// over the cross-provider contact merge engine (#11, ADR-0022 / SPEC-0015). It
// exposes three things:
//
//   - The merge RULES — which identifier kinds to trust for candidates and
//     auto-merge, whether auto-merge is on at all, and whether the native
//     address book contributes hint suggestions — persisted through
//     store.SetMergeRules / loaded through GetMergeRules.
//   - The de-dup CANDIDATE review: MergeCandidates lists the suggested
//     cross-source merges under the current rules, each with a one-click Merge.
//   - The manual OVERRIDES: merge two contacts (MergeContacts) and split a
//     mistakenly-merged person (SplitContact). Both record durable decisions in
//     the engine, so an override survives re-ingest — the handler just drives
//     the engine, it guarantees nothing itself.
//
// The address book is only ever a HINT source, and only when the wired
// resolver reports contacts.Available. When it is Absent (Linux, browser mode,
// no provider) or NeedsPermission (macOS grant missing), the address-book
// toggle renders disabled in the matching absent/needs-permission state — the
// same tri-state the setup permission surface uses — and a save never flips the
// stored preference from that disabled control.
//
// Every mutating POST is privileged (it changes how identities are unified) and
// gated exactly like the Setup POSTs: same-origin + per-session token +
// MaxBytesReader, rejected 403 before any work (checkSetupPOST, SPEC-0013
// §Security). Each re-renders the tab with a fixed-enum result banner — never
// request-derived prose — mirroring the PairResult/UnpairResult contract. The
// render is a boosted partial (SPEC-0008: *_content owns <title>, the cheap
// isPartialRequest path skips the sidebar), and interactive controls are plain
// same-origin forms — no inline JS (CSP script-src 'self').
package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/joestump/msgbrowse/internal/contacts"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// contactSettingsData drives the Contacts tab. The three *Result fields are
// fixed enums mapped to prose by the template (at most one is set per render);
// nothing here carries request-derived text beyond the escaped contact display
// names and identifier values, which html/template escapes like all content.
type contactSettingsData struct {
	baseData
	// Rules mirrors the persisted store.MergeRules for the toggle form.
	Rules store.MergeRules
	// AddressBookState is the wired resolver's tri-state readiness token —
	// "available", "needs-permission", or "absent" (contacts.Availability.String)
	// — driving whether the address-book toggle is live or disabled, and which
	// absent affordance renders beside it.
	AddressBookState string
	// Candidates are the suggested cross-source merges under the current rules.
	Candidates []contactCandidateRow
	// Merged are the multi-identifier contacts the split control can act on.
	Merged []contactMergedRow
	// SetupToken arms every form through the same checkSetupPOST gate as the
	// Setup POSTs.
	SetupToken string
	// RulesResult: "" | "ok" | "error".
	RulesResult string
	// MergeResult: "" | "ok" | "same" | "invalid" | "error".
	MergeResult string
	// SplitResult: "" | "ok" | "invalid" | "error".
	SplitResult string
}

// AddressBookAvailable reports whether the address-book toggle is live (the
// resolver answered Available); the template disables the control otherwise.
func (d contactSettingsData) AddressBookAvailable() bool {
	return d.AddressBookState == contacts.Available.String()
}

// contactCandidateRow is one suggested merge for the review list.
type contactCandidateRow struct {
	A, B         int64
	NameA, NameB string
	// Reason is the engine's match reason token ("phone" / "email" /
	// "address-book"); Value is the shared identifier that matched.
	Reason string
	Value  string
}

// contactMergedRow is one multi-identifier contact the split form works over.
type contactMergedRow struct {
	ID          int64
	Name        string
	Identifiers []contactIdentifierRow
}

// contactIdentifierRow is one movable identifier of a merged contact. Token is
// the form value the split checkbox submits — "source:identifier", parsed back
// with splitIdentifierToken (source is a fixed enum and carries no colon, so a
// split on the first colon is unambiguous even for a handle that contains one).
type contactIdentifierRow struct {
	Source     string
	Identifier string
	Token      string
}

// handleSettingsContacts renders the Contacts tab (GET /settings/contacts):
// the current rules, the address-book state, the live candidate suggestions,
// and the merged-contact review set. Safe GET — no mutation; the minted token
// arms the forms.
func (s *Server) handleSettingsContacts(w http.ResponseWriter, r *http.Request) {
	s.renderContactSettings(w, r, contactSettingsData{})
}

// handleSettingsMergeRules is POST /settings/contacts/rules — persists the
// merge-rules toggles. Gate first (checkSetupPOST), then read the checkboxes.
// The address-book preference is only taken from the form when the resolver is
// Available; otherwise its stored value is preserved so a disabled control can
// never silently clear it.
func (s *Server) handleSettingsMergeRules(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; nothing was persisted
	}
	rules := store.MergeRules{
		AutoMerge:  r.PostFormValue("auto_merge") == "1",
		MatchPhone: r.PostFormValue("match_phone") == "1",
		MatchEmail: r.PostFormValue("match_email") == "1",
	}
	if s.contactResolver().Availability(r.Context()) == contacts.Available {
		rules.UseAddressBook = r.PostFormValue("use_address_book") == "1"
	} else {
		// The toggle was disabled and thus not submitted: keep the stored intent.
		if cur, err := s.store.GetMergeRules(r.Context()); err == nil {
			rules.UseAddressBook = cur.UseAddressBook
		}
	}
	if err := s.store.SetMergeRules(r.Context(), rules); err != nil {
		s.log.Error("merge rules save failed", "error", err)
		s.renderContactSettings(w, r, contactSettingsData{RulesResult: "error"})
		return
	}
	s.log.Info("merge rules saved",
		"auto_merge", rules.AutoMerge, "match_phone", rules.MatchPhone,
		"match_email", rules.MatchEmail, "use_address_book", rules.UseAddressBook)
	s.renderContactSettings(w, r, contactSettingsData{RulesResult: "ok"})
}

// handleSettingsMerge is POST /settings/contacts/merge — the manual merge of
// two contacts (MergeContacts). Gate first; the two ids come from the review
// row's hidden fields. A malformed id is "invalid"; identical ids are "same";
// an engine failure is "error"; success is "ok".
func (s *Server) handleSettingsMerge(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; nothing was merged
	}
	a, errA := strconv.ParseInt(r.PostFormValue("a"), 10, 64)
	b, errB := strconv.ParseInt(r.PostFormValue("b"), 10, 64)
	if errA != nil || errB != nil || a <= 0 || b <= 0 {
		s.renderContactSettings(w, r, contactSettingsData{MergeResult: "invalid"})
		return
	}
	if a == b {
		s.renderContactSettings(w, r, contactSettingsData{MergeResult: "same"})
		return
	}
	winner, err := s.store.MergeContacts(r.Context(), a, b)
	if err != nil {
		s.log.Error("manual contact merge failed", "a", a, "b", b, "error", err)
		s.renderContactSettings(w, r, contactSettingsData{MergeResult: "error"})
		return
	}
	s.log.Info("contacts merged from settings", "a", a, "b", b, "winner", winner)
	s.renderContactSettings(w, r, contactSettingsData{MergeResult: "ok"})
}

// handleSettingsSplit is POST /settings/contacts/split — the manual split of
// chosen identifiers off a merged contact (SplitContact). Gate first; the
// contact id and the moved-identifier tokens come from the merged-row form.
// Bad form input (unparseable id, no selection, unknown source token) is
// "invalid"; an engine failure — including "cannot move every identifier" and
// "identifier is not on this contact" — is "error"; success is "ok".
func (s *Server) handleSettingsSplit(w http.ResponseWriter, r *http.Request) {
	if !s.checkSetupPOST(w, r) {
		return // 403 already written; nothing was split
	}
	contactID, err := strconv.ParseInt(r.PostFormValue("contact_id"), 10, 64)
	if err != nil || contactID <= 0 {
		s.renderContactSettings(w, r, contactSettingsData{SplitResult: "invalid"})
		return
	}
	tokens := r.PostForm["move"]
	moved := make([]store.ContactIdentifier, 0, len(tokens))
	for _, tok := range tokens {
		src, id, ok := splitIdentifierToken(tok)
		if !ok {
			s.renderContactSettings(w, r, contactSettingsData{SplitResult: "invalid"})
			return
		}
		moved = append(moved, store.ContactIdentifier{Source: src, Identifier: id})
	}
	if len(moved) == 0 {
		s.renderContactSettings(w, r, contactSettingsData{SplitResult: "invalid"})
		return
	}
	newID, err := s.store.SplitContact(r.Context(), contactID, moved)
	if err != nil {
		s.log.Error("manual contact split failed", "contact_id", contactID, "error", err)
		s.renderContactSettings(w, r, contactSettingsData{SplitResult: "error"})
		return
	}
	s.log.Info("contact split from settings", "contact_id", contactID, "new_id", newID, "moved", len(moved))
	s.renderContactSettings(w, r, contactSettingsData{SplitResult: "ok"})
}

// splitIdentifierToken parses a split checkbox value "source:identifier" back
// into its parts, validating the source against the fixed enum. The source
// token never contains a colon, so the first colon is the unambiguous
// delimiter even when the identifier (a service handle) contains one.
func splitIdentifierToken(tok string) (src, id string, ok bool) {
	src, id, found := strings.Cut(tok, ":")
	if !found || id == "" || !source.IsKnown(src) {
		return "", "", false
	}
	return src, id, true
}

// renderContactSettings loads the current rules, address-book state, candidate
// suggestions, and merged-contact set, then finishes any Contacts-tab response:
// shell (full or boosted partial), a fresh per-session token, and render. The
// caller supplies only the fixed-enum result banner it wants shown.
func (s *Server) renderContactSettings(w http.ResponseWriter, r *http.Request, data contactSettingsData) {
	const title = "Contacts · msgbrowse"
	if isPartialRequest(r) {
		data.baseData = partialBase(title, 0)
	} else {
		base, err := s.baseData(r.Context(), title, 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
		data.baseData = base
	}

	resolver := s.contactResolver()
	data.AddressBookState = resolver.Availability(r.Context()).String()

	rules, err := s.store.GetMergeRules(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	data.Rules = rules

	cands, err := s.store.MergeCandidates(r.Context(), resolver)
	if err != nil {
		s.serverError(w, err)
		return
	}
	for _, c := range cands {
		data.Candidates = append(data.Candidates, contactCandidateRow{
			A: c.ContactA, B: c.ContactB,
			NameA: c.NameA, NameB: c.NameB,
			Reason: c.Reason, Value: c.Value,
		})
	}

	merged, err := s.store.MergedContacts(r.Context())
	if err != nil {
		s.serverError(w, err)
		return
	}
	for _, mc := range merged {
		row := contactMergedRow{ID: mc.ID, Name: mc.DisplayName}
		for _, ci := range mc.Identifiers {
			row.Identifiers = append(row.Identifiers, contactIdentifierRow{
				Source:     ci.Source,
				Identifier: ci.Identifier,
				Token:      ci.Source + ":" + ci.Identifier,
			})
		}
		data.Merged = append(data.Merged, row)
	}

	tok, err := s.setupTokens.mint()
	if err != nil {
		s.serverError(w, err)
		return
	}
	data.SetupToken = tok

	s.render(w, r, "contactsettings", data)
}
