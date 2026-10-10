package backend

import (
	"context"
	"strings"
	"testing"
	"time"
)

func phase7Stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }
func phase7SetItemDates(rows []map[string]any, created, saved time.Time) {
	for _, row := range rows {
		row["DateCreated"] = phase7Stamp(created)
		row["DateLastSaved"] = phase7Stamp(saved)
	}
}
func phase7CompleteFull(t *testing.T, app *App) {
	t.Helper()
	run := phase5Enable(t, app, "server-a")
	if err := app.scannerRunFull(context.Background(), "server-a", run.ID); err != nil {
		t.Fatal(err)
	}
}
func phase7BeginDelta(t *testing.T, app *App) *scanRun {
	t.Helper()
	run, err := app.scannerCommand("server-a", "start")
	if err != nil {
		t.Fatal(err)
	}
	if run.Type != "delta" {
		t.Fatalf("expected Delta got %q", run.Type)
	}
	return run
}
func TestPhase7FallbackOverlapTailTwoPages(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	rows := phase6SampleMovies("a", 420, 40000)
	phase7SetItemDates(rows, old, old)
	a := newPhase6MockSource(t, "a", rows)
	b := newPhase6MockSource(t, "b", nil)
	withPhase6MockApp(t, a, b, func(app *App) {
		app.Scanner.now = func() time.Time { return now.Add(-time.Hour) }
		phase7CompleteFull(t, app)
		app.Scanner.now = func() time.Time { return now }
		run := phase7BeginDelta(t, app)
		a.mu.Lock()
		a.calls = nil
		a.mu.Unlock()
		if err := app.scannerRunDelta(context.Background(), "server-a", run.ID); err != nil {
			t.Fatal(err)
		}
		pages := 0
		for _, c := range a.onlyScannerCalls() {
			if strings.HasSuffix(c.Path, "/Items") && c.Query.Get("Limit") == "60" {
				pages++
				if c.Query.Get("SortBy") != "DateCreated" || c.Query.Get("SortOrder") != "Descending" {
					t.Fatalf("fallback sort incorrect: %v", c.Query)
				}
			}
		}
		if pages != 3 {
			t.Fatalf("fallback should stop at old page + 2 verification pages, got %d", pages)
		}
		state, err := app.Scanner.runLocked("server-a", false)
		if err != nil || state.State != scanCompleted {
			t.Fatalf("delta never completed %+v %v", state, err)
		}
		status, err := app.scannerState("server-a")
		if err != nil {
			t.Fatal(err)
		}
		libs := status["libraries"].([]map[string]any)
		if len(libs) != 1 || libs[0]["committedCursor"] != run.StartedAt {
			t.Fatalf("delta safe cursor not advanced: %+v", libs)
		}
	})
}
func TestPhase7ReliableMinDateLastSavedProbeAndFilteredDelta(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-2 * time.Hour)
	rows := phase6SampleMovies("a", 64, 50000)
	phase7SetItemDates(rows, old, old)
	a := newPhase6MockSource(t, "a", rows)
	a.deltaFilterSupported = true
	b := newPhase6MockSource(t, "b", nil)
	withPhase6MockApp(t, a, b, func(app *App) {
		app.Scanner.now = func() time.Time { return now.Add(-30 * time.Minute) }
		phase7CompleteFull(t, app)
		appended := phase6SampleMovies("a-new", 1, 890123)
		phase7SetItemDates(appended, now.Add(-10*time.Minute), now.Add(-10*time.Minute))
		a.mu.Lock()
		a.rows = append(a.rows, appended...)
		a.calls = nil
		a.mu.Unlock()
		app.Scanner.now = func() time.Time { return now }
		run := phase7BeginDelta(t, app)
		if err := app.scannerRunDelta(context.Background(), "server-a", run.ID); err != nil {
			t.Fatal(err)
		}
		w, ok, err := app.scannerDeltaWindow("server-a", run.ID, "library-a")
		if err != nil || !ok || w.Mode != "filtered" {
			t.Fatalf("reliable filter rejected: %+v %v", w, err)
		}
		libraries, err := app.Scanner.librariesLocked("server-a")
		if err != nil || len(libraries) != 1 || libraries[0]["capability"] != "filtered" {
			t.Fatalf("successful filtered Delta did not persist capability: %+v %v", libraries, err)
		}
		calls := a.onlyScannerCalls()
		filtered := 0
		for _, c := range calls {
			if strings.HasSuffix(c.Path, "/Items") && c.Query.Get("Limit") == "60" {
				filtered++
				if c.Query.Get("MinDateLastSaved") == "" {
					t.Fatal("filtered Delta request lost frozen cutoff")
				}
			}
		}
		if filtered != 1 {
			t.Fatalf("expected one filtered delta page, got %d", filtered)
		}
		if id := app.IDStore.ResolveMergeMember("server-a", "a-new-000", ""); id == "" {
			t.Fatal("filtered delta did not register newly added movie")
		}
	})
}
func TestPhase7UntrustedFilterFallsBack(t *testing.T) {
	now := time.Now().UTC()
	rows := phase6SampleMovies("a", 145, 61100)
	phase7SetItemDates(rows, now.Add(-72*time.Hour), now.Add(-72*time.Hour))
	a := newPhase6MockSource(t, "a", rows)
	// a.deltaFilterSupported=false; fake upstream ignores MinDateLastSaved.
	b := newPhase6MockSource(t, "b", nil)
	withPhase6MockApp(t, a, b, func(app *App) {
		app.Scanner.now = func() time.Time { return now.Add(-time.Hour) }
		phase7CompleteFull(t, app)
		app.Scanner.now = func() time.Time { return now }
		run := phase7BeginDelta(t, app)
		if err := app.scannerRunDelta(context.Background(), "server-a", run.ID); err != nil {
			t.Fatal(err)
		}
		w, ok, err := app.scannerDeltaWindow("server-a", run.ID, "library-a")
		if err != nil || !ok || w.Mode != "fallback" {
			t.Fatalf("ignored MinDateLastSaved was trusted %+v %v", w, err)
		}
		libraries, err := app.Scanner.librariesLocked("server-a")
		if err != nil || len(libraries) != 1 || libraries[0]["capability"] != "fallback" {
			t.Fatalf("successful fallback Delta did not persist capability: %+v %v", libraries, err)
		}
		for _, c := range a.onlyScannerCalls() {
			if c.Query.Get("Limit") == "60" && c.Query.Get("MinDateLastSaved") != "" {
				t.Fatal("fallback unexpectedly filtering")
			}
		}
	})
}
func TestPhase7DeltaStopKeepsPreviousCursorAndNoCatchUp(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-24 * time.Hour)
	rows := phase6SampleMovies("a", 1, 71200)
	phase7SetItemDates(rows, old, old)
	a := newPhase6MockSource(t, "a", rows)
	b := newPhase6MockSource(t, "b", nil)
	withPhase6MockApp(t, a, b, func(app *App) {
		app.Scanner.now = func() time.Time { return now.Add(-time.Hour) }
		phase7CompleteFull(t, app)
		prior, _, err := app.scannerCommittedLibrary("server-a", "library-a")
		if err != nil {
			t.Fatal(err)
		}
		run := phase7BeginDelta(t, app)
		if _, err := app.scannerCommand("server-a", "stop"); err != nil {
			t.Fatal(err)
		}
		if err := app.scannerRunDelta(context.Background(), "server-a", run.ID); err == nil {
			t.Fatal("stopped Delta was executable")
		}
		after, _, err := app.scannerCommittedLibrary("server-a", "library-a")
		if err != nil || after != prior {
			t.Fatalf("stopped run moved cursor %q => %q", prior, after)
		}
		sh, _ := time.LoadLocation("Asia/Shanghai")
		missed := time.Date(2026, 10, 11, 7, 0, 0, 0, sh)
		reason, err := app.scannerDailySlot("server-a", missed)
		if err != nil || reason != "outside_slot" {
			t.Fatalf("startup caught up missed 05:00 %q %v", reason, err)
		}
		count := 0
		stmt, e := app.Scanner.db.prepare("SELECT count(*) FROM scanner_schedule_slots")
		if e != nil {
			t.Fatal(e)
		}
		present, e := stmt.step()
		if present {
			count = stmt.columnInt(0)
		}
		stmt.finalize()
		if e != nil || count != 0 {
			t.Fatalf("missed slot was consumed %d %v", count, e)
		}
	})
}
func TestPhase7OverlapIsFiveMinutes(t *testing.T) {
	prior := time.Date(2026, 10, 10, 5, 0, 0, 0, time.UTC)
	got, err := scannerOverlap(phase7Stamp(prior))
	if err != nil || !got.Equal(prior.Add(-5*time.Minute)) {
		t.Fatalf("overlap wrong: %v %v", got, err)
	}
	if _, err := scannerOverlap("broken"); err == nil {
		t.Fatal("invalid cursor was accepted")
	}
}
