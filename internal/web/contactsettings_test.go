package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/contacts"
	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// fakeResolver is a contacts.Resolver test double that answers a fixed
// availability and no people — enough to drive the address-book toggle's
// tri-state rendering without the macOS provider (#10).
type fakeResolver struct{ avail contacts.Availability }

func (f fakeResolver) Availability(context.Context) contacts.Availability { return f.avail }
func (f fakeResolver) Resolve(context.Context, contacts.Identifier) ([]contacts.Person, error) {
	return nil, nil
}
func (f fakeResolver) People(context.Context) ([]contacts.Person, error) { return nil, nil }

// contactPOST builds a same-origin (or given-origin) form POST to a
// /settings/contacts/* route with the given token and fields, mirroring
// llmPOST. Repeatable fields (the split "move" checkboxes) are passed via
// multi.
func contactPOST(t *testing.T, srv *Server, path, origin, token string, fields map[string]string, multi map[string][]string) *httptest.ResponseRecorder {
	t.Helper()
	form := url.Values{}
	for k, v := range fields {
		form.Set(k, v)
	}
	for k, vs := range multi {
		for _, v := range vs {
			form.Add(k, v)
		}
	}
	if token != "" {
		form.Set(setupTokenField, token)
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// contactIDForConv reads the contact id linked to a conversation directly from
// the store, so a test can drive the merge/split handlers with real ids.
func contactIDForConv(t *testing.T, st *store.Store, convID int64) int64 {
	t.Helper()
	var id int64
	if err := st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE id = ?`, convID).Scan(&id); err != nil {
		t.Fatalf("contact id of conv %d: %v", convID, err)
	}
	return id
}

func countContacts(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM contacts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestContactsTabRenders: the tab renders the three cards with the current
// rules reflected as checked toggles.
func TestContactsTabRenders(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	// Default rules: phone+email+address-book on, auto-merge off.
	if err := st.SetMergeRules(ctx, store.MergeRules{MatchPhone: true, MatchEmail: false, UseAddressBook: true, AutoMerge: true}); err != nil {
		t.Fatal(err)
	}

	body := get(t, srv, "/settings/contacts").Body.String()
	if !contains(body, `href="/settings/contacts" class="settings-tab settings-tab-active"`) {
		t.Error("Contacts tab not active on its own page")
	}
	for _, want := range []string{
		`name="match_phone"`, `name="match_email"`, `name="use_address_book"`, `name="auto_merge"`,
		"Matching rules", "Suggested merges", "Merged contacts",
	} {
		if !contains(body, want) {
			t.Errorf("Contacts tab missing %q", want)
		}
	}
	// The persisted rules are reflected: phone + auto-merge checked, email not.
	phoneIdx := strings.Index(body, `name="match_phone"`)
	emailIdx := strings.Index(body, `name="match_email"`)
	if !strings.Contains(body[phoneIdx:phoneIdx+40], "checked") {
		t.Error("match_phone should render checked")
	}
	if strings.Contains(body[emailIdx:emailIdx+40], "checked") {
		t.Error("match_email should render unchecked")
	}
	// The forms are armed with a live setup token.
	if !contains(body, `name="setup_token"`) {
		t.Error("Contacts forms missing the setup token")
	}
}

// TestContactsRulesRoundTrip: a save POST persists exactly the submitted
// toggles and the re-rendered tab shows the saved banner.
func TestContactsRulesRoundTrip(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	tok := mintToken(t, srv)
	rec := contactPOST(t, srv, "/settings/contacts/rules", selfOrigin, tok, map[string]string{
		"auto_merge":       "1",
		"match_phone":      "1",
		"match_email":      "1",
		"use_address_book": "1", // resolver is Unavailable (Absent) → this is IGNORED
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !contains(rec.Body.String(), "Merge rules saved.") {
		t.Error("missing the saved banner")
	}

	got, err := st.GetMergeRules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// auto/phone/email applied from the form; address book preserved (not read
	// from the disabled control) — its stored default is true.
	if !got.AutoMerge || !got.MatchPhone || !got.MatchEmail {
		t.Errorf("saved rules = %+v, want auto/phone/email on", got)
	}
	if !got.UseAddressBook {
		t.Errorf("use_address_book should be preserved (default true) when the resolver is unavailable, got %+v", got)
	}

	// Toggle two off and confirm they persist as off.
	tok = mintToken(t, srv)
	contactPOST(t, srv, "/settings/contacts/rules", selfOrigin, tok, map[string]string{
		"match_phone": "1", // only phone
	}, nil)
	got, _ = st.GetMergeRules(ctx)
	if got.AutoMerge || got.MatchEmail {
		t.Errorf("unchecked toggles should persist off, got %+v", got)
	}
	if !got.MatchPhone {
		t.Errorf("match_phone should persist on, got %+v", got)
	}
}

// TestContactsRulesAddressBookHonoredWhenAvailable: with an Available resolver
// the address-book toggle IS read from the form.
func TestContactsRulesAddressBookHonoredWhenAvailable(t *testing.T) {
	srv, st, _ := newTestServer(t)
	srv.SetContactResolver(fakeResolver{avail: contacts.Available})
	ctx := context.Background()
	// Seed it ON, then submit a form WITHOUT the checkbox → must turn OFF.
	if err := st.SetMergeRules(ctx, store.MergeRules{UseAddressBook: true, MatchPhone: true}); err != nil {
		t.Fatal(err)
	}
	tok := mintToken(t, srv)
	contactPOST(t, srv, "/settings/contacts/rules", selfOrigin, tok, map[string]string{"match_phone": "1"}, nil)
	got, _ := st.GetMergeRules(ctx)
	if got.UseAddressBook {
		t.Errorf("available address book toggle should be read from the form (off), got %+v", got)
	}
}

// TestContactsMergeAction: a merge POST unifies two contacts and re-renders
// with the merged banner.
func TestContactsMergeAction(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	sig, err := st.UpsertConversation(ctx, source.Signal, "+15557770001")
	if err != nil {
		t.Fatal(err)
	}
	im, err := st.UpsertConversation(ctx, source.IMessage, "+15557770002")
	if err != nil {
		t.Fatal(err)
	}
	a, b := contactIDForConv(t, st, sig), contactIDForConv(t, st, im)
	before := countContacts(t, st)

	tok := mintToken(t, srv)
	rec := contactPOST(t, srv, "/settings/contacts/merge", selfOrigin, tok, map[string]string{
		"a": strconv.FormatInt(a, 10),
		"b": strconv.FormatInt(b, 10),
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !contains(rec.Body.String(), "Contacts merged.") {
		t.Error("missing the merged banner")
	}
	if countContacts(t, st) != before-1 {
		t.Errorf("contacts = %d, want one fewer than %d after merge", countContacts(t, st), before)
	}
}

// TestContactsMergeSameRejected: merging a contact with itself is the "same"
// banner and changes nothing.
func TestContactsMergeSameRejected(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	sig, _ := st.UpsertConversation(ctx, source.Signal, "+15557770003")
	a := contactIDForConv(t, st, sig)
	before := countContacts(t, st)

	tok := mintToken(t, srv)
	rec := contactPOST(t, srv, "/settings/contacts/merge", selfOrigin, tok, map[string]string{
		"a": strconv.FormatInt(a, 10),
		"b": strconv.FormatInt(a, 10),
	}, nil)
	if !contains(rec.Body.String(), "Nothing to merge.") {
		t.Error("missing the same-contact banner")
	}
	if countContacts(t, st) != before {
		t.Error("a same-contact merge must not change the contact count")
	}
}

// TestContactsSplitAction: a split POST separates the chosen identifier onto a
// new contact and re-renders with the split banner.
func TestContactsSplitAction(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()

	sig, _ := st.UpsertConversation(ctx, source.Signal, "MJ")
	im, _ := st.UpsertConversation(ctx, source.IMessage, "+15557770004")
	a, b := contactIDForConv(t, st, sig), contactIDForConv(t, st, im)
	winner, err := st.MergeContacts(ctx, a, b)
	if err != nil {
		t.Fatal(err)
	}
	before := countContacts(t, st)

	tok := mintToken(t, srv)
	rec := contactPOST(t, srv, "/settings/contacts/split", selfOrigin, tok,
		map[string]string{"contact_id": strconv.FormatInt(winner, 10)},
		map[string][]string{"move": {source.IMessage + ":+15557770004"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !contains(rec.Body.String(), "Contact split.") {
		t.Error("missing the split banner")
	}
	if countContacts(t, st) != before+1 {
		t.Errorf("contacts = %d, want one more than %d after split", countContacts(t, st), before)
	}
}

// TestContactsSplitNoSelectionInvalid: a split with no moved identifiers is the
// "invalid" banner and changes nothing.
func TestContactsSplitNoSelectionInvalid(t *testing.T) {
	srv, st, _ := newTestServer(t)
	ctx := context.Background()
	sig, _ := st.UpsertConversation(ctx, source.Signal, "MJ2")
	im, _ := st.UpsertConversation(ctx, source.IMessage, "+15557770005")
	winner, _ := st.MergeContacts(ctx, contactIDForConv(t, st, sig), contactIDForConv(t, st, im))
	before := countContacts(t, st)

	tok := mintToken(t, srv)
	rec := contactPOST(t, srv, "/settings/contacts/split", selfOrigin, tok,
		map[string]string{"contact_id": strconv.FormatInt(winner, 10)}, nil)
	if !contains(rec.Body.String(), "Nothing was split.") {
		t.Error("missing the no-selection banner")
	}
	if countContacts(t, st) != before {
		t.Error("a no-selection split must not change the contact count")
	}
}

// TestContactsAddressBookDisabledWhenAbsent: with the default (Unavailable)
// resolver the address-book toggle renders disabled with the absent note.
func TestContactsAddressBookDisabledWhenAbsent(t *testing.T) {
	srv, _, _ := newTestServer(t) // no resolver wired → contacts.Unavailable → Absent

	body := get(t, srv, "/settings/contacts").Body.String()
	idx := strings.Index(body, `name="use_address_book"`)
	if idx < 0 {
		t.Fatal("address-book toggle missing")
	}
	// The input carries the disabled attribute.
	if !strings.Contains(body[idx:idx+80], "disabled") {
		t.Errorf("address-book toggle should be disabled when no address book is present:\n%s", body[idx:idx+80])
	}
	if !contains(body, "No address book is available on this machine") {
		t.Error("missing the absent address-book note")
	}
}

// TestContactsAddressBookNeedsPermission: a NeedsPermission resolver renders
// the grant-permission affordance, distinct from the absent state, and still
// disables the toggle.
func TestContactsAddressBookNeedsPermission(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetContactResolver(fakeResolver{avail: contacts.NeedsPermission})

	body := get(t, srv, "/settings/contacts").Body.String()
	idx := strings.Index(body, `name="use_address_book"`)
	if idx < 0 || !strings.Contains(body[idx:idx+80], "disabled") {
		t.Error("address-book toggle should be disabled when permission is missing")
	}
	if !contains(body, "grant Contacts access") {
		t.Error("missing the needs-permission grant affordance")
	}
	if contains(body, "No address book is available on this machine") {
		t.Error("needs-permission must not render the absent note")
	}
}

// TestContactsAddressBookAvailableEnabled: an Available resolver leaves the
// toggle interactive (no disabled attribute) and shows no absent/permission note.
func TestContactsAddressBookAvailableEnabled(t *testing.T) {
	srv, _, _ := newTestServer(t)
	srv.SetContactResolver(fakeResolver{avail: contacts.Available})

	body := get(t, srv, "/settings/contacts").Body.String()
	idx := strings.Index(body, `name="use_address_book"`)
	if idx < 0 {
		t.Fatal("address-book toggle missing")
	}
	if strings.Contains(body[idx:idx+80], "disabled") {
		t.Error("address-book toggle should be interactive when the address book is available")
	}
	if contains(body, "grant Contacts access") || contains(body, "No address book is available on this machine") {
		t.Error("available address book must render no absent/permission note")
	}
}

// TestContactsBoostedPartial: an HX-Request gets the *_content partial —
// <title> + #main-content, no full document shell (REQ-0008-006).
func TestContactsBoostedPartial(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := getPartial(t, srv, "/settings/contacts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !contains(body, `id="main-content"`) || !contains(body, "<title>") {
		t.Error("boosted response missing the swap unit")
	}
	if contains(body, "<!doctype html") || contains(body, "<html") {
		t.Error("boosted response carried the full document shell")
	}
}

// TestContactsPOSTCrossOriginRejected: every mutating POST is rejected 403 from
// a cross origin and mutates nothing (the checkSetupPOST contract).
func TestContactsPOSTCrossOriginRejected(t *testing.T) {
	for _, path := range []string{"/settings/contacts/rules", "/settings/contacts/merge", "/settings/contacts/split"} {
		t.Run(path, func(t *testing.T) {
			srv, st, _ := newTestServer(t)
			ctx := context.Background()
			// Seed a distinctive rule so we can prove nothing changed.
			if err := st.SetMergeRules(ctx, store.MergeRules{MatchPhone: true, AutoMerge: false}); err != nil {
				t.Fatal(err)
			}
			tok := mintToken(t, srv) // a VALID token — the origin check alone must reject
			rec := contactPOST(t, srv, path, "http://evil.example", tok, map[string]string{
				"auto_merge": "1", "a": "1", "b": "2", "contact_id": "1",
			}, map[string][]string{"move": {source.Signal + ":x"}})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("cross-origin POST status = %d, want 403", rec.Code)
			}
			got, _ := st.GetMergeRules(ctx)
			if got.AutoMerge {
				t.Error("a rejected POST must not have persisted rules")
			}
		})
	}
}

// TestContactsPOSTMissingTokenRejected: same-origin but tokenless → 403.
func TestContactsPOSTMissingTokenRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	rec := contactPOST(t, srv, "/settings/contacts/rules", selfOrigin, "", map[string]string{"auto_merge": "1"}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing-token POST status = %d, want 403", rec.Code)
	}
}
