package web

import (
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/joestump/msgbrowse/internal/source"
	"github.com/joestump/msgbrowse/internal/store"
)

// galleryFilterForm is the re-renderable filter state for the gallery.
type galleryFilterForm struct {
	Tab            string
	ConversationID int64
	Source         string
	Start          string
	End            string
}

// galleryFileView decorates a file attachment with its on-disk size and type,
// computed on demand from the read-only archive.
type galleryFileView struct {
	store.MediaItem
	SizeHuman   string
	ContentType string
}

// linkGroup is a set of deduplicated links sharing a domain. Total is the
// domain's distinct-URL count across the whole filtered result, not just the
// links loaded so far, so the badge stays truthful when a domain spans
// infinite-scroll page boundaries.
type linkGroup struct {
	Domain string
	Total  int
	Links  []store.LinkItem
}

// galleryImagesData / galleryFilesData / galleryLinksData are the per-tab
// page payloads shared by the full gallery render and the /gallery/items
// infinite-scroll fragments (SPEC-0008 REQ-0008-009). NextURL, when non-empty,
// is the /gallery/items URL of the next page; the templates render it as an
// hx-trigger="revealed" sentinel, the same load-more pattern the transcript
// uses.
type galleryImagesData struct {
	Images  []store.MediaItem
	NextURL string
}

type galleryFilesData struct {
	Files   []galleryFileView
	NextURL string
}

type galleryLinksData struct {
	Groups  []linkGroup
	NextURL string
}

type galleryData struct {
	baseData
	// FilterConversations feeds the conversation dropdown: the lightweight
	// id+name listing, NOT the sidebar summaries, so partial renders never need
	// the expensive listing (SPEC-0008 REQ-0008-006). Ordered alphabetically.
	FilterConversations []store.ConversationRef
	Filter              galleryFilterForm
	Sources             []string
	Counts              store.MediaCounts
	ImagesPage          galleryImagesData
	FilesPage           galleryFilesData
	LinksPage           galleryLinksData
}

// validTabs are the gallery's three views.
var validTabs = map[string]bool{"images": true, "files": true, "links": true}

func (s *Server) handleGallery(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var base baseData
	if isPartialRequest(r) {
		base = partialBase("Media · msgbrowse", 0)
	} else {
		var err error
		base, err = s.baseData(ctx, "Media · msgbrowse", 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
	}
	// The gallery is the Media surface: the header's Media tab reads active
	// (#190) whether it renders at /gallery or the /media alias.
	base.NavTab = navTabMedia
	// The dropdown uses the cheap id+name listing on BOTH paths so partial and
	// full renders show the identical (alphabetical) option order.
	refs, err := s.store.ConversationRefs(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}

	form, filter := parseGalleryFilter(r)
	counts, err := s.store.CountMedia(ctx, filter)
	if err != nil {
		s.serverError(w, err)
		return
	}

	data := galleryData{
		baseData:            base,
		FilterConversations: refs,
		Filter:              form,
		Sources:             source.All,
		Counts:              counts,
	}

	switch form.Tab {
	case "files":
		page, err := s.store.ListAttachments(ctx, "file", filter, 0, 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
		data.FilesPage = galleryFilesData{Files: s.decorateFiles(page.Items), NextURL: form.attachmentsNextURL(page)}
	case "links":
		page, err := s.store.ListLinks(ctx, filter, store.LinkCursor{})
		if err != nil {
			s.serverError(w, err)
			return
		}
		data.LinksPage = galleryLinksData{Groups: groupLinksByDomain(page.Links), NextURL: form.linksNextURL(page)}
	default: // images
		page, err := s.store.ListAttachments(ctx, "image", filter, 0, 0)
		if err != nil {
			s.serverError(w, err)
			return
		}
		data.ImagesPage = galleryImagesData{Images: page.Items, NextURL: form.attachmentsNextURL(page)}
	}

	s.render(w, r, "gallery", data)
}

// handleGalleryItems serves one infinite-scroll continuation page for the
// active gallery tab (SPEC-0008 REQ-0008-009: the links tab used to render
// ~20k anchors in one response). Cursor parameters are parsed as integers
// where numeric (bad values read as zero = "from the top"); the string cursor
// parts only ever travel as bound SQL parameters and re-render through
// html/template escaping.
func (s *Server) handleGalleryItems(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	form, filter := parseGalleryFilter(r)
	q := r.URL.Query()

	switch form.Tab {
	case "links":
		cur := store.LinkCursor{
			Domain: q.Get("after_domain"),
			Count:  int(parseInt64(q.Get("after_count"))),
			TSUnix: parseInt64(q.Get("after_ts")),
			URL:    q.Get("after_url"),
		}
		page, err := s.store.ListLinks(ctx, filter, cur)
		if err != nil {
			s.serverError(w, err)
			return
		}
		s.render(w, r, "gallery_links_page", galleryLinksData{Groups: groupLinksByDomain(page.Links), NextURL: form.linksNextURL(page)})
	case "files":
		page, err := s.store.ListAttachments(ctx, "file", filter, parseInt64(q.Get("after_ts")), parseInt64(q.Get("after_id")))
		if err != nil {
			s.serverError(w, err)
			return
		}
		s.render(w, r, "gallery_files_page", galleryFilesData{Files: s.decorateFiles(page.Items), NextURL: form.attachmentsNextURL(page)})
	default: // images
		page, err := s.store.ListAttachments(ctx, "image", filter, parseInt64(q.Get("after_ts")), parseInt64(q.Get("after_id")))
		if err != nil {
			s.serverError(w, err)
			return
		}
		s.render(w, r, "gallery_images_page", galleryImagesData{Images: page.Items, NextURL: form.attachmentsNextURL(page)})
	}
}

// parseInt64 reads a decimal int64, treating garbage as zero (= no cursor).
func parseInt64(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// decorateFiles stats each file in the read-only archive to add size and type.
// Files that can't be stat'd (missing/renamed) still render, just without
// size/type, so the listing never fails on a single bad attachment.
func (s *Server) decorateFiles(items []store.MediaItem) []galleryFileView {
	out := make([]galleryFileView, 0, len(items))
	for _, it := range items {
		v := galleryFileView{MediaItem: it}
		if full, ok := s.mediaFilePath(it.Source, it.ConversationName, it.RelPath); ok {
			if info, err := os.Stat(full); err == nil && !info.IsDir() {
				v.SizeHuman = humanSize(info.Size())
				v.ContentType = fileContentType(full)
			}
		}
		out = append(out, v)
	}
	return out
}

// fileContentType resolves a file's type by extension, falling back to sniffing
// the first 512 bytes (http.DetectContentType) when the extension is unknown.
func fileContentType(full string) string {
	if ct := mime.TypeByExtension(filepath.Ext(full)); ct != "" {
		// Trim any "; charset=..." for a compact display label.
		if i := strings.IndexByte(ct, ';'); i >= 0 {
			ct = ct[:i]
		}
		return ct
	}
	f, err := os.Open(full)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := f.Read(buf)
	ct := http.DetectContentType(buf[:n])
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	return ct
}

// groupLinksByDomain groups already-domain-sorted links into per-domain blocks.
func groupLinksByDomain(links []store.LinkItem) []linkGroup {
	var groups []linkGroup
	for _, l := range links {
		if n := len(groups); n > 0 && groups[n-1].Domain == l.Domain {
			groups[n-1].Links = append(groups[n-1].Links, l)
			continue
		}
		groups = append(groups, linkGroup{Domain: l.Domain, Total: l.DomainTotal, Links: []store.LinkItem{l}})
	}
	return groups
}

// parseGalleryFilter reads the gallery's tab + filters from the query string.
func parseGalleryFilter(r *http.Request) (galleryFilterForm, store.GalleryFilter) {
	tab := r.URL.Query().Get("tab")
	if !validTabs[tab] {
		tab = "images"
	}
	convID, _ := strconv.ParseInt(r.URL.Query().Get("conversation"), 10, 64)
	src := r.URL.Query().Get("source")
	if !source.IsKnown(src) {
		src = ""
	}
	start := r.URL.Query().Get("start")
	end := r.URL.Query().Get("end")

	form := galleryFilterForm{Tab: tab, ConversationID: convID, Source: src, Start: start, End: end}
	filter := store.GalleryFilter{
		ConversationID: convID,
		Source:         src,
		StartUnix:      dayStartUnix(start),
		EndUnix:        dayEndUnix(end),
	}
	return form, filter
}

// filterValues returns the querystring values that preserve the current
// filters across tab switches and pagination requests.
func (f galleryFilterForm) filterValues(tab string) url.Values {
	v := url.Values{}
	v.Set("tab", tab)
	if f.ConversationID > 0 {
		v.Set("conversation", strconv.FormatInt(f.ConversationID, 10))
	}
	if f.Source != "" {
		v.Set("source", f.Source)
	}
	if f.Start != "" {
		v.Set("start", f.Start)
	}
	if f.End != "" {
		v.Set("end", f.End)
	}
	return v
}

// GalleryQuery builds the querystring that preserves the current filters when
// switching tabs (used by the tab links in the template). Exported so the
// html/template can call it as a method.
func (f galleryFilterForm) GalleryQuery(tab string) string {
	return "/gallery?" + f.filterValues(tab).Encode()
}

// galleryConvURL is the FuncMap helper behind the conversation header's meta
// chips (#177): the /gallery URL filtered to one conversation on the given tab.
// It routes through the same filterValues/Encode path as the gallery's own tab
// links, so the deep link's shape (and parseGalleryFilter round-trip) stays
// identical to what the gallery emits for itself.
func galleryConvURL(tab string, convID int64) string {
	return galleryFilterForm{Tab: tab, ConversationID: convID}.GalleryQuery(tab)
}

// attachmentsNextURL builds the /gallery/items URL for the page after this
// one ("" when the walk is done). The cursor is the (ts_unix, id) keyset pair
// of the last row, mirroring the transcript's before_ts/before_id contract.
func (f galleryFilterForm) attachmentsNextURL(page *store.MediaPage) string {
	if !page.HasMore {
		return ""
	}
	v := f.filterValues(f.Tab)
	v.Set("after_ts", strconv.FormatInt(page.NextTSUnix, 10))
	v.Set("after_id", strconv.FormatInt(page.NextID, 10))
	return "/gallery/items?" + v.Encode()
}

// linksNextURL builds the /gallery/items URL for the links page after this
// one ("" when the walk is done). The cursor is the full ordering tuple
// (domain, count, earliest ts, url) of the last deduplicated row.
func (f galleryFilterForm) linksNextURL(page *store.LinkPage) string {
	if !page.HasMore {
		return ""
	}
	v := f.filterValues("links")
	v.Set("after_domain", page.Next.Domain)
	v.Set("after_count", strconv.Itoa(page.Next.Count))
	v.Set("after_ts", strconv.FormatInt(page.Next.TSUnix, 10))
	v.Set("after_url", page.Next.URL)
	return "/gallery/items?" + v.Encode()
}
