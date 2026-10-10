package backend

import (
	"testing"
	"time"
)

func TestPhase5DeltaCursorPromotesOnlyAtWholeRunSuccess(t *testing.T) {
	withTask6HTTPFixture(t, nil, nil, func(f *task6HTTPFixture) {
		a := f.app
		a.Scanner.now = func() time.Time { return time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC) }
		full := phase5Enable(t, a, "server-a")
		if err := a.scannerSetRunLibraries("server-a", full.ID, []string{"movies", "series"}); err != nil {
			t.Fatal(err)
		}
		fullSafe := "2026-10-10T00:55:00Z"
		for _, lib := range []string{"movies", "series"} {
			if err := a.scannerCompleteLibrary("server-a", full.ID, lib, fullSafe); err != nil {
				t.Fatal(err)
			}
		}
		if err := a.scannerFinishRun("server-a", full.ID); err != nil {
			t.Fatal(err)
		}
		before, err := a.scannerState("server-a")
		if err != nil {
			t.Fatal(err)
		}
		previous := before["source"].(scanSourceSettings).LastCompletedAt
		a.Scanner.now = func() time.Time { return time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC) }
		delta, err := a.scannerCommand("server-a", "start")
		if err != nil || delta.Type != "delta" {
			t.Fatalf("expected delta, got %+v err=%v", delta, err)
		}
		if err := a.scannerSetRunLibraries("server-a", delta.ID, []string{"movies", "series"}); err != nil {
			t.Fatal(err)
		}
		newSafe := "2026-10-11T00:55:00Z"
		if err := a.scannerCompleteLibrary("server-a", delta.ID, "movies", newSafe); err != nil {
			t.Fatal(err)
		}
		midway, err := a.scannerState("server-a")
		if err != nil {
			t.Fatal(err)
		}
		for _, lib := range midway["libraries"].([]map[string]any) {
			if lib["committedCursor"] != fullSafe {
				t.Fatalf("partial delta advanced cursor: %+v", lib)
			}
		}
		if err := a.scannerCompleteLibrary("server-a", delta.ID, "series", newSafe); err != nil {
			t.Fatal(err)
		}
		if err := a.scannerFinishRun("server-a", delta.ID); err != nil {
			t.Fatal(err)
		}
		after, err := a.scannerState("server-a")
		if err != nil {
			t.Fatal(err)
		}
		source := after["source"].(scanSourceSettings)
		if !source.InitialFullCompleted || source.LastCompletedAt == previous || source.LastCompletedAt == "" {
			t.Fatalf("delta did not update source completion time: %+v", source)
		}
		for _, lib := range after["libraries"].([]map[string]any) {
			if lib["committedCursor"] != newSafe || lib["fullSafeWatermark"] != fullSafe {
				t.Fatalf("delta did not advance cursor while retaining full-safe watermark: %+v", lib)
			}
		}
	})
}
