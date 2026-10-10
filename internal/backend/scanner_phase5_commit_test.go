package backend

import (
	"errors"
	"testing"
)

func TestPhase5ImmutableTargetsAndOnlyCommitCursorOnRunSuccess(t *testing.T) {
	withTask6HTTPFixture(t, nil, nil, func(f *task6HTTPFixture) {
		a := f.app
		run := phase5Enable(t, a, "server-a")
		frozen := []string{"movies", "series"}
		if err := a.scannerSetRunLibraries("server-a", run.ID, frozen); err != nil {
			t.Fatal(err)
		}
		if err := a.scannerSetRunLibraries("server-a", run.ID, frozen); err != nil {
			t.Fatalf("identical replay rejected: %v", err)
		}
		if err := a.scannerSetRunLibraries("server-a", run.ID, []string{"movies", "series", "extra"}); !errors.Is(err, errScanTransition) {
			t.Fatalf("mutated frozen targets admitted: %v", err)
		}
		if err := a.scannerSetRunLibraries("server-a", run.ID, []string{"movies"}); !errors.Is(err, errScanTransition) {
			t.Fatalf("removed frozen target admitted: %v", err)
		}
		if err := a.scannerRecordCommittedPage("server-a", run.ID, "unexpected-library", 0, 60, 1); !errors.Is(err, errScanTransition) {
			t.Fatalf("non-target checkpoint admitted: %v", err)
		}
		safe := "2026-10-10T01:00:00Z"
		if err := a.scannerCompleteLibrary("server-a", run.ID, "movies", safe); err != nil {
			t.Fatal(err)
		}
		if err := a.scannerRecordCommittedPage("server-a", run.ID, "movies", 0, 60, 1); !errors.Is(err, errScanTransition) {
			t.Fatalf("already completed library accepted another page: %v", err)
		}
		if err := a.scannerCompleteLibrary("server-a", run.ID, "movies", safe); err != nil {
			t.Fatalf("identical completed replay: %v", err)
		}
		if err := a.scannerCompleteLibrary("server-a", run.ID, "movies", "2099-01-01T00:00:00Z"); !errors.Is(err, errScanTransition) {
			t.Fatalf("mismatched watermark replay admitted: %v", err)
		}
		if n := task6SQLCount(t, a.IDStore.db, "scanner_libraries"); n != 0 {
			t.Fatalf("incomplete full advanced %d persistent library cursors", n)
		}
		if _, err := a.scannerCommand("server-a", "stop"); err != nil {
			t.Fatal(err)
		}
		if n := task6SQLCount(t, a.IDStore.db, "scanner_libraries"); n != 0 {
			t.Fatalf("stopped full retained %d prematurely committed cursors", n)
		}
		next, err := a.scannerCommand("server-a", "start")
		if err != nil {
			t.Fatal(err)
		}
		if next.Type != "full" {
			t.Fatalf("stopped initial full should restart with full, got %s", next.Type)
		}
		if err := a.scannerFinishRun("server-a", next.ID); !errors.Is(err, errScanTransition) {
			t.Fatalf("no frozen libraries still allowed completion: %v", err)
		}
	})
}
