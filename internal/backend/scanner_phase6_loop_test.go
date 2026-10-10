package backend

import (
	"context"
	"strings"
	"testing"
)

func TestPhase6RepeatedPageWithoutCursorMetadataFailsClosed(t *testing.T) {
	a := newPhase6MockSource(t, "a", phase6SampleMovies("a", 60, 34560))
	b := newPhase6MockSource(t, "b", nil)
	a.repeatPages = true
	withPhase6MockApp(t, a, b, func(app *App) {
		run := phase5Enable(t, app, "server-a")
		err := app.scannerRunFull(context.Background(), "server-a", run.ID)
		if err == nil || !strings.Contains(err.Error(), "repeated") {
			t.Fatalf("looping upstream was accepted: %v", err)
		}
		source, err := app.Scanner.sourceLocked("server-a")
		if err != nil || source.InitialFullCompleted {
			t.Fatalf("loop marked full complete: %+v %v", source, err)
		}
		checkpoints, err := app.Scanner.checkpointsLocked("server-a")
		if err != nil || len(checkpoints) != 1 || checkpoints[0].NextStartIndex != 60 {
			t.Fatalf("repeating page crossed checkpoint: %+v %v", checkpoints, err)
		}
		total := 0
		for _, call := range a.onlyScannerCalls() {
			if strings.HasSuffix(call.Path, "/Items") {
				total++
			}
		}
		if total != 2 {
			t.Fatalf("unexpected looping requests: %d", total)
		}
	})
}
