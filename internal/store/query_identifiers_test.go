package store

import (
	"context"
	"testing"

	"github.com/joestump/msgbrowse/internal/source"
)

// mergeContacts repoints all of conversation `loser`'s contact identifiers and
// the conversation itself onto conversation `keeper`'s contact, simulating a
// manual contacts-page identity reconciliation across sources.
func mergeContacts(t *testing.T, st *Store, keeper, loser int64) {
	t.Helper()
	var keepContact, loseContact int64
	if err := st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE id = ?`, keeper).Scan(&keepContact); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE id = ?`, loser).Scan(&loseContact); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE contact_identifiers SET contact_id = ? WHERE contact_id = ?`, keepContact, loseContact); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE conversations SET contact_id = ? WHERE id = ?`, keepContact, loser); err != nil {
		t.Fatal(err)
	}
}

// TestConversationIdentifiers covers identifier (phone/email/handle) metadata:
// a conversation's linked contact may carry cross-source handles (e.g. an
// iMessage phone merged onto a Signal contact). Only the conversation's own
// (source, name) identity is excluded so it isn't echoed back, while genuine
// cross-source handles — even ones that share the conversation's name — are
// surfaced.
func TestConversationIdentifiers(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	t.Run("own identity excluded, merged handle surfaced", func(t *testing.T) {
		conv, err := st.UpsertConversation(ctx, source.Signal, "MJ")
		if err != nil {
			t.Fatal(err)
		}
		// Before any merge: only the bootstrap (signal, "MJ") identifier exists,
		// and it is the conversation's own identity, so nothing extra shows.
		if ids, err := st.ConversationIdentifiers(ctx, conv); err != nil || len(ids) != 0 {
			t.Fatalf("pre-merge identifiers = %+v (err=%v), want none", ids, err)
		}
		var contactID int64
		if err := st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE id = ?`, conv).Scan(&contactID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(
			`INSERT INTO contact_identifiers(contact_id, source, identifier) VALUES (?, ?, ?)`,
			contactID, source.IMessage, "+15551234567"); err != nil {
			t.Fatal(err)
		}
		ids, err := st.ConversationIdentifiers(ctx, conv)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 || ids[0].Source != source.IMessage || ids[0].Identifier != "+15551234567" {
			t.Errorf("identifiers = %+v, want [{imessage +15551234567}]", ids)
		}
	})

	t.Run("merged conversations each see the other's identity", func(t *testing.T) {
		sig, err := st.UpsertConversation(ctx, source.Signal, "Alex")
		if err != nil {
			t.Fatal(err)
		}
		im, err := st.UpsertConversation(ctx, source.IMessage, "+15557654321")
		if err != nil {
			t.Fatal(err)
		}
		mergeContacts(t, st, sig, im)

		// The Signal conversation surfaces the iMessage handle, not its own name.
		ids, err := st.ConversationIdentifiers(ctx, sig)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 || ids[0].Source != source.IMessage || ids[0].Identifier != "+15557654321" {
			t.Errorf("signal-side identifiers = %+v, want [{imessage +15557654321}]", ids)
		}
		// The iMessage conversation surfaces the Signal name, not its own number.
		ids, err = st.ConversationIdentifiers(ctx, im)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 || ids[0].Source != source.Signal || ids[0].Identifier != "Alex" {
			t.Errorf("imessage-side identifiers = %+v, want [{signal Alex}]", ids)
		}
	})

	t.Run("same-valued cross-source handle is not collaterally hidden", func(t *testing.T) {
		// A phone-named Signal conversation merged with the same number on
		// iMessage. The exclusion must drop only the Signal (own) row, not the
		// equally-valued iMessage row — otherwise the badge silently vanishes.
		conv, err := st.UpsertConversation(ctx, source.Signal, "+15550001111")
		if err != nil {
			t.Fatal(err)
		}
		var contactID int64
		if err := st.DB().QueryRow(`SELECT contact_id FROM conversations WHERE id = ?`, conv).Scan(&contactID); err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(
			`INSERT INTO contact_identifiers(contact_id, source, identifier) VALUES (?, ?, ?)`,
			contactID, source.IMessage, "+15550001111"); err != nil {
			t.Fatal(err)
		}
		ids, err := st.ConversationIdentifiers(ctx, conv)
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 || ids[0].Source != source.IMessage || ids[0].Identifier != "+15550001111" {
			t.Errorf("identifiers = %+v, want [{imessage +15550001111}] (same-valued other-source row kept)", ids)
		}
	})

	t.Run("conversation with no linked contact returns nil", func(t *testing.T) {
		conv, err := st.UpsertConversation(ctx, source.Signal, "GroupChat")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.DB().Exec(`UPDATE conversations SET contact_id = NULL WHERE id = ?`, conv); err != nil {
			t.Fatal(err)
		}
		ids, err := st.ConversationIdentifiers(ctx, conv)
		if err != nil {
			t.Fatal(err)
		}
		if ids != nil {
			t.Errorf("identifiers = %+v, want nil for a contactless conversation", ids)
		}
	})
}
