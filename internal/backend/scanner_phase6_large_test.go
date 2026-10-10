package backend

import (
	"context"
	"testing"
)

func TestPhase6IncompleteViewsInventoryFailsClosed(t *testing.T) {
	a := newPhase6MockSource(t, "a", phase6SampleMovies("a", 1, 100))
	b := newPhase6MockSource(t, "b", nil)
	a.viewsTotalOverride = 8 // server announced eight Views but returned just two
	withPhase6MockApp(t, a, b, func(app *App) {
		run := phase5Enable(t, app, "server-a")
		if err := app.scannerRunFull(context.Background(), "server-a", run.ID); err == nil {
			t.Fatal("incomplete /Views was treated as successful")
		}
		s, err := app.Scanner.sourceLocked("server-a")
		if err != nil || s.InitialFullCompleted {
			t.Fatalf("premature full completion %+v %v", s, err)
		}
		if n := task6SQLCount(t, app.IDStore.db, "scanner_libraries"); n != 0 {
			t.Fatalf("truncated inventory committed %d library cursors", n)
		}
		for _, call := range a.onlyScannerCalls() {
			if call.Path == "/Users/user-a/Items" {
				t.Fatal("incomplete inventory proceeded to scan items")
			}
		}
	})
}

func TestPhase6OverFiveHundredItemsProgressWithoutGlobalCap(t *testing.T) {
	a := newPhase6MockSource(t, "a", phase6SampleMovies("a", 541, 20000))
	b := newPhase6MockSource(t, "b", nil)
	withPhase6MockApp(t, a, b, func(app *App) {
		run := phase5Enable(t, app, "server-a")
		if err := app.scannerRunFull(context.Background(), "server-a", run.ID); err != nil {
			t.Fatal(err)
		}
		completed, err := app.Scanner.runLocked("server-a", false)
		if err != nil || completed.State != scanCompleted || completed.Items != 541 || completed.Pages != 10 {
			t.Fatalf("full scan 541 item result incorrect %+v %v", completed, err)
		}
		pageCount := 0
		for _, call := range a.onlyScannerCalls() {
			if call.Path == "/Users/user-a/Items" {
				if call.Query.Get("Limit") != "60" {
					t.Fatalf("scanner escaped 60 page bound: %v", call.Query)
				}
				pageCount++
			}
		}
		if pageCount != 10 {
			t.Fatalf("expected 10 bounded pages, got %d", pageCount)
		}
	})
}
