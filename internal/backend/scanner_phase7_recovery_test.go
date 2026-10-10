package backend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPhase7NewLibraryGetsIndependentInitialInventory(t *testing.T) {
	now := time.Now().UTC()
	rows := phase6SampleMovies("a", 1, 75000)
	phase7SetItemDates(rows, now.Add(-24*time.Hour), now.Add(-24*time.Hour))
	a := newPhase6MockSource(t, "a", rows)
	b := newPhase6MockSource(t, "b", nil)
	withPhase6MockApp(t, a, b, func(app *App) {
		app.Scanner.now = func() time.Time { return now.Add(-time.Hour) }
		phase7CompleteFull(t, app)
		added := phase6SampleMovies("a-extra", 2, 76000)
		phase7SetItemDates(added, now.Add(-15*time.Minute), now.Add(-15*time.Minute))
		for _, item := range added {
			item["ParentId"] = "library-a-new"
		}
		a.mu.Lock()
		a.rows = append(a.rows, added...)
		a.libraries = append(a.libraries, map[string]any{"Id": "library-a-new", "CollectionType": "tvshows"})
		a.calls = nil
		a.mu.Unlock()
		app.Scanner.now = func() time.Time { return now }
		run := phase7BeginDelta(t, app)
		if err := app.scannerRunDelta(context.Background(), "server-a", run.ID); err != nil {
			t.Fatal(err)
		}
		libraries, err := app.Scanner.librariesLocked("server-a")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, lib := range libraries {
			if lib["libraryId"] == "library-a-new" {
				found = true
				if lib["initialCompleted"] != true || lib["fullSafeWatermark"] != run.StartedAt {
					t.Fatalf("new library borrowed old cursor or was not initially scanned: %+v", lib)
				}
			}
		}
		if !found {
			t.Fatal("new library did not enter scanner catalog")
		}
		for _, item := range added {
			id, _ := item["Id"].(string)
			if got := app.IDStore.ResolveMergeMember("server-a", id, ""); got == "" {
				t.Fatalf("new library work %q not indexed", id)
			}
		}
	})
}
func TestPhase7RestartRetainsFallbackTwoPageTail(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	rows := phase6SampleMovies("a", 300, 77000)
	for _, row := range rows {
		row["DateCreated"] = phase7Stamp(old)
	}
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
		paused := false
		app.Scanner.wait = func(_ context.Context, _ time.Duration) error {
			if !paused {
				paused = true
				return context.Canceled
			}
			return nil
		}
		if err := app.scannerRunDelta(context.Background(), "server-a", run.ID); !errors.Is(err, context.Canceled) {
			t.Fatalf("first page did not simulate interruption: %v", err)
		}
		w, found, err := app.scannerDeltaWindow("server-a", run.ID, "library-a")
		if err != nil || !found || w.TailRemaining != 2 {
			t.Fatalf("crash lost tail state: %+v %v", w, err)
		}
		if err := app.recoverScannerState(); err != nil {
			t.Fatal(err)
		}
		app.Scanner.wait = func(context.Context, time.Duration) error { return nil }
		if err := app.scannerRunDelta(context.Background(), "server-a", run.ID); err != nil {
			t.Fatal(err)
		}
		pages := map[string]int{}
		for _, call := range a.onlyScannerCalls() {
			if call.Query.Get("Limit") == "60" {
				pages[call.Query.Get("StartIndex")]++
			}
		}
		if pages["0"] != 1 || pages["60"] != 1 || pages["120"] != 1 || pages["180"] != 0 {
			t.Fatalf("fallback restarted outside saved validation tail: %v", pages)
		}
	})
}
func TestPhase7FallbackWithoutDatesDoesNotRecrawlForever(t *testing.T) {
	now := time.Now().UTC()
	rows := phase6SampleMovies("a", 120, 91234)
	a := newPhase6MockSource(t, "a", rows)
	b := newPhase6MockSource(t, "b", nil)
	withPhase6MockApp(t, a, b, func(app *App) {
		app.Scanner.now = func() time.Time { return now.Add(-time.Hour) }
		phase7CompleteFull(t, app)
		before, _, err := app.scannerCommittedLibrary("server-a", "library-a")
		if err != nil {
			t.Fatal(err)
		}
		app.Scanner.now = func() time.Time { return now }
		run := phase7BeginDelta(t, app)
		err = app.scannerRunDelta(context.Background(), "server-a", run.ID)
		if err == nil || !strings.Contains(err.Error(), "DateCreated") {
			t.Fatalf("missing DateCreated was not rejected: %v", err)
		}
		after, _, err := app.scannerCommittedLibrary("server-a", "library-a")
		if err != nil || after != before {
			t.Fatalf("missing timestamp advanced Delta cursor: %q => %q (%v)", before, after, err)
		}
	})
}

func TestPhase7AllLibrariesDisappearWithoutCursorLoss(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-24 * time.Hour)
	rows := phase6SampleMovies("a", 2, 88000)
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
		app.Scanner.now = func() time.Time { return now }
		run := phase7BeginDelta(t, app)
		a.mu.Lock()
		a.libraries = []map[string]any{{"Id": "audio-a", "CollectionType": "music"}}
		a.calls = nil
		a.mu.Unlock()
		if err := app.scannerRunDelta(context.Background(), "server-a", run.ID); err != nil {
			t.Fatal(err)
		}
		newCursor, _, err := app.scannerCommittedLibrary("server-a", "library-a")
		if err != nil || newCursor != prior {
			t.Fatalf("all-absent Delta mutated cursor: %q => %q %v", prior, newCursor, err)
		}
		libs, err := app.Scanner.librariesLocked("server-a")
		if err != nil || len(libs) != 1 || libs[0]["inactive"] != true {
			t.Fatalf("complete empty inventory did not mark only library inactive: %+v %v", libs, err)
		}
		if id := app.IDStore.ResolveMergeMember("server-a", "a-000", ""); id == "" {
			t.Fatal("empty library inventory wrongly deleted historical media mapping")
		}
	})
}

func TestPhase7DisappearedLibraryNeverAdvancesCursor(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-24 * time.Hour)
	rows := phase6SampleMovies("a", 1, 78000)
	phase7SetItemDates(rows, old, old)
	a := newPhase6MockSource(t, "a", rows)
	b := newPhase6MockSource(t, "b", nil)
	withPhase6MockApp(t, a, b, func(app *App) {
		app.Scanner.now = func() time.Time { return now.Add(-time.Hour) }
		phase7CompleteFull(t, app)
		cursor, _, err := app.scannerCommittedLibrary("server-a", "library-a")
		if err != nil {
			t.Fatal(err)
		}
		app.Scanner.now = func() time.Time { return now }
		run := phase7BeginDelta(t, app)
		// Freeze while the library is visible, then simulate revoking that library.
		if err := app.scannerSetRunLibraries("server-a", run.ID, []string{"library-a"}); err != nil {
			t.Fatal(err)
		}
		a.mu.Lock()
		a.libraries = []map[string]any{{"Id": "audio-a", "CollectionType": "music"}}
		a.calls = nil
		a.mu.Unlock()
		if err := app.scannerRunDelta(context.Background(), "server-a", run.ID); err != nil {
			t.Fatal(err)
		}
		after, _, err := app.scannerCommittedLibrary("server-a", "library-a")
		if err != nil || after != cursor {
			t.Fatalf("missing library advanced cursor: %q => %q %v", cursor, after, err)
		}
		libs, err := app.Scanner.librariesLocked("server-a")
		if err != nil {
			t.Fatal(err)
		}
		if len(libs) != 1 || libs[0]["inactive"] != true {
			t.Fatalf("missing library was not marked inactive: %+v", libs)
		}
		for _, call := range a.onlyScannerCalls() {
			if strings.Contains(call.Path, "/Episodes") || strings.Contains(call.Path, "/PlaybackInfo") {
				t.Fatalf("delta touched passive-only endpoint %s", call.Path)
			}
		}
	})
}
