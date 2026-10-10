package backend

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPhase6StopRejectsLatePageAndCheckpoint(t *testing.T) {
	a := newPhase6MockSource(t, "a", phase6SampleMovies("a", 1, 889))
	b := newPhase6MockSource(t, "b", nil)
	a.pageStarted = make(chan struct{})
	a.pageContinue = make(chan struct{})
	withPhase6MockApp(t, a, b, func(app *App) {
		run := phase5Enable(t, app, "server-a")
		finished := make(chan error, 1)
		go func() { finished <- app.scannerRunFull(context.Background(), "server-a", run.ID) }()
		select {
		case <-a.pageStarted:
		case <-time.After(12 * time.Second):
			t.Fatal("scan request not started")
		}
		if _, err := app.scannerCommand("server-a", "stop"); err != nil {
			t.Fatal(err)
		}
		close(a.pageContinue)
		select {
		case err := <-finished:
			if err == nil {
				t.Fatal("stopped run completed")
			}
		case <-time.After(12 * time.Second):
			t.Fatal("stopped page hung")
		}
		if n := task6SQLCount(t, app.IDStore.db, "scanner_checkpoints"); n != 0 {
			t.Fatalf("late checkpoint committed: %d", n)
		}
		if id := app.IDStore.ResolveMergeMember("server-a", "a-000", ""); id != "" {
			t.Fatalf("late media mapping committed: %s", id)
		}
	})
}
func TestPhase6RestartResumesCommittedOffset(t *testing.T) {
	a := newPhase6MockSource(t, "a", phase6SampleMovies("a", 124, 72000))
	b := newPhase6MockSource(t, "b", nil)
	withPhase6MockApp(t, a, b, func(app *App) {
		run := phase5Enable(t, app, "server-a")
		first := true
		app.Scanner.wait = func(context.Context, time.Duration) error {
			if first {
				first = false
				return context.Canceled
			}
			return nil
		}
		if err := app.scannerRunFull(context.Background(), "server-a", run.ID); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected simulated crash: %v", err)
		}
		before, err := app.scannerState("server-a")
		if err != nil {
			t.Fatal(err)
		}
		cps := before["checkpoints"].([]scanLibraryCheckpoint)
		if len(cps) != 1 || cps[0].NextStartIndex != 60 {
			t.Fatalf("lost first checkpoint %+v", cps)
		}
		if err := app.recoverScannerState(); err != nil {
			t.Fatal(err)
		}
		app.Scanner.wait = func(context.Context, time.Duration) error { return nil }
		if err := app.scannerRunFull(context.Background(), "server-a", run.ID); err != nil {
			t.Fatal(err)
		}
		count := map[string]int{}
		for _, call := range a.onlyScannerCalls() {
			if strings.HasSuffix(call.Path, "/Items") {
				count[call.Query.Get("StartIndex")]++
			}
		}
		if count["0"] != 1 || count["60"] != 1 || count["120"] != 1 {
			t.Fatalf("checkpoint did not resume exact next offset: %+v", count)
		}
	})
}
func TestPhase6HTTPPermissionCircuitAndRetry(t *testing.T) {
	for _, tc := range []struct {
		code  int
		state string
	}{{403, scanCircuitOpen}, {429, scanBackoff}, {500, scanCircuitOpen}} {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			a := newPhase6MockSource(t, "a", phase6SampleMovies("a", 2, 86000))
			b := newPhase6MockSource(t, "b", nil)
			a.failStatus = tc.code
			withPhase6MockApp(t, a, b, func(app *App) {
				run := phase5Enable(t, app, "server-a")
				if err := app.scannerRunFull(context.Background(), "server-a", run.ID); err == nil {
					t.Fatal("error response ignored")
				}
				state, err := app.Scanner.runLocked("server-a", true)
				if err != nil || state == nil || state.State != tc.state {
					t.Fatalf("HTTP %d state %+v err=%v", tc.code, state, err)
				}
				if n := task6SQLCount(t, app.IDStore.db, "scanner_checkpoints"); n != 0 {
					t.Fatal("error page committed checkpoint")
				}
			})
		})
	}
}
