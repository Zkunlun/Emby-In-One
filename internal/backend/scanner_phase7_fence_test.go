package backend

import (
	"context"
	"testing"
	"time"
)

// Stopping while a Delta capability probe is in flight must reject all late
// window/checkpoint commits, not merely prevent the next HTTP page.
func TestPhase7StopInFlightProbeCannotCommitDelta(t *testing.T) {
	now := time.Now().UTC()
	rows := phase6SampleMovies("a", 1, 99150)
	phase7SetItemDates(rows, now.Add(-24*time.Hour), now.Add(-24*time.Hour))
	source := newPhase6MockSource(t, "a", rows)
	other := newPhase6MockSource(t, "b", nil)
	withPhase6MockApp(t, source, other, func(app *App) {
		app.Scanner.now = func() time.Time { return now.Add(-time.Hour) }
		phase7CompleteFull(t, app)
		prior, _, err := app.scannerCommittedLibrary("server-a", "library-a")
		if err != nil {
			t.Fatal(err)
		}
		app.Scanner.now = func() time.Time { return now }
		run := phase7BeginDelta(t, app)
		started, release := make(chan struct{}), make(chan struct{})
		source.mu.Lock()
		source.pageStarted, source.pageContinue = started, release
		source.mu.Unlock()
		result := make(chan error, 1)
		go func() { result <- app.scannerRunDelta(context.Background(), "server-a", run.ID) }()
		select {
		case <-started:
		case <-time.After(15 * time.Second):
			close(release)
			t.Fatal("delta probe did not reach the upstream")
		}
		_, stopErr := app.scannerCommand("server-a", "stop")
		close(release)
		if stopErr != nil {
			t.Fatal(stopErr)
		}
		select {
		case runErr := <-result:
			if runErr == nil {
				t.Fatal("stopped in-flight probe finished Delta successfully")
			}
		case <-time.After(15 * time.Second):
			t.Fatal("stopped Delta worker did not release")
		}
		after, _, err := app.scannerCommittedLibrary("server-a", "library-a")
		if err != nil || after != prior {
			t.Fatalf("stopped probe changed safe cursor: %q -> %q; %v", prior, after, err)
		}
		if _, persisted, err := app.scannerDeltaWindow("server-a", run.ID, "library-a"); err != nil || persisted {
			t.Fatalf("stopped probe published a late delta window: persisted=%v err=%v", persisted, err)
		}
	})
}
