// Pure-Go tests for the macOS Contacts provider: every path a real Mac would
// exercise — permission-state → Availability mapping, raw-record → Person
// normalization, the enumeration-dump parser, and Resolve/People behavior — is
// driven through a fake backend, so the full pipeline is verified in the
// CGO_ENABLED=0 CI where Contacts.framework cannot be linked. The thin cgo
// binding (backend_darwin.go) carries no logic beyond calling these functions.
package macoscontacts

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/joestump/msgbrowse/internal/contacts"
)

// fakeBackend is the test double for the address-book seam.
type fakeBackend struct {
	status authStatus
	people []rawPerson
	err    error
	calls  int // number of people() invocations, to prove Availability gating
}

func (f *fakeBackend) authorization(context.Context) authStatus { return f.status }

func (f *fakeBackend) peopleFn(context.Context) ([]rawPerson, error) {
	f.calls++
	return f.people, f.err
}

// backend adapter: fakeBackend method set. (people() collides with the field
// name, so route through peopleFn via a tiny wrapper.)
func (f *fakeBackend) toBackend() backend { return backendAdapter{f} }

type backendAdapter struct{ f *fakeBackend }

func (a backendAdapter) authorization(ctx context.Context) authStatus    { return a.f.authorization(ctx) }
func (a backendAdapter) people(ctx context.Context) ([]rawPerson, error) { return a.f.peopleFn(ctx) }

func providerWith(f *fakeBackend) *Provider { return &Provider{be: f.toBackend()} }

func TestAuthStatusFromCN(t *testing.T) {
	cases := map[int]authStatus{
		cnStatusNotDetermined: authNotDetermined,
		cnStatusRestricted:    authRestricted,
		cnStatusDenied:        authDenied,
		cnStatusAuthorized:    authAuthorized,
		cnStatusLimited:       authLimited,
		99:                    authNotDetermined, // unknown → conservative
	}
	for raw, want := range cases {
		if got := authStatusFromCN(raw); got != want {
			t.Errorf("authStatusFromCN(%d) = %v, want %v", raw, got, want)
		}
	}
}

func TestAvailabilityFor(t *testing.T) {
	cases := map[authStatus]contacts.Availability{
		authAuthorized:    contacts.Available,
		authLimited:       contacts.Available,
		authNotDetermined: contacts.NeedsPermission,
		authRestricted:    contacts.NeedsPermission,
		authDenied:        contacts.NeedsPermission,
		authNoProvider:    contacts.Absent,
	}
	for s, want := range cases {
		if got := availabilityFor(s); got != want {
			t.Errorf("availabilityFor(%v) = %v, want %v", s, got, want)
		}
	}
}

func TestProviderAvailability(t *testing.T) {
	for _, tc := range []struct {
		status authStatus
		want   contacts.Availability
	}{
		{authAuthorized, contacts.Available},
		{authDenied, contacts.NeedsPermission},
		{authNotDetermined, contacts.NeedsPermission},
		{authNoProvider, contacts.Absent},
	} {
		p := providerWith(&fakeBackend{status: tc.status})
		if got := p.Availability(context.Background()); got != tc.want {
			t.Errorf("status %v: Availability = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// People must return (nil, nil) and NOT touch the backend enumeration when the
// grant is missing — the "no address book is never an error" contract, and the
// guard that a NeedsPermission provider never reads contacts.
func TestPeopleGatedOnAvailability(t *testing.T) {
	for _, status := range []authStatus{authNotDetermined, authDenied, authRestricted, authNoProvider} {
		f := &fakeBackend{status: status, people: []rawPerson{{Key: "1", Phones: []string{"+15558675309"}}}}
		p := providerWith(f)
		got, err := p.People(context.Background())
		if err != nil {
			t.Errorf("status %v: unexpected error %v", status, err)
		}
		if got != nil {
			t.Errorf("status %v: People = %v, want nil", status, got)
		}
		if f.calls != 0 {
			t.Errorf("status %v: backend.people called %d times, want 0", status, f.calls)
		}
	}
}

// A genuine backend I/O failure propagates (only "not found"/"no book" degrade
// to empty; a broken provider is an error).
func TestPeopleBackendError(t *testing.T) {
	sentinel := errors.New("boom")
	f := &fakeBackend{status: authAuthorized, err: sentinel}
	_, err := providerWith(f).People(context.Background())
	if !errors.Is(err, sentinel) {
		t.Fatalf("People err = %v, want %v", err, sentinel)
	}
}

func TestMapPeopleNormalizesAndFilters(t *testing.T) {
	raw := []rawPerson{
		{
			Key:         "abc",
			DisplayName: "  Ada Lovelace  ",
			Phones:      []string{"(555) 867-5309", "+1 555-867-5309", "not-a-phone"},
			Emails:      []string{"Ada@Example.COM", "ada@example.com", "bogus@"},
		},
		{Key: "empty", DisplayName: "No Identifiers"},                       // dropped: no valid ids
		{Key: "short", DisplayName: "Zip", Phones: []string{"12345"}},       // dropped: phone too short
		{Key: "mail", DisplayName: "M", Emails: []string{"only@mail.test"}}, // kept
	}
	got := mapPeople(raw)

	want := []contacts.Person{
		{
			Key:         "abc",
			DisplayName: "Ada Lovelace",
			Identifiers: []contacts.Identifier{
				{Kind: contacts.KindPhone, Value: "5558675309"},   // national shape, deduped
				{Kind: contacts.KindPhone, Value: "+15558675309"}, // international shape (distinct value)
				{Kind: contacts.KindEmail, Value: "ada@example.com"},
			},
		},
		{
			Key:         "mail",
			DisplayName: "M",
			Identifiers: []contacts.Identifier{{Kind: contacts.KindEmail, Value: "only@mail.test"}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mapPeople mismatch:\n got %#v\nwant %#v", got, want)
	}
}

func TestParseDump(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if got := parseDump(""); got != nil {
			t.Errorf("parseDump(\"\") = %v, want nil", got)
		}
	})
	t.Run("records", func(t *testing.T) {
		dump := "k1" + dumpFieldSep + "Ada" + dumpFieldSep + "p+15558675309" + dumpFieldSep + "eada@example.com" +
			dumpRecordSep +
			"k2" + dumpFieldSep + "Bob" + dumpFieldSep + "p5551112222" +
			dumpRecordSep + "" + // empty record skipped
			dumpRecordSep + "k3" + dumpFieldSep + "NoIds"
		got := parseDump(dump)
		want := []rawPerson{
			{Key: "k1", DisplayName: "Ada", Phones: []string{"+15558675309"}, Emails: []string{"ada@example.com"}},
			{Key: "k2", DisplayName: "Bob", Phones: []string{"5551112222"}},
			{Key: "k3", DisplayName: "NoIds"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parseDump mismatch:\n got %#v\nwant %#v", got, want)
		}
	})
	t.Run("malformed tokens skipped", func(t *testing.T) {
		// A one-field record (no name) is skipped; empty and single-char tokens dropped.
		dump := "onlykey" + dumpRecordSep + "k" + dumpFieldSep + "N" + dumpFieldSep + "p" + dumpFieldSep + "xzz"
		got := parseDump(dump)
		want := []rawPerson{{Key: "k", DisplayName: "N"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parseDump mismatch:\n got %#v\nwant %#v", got, want)
		}
	})
}

// parseDump feeding mapPeople is the exact pipeline the cgo backend runs, so
// verify the seam end to end without a framework.
func TestParsePlusMapPipeline(t *testing.T) {
	dump := "k1" + dumpFieldSep + "Ada" + dumpFieldSep + "p(555) 867-5309" + dumpFieldSep + "eADA@EXAMPLE.com"
	got := mapPeople(parseDump(dump))
	want := []contacts.Person{{
		Key:         "k1",
		DisplayName: "Ada",
		Identifiers: []contacts.Identifier{
			{Kind: contacts.KindPhone, Value: "5558675309"},
			{Kind: contacts.KindEmail, Value: "ada@example.com"},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pipeline mismatch:\n got %#v\nwant %#v", got, want)
	}
}

func TestResolve(t *testing.T) {
	f := &fakeBackend{
		status: authAuthorized,
		people: []rawPerson{
			{Key: "ada", DisplayName: "Ada", Phones: []string{"+15558675309"}, Emails: []string{"ada@example.com"}},
			{Key: "bob", DisplayName: "Bob", Phones: []string{"5551112222"}},
			{Key: "ada2", DisplayName: "Ada Alt", Emails: []string{"ada@example.com"}},
		},
	}
	p := providerWith(f)
	ctx := context.Background()

	t.Run("email matches two people", func(t *testing.T) {
		got, err := p.Resolve(ctx, contacts.Identifier{Kind: contacts.KindEmail, Value: "ada@example.com"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got[0].Key != "ada" || got[1].Key != "ada2" {
			t.Fatalf("Resolve email = %+v", got)
		}
	})
	t.Run("exact phone shape only (no cross-shape widening)", func(t *testing.T) {
		// The international-shape id matches ada; the national shape does NOT
		// (widening is the engine's job, not the provider's).
		got, err := p.Resolve(ctx, contacts.Identifier{Kind: contacts.KindPhone, Value: "+15558675309"})
		if err != nil || len(got) != 1 || got[0].Key != "ada" {
			t.Fatalf("Resolve intl phone = %+v, err %v", got, err)
		}
		got, err = p.Resolve(ctx, contacts.Identifier{Kind: contacts.KindPhone, Value: "5558675309"})
		if err != nil || len(got) != 0 {
			t.Fatalf("Resolve national phone = %+v, err %v (want no match)", got, err)
		}
	})
	t.Run("zero identifier and no match are empty", func(t *testing.T) {
		if got, _ := p.Resolve(ctx, contacts.Identifier{}); got != nil {
			t.Errorf("Resolve(zero) = %v, want nil", got)
		}
		if got, _ := p.Resolve(ctx, contacts.Identifier{Kind: contacts.KindEmail, Value: "nobody@x.test"}); got != nil {
			t.Errorf("Resolve(miss) = %v, want nil", got)
		}
	})
	t.Run("unavailable resolves to empty", func(t *testing.T) {
		p2 := providerWith(&fakeBackend{status: authDenied, people: f.people})
		if got, err := p2.Resolve(ctx, contacts.Identifier{Kind: contacts.KindEmail, Value: "ada@example.com"}); err != nil || got != nil {
			t.Fatalf("Resolve while denied = %v, err %v", got, err)
		}
	})
}

// New must never wire the real framework backend off macOS: the runtime guard
// (on top of the build tag) keeps the provider at Absent so a stray tag can
// never touch Contacts on a non-Mac. On darwin without the tag the stub backend
// is selected, which is also Absent — so this holds on every CI platform.
func TestNewFallsBackToNoProvider(t *testing.T) {
	p := New(nil)
	if p == nil {
		t.Fatal("New returned nil")
	}
	if got := p.Availability(context.Background()); runtime.GOOS != "darwin" && got != contacts.Absent {
		t.Fatalf("off-darwin New Availability = %v, want Absent", got)
	}
	// A no-provider provider enumerates nothing and never errors.
	if people, err := p.People(context.Background()); err != nil || people != nil {
		t.Fatalf("New People = %v, err %v", people, err)
	}
}

// TestDefaultBuildLinksNoContactsBackend is the build-constraint proof (the
// approach #20 used for devicesync): in the default test build — no
// `macoscontacts` tag — the stub backend is selected, so backendCompiledIn is
// false and no Contacts.framework symbol is linked. Under `-tags macoscontacts`
// on a Mac this flips to true; here it must be false.
func TestDefaultBuildLinksNoContactsBackend(t *testing.T) {
	if backendCompiledIn {
		t.Fatal("backendCompiledIn = true in a default (no-tag) build; the Contacts framework leaked into the default binary")
	}
}

// TestDefaultBuildHasNoContactsSymbols is the stronger `go tool nm` proof: it
// compiles the real default msgbrowse CLI (in-module, no `macoscontacts` tag,
// cgo off) and asserts the linked binary carries no Contacts/CNContact symbol —
// the literal "the default binary keeps zero Contacts symbols" acceptance bar.
// Because backend_darwin.go (the sole source of any Contacts / mb_contacts_
// symbol) is excluded from every non-tagged build, no default binary can carry
// them; this exercises that end to end on the shipped command. Skips gracefully
// when the toolchain is unavailable; a build that reveals a framework symbol is
// a hard failure. The build-constraint backstop above
// (TestDefaultBuildLinksNoContactsBackend, the #20 style) always runs even when
// this one skips.
func TestDefaultBuildHasNoContactsSymbols(t *testing.T) {
	if backendCompiledIn {
		t.Skip("built with -tags macoscontacts; symbol-absence check applies to the default build only")
	}
	if testing.Short() {
		t.Skip("skipping binary build in -short mode")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	// Repo root: this file is <root>/internal/macoscontacts/provider_test.go.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Skip("cannot locate test source")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))

	bin := filepath.Join(t.TempDir(), "msgbrowse")
	build := exec.Command(goBin, "build", "-o", bin, "./cmd/msgbrowse")
	build.Dir = repoRoot
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("default binary build unavailable in this environment: %v\n%s", err, out)
	}
	out, err := exec.Command(goBin, "tool", "nm", bin).CombinedOutput()
	if err != nil {
		t.Skipf("go tool nm unavailable: %v\n%s", err, out)
	}
	for _, needle := range []string{"CNContact", "CNContactStore", "mb_contacts_"} {
		if strings.Contains(string(out), needle) {
			t.Errorf("default msgbrowse binary links Contacts symbol %q", needle)
		}
	}
}
