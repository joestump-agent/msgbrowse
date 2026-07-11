package contacts

import (
	"context"
	"testing"
)

// TestUnavailable pins the no-address-book contract issue #9 requires: the
// default Resolver answers every query with empty results and a nil error —
// the merge path must degrade to a no-op, never fail, on platforms without
// a native address book.
func TestUnavailable(t *testing.T) {
	ctx := context.Background()
	var r Resolver = Unavailable{}

	if r.Available(ctx) {
		t.Fatal("Unavailable.Available() = true, want false")
	}

	people, err := r.People(ctx)
	if err != nil {
		t.Fatalf("Unavailable.People() error = %v, want nil (no-address-book is never an error)", err)
	}
	if len(people) != 0 {
		t.Fatalf("Unavailable.People() = %v, want empty", people)
	}

	got, err := r.Resolve(ctx, Normalize("+1 555 867 5309"))
	if err != nil {
		t.Fatalf("Unavailable.Resolve() error = %v, want nil (no-address-book is never an error)", err)
	}
	if len(got) != 0 {
		t.Fatalf("Unavailable.Resolve() = %v, want empty", got)
	}
}
