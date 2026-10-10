package backend

import (
	"errors"
	"testing"
	"time"
)

func TestPhase5CircuitAndBackoffStateMachine(t *testing.T) {
	withTask6HTTPFixture(t, nil, nil, func(f *task6HTTPFixture) {
		a := f.app
		base := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
		a.Scanner.now = func() time.Time { return base }
		run := phase5Enable(t, a, "server-a")
		if err := a.scannerRuntimeEvent("server-a", run.ID, "dispatch", time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err := a.scannerRuntimeEvent("server-a", run.ID, "http429", base.Add(90*time.Minute)); err != nil {
			t.Fatal(err)
		}
		active, _ := a.Scanner.runLocked("server-a", true)
		if active.State != scanBackoff {
			t.Fatalf("429 not in backoff %s", active.State)
		}
		circuit, err := a.Scanner.circuitLocked("server-a")
		if err != nil {
			t.Fatal(err)
		}
		until, err := time.Parse(time.RFC3339Nano, circuit["retryAt"].(string))
		if err != nil || until.Before(base.Add(90*time.Minute)) {
			t.Fatalf("Retry-After not respected %+v %v", circuit, err)
		}
		if _, err := a.scannerCommand("server-a", "resume"); !errors.Is(err, errScanTransition) {
			t.Fatalf("429 bypassed cooldown: %v", err)
		}
		a.Scanner.now = func() time.Time { return base.Add(91 * time.Minute) }
		resumed, err := a.scannerCommand("server-a", "resume")
		if err != nil || resumed.ID != run.ID || resumed.State != scanQueued {
			t.Fatalf("cooldown resume %+v %v", resumed, err)
		}
		if err := a.scannerRuntimeEvent("server-a", run.ID, "dispatch", time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err := a.scannerRuntimeEvent("server-a", run.ID, "http403", time.Time{}); err != nil {
			t.Fatal(err)
		}
		active, _ = a.Scanner.runLocked("server-a", true)
		if active.State != scanCircuitOpen {
			t.Fatalf("403 failed to circuit-open %+v", active)
		}
		if err := a.scannerRuntimeEvent("server-a", run.ID, "dispatch", time.Time{}); !errors.Is(err, errScanTransition) {
			t.Fatalf("open circuit dispatched page %v", err)
		}
		circuit, _ = a.Scanner.circuitLocked("server-a")
		if circuit["state"] != "open" {
			t.Fatal("open state lost", circuit)
		}
		resumed, err = a.scannerCommand("server-a", "resume")
		if err != nil || resumed.State != scanQueued {
			t.Fatalf("manual circuit rearm %+v %v", resumed, err)
		}
		circuit, _ = a.Scanner.circuitLocked("server-a")
		if circuit["state"] != "closed" {
			t.Fatal("admin resume left circuit open", circuit)
		}
	})
}

func TestPhase5OfflineManualStartLeavesNoRun(t *testing.T) {
	withTask6HTTPFixture(t, nil, nil, func(f *task6HTTPFixture) {
		a := f.app
		if err := a.setScannerGlobal(true); err != nil {
			t.Fatal(err)
		}
		if err := a.setScannerSource("server-a", true); err != nil {
			t.Fatal(err)
		}
		a.Upstream.ClientByID("server-a").setOffline("test manual start requires session")
		if _, err := a.scannerCommand("server-a", "start"); err == nil {
			t.Fatal("offline start admitted")
		}
		if n := task6SQLCount(t, a.IDStore.db, "scanner_runs"); n != 0 {
			t.Fatalf("offline start inserted run: %d", n)
		}
		if n := f.upstream[0].count("/Items"); n != 0 {
			t.Fatal("offline control-plane emitted listing request")
		}
	})
}
