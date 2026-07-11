package store

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

func seedGalleryCorpus(t *testing.T) (*Store, int64, int64) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()

	harper, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	group, err := st.UpsertConversation(ctx, source.Signal, "Group Trip")
	if err != nil {
		t.Fatal(err)
	}

	img := func(name string) []signal.Attachment {
		return []signal.Attachment{{Kind: signal.KindImage, RelPath: "media/" + name, OriginalName: name}}
	}
	file := func(name string) []signal.Attachment {
		return []signal.Attachment{{Kind: signal.KindFile, RelPath: "media/" + name, OriginalName: name}}
	}
	link := func(u string) []signal.Link { return []signal.Link{{URL: u}} }

	harperMsgs := []signal.Message{
		msg("Harper", "2022-03-01 09:00:00", "Harper", "pic", img("cabin.jpg"), nil),
		msg("Harper", "2022-03-01 09:01:00", "Harper", "doc", file("lease.pdf"), nil),
		msg("Harper", "2022-03-01 09:02:00", "Me", "map", nil, link("https://maps.example.com/a")),
		msg("Harper", "2022-03-02 09:03:00", "Me", "map again", nil, link("https://maps.example.com/a")),
		msg("Harper", "2022-03-02 09:04:00", "Me", "food", nil, link("https://www.yelp.com/biz/foo")),
	}
	if _, err := st.ReplaceConversationMessages(ctx, harper, source.Signal, harperMsgs); err != nil {
		t.Fatal(err)
	}
	groupMsgs := []signal.Message{
		msg("Group Trip", "2022-04-01 18:00:00", "MJ", "sunset", img("sunset.png"), nil),
	}
	if _, err := st.ReplaceConversationMessages(ctx, group, source.Signal, groupMsgs); err != nil {
		t.Fatal(err)
	}
	return st, harper, group
}

func TestListAttachments(t *testing.T) {
	st, harper, _ := seedGalleryCorpus(t)
	ctx := context.Background()

	images, err := st.ListAttachments(ctx, "image", GalleryFilter{}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(images.Items) != 2 { // cabin.jpg + sunset.png
		t.Errorf("images = %d, want 2", len(images.Items))
	}
	// Newest first: sunset (2022-04) before cabin (2022-03).
	if len(images.Items) == 2 && images.Items[0].OriginalName != "sunset.png" {
		t.Errorf("images not newest-first: %+v", images.Items)
	}
	if images.HasMore {
		t.Errorf("HasMore = true for a 2-image corpus")
	}

	files, err := st.ListAttachments(ctx, "file", GalleryFilter{}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(files.Items) != 1 || files.Items[0].OriginalName != "lease.pdf" {
		t.Errorf("files = %+v, want [lease.pdf]", files.Items)
	}

	// Conversation filter: only Harper's image.
	hImages, err := st.ListAttachments(ctx, "image", GalleryFilter{ConversationID: harper}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hImages.Items) != 1 || hImages.Items[0].OriginalName != "cabin.jpg" {
		t.Errorf("harper images = %+v, want [cabin.jpg]", hImages.Items)
	}

	// Date filter: only March images (excludes April sunset).
	marchImages, err := st.ListAttachments(ctx, "image", GalleryFilter{EndUnix: dayUnix(t, "2022-03-31")}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(marchImages.Items) != 1 || marchImages.Items[0].OriginalName != "cabin.jpg" {
		t.Errorf("march images = %+v, want [cabin.jpg]", marchImages.Items)
	}

	// Source filter: imessage has none.
	none, err := st.ListAttachments(ctx, "image", GalleryFilter{Source: source.IMessage}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Items) != 0 {
		t.Errorf("imessage images = %d, want 0", len(none.Items))
	}
}

// TestListAttachmentsSortAsc: the SortAsc filter flips the walk to oldest-first
// (issue #5) — the reverse of the default — and its keyset cursor still pages
// forward without overlap or gaps.
func TestListAttachmentsSortAsc(t *testing.T) {
	st, _, _ := seedGalleryCorpus(t)
	ctx := context.Background()

	// Default is newest-first (sunset 2022-04 before cabin 2022-03).
	desc, err := st.ListAttachments(ctx, "image", GalleryFilter{}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(desc.Items) != 2 || desc.Items[0].OriginalName != "sunset.png" {
		t.Fatalf("default order = %+v, want [sunset.png cabin.jpg]", desc.Items)
	}

	// SortAsc reverses it: cabin (older) first, sunset (newer) last.
	asc, err := st.ListAttachments(ctx, "image", GalleryFilter{SortAsc: true}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(asc.Items) != 2 || asc.Items[0].OriginalName != "cabin.jpg" || asc.Items[1].OriginalName != "sunset.png" {
		t.Errorf("SortAsc order = %+v, want [cabin.jpg sunset.png]", asc.Items)
	}

	// The oldest-first cursor pages forward through the same rows with a limit of
	// 1: page one is cabin, its cursor yields sunset, no overlap.
	f := GalleryFilter{SortAsc: true, Limit: 1}
	p1, err := st.ListAttachments(ctx, "image", f, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Items) != 1 || p1.Items[0].OriginalName != "cabin.jpg" || !p1.HasMore {
		t.Fatalf("asc page 1 = %+v (HasMore=%v), want [cabin.jpg] (true)", p1.Items, p1.HasMore)
	}
	p2, err := st.ListAttachments(ctx, "image", f, p1.NextTSUnix, p1.NextID)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Items) != 1 || p2.Items[0].OriginalName != "sunset.png" || p2.HasMore {
		t.Errorf("asc page 2 = %+v (HasMore=%v), want [sunset.png] (false)", p2.Items, p2.HasMore)
	}
}

// TestListAttachmentsPagination walks a corpus larger than the page size and
// asserts every page respects the limit, pages are disjoint, and the union in
// cursor order equals the single-shot listing (issue #77: no >LIMIT rows).
func TestListAttachmentsPagination(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	var msgs []signal.Message
	for i := 0; i < 7; i++ {
		name := fmt.Sprintf("img-%d.jpg", i)
		msgs = append(msgs, msg("Harper", fmt.Sprintf("2022-03-01 09:0%d:00", i), "Harper", "pic",
			[]signal.Attachment{{Kind: signal.KindImage, RelPath: "media/" + name, OriginalName: name}}, nil))
	}
	if _, err := st.ReplaceConversationMessages(ctx, conv, source.Signal, msgs); err != nil {
		t.Fatal(err)
	}

	all, err := st.ListAttachments(ctx, "image", GalleryFilter{}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Items) != 7 || all.HasMore {
		t.Fatalf("full listing = %d items (HasMore=%v), want 7 (false)", len(all.Items), all.HasMore)
	}

	f := GalleryFilter{Limit: 3}
	var walked []MediaItem
	var cursorTS, cursorID int64
	pages := 0
	for {
		page, err := st.ListAttachments(ctx, "image", f, cursorTS, cursorID)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > f.Limit {
			t.Fatalf("page %d has %d items, exceeds limit %d", pages, len(page.Items), f.Limit)
		}
		walked = append(walked, page.Items...)
		pages++
		if !page.HasMore {
			break
		}
		cursorTS, cursorID = page.NextTSUnix, page.NextID
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if pages != 3 { // 3 + 3 + 1
		t.Errorf("pages = %d, want 3", pages)
	}
	if len(walked) != len(all.Items) {
		t.Fatalf("walked %d items, want %d", len(walked), len(all.Items))
	}
	for i := range walked {
		if walked[i].ID != all.Items[i].ID {
			t.Errorf("walked[%d].ID = %d, want %d (pages overlap or skip)", i, walked[i].ID, all.Items[i].ID)
		}
	}
}

func TestListLinksDedupAndGroup(t *testing.T) {
	st, _, _ := seedGalleryCorpus(t)
	page, err := st.ListLinks(context.Background(), GalleryFilter{}, LinkCursor{})
	if err != nil {
		t.Fatal(err)
	}
	links := page.Links
	// Two distinct URLs (maps.example.com/a appears twice, yelp once).
	if len(links) != 2 {
		t.Fatalf("distinct links = %d, want 2: %+v", len(links), links)
	}
	if page.HasMore {
		t.Errorf("HasMore = true for a 2-link corpus")
	}
	// Ordered by domain asc: maps.example.com before yelp.com.
	if links[0].Domain != "maps.example.com" || links[1].Domain != "yelp.com" {
		t.Errorf("domain order wrong: %q, %q", links[0].Domain, links[1].Domain)
	}
	// The maps link was seen twice → Count 2; earliest occurrence retained.
	if links[0].Count != 2 {
		t.Errorf("maps link count = %d, want 2", links[0].Count)
	}
	if links[0].TS != "2022-03-01 09:02:00" {
		t.Errorf("maps link earliest TS = %q, want 2022-03-01 09:02:00", links[0].TS)
	}
	// www. stripped from yelp domain.
	if links[1].Domain != "yelp.com" {
		t.Errorf("yelp domain = %q, want yelp.com (www stripped)", links[1].Domain)
	}
	// Each domain holds one distinct URL here.
	if links[0].DomainTotal != 1 || links[1].DomainTotal != 1 {
		t.Errorf("domain totals = %d/%d, want 1/1", links[0].DomainTotal, links[1].DomainTotal)
	}
}

// TestListLinksPagination pages through more distinct URLs than the limit —
// with one domain spanning a page boundary — and asserts bounds, disjointness,
// completeness, and that DomainTotal reports the whole-result total on every
// page (issue #77).
func TestListLinksPagination(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	conv, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	// 5 distinct URLs on big.example.com + 2 on tiny.example.org = 7 rows.
	var msgs []signal.Message
	for i := 0; i < 5; i++ {
		msgs = append(msgs, msg("Harper", fmt.Sprintf("2022-03-01 09:0%d:00", i), "Me", "l", nil,
			[]signal.Link{{URL: fmt.Sprintf("https://big.example.com/p%d", i)}}))
	}
	for i := 0; i < 2; i++ {
		msgs = append(msgs, msg("Harper", fmt.Sprintf("2022-03-02 09:0%d:00", i), "Me", "l", nil,
			[]signal.Link{{URL: fmt.Sprintf("https://tiny.example.org/p%d", i)}}))
	}
	if _, err := st.ReplaceConversationMessages(ctx, conv, source.Signal, msgs); err != nil {
		t.Fatal(err)
	}

	all, err := st.ListLinks(ctx, GalleryFilter{}, LinkCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Links) != 7 || all.HasMore {
		t.Fatalf("full listing = %d links (HasMore=%v), want 7 (false)", len(all.Links), all.HasMore)
	}

	f := GalleryFilter{Limit: 3}
	var walked []LinkItem
	cur := LinkCursor{}
	pages := 0
	for {
		page, err := st.ListLinks(ctx, f, cur)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Links) > f.Limit {
			t.Fatalf("page %d has %d links, exceeds limit %d", pages, len(page.Links), f.Limit)
		}
		for _, l := range page.Links {
			// big.example.com spans pages 1–2; its total must not shrink to the
			// per-page count.
			switch l.Domain {
			case "big.example.com":
				if l.DomainTotal != 5 {
					t.Errorf("big.example.com DomainTotal = %d on page %d, want 5", l.DomainTotal, pages)
				}
			case "tiny.example.org":
				if l.DomainTotal != 2 {
					t.Errorf("tiny.example.org DomainTotal = %d on page %d, want 2", l.DomainTotal, pages)
				}
			}
		}
		walked = append(walked, page.Links...)
		pages++
		if !page.HasMore {
			break
		}
		cur = page.Next
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
	}
	if pages != 3 { // 3 + 3 + 1
		t.Errorf("pages = %d, want 3", pages)
	}
	if len(walked) != len(all.Links) {
		t.Fatalf("walked %d links, want %d", len(walked), len(all.Links))
	}
	for i := range walked {
		if walked[i].URL != all.Links[i].URL {
			t.Errorf("walked[%d].URL = %q, want %q (pages overlap or skip)", i, walked[i].URL, all.Links[i].URL)
		}
	}
}

func TestCountMedia(t *testing.T) {
	st, harper, _ := seedGalleryCorpus(t)
	ctx := context.Background()

	all, err := st.CountMedia(ctx, GalleryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if all.Images != 2 || all.Files != 1 || all.Links != 2 {
		t.Errorf("counts = %+v, want {Images:2 Files:1 Links:2}", all)
	}

	h, err := st.CountMedia(ctx, GalleryFilter{ConversationID: harper})
	if err != nil {
		t.Fatal(err)
	}
	if h.Images != 1 || h.Files != 1 || h.Links != 2 {
		t.Errorf("harper counts = %+v, want {Images:1 Files:1 Links:2}", h)
	}

	// Source filter: everything is Signal, so the counts equal the unfiltered
	// baseline; iMessage matches nothing.
	sig, err := st.CountMedia(ctx, GalleryFilter{Source: source.Signal})
	if err != nil {
		t.Fatal(err)
	}
	if sig != all {
		t.Errorf("signal counts = %+v, want %+v", sig, all)
	}
	im, err := st.CountMedia(ctx, GalleryFilter{Source: source.IMessage})
	if err != nil {
		t.Fatal(err)
	}
	if im.Images != 0 || im.Files != 0 || im.Links != 0 {
		t.Errorf("imessage counts = %+v, want zeroes", im)
	}

	// Date filter: March only (drops April's sunset.png).
	march, err := st.CountMedia(ctx, GalleryFilter{EndUnix: dayUnix(t, "2022-03-31")})
	if err != nil {
		t.Fatal(err)
	}
	if march.Images != 1 || march.Files != 1 || march.Links != 2 {
		t.Errorf("march counts = %+v, want {Images:1 Files:1 Links:2}", march)
	}
}

// explainPlan returns the EXPLAIN QUERY PLAN detail lines for q.
func explainPlan(t *testing.T, st *Store, q string, args ...any) []string {
	t.Helper()
	rows, err := st.db.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatalf("explain: %v\nquery: %s", err, q)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return details
}

// TestGalleryQueryPlans pins the plan shapes REQ-0008-009 demands: gallery
// counts touch only attachments/links (never messages, under any filter —
// the v8 denormalized ts_unix makes even date filters single-table), and the
// listing queries never SCAN messages or attachments — messages/conversations
// appear only as bounded primary-key SEARCHes for the page's rows, and the
// attachment walk itself runs on an index (no whole-table sort).
func TestGalleryQueryPlans(t *testing.T) {
	st, harper, _ := seedGalleryCorpus(t)

	filters := map[string]GalleryFilter{
		"unfiltered":   {},
		"conversation": {ConversationID: harper},
		"source":       {Source: source.Signal},
		"date":         {StartUnix: 1, EndUnix: 2000000000},
		"sort-asc":     {SortAsc: true}, // oldest-first must also drive from the index
	}

	for name, f := range filters {
		t.Run("count-attachments/"+name, func(t *testing.T) {
			q, args := countAttachmentsSQL(f)
			for _, d := range explainPlan(t, st, q, args...) {
				if strings.Contains(d, "messages") {
					t.Errorf("count plan touches messages: %s", d)
				}
			}
		})
		t.Run("count-links/"+name, func(t *testing.T) {
			q, args := countLinksSQL(f)
			for _, d := range explainPlan(t, st, q, args...) {
				if strings.Contains(d, "messages") {
					t.Errorf("count plan touches messages: %s", d)
				}
			}
		})
		t.Run("list-attachments/"+name, func(t *testing.T) {
			q, args := listAttachmentsSQL("image", f, 0, 0, 201)
			plan := strings.Join(explainPlan(t, st, q, args...), "\n")
			if strings.Contains(plan, "SCAN messages") {
				t.Errorf("listing plan scans messages:\n%s", plan)
			}
			if strings.Contains(plan, "SCAN a") {
				t.Errorf("listing plan scans the attachments table (whole-table sort):\n%s", plan)
			}
			if !strings.Contains(plan, "SEARCH a USING INDEX") {
				t.Errorf("listing plan does not drive from an attachments index:\n%s", plan)
			}
		})
		t.Run("list-links/"+name, func(t *testing.T) {
			q, args := listLinksSQL(f, LinkCursor{}, 201)
			plan := strings.Join(explainPlan(t, st, q, args...), "\n")
			if strings.Contains(plan, "SCAN messages") {
				t.Errorf("links plan scans messages:\n%s", plan)
			}
		})
	}

	// The unfiltered count plans are the REQ-0008-009 scenario proper: pure
	// covering-index work on the single table.
	q, args := countAttachmentsSQL(GalleryFilter{})
	plan := strings.Join(explainPlan(t, st, q, args...), "\n")
	if !strings.Contains(plan, "COVERING INDEX") {
		t.Errorf("unfiltered attachment count is not a covering index scan:\n%s", plan)
	}
	q, args = countLinksSQL(GalleryFilter{})
	plan = strings.Join(explainPlan(t, st, q, args...), "\n")
	if !strings.Contains(plan, "COVERING INDEX") {
		t.Errorf("unfiltered link count is not a covering index scan:\n%s", plan)
	}

	// The unfiltered listing must also come out of the index pre-ordered — a
	// temp b-tree here would mean the whole-table sort is back.
	q, args = listAttachmentsSQL("image", GalleryFilter{}, 0, 0, 201)
	plan = strings.Join(explainPlan(t, st, q, args...), "\n")
	if strings.Contains(plan, "TEMP B-TREE") {
		t.Errorf("unfiltered attachment listing sorts instead of walking the index:\n%s", plan)
	}
}
