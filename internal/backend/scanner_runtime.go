package backend

import (
	"errors"
	"fmt"
	"time"
)

// scannerRuntimeEvent changes durable execution state, WITHOUT executing any
// upstream request. Phase 6 supplies the page worker and ActivityGate.
// State changes and page-control commands are serialized by scanner.mu.
func (a *App) scannerRuntimeEvent(source, runID, event string, retryAfter time.Time) error {
	if a.Scanner == nil {
		return errors.New("scanner persistence unavailable")
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	if a.lifecyclePending {
		return errScanStale
	}
	if !configuredSourceIDs(a.ConfigStore.Snapshot())[source] {
		return errScanStale
	}
	sourceGeneration := a.scannerGenerationSnapshot(source)
	return a.Scanner.db.withWriteTx(func() error {
		run, err := a.Scanner.runLocked(source, true)
		if err != nil {
			return err
		}
		if run == nil || run.ID != runID {
			return errScanNotFound
		}
		same := sourceGeneration == run.Generation
		if !same {
			return errScanStale
		}
		enabled, err := a.Scanner.enabledLocked()
		if err != nil {
			return err
		}
		settings, err := a.Scanner.sourceLocked(source)
		if err != nil {
			return err
		}
		next := ""
		switch event {
		case "dispatch":
			if run.State != scanQueued {
				return errScanTransition
			}
			if !enabled || !settings.AllowScan {
				return errScanDisabled
			}
			if !a.scannerSourceOnline(source) {
				return errors.New("upstream unavailable or unauthenticated")
			}
			next = scanScanning
		case "user_active":
			if run.State != scanScanning {
				return errScanTransition
			}
			next = scanPausedActivity
		case "activity_clear":
			if run.State != scanPausedActivity {
				return errScanTransition
			}
			// Caller (Phase6 ActivityGate) must prove 60s of upstream inactivity.
			next = scanQueued
		case "http401":
			if run.State != scanScanning {
				return errScanTransition
			}
			next = scanPausedPermission
		case "http403":
			if run.State != scanScanning {
				return errScanTransition
			}
			next = scanCircuitOpen
			if err := a.Scanner.db.execParams(`INSERT INTO scanner_circuits
     (source_id,failure_count,state,retry_at,last_error) VALUES(?,1,'open','','http403')
     ON CONFLICT(source_id) DO UPDATE SET failure_count=failure_count+1,
      state='open',retry_at='',last_error='http403'`, source); err != nil {
				return err
			}
		case "http429":
			if run.State != scanScanning {
				return errScanTransition
			}
			next = scanBackoff
			earliest := a.Scanner.now().Add(time.Hour)
			if retryAfter.After(earliest) {
				earliest = retryAfter
			}
			if err := a.Scanner.db.execParams(`INSERT INTO scanner_circuits
      (source_id,failure_count,state,retry_at,last_error)
      VALUES(?,1,'backoff',?,'http429')
      ON CONFLICT(source_id) DO UPDATE SET failure_count=failure_count+1,
       state='backoff',retry_at=excluded.retry_at,last_error='http429'`,
				source, earliest.UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}
		case "failed":
			if run.State != scanScanning && run.State != scanBackoff && run.State != scanQueued {
				return errScanTransition
			}
			next = scanFailed
		default:
			return fmt.Errorf("%w: unsupported event", errScanTransition)
		}
		if event == "http401" || event == "http403" || event == "http429" || event == "failed" {
			run.LastError = event
		}
		return a.Scanner.transitionSQL(run, next)
	})
}

// Record the run's frozen library set before a page is ever dispatched.
// A missing library must never be silently omitted from full completion.
func (a *App) scannerSetRunLibraries(source, runID string, libraries []string) error {
	if len(libraries) == 0 {
		return errScanTransition
	}
	if a.Scanner == nil {
		return errors.New("scanner unavailable")
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	if a.lifecyclePending || !configuredSourceIDs(a.ConfigStore.Snapshot())[source] {
		return errScanStale
	}
	return a.Scanner.db.withWriteTx(func() error {
		run, err := a.Scanner.runLocked(source, true)
		if err != nil {
			return err
		}
		if run == nil || run.ID != runID {
			return errScanNotFound
		}
		if run.State != scanQueued && run.State != scanScanning {
			return errScanTransition
		}
		// Target list immutable once first frozen, even before first page.
		stmt, err := a.Scanner.db.prepare(`SELECT count(*) FROM scanner_checkpoints WHERE run_id=?`)
		if err != nil {
			return err
		}
		if err = stmt.bindAll(runID); err != nil {
			stmt.finalize()
			return err
		}
		ok, err := stmt.step()
		count := 0
		if ok {
			count = stmt.columnInt(0)
		}
		stmt.finalize()
		if err != nil {
			return err
		}
		if count != 0 {
			return errScanTransition
		}
		// Once selected, the target library set is immutable, even when queued.
		requested := make(map[string]bool, len(libraries))
		for _, lib := range libraries {
			if lib == "" || requested[lib] {
				return errScanTransition
			}
			requested[lib] = true
		}
		frozen, err := a.Scanner.db.prepare("SELECT library_id FROM scanner_run_libraries WHERE run_id=?")
		if err != nil {
			return err
		}
		if err = frozen.bindAll(runID); err != nil {
			frozen.finalize()
			return err
		}
		existing := make(map[string]bool)
		for {
			present, err := frozen.step()
			if err != nil {
				frozen.finalize()
				return err
			}
			if !present {
				break
			}
			existing[frozen.columnText(0)] = true
		}
		frozen.finalize()
		if len(existing) > 0 {
			if len(existing) != len(requested) {
				return errScanTransition
			}
			for lib := range requested {
				if !existing[lib] {
					return errScanTransition
				}
			}
			return nil
		}
		for _, lib := range libraries {
			if lib == "" {
				return errScanTransition
			}
			if err := a.Scanner.db.execParams(`INSERT OR IGNORE INTO scanner_run_libraries
     (run_id,source_id,library_id,state) VALUES (?,?,?,'queued')`, runID, source, lib); err != nil {
				return err
			}
		}
		return nil
	})
}

// Stage one completed library and its safe watermark within its run only.
// Cross-run cursors advance atomically when ALL frozen libraries complete.
// Full uses the library's start-of-full watermark, not completion time.
// Caller must have durable page/mapping commits before invoking this method.
func (a *App) scannerCompleteLibrary(source, runID, library, safeWatermark string) error {
	if a.Scanner == nil {
		return errors.New("scanner unavailable")
	}
	if library == "" || safeWatermark == "" {
		return errScanTransition
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	if a.lifecyclePending || !configuredSourceIDs(a.ConfigStore.Snapshot())[source] {
		return errScanStale
	}
	sourceGeneration := a.scannerGenerationSnapshot(source)
	return a.Scanner.db.withWriteTx(func() error {
		run, err := a.Scanner.runLocked(source, true)
		if err != nil {
			return err
		}
		if run == nil || run.ID != runID {
			return errScanNotFound
		}
		valid := sourceGeneration == run.Generation
		if !valid {
			return errScanStale
		}
		stmt, err := a.Scanner.db.prepare(`SELECT state,pending_watermark FROM scanner_run_libraries WHERE run_id=? AND library_id=?`)
		if err != nil {
			return err
		}
		if err := stmt.bindAll(runID, library); err != nil {
			stmt.finalize()
			return err
		}
		ok, err := stmt.step()
		state, priorWatermark := "", ""
		if ok {
			state, priorWatermark = stmt.columnText(0), stmt.columnText(1)
		}
		stmt.finalize()
		if err != nil {
			return err
		}
		if !ok {
			return errScanNotFound
		}
		if state == "completed" {
			if priorWatermark != safeWatermark {
				return errScanTransition
			}
			return nil
		}
		if err := a.Scanner.db.execParams(`UPDATE scanner_run_libraries SET state='completed',pending_watermark=? WHERE run_id=? AND library_id=?`,
			safeWatermark, runID, library); err != nil {
			return err
		}
		// Remain staged until scannerFinishRun atomically commits all cursors.
		return nil
	})
}

// A run can only become completed if ALL frozen target libraries finished.
// No initial_full_completed or delta cursor may advance on failed/stopped runs.
func (a *App) scannerFinishRun(source, runID string) error {
	if a.Scanner == nil {
		return errors.New("scanner unavailable")
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	if a.lifecyclePending || !configuredSourceIDs(a.ConfigStore.Snapshot())[source] {
		return errScanStale
	}
	sourceGeneration := a.scannerGenerationSnapshot(source)
	return a.Scanner.db.withWriteTx(func() error {
		run, err := a.Scanner.runLocked(source, true)
		if err != nil {
			return err
		}
		if run == nil || run.ID != runID {
			return errScanNotFound
		}
		if run.State != scanScanning && run.State != scanQueued {
			return errScanTransition
		}
		same := sourceGeneration == run.Generation
		if !same {
			return errScanStale
		}
		stmt, err := a.Scanner.db.prepare(`SELECT count(*),
    coalesce(sum(CASE WHEN state IN ('completed','inactive') THEN 1 ELSE 0 END),0)
    FROM scanner_run_libraries WHERE run_id=?`)
		if err != nil {
			return err
		}
		if err = stmt.bindAll(runID); err != nil {
			stmt.finalize()
			return err
		}
		ok, err := stmt.step()
		total, done := 0, 0
		if ok {
			total = stmt.columnInt(0)
			done = stmt.columnInt(1)
		}
		stmt.finalize()
		if err != nil {
			return err
		}
		if total == 0 || done != total {
			return errScanTransition
		}
		// Commit every library cursor ONLY after the entire run is complete.
		// A failed/stopped run can retain checkpoints but never advance delta.
		full := 0
		if run.Type == "full" || run.Type == "force_full" || run.Type == "library_initial" {
			full = 1
		}
		stage, err := a.Scanner.db.prepare("SELECT library_id,pending_watermark,state FROM scanner_run_libraries WHERE run_id=?")
		if err != nil {
			return err
		}
		if err = stage.bindAll(runID); err != nil {
			stage.finalize()
			return err
		}
		type stagedCursor struct{ library, watermark, state string }
		cursors := make([]stagedCursor, 0, total)
		for {
			more, err := stage.step()
			if err != nil {
				stage.finalize()
				return err
			}
			if !more {
				break
			}
			library, watermark, state := stage.columnText(0), stage.columnText(1), stage.columnText(2)
			if library == "" || (state != "inactive" && watermark == "") {
				stage.finalize()
				return errScanTransition
			}
			cursors = append(cursors, stagedCursor{library, watermark, state})
		}
		stage.finalize()
		if len(cursors) != total {
			return errScanTransition
		}
		// A complete full inventory may mark no-longer-visible libraries inactive;
		// partial/failed/stopped scans must never apply inventory absence.
		// Both Full and Delta first froze a complete /Views inventory.
		// Absence affects ONLY scanner library visibility, never old media IDs
		// or work identity. A failed run cannot reach this transaction.
		if err := a.Scanner.db.execParams("UPDATE scanner_libraries SET inactive=1 WHERE source_id=?", source); err != nil {
			return err
		}
		for _, cursor := range cursors {
			if cursor.state == "inactive" {
				continue
			} // no committed cursor advances on disappearing library
			modeForLibrary := ""
			libraryInitial := full
			if run.Type == "delta" {
				stmt, err := a.Scanner.db.prepare("SELECT mode FROM scanner_delta_windows WHERE run_id=? AND library_id=?")
				if err != nil {
					return err
				}
				if err = stmt.bindAll(runID, cursor.library); err != nil {
					stmt.finalize()
					return err
				}
				found, err := stmt.step()
				mode := ""
				if found {
					mode = stmt.columnText(0)
				}
				stmt.finalize()
				if err != nil {
					return err
				}
				// Legacy Phase5 direct-control tests may manually complete a
				// delta without execution; those cursors still remain delta-only.
				modeForLibrary = mode
				if mode == "library_initial" {
					libraryInitial = 1
				}
			}
			fullSafe := ""
			if libraryInitial == 1 {
				fullSafe = cursor.watermark
			}
			if err := a.Scanner.db.execParams("INSERT INTO scanner_libraries (source_id,library_id,committed_cursor,initial_completed,full_safe_watermark,inactive) VALUES(?,?,?,?,?,0) ON CONFLICT(source_id,library_id) DO UPDATE SET committed_cursor=excluded.committed_cursor, initial_completed=MAX(initial_completed,excluded.initial_completed), full_safe_watermark=CASE WHEN excluded.initial_completed=1 THEN excluded.full_safe_watermark ELSE scanner_libraries.full_safe_watermark END,inactive=0", source, cursor.library, cursor.watermark, libraryInitial, fullSafe); err != nil {
				return err
			}
			if modeForLibrary == "filtered" || modeForLibrary == "fallback" {
				if err := a.Scanner.db.execParams("UPDATE scanner_libraries SET capability=? WHERE source_id=? AND library_id=?", modeForLibrary, source, cursor.library); err != nil {
					return err
				}
			}
		}
		if err := a.Scanner.transitionSQL(run, scanCompleted); err != nil {
			return err
		}
		if err := a.Scanner.db.execParams(`INSERT INTO scanner_sources
    (source_id,initial_full_completed,last_completed_at) VALUES(?,?,?)
    ON CONFLICT(source_id) DO UPDATE SET initial_full_completed=MAX(scanner_sources.initial_full_completed,excluded.initial_full_completed),
    last_completed_at=excluded.last_completed_at`, source, full, run.UpdatedAt); err != nil {
			return err
		}
		return nil
	})
}
