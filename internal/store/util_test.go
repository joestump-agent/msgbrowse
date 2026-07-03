package store

import "testing"

// TestPreview covers the sidebar excerpt cleanup: raw Markdown must never leak
// (images collapse to a 📷 placeholder, links to their text), zero-width Cf
// format runes are dropped, leading blockquote markers are stripped, and the
// result is whitespace-collapsed and rune-truncated.
func TestPreview(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{
			name: "plain text collapses whitespace",
			in:   "hello\n  world",
			max:  80,
			want: "hello world",
		},
		{
			name: "image markdown becomes camera placeholder",
			in:   "look ![a cat](media/cat.jpg)",
			max:  80,
			want: "look 📷",
		},
		{
			name: "image with parens in target fully stripped",
			in:   "pic ![Image_from_iOS_(1).jpg](media/Image_from_iOS_(1).jpg) sent",
			max:  80,
			want: "pic 📷 sent",
		},
		{
			name: "link markdown unwraps to its text",
			in:   "see [the menu](https://example.com/menu) today",
			max:  80,
			want: "see the menu today",
		},
		{
			name: "format runes dropped",
			in:   "wat\u200b\u200dch this\ufeff", // zero-width space/joiner + BOM (all Cf)
			max:  80,
			want: "watch this",
		},
		{
			name: "leading quote markers stripped",
			in:   "> quoted reply\n> > nested\nsure thing",
			max:  80,
			want: "quoted reply nested sure thing",
		},
		{
			name: "mid-line gt is not a quote marker",
			in:   "5 > 3 obviously",
			max:  80,
			want: "5 > 3 obviously",
		},
		{
			name: "truncates on a rune boundary with ellipsis",
			in:   "ααααααααα",
			max:  5,
			want: "ααααα…",
		},
		{
			name: "empty body",
			in:   "",
			max:  80,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := preview(tt.in, tt.max); got != tt.want {
				t.Errorf("preview(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
			}
		})
	}
}
