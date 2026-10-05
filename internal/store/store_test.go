package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Catches: state lost or misread across a reopen (a restart must resume, not repeat): runs, kv, events.
func TestStateSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 5, 6, 0, 0, 123, time.UTC)
	id, _ := s.StartRun(ctx, "alpha", t0)
	if err := s.FinishRun(ctx, id, t0.Add(time.Minute), "alert", "1 alert"); err != nil {
		t.Fatal(err)
	}
	s.StartRun(ctx, "alpha", t0.Add(time.Hour)) // dies mid-run
	if err := s.PutJSON(ctx, "alpha", "offset", 42); err != nil {
		t.Fatal(err)
	}
	s.PutJSON(ctx, "alpha", "offset", 43)
	s.AddEvent(ctx, t0, "alpha", "Heat (1995)", "made", map[string]any{"gb": 9.5})
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run, ok, err := s.LastRun(ctx, "alpha")
	if err != nil || !ok || !run.StartedAt.Equal(t0.Add(time.Hour)) || !run.FinishedAt.IsZero() {
		t.Fatalf("last run = %+v ok=%v err=%v", run, ok, err)
	}
	var offset int
	if found, err := s.GetJSON(ctx, "alpha", "offset", &offset); !found || err != nil || offset != 43 {
		t.Fatalf("offset = %d found=%v err=%v", offset, found, err)
	}
	if found, _ := s.GetJSON(ctx, "alpha", "nope", &offset); found {
		t.Fatal("missing key reported found")
	}
	status, fields, ok, err := s.LastEvent(ctx, "alpha", "Heat (1995)")
	if !ok || err != nil || status != "made" || fields["gb"] != 9.5 {
		t.Fatalf("event = %q %v ok=%v err=%v", status, fields, ok, err)
	}
}
