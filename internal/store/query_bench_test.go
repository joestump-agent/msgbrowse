package store

import (
	"context"
	"os"
	"testing"
)

// Archive-scale benchmarks for the SPEC-0008 store rewrites. They need a real
// database, so they are gated on MSGBROWSE_BENCH_DB and skip otherwise:
//
//	cp /path/to/msgbrowse.sqlite /tmp/bench.sqlite   # NEVER the live file —
//	MSGBROWSE_BENCH_DB=/tmp/bench.sqlite \           # Open() migrates it
//	  go test ./internal/store -bench 'ListConversations|NewestMessageTS' -run '^$'
//
// The Legacy variants run the pre-rewrite code preserved in
// query_golden_test.go, so the old-vs-new gap stays measurable after the old
// paths are gone from production code.

func benchStore(b *testing.B) *Store {
	b.Helper()
	path := os.Getenv("MSGBROWSE_BENCH_DB")
	if path == "" {
		b.Skip("MSGBROWSE_BENCH_DB not set; skipping archive-scale benchmark")
	}
	st, err := Open(path)
	if err != nil {
		b.Fatalf("open %s: %v", path, err)
	}
	b.Cleanup(func() { _ = st.Close() })
	return st
}

func BenchmarkListConversations(b *testing.B) {
	st := benchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.ListConversations(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkListConversationsLegacy(b *testing.B) {
	st := benchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := legacyListConversations(ctx, st); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNewestMessageTS(b *testing.B) {
	st := benchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.NewestMessageTS(ctx); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNewestMessageTSLegacy(b *testing.B) {
	st := benchStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var ts string
		// The retired MAX(ts) full-scan string aggregation, kept for comparison.
		if err := st.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(ts), '') FROM messages`).Scan(&ts); err != nil {
			b.Fatal(err)
		}
	}
}
