package store

import (
	"context"
	"testing"
	"time"

	"github.com/joestump/msgbrowse/internal/signal"
	"github.com/joestump/msgbrowse/internal/source"
)

// TestEmbedRunLifecycle walks a run through begin → per-batch progress →
// finish and pins what each stage exposes through LatestEmbedRun: the
// in-flight sentinel (zero FinishedAt), the moving heartbeat, and the terminal
// totals.
func TestEmbedRunLifecycle(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	if r, err := st.LatestEmbedRun(ctx); err != nil || r != nil {
		t.Fatalf("empty store LatestEmbedRun = %v, %v; want nil, nil", r, err)
	}

	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	id, err := st.BeginEmbedRun(ctx, "test-embed", start)
	if err != nil {
		t.Fatal(err)
	}

	r, err := st.LatestEmbedRun(ctx)
	if err != nil || r == nil {
		t.Fatalf("LatestEmbedRun after begin = %v, %v", r, err)
	}
	if !r.InFlight() {
		t.Error("freshly begun run should be in flight")
	}
	if r.Model != "test-embed" || !r.StartedAt.Equal(start) || !r.UpdatedAt.Equal(start) {
		t.Errorf("begun run = %+v, want model/started/heartbeat from Begin", r)
	}

	beat := start.Add(30 * time.Second)
	if err := st.UpdateEmbedRunProgress(ctx, id, 128, 2, beat); err != nil {
		t.Fatal(err)
	}
	r, err = st.LatestEmbedRun(ctx)
	if err != nil || r == nil {
		t.Fatalf("LatestEmbedRun after progress = %v, %v", r, err)
	}
	if !r.InFlight() || r.Embedded != 128 || r.Batches != 2 || !r.UpdatedAt.Equal(beat) {
		t.Errorf("progressed run = %+v, want in-flight 128/2 with moved heartbeat", r)
	}

	fin := start.Add(time.Minute)
	if err := st.FinishEmbedRun(ctx, EmbedRun{
		ID: id, FinishedAt: fin, DurationMS: 60000, Embedded: 256, Pruned: 3, Batches: 4,
	}); err != nil {
		t.Fatal(err)
	}
	r, err = st.LatestEmbedRun(ctx)
	if err != nil || r == nil {
		t.Fatalf("LatestEmbedRun after finish = %v, %v", r, err)
	}
	if r.InFlight() {
		t.Error("finished run still reads as in flight")
	}
	if !r.FinishedAt.Equal(fin) || r.DurationMS != 60000 || r.Embedded != 256 || r.Pruned != 3 || r.Batches != 4 || r.Error != "" {
		t.Errorf("finished run = %+v, want the terminal totals", r)
	}

	// A failed run records its abort reason; LatestEmbedRun returns the newest
	// row (this one), not the earlier success.
	id2, err := st.BeginEmbedRun(ctx, "test-embed", fin.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishEmbedRun(ctx, EmbedRun{
		ID: id2, FinishedAt: fin.Add(time.Hour), Error: "provider unreachable",
	}); err != nil {
		t.Fatal(err)
	}
	r, err = st.LatestEmbedRun(ctx)
	if err != nil || r == nil {
		t.Fatalf("LatestEmbedRun after failed run = %v, %v", r, err)
	}
	if r.ID != id2 || r.Error != "provider unreachable" {
		t.Errorf("latest run = %+v, want the newest (failed) run", r)
	}
}

// TestEmbeddingCoverage pins the coverage aggregate: the denominator is the
// embeddable set (non-system, non-blank body — the exact
// CountMissingEmbeddings predicate) and the numerator counts only vectors for
// the asked-for model.
func TestEmbeddingCoverage(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	cid, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, cid, source.Signal, []signal.Message{
		msg("Harper", "2022-03-01 09:00:00", "Harper", "the lease agreement", nil, nil),
		msg("Harper", "2022-03-01 09:01:00", "Me", "lunch tomorrow", nil, nil),
		msg("Harper", "2022-03-01 09:02:00", "Harper", "   ", nil, nil), // blank: not embeddable
		sysMsg("Harper", "2022-03-01 09:03:00", "group renamed"),        // system: not embeddable
	}); err != nil {
		t.Fatal(err)
	}

	cov, err := st.EmbeddingCoverage(ctx, "test-embed")
	if err != nil {
		t.Fatal(err)
	}
	if cov.Embeddable != 2 || cov.Embedded != 0 {
		t.Errorf("pre-embed coverage = %+v, want 0 of 2", cov)
	}

	// Embed one of the two under the asked-for model, and one under a DIFFERENT
	// model (which must not count).
	targets, err := st.MessagesNeedingEmbedding(ctx, "test-embed", 10)
	if err != nil || len(targets) != 2 {
		t.Fatalf("targets = %v, %v; want 2", targets, err)
	}
	if err := st.PutEmbedding(ctx, targets[0].Hash, "test-embed", []float32{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutEmbedding(ctx, targets[1].Hash, "other-model", []float32{3, 4}); err != nil {
		t.Fatal(err)
	}

	cov, err = st.EmbeddingCoverage(ctx, "test-embed")
	if err != nil {
		t.Fatal(err)
	}
	if cov.Embeddable != 2 || cov.Embedded != 1 {
		t.Errorf("coverage = %+v, want 1 of 2 (other model's vector must not count)", cov)
	}

	// Invariant with the embed pipeline: Embeddable - Embedded == pending.
	missing, err := st.CountMissingEmbeddings(ctx, "test-embed")
	if err != nil {
		t.Fatal(err)
	}
	if cov.Embeddable-cov.Embedded != missing {
		t.Errorf("coverage gap %d != CountMissingEmbeddings %d", cov.Embeddable-cov.Embedded, missing)
	}
}

// TestResetEmbeddings pins the Status page's "Reset & rebuild" primitive
// (issue #191): after ResetEmbeddings both the vectors AND the run log are
// gone, so coverage reads 0 and LatestEmbedRun returns nil ("never").
func TestResetEmbeddings(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	cid, err := st.UpsertConversation(ctx, source.Signal, "Harper")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReplaceConversationMessages(ctx, cid, source.Signal, []signal.Message{
		msg("Harper", "2022-03-01 09:00:00", "Harper", "the lease agreement", nil, nil),
		msg("Harper", "2022-03-01 09:01:00", "Me", "lunch tomorrow", nil, nil),
	}); err != nil {
		t.Fatal(err)
	}
	// Seed both tables: two vectors and a finished run.
	targets, err := st.MessagesNeedingEmbedding(ctx, "test-embed", 10)
	if err != nil || len(targets) != 2 {
		t.Fatalf("targets = %v, %v; want 2", targets, err)
	}
	for _, tg := range targets {
		if err := st.PutEmbedding(ctx, tg.Hash, "test-embed", []float32{1, 2}); err != nil {
			t.Fatal(err)
		}
	}
	runID, err := st.BeginEmbedRun(ctx, "test-embed", time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishEmbedRun(ctx, EmbedRun{ID: runID, FinishedAt: time.Now(), Embedded: 2, Batches: 1}); err != nil {
		t.Fatal(err)
	}

	if cov, err := st.EmbeddingCoverage(ctx, "test-embed"); err != nil || cov.Embedded != 2 {
		t.Fatalf("pre-reset coverage = %+v, %v; want 2 embedded", cov, err)
	}

	if err := st.ResetEmbeddings(ctx); err != nil {
		t.Fatal(err)
	}

	// Vectors gone: coverage embedded drops to 0 (the embeddable denominator,
	// the messages themselves, is untouched).
	cov, err := st.EmbeddingCoverage(ctx, "test-embed")
	if err != nil {
		t.Fatal(err)
	}
	if cov.Embedded != 0 || cov.Embeddable != 2 {
		t.Errorf("post-reset coverage = %+v, want 0 of 2", cov)
	}
	// Run log gone: LatestEmbedRun reads "never".
	run, err := st.LatestEmbedRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if run != nil {
		t.Errorf("post-reset LatestEmbedRun = %+v, want nil (never)", run)
	}
	// The messages survive (reset clears the index, never the archive).
	if missing, err := st.CountMissingEmbeddings(ctx, "test-embed"); err != nil || missing != 2 {
		t.Errorf("post-reset missing = %d, %v; want both messages pending again", missing, err)
	}

	// Idempotent: a second reset on an already-empty index is a no-op, not an
	// error.
	if err := st.ResetEmbeddings(ctx); err != nil {
		t.Errorf("second ResetEmbeddings errored: %v", err)
	}
}

// sysMsg builds a system message (not embeddable).
func sysMsg(conv, ts, body string) signal.Message {
	m := msg(conv, ts, "No-Sender", body, nil, nil)
	m.IsSystem = true
	return m
}
