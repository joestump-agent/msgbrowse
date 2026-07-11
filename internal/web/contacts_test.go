package web

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/joestump/msgbrowse/internal/contacts"
)

// fakeContactResolver is a test double for the address-book seam (issue #9,
// the fakeEnabler style): it records calls and returns scripted people so
// seam tests can prove the server hands consumers exactly the wired provider.
type fakeContactResolver struct {
	available bool
	people    []contacts.Person
	resolves  int32 // atomic: number of Resolve calls
}

func (f *fakeContactResolver) Available(context.Context) bool { return f.available }

func (f *fakeContactResolver) Resolve(_ context.Context, id contacts.Identifier) ([]contacts.Person, error) {
	atomic.AddInt32(&f.resolves, 1)
	var out []contacts.Person
	for _, p := range f.people {
		for _, pid := range p.Identifiers {
			if pid == id {
				out = append(out, p)
				break
			}
		}
	}
	return out, nil
}

func (f *fakeContactResolver) People(context.Context) ([]contacts.Person, error) {
	return append([]contacts.Person(nil), f.people...), nil
}

// TestContactResolverDefaultsToUnavailable pins the unwired contract: with no
// SetContactResolver call (Linux, browser mode), the accessor substitutes
// contacts.Unavailable{} — non-nil, answering "no address book" with empty
// results and nil errors — so the merge path never nil-checks and never errors.
func TestContactResolverDefaultsToUnavailable(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	r := srv.contactResolver()
	if r == nil {
		t.Fatal("contactResolver() = nil; must substitute contacts.Unavailable{}")
	}
	if _, ok := r.(contacts.Unavailable); !ok {
		t.Fatalf("contactResolver() = %T, want contacts.Unavailable", r)
	}
	if r.Available(ctx) {
		t.Fatal("unwired resolver must report Available() = false")
	}
	people, err := r.People(ctx)
	if err != nil || len(people) != 0 {
		t.Fatalf("unwired People() = (%v, %v), want (empty, nil)", people, err)
	}
	got, err := r.Resolve(ctx, contacts.Normalize("+1 555 867 5309"))
	if err != nil || len(got) != 0 {
		t.Fatalf("unwired Resolve() = (%v, %v), want (empty, nil)", got, err)
	}
}

// TestSetContactResolverWiresProvider pins the injection seam: after
// SetContactResolver (called post-NewServer, the SetEnabler/SetPairingSource
// contract) the accessor returns the wired provider verbatim, and queries
// reach it.
func TestSetContactResolverWiresProvider(t *testing.T) {
	srv, _, _ := newTestServer(t)
	ctx := context.Background()

	alice := contacts.Person{
		Key:         "ab-key-1",
		DisplayName: "Alice Example",
		Identifiers: []contacts.Identifier{
			contacts.Normalize("+1 (555) 867-5309"),
			contacts.Normalize("Alice@Example.com"),
		},
	}
	fake := &fakeContactResolver{available: true, people: []contacts.Person{alice}}
	srv.SetContactResolver(fake)

	r := srv.contactResolver()
	if r != contacts.Resolver(fake) {
		t.Fatalf("contactResolver() = %T, want the wired fake", r)
	}
	if !r.Available(ctx) {
		t.Fatal("wired resolver must report Available() = true")
	}

	// A canonical identifier round-trips to the scripted person: the same
	// Normalize helpers canonicalize both sides, so a differently-formatted
	// input still matches.
	got, err := r.Resolve(ctx, contacts.Normalize("alice@example.COM"))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 || got[0].Key != alice.Key || got[0].DisplayName != alice.DisplayName {
		t.Fatalf("Resolve() = %+v, want [%+v]", got, alice)
	}
	if n := atomic.LoadInt32(&fake.resolves); n != 1 {
		t.Fatalf("fake.resolves = %d, want 1 (query must reach the wired provider)", n)
	}

	people, err := r.People(ctx)
	if err != nil || len(people) != 1 {
		t.Fatalf("People() = (%v, %v), want the one scripted person", people, err)
	}
}
