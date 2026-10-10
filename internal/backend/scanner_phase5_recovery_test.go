package backend

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

func phase5Enable(t *testing.T, a *App, source string) *scanRun {
	t.Helper()
	if err := a.setScannerGlobal(true); err != nil {
		t.Fatal(err)
	}
	if err := a.setScannerSource(source, true); err != nil {
		t.Fatal(err)
	}
	run, err := a.scannerCommand(source, "start")
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestPhase5RuntimeLibraryCompletionAndWatermark(t *testing.T) {
	withTask6HTTPFixture(t, nil, nil, func(f *task6HTTPFixture) {
		a := f.app
		run := phase5Enable(t, a, "server-a")
		if err := a.scannerSetRunLibraries("server-a", run.ID, []string{"library-1", "library-2"}); err != nil {
			t.Fatal(err)
		}
		if err := a.scannerFinishRun("server-a", run.ID); !errors.Is(err, errScanTransition) {
			t.Fatalf("completed without libraries: %v", err)
		}
		if err := a.scannerRuntimeEvent("server-a", run.ID, "dispatch", time.Time{}); err != nil {
			t.Fatal("dispatch", err)
		}
		if err := a.scannerRuntimeEvent("server-a", run.ID, "user_active", time.Time{}); err != nil {
			t.Fatal("activity", err)
		}
		state, _ := a.Scanner.runLocked("server-a", true)
		if state.State != scanPausedActivity {
			t.Fatalf("user activity %s", state.State)
		}
		if err := a.scannerRuntimeEvent("server-a", run.ID, "activity_clear", time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err := a.scannerRuntimeEvent("server-a", run.ID, "dispatch", time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err := a.scannerRecordCommittedPage("server-a", run.ID, "library-1", 0, 60, 42); err != nil {
			t.Fatal(err)
		}
		safe := "2026-10-10T01:00:00Z"
		if err := a.scannerCompleteLibrary("server-a", run.ID, "library-1", safe); err != nil {
			t.Fatal(err)
		}
		// One library finishing cannot advance the shared delta cursor while
		// the overall run still has unfinished libraries.
		beforeFinish, err := a.scannerState("server-a")
		if err != nil {
			t.Fatal(err)
		}
		if committed := beforeFinish["libraries"].([]map[string]any); len(committed) != 0 {
			t.Fatalf("premature cursor commit: %+v", committed)
		}
		if err := a.scannerFinishRun("server-a", run.ID); !errors.Is(err, errScanTransition) {
			t.Fatalf("partial full marked completed %v", err)
		}
		if err := a.scannerCompleteLibrary("server-a", run.ID, "library-2", safe); err != nil {
			t.Fatal(err)
		}
		if err := a.scannerFinishRun("server-a", run.ID); err != nil {
			t.Fatal("full completion", err)
		}
		settings, err := a.Scanner.sourceLocked("server-a")
		if err != nil || !settings.InitialFullCompleted {
			t.Fatalf("full not marked complete %+v %v", settings, err)
		}
		state, err = a.scannerCommand("server-a", "start")
		if err != nil || state.Type != "delta" {
			t.Fatalf("subsequent task is not delta %+v %v", state, err)
		}
		status, err := a.scannerState("server-a")
		if err != nil {
			t.Fatal(err)
		}
		libraries := status["libraries"].([]map[string]any)
		if len(libraries) != 2 {
			t.Fatalf("library status rows %d", len(libraries))
		}
		for _, lib := range libraries {
			if lib["committedCursor"] != safe {
				t.Fatal("unsafe full-finish watermark", lib)
			}
		}
	})
}

func TestPhase5CrashRecoverySameRunAndPage(t *testing.T) {
	withTask6HTTPFixture(t, nil, nil, func(f *task6HTTPFixture) {
		a := f.app
		run := phase5Enable(t, a, "server-a")
		if err := a.scannerSetRunLibraries("server-a", run.ID, []string{"library-a"}); err != nil {
			t.Fatal(err)
		}
		if err := a.scannerRuntimeEvent("server-a", run.ID, "dispatch", time.Time{}); err != nil {
			t.Fatal(err)
		}
		if err := a.scannerRecordCommittedPage("server-a", run.ID, "library-a", 0, 60, 60); err != nil {
			t.Fatal(err)
		}
		before, err := a.Scanner.runLocked("server-a", true)
		if err != nil {
			t.Fatal(err)
		}
		a.Scanner, err = newScannerControl(a.IDStore.DB()) // simulate process reinitialization over durable sqlite
		if err != nil {
			t.Fatal(err)
		}
		if err := a.recoverScannerState(); err != nil {
			t.Fatal(err)
		}
		after, err := a.Scanner.runLocked("server-a", true)
		if err != nil || after.ID != run.ID || after.State != scanQueued || !after.Recovering {
			t.Fatalf("crash recovery changed run %+v %v", after, err)
		}
		if err := a.recoverScannerState(); err != nil {
			t.Fatal(err)
		}
		repeat, _ := a.Scanner.runLocked("server-a", true)
		if repeat.Revision != after.Revision {
			t.Fatalf("repeat recovery changed revision %d -> %d", after.Revision, repeat.Revision)
		}
		if repeat.Pages != before.Pages || repeat.Items != before.Items {
			t.Fatal("recovery lost progress")
		}
		status, err := a.scannerState("server-a")
		if err != nil {
			t.Fatal(err)
		}
		cps := status["checkpoints"].([]scanLibraryCheckpoint)
		if len(cps) != 1 || cps[0].NextStartIndex != 60 {
			t.Fatalf("recovery lost committed cursor %+v", cps)
		}
		if n := f.upstream[0].count("/Items"); n != 0 {
			t.Fatal("recovery started active scanner requests")
		}
	})
}

func TestPhase5SQLiteReopenPersistsScannerControls(t *testing.T) {
	dir := t.TempDir()
	store, err := NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	control, err := newScannerControl(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err := control.db.writeParams(`UPDATE scanner_meta SET value='1' WHERE key='scan_enabled'`); err != nil {
		t.Fatal(err)
	}
	if err := control.db.writeParams(`INSERT INTO scanner_sources(source_id,allow_scan) VALUES ('server-a',1)`); err != nil {
		t.Fatal(err)
	}
	if err := control.db.withWriteTx(func() error {
		_, err := control.createRunSQL("server-a", "full", 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	control, err = newScannerControl(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := control.enabledLocked()
	if err != nil || !enabled {
		t.Fatal("lost global enable", err)
	}
	source, err := control.sourceLocked("server-a")
	if err != nil || !source.AllowScan {
		t.Fatal("lost per-source allow scan", err)
	}
	run, err := control.runLocked("server-a", true)
	if err != nil || run == nil || run.State != scanQueued {
		t.Fatalf("lost pending run %+v %v", run, err)
	}
}

func TestPhase5AutoSlotConcurrentExactlyOnce(t *testing.T) {
	withTask6HTTPFixture(t, nil, nil, func(f *task6HTTPFixture) {
		a := f.app
		_ = phase5Enable(t, a, "server-a")
		if _, err := a.scannerCommand("server-a", "stop"); err != nil {
			t.Fatal(err)
		}
		if err := a.Scanner.db.writeParams(`UPDATE scanner_sources SET initial_full_completed=1 WHERE source_id=?`, "server-a"); err != nil {
			t.Fatal(err)
		}
		moment := time.Date(2026, 10, 12, 5, 0, 0, 0, time.FixedZone("CST", 8*3600))
		const parallel = 16
		var wg sync.WaitGroup
		errs := make(chan error, parallel)
		for i := 0; i < parallel; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); _, err := a.scannerDailySlot("server-a", moment); errs <- err }()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if n := task6SQLCount(t, a.IDStore.db, "scanner_schedule_slots"); n != 1 {
			t.Fatalf("duplicate schedule slots %d", n)
		}
		if n := task6SQLCount(t, a.IDStore.db, "scanner_runs"); n != 2 {
			t.Fatalf("expected stopped full and exactly one delta, got %d", n)
		}
	})
}

func TestPhase5ScannerCleanupFailureRecoversAtomically(t *testing.T) {
	withTask6HTTPFixture(t, nil, nil, func(f *task6HTTPFixture) {
		a := f.app
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		run := phase5Enable(t, a, "server-b")
		if err := a.scannerSetRunLibraries("server-b", run.ID, []string{"lib-b"}); err != nil {
			t.Fatal(err)
		}
		if err := a.scannerRecordCommittedPage("server-b", run.ID, "lib-b", 0, 60, 60); err != nil {
			t.Fatal(err)
		}
		if err := a.IDStore.db.exec(`CREATE TRIGGER fail_scanner_cleanup
  BEFORE DELETE ON scanner_checkpoints BEGIN SELECT RAISE(ABORT,'forced scanner delete failure'); END`); err != nil {
			t.Fatal(err)
		}
		rr := doAuthJSON(t, f.handler, http.MethodDelete, "/admin/api/upstream/server-b", nil, admin)
		if rr.Code != 500 || !a.lifecyclePending {
			t.Fatalf("expected cleanup pending %d %s", rr.Code, rr.Body.String())
		}
		if got := phase3ESourceVersion(a.IDStore, "server-b"); got != 1 {
			t.Fatal("failed scan cleanup advanced generation", got)
		}
		if n := task6SQLCount(t, a.IDStore.db, "scanner_checkpoints"); n != 1 {
			t.Fatal("non-atomic failure lost scan checkpoint")
		}
		if _, err := a.scannerCommand("server-b", "start"); err == nil {
			t.Fatal("accepted scan during cleanup pending")
		}
		if err := a.IDStore.db.exec("DROP TRIGGER fail_scanner_cleanup"); err != nil {
			t.Fatal(err)
		}
		a.watchLifecycleMu.Lock()
		err := a.recoverWatchLifecycleLocked()
		a.watchLifecycleMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if got := phase3ESourceVersion(a.IDStore, "server-b"); got != 2 {
			t.Fatal("source epoch after recovery", got)
		}
		if n := task6SQLCount(t, a.IDStore.db, "scanner_checkpoints"); n != 0 {
			t.Fatal("scanner checkpoint survived recovered delete")
		}
		if n := task6SQLCount(t, a.IDStore.db, "scanner_runs"); n != 0 {
			t.Fatal("scanner run survived recovered delete")
		}
	})
}
