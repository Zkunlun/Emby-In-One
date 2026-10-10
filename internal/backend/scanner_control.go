package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// All entrypoints on App hold the lifecycle read lock before scanner.mu.
// There are intentionally no page-fetching workers in Phase 5.
func (a *App) scannerState(source string) (map[string]any, error) {
	if a.Scanner == nil {
		return nil, errors.New("scanner database unavailable")
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	if a.lifecyclePending {
		return nil, errors.New("source cleanup pending")
	}
	enabled, err := a.Scanner.enabledLocked()
	if err != nil {
		return nil, err
	}
	result := map[string]any{"scanEnabled": enabled}
	if source == "" {
		return result, nil
	}
	if !configuredSourceIDs(a.ConfigStore.Snapshot())[source] {
		return nil, errScanNotFound
	}
	settings, err := a.Scanner.sourceLocked(source)
	if err != nil {
		return nil, err
	}
	run, err := a.Scanner.runLocked(source, false)
	if err != nil {
		return nil, err
	}
	state := scanPending
	if settings.InitialFullCompleted {
		state = scanIdle
	}
	if run != nil {
		state = run.State
	}
	checkpoints, err := a.Scanner.checkpointsLocked(source)
	if err != nil {
		return nil, err
	}
	libraries, err := a.Scanner.librariesLocked(source)
	if err != nil {
		return nil, err
	}
	circuit, err := a.Scanner.circuitLocked(source)
	if err != nil {
		return nil, err
	}
	result["checkpoints"] = checkpoints
	result["libraries"] = libraries
	result["circuit"] = circuit
	result["source"] = settings
	result["state"] = state
	result["run"] = run
	return result, nil
}

func (a *App) setScannerGlobal(enabled bool) error {
	if a.Scanner == nil {
		return errors.New("scanner database unavailable")
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	if a.lifecyclePending {
		return errors.New("source cleanup pending")
	}
	flag := "0"
	if enabled {
		flag = "1"
	}
	return a.Scanner.db.withWriteTx(func() error {
		if err := a.Scanner.db.execParams(`UPDATE scanner_meta SET value=? WHERE key='scan_enabled'`, flag); err != nil {
			return err
		}
		if enabled {
			return nil
		}
		for source := range configuredSourceIDs(a.ConfigStore.Snapshot()) {
			run, err := a.Scanner.runLocked(source, true)
			if err != nil {
				return err
			}
			if err := a.Scanner.revokeRunSQL(run); err != nil {
				return err
			}
		}
		return nil
	})
}

func (a *App) setScannerSource(source string, allow bool) error {
	if a.Scanner == nil {
		return errors.New("scanner database unavailable")
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	if a.lifecyclePending {
		return errors.New("source cleanup pending")
	}
	if !configuredSourceIDs(a.ConfigStore.Snapshot())[source] {
		return errScanNotFound
	}
	flag := 0
	if allow {
		flag = 1
	}
	return a.Scanner.db.withWriteTx(func() error {
		if err := a.Scanner.db.execParams(`INSERT INTO scanner_sources(source_id,allow_scan)
   VALUES(?,?) ON CONFLICT(source_id) DO UPDATE SET allow_scan=excluded.allow_scan`, source, flag); err != nil {
			return err
		}
		if !allow {
			run, err := a.Scanner.runLocked(source, true)
			if err != nil {
				return err
			}
			return a.Scanner.revokeRunSQL(run)
		}
		return nil
	})
}

func (s *ScannerControl) revokeRunSQL(run *scanRun) error {
	if run == nil || run.State == scanPausedAdmin || run.State == scanStopped {
		return nil
	}
	// Administrative permission revocation must override a prior HTTP401
	// recovery hint, including when already paused_permission. Otherwise an
	// enable toggle could accidentally auto-resume a revoked run.
	if run.State == scanPausedPermission {
		run.LastError = "permission_revoked"
		run.Revision++
		run.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
		return s.saveRunSQL(*run)
	}
	run.LastError = "permission_revoked"
	return s.transitionSQL(run, scanPausedPermission)
}
func (a *App) scannerSourceOnline(source string) bool {
	if a.Upstream == nil {
		return false
	}
	client := a.Upstream.ClientByID(source)
	return client != nil && client.IsOnline()
}

func (a *App) scannerCommand(source, command string) (*scanRun, error) {
	if a.Scanner == nil {
		return nil, errors.New("scanner database unavailable")
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	if a.lifecyclePending {
		return nil, errors.New("source cleanup pending")
	}
	if source == "" || !configuredSourceIDs(a.ConfigStore.Snapshot())[source] {
		return nil, errScanNotFound
	}
	sourceGeneration := a.scannerGenerationSnapshot(source)
	result := (*scanRun)(nil)
	err := a.Scanner.db.withWriteTx(func() error {
		enabled, err := a.Scanner.enabledLocked()
		if err != nil {
			return err
		}
		settings, err := a.Scanner.sourceLocked(source)
		if err != nil {
			return err
		}
		active, err := a.Scanner.runLocked(source, true)
		if err != nil {
			return err
		}
		if command == "start" || command == "force_full" || command == "resume" {
			if !enabled || !settings.AllowScan {
				return errScanDisabled
			}
			if !a.scannerSourceOnline(source) {
				return errors.New("upstream unavailable or unauthenticated")
			}
		}
		// A 429 hold cannot be circumvented by Stop followed by Start.
		if command == "start" || command == "force_full" || command == "resume" {
			circuit, err := a.Scanner.circuitLocked(source)
			if err != nil {
				return err
			}
			if circuit["state"] == "backoff" {
				raw, _ := circuit["retryAt"].(string)
				until, err := time.Parse(time.RFC3339Nano, raw)
				if err != nil || a.Scanner.now().Before(until) {
					return errScanTransition
				}
			}
		}
		switch command {
		case "start", "force_full":
			if active != nil {
				return errScanConflict
			}
			typ := "full"
			if command == "force_full" {
				typ = "force_full"
			} else if settings.InitialFullCompleted {
				typ = "delta"
			}
			result, err = a.Scanner.createRunSQL(source, typ, sourceGeneration)
			if err == nil {
				err = a.Scanner.db.execParams(`UPDATE scanner_circuits SET state='closed',failure_count=0,retry_at='',last_error='' WHERE source_id=?`, source)
			}
			return err
		case "pause":
			if active == nil {
				return errScanNotFound
			}
			if active.State == scanPausedAdmin {
				result = active
				return nil
			}
			if active.State == scanPausedPermission {
				return errScanTransition
			}
			if err := a.Scanner.transitionSQL(active, scanPausedAdmin); err != nil {
				return err
			}
			result = active
			return nil
		case "resume":
			if active == nil {
				return errScanNotFound
			}
			switch active.State {
			case scanPausedAdmin, scanPausedPermission, scanPausedActivity, scanBackoff, scanCircuitOpen:
				valid := active.Generation == sourceGeneration
				if !valid {
					return errScanStale
				}
				if err := a.Scanner.transitionSQL(active, scanQueued); err != nil {
					return err
				}
				if err := a.Scanner.db.execParams(`UPDATE scanner_circuits SET state='closed',failure_count=0,retry_at='',last_error='' WHERE source_id=?`, source); err != nil {
					return err
				}
				result = active
				return nil
			default:
				return errScanTransition
			}
		case "stop":
			if active == nil {
				latest, err := a.Scanner.runLocked(source, false)
				if err != nil {
					return err
				}
				if latest != nil && latest.State == scanStopped {
					result = latest
					return nil
				}
				return errScanNotFound
			}
			if err := a.Scanner.transitionSQL(active, scanStopped); err != nil {
				return err
			}
			result = active
			return nil
		default:
			return errScanTransition
		}
	})
	return result, err
}

// Shanghai time is used regardless of the host timezone. This method is a
// deterministic tick entrypoint for Phase 7; Phase 5 does not start a ticker
// or dispatch pages. A missed 05:00 is never replayed at 07:00.
func (a *App) scannerDailySlot(source string, instant time.Time) (string, error) {
	if a.Scanner == nil {
		return "", errors.New("scanner database unavailable")
	}
	tz, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return "", err
	}
	local := instant.In(tz)
	if local.Hour() != 5 || local.Minute() != 0 {
		return "outside_slot", nil
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	if a.lifecyclePending {
		return "", errors.New("source cleanup pending")
	}
	if !configuredSourceIDs(a.ConfigStore.Snapshot())[source] {
		return "", errScanNotFound
	}
	sourceGeneration := a.scannerGenerationSnapshot(source)
	reason := "already_consumed"
	err = a.Scanner.db.withWriteTx(func() error {
		date := local.Format("2006-01-02")
		stmt, err := a.Scanner.db.prepare(`SELECT reason FROM scanner_schedule_slots
   WHERE source_id=? AND schedule_date=? AND trigger_type='auto_delta'`)
		if err != nil {
			return err
		}
		if err = stmt.bindAll(source, date); err != nil {
			stmt.finalize()
			return err
		}
		exists, err := stmt.step()
		if err != nil {
			stmt.finalize()
			return err
		}
		stmt.finalize()
		if exists {
			return nil
		}
		reason = "skipped_disabled"
		enabled, err := a.Scanner.enabledLocked()
		if err != nil {
			return err
		}
		settings, err := a.Scanner.sourceLocked(source)
		if err != nil {
			return err
		}
		active, err := a.Scanner.runLocked(source, true)
		if err != nil {
			return err
		}
		if enabled && settings.AllowScan && settings.InitialFullCompleted && active == nil &&
			a.scannerSourceOnline(source) {
			reason = "queued"
		}
		if active != nil {
			reason = "skipped_active"
		}
		if !settings.InitialFullCompleted {
			reason = "skipped_initial_pending"
		}
		if !a.scannerSourceOnline(source) {
			reason = "skipped_offline"
		}
		if !enabled || !settings.AllowScan {
			reason = "skipped_disabled"
		}
		runID := ""
		if reason == "queued" {
			run, err := a.Scanner.createRunSQL(source, "delta", sourceGeneration)
			if err != nil {
				return err
			}
			runID = run.ID
		}
		return a.Scanner.db.execParams(`INSERT INTO scanner_schedule_slots
   (source_id,schedule_date,trigger_type,run_id,reason)
   VALUES (?,?,'auto_delta',?,?)`, source, date, runID, reason)
	})
	return reason, err
}

// Recovery never creates a new run. Phase 5 leaves a recovered run queued for
// Phase 6's future worker; it performs no upstream network requests.
func (a *App) recoverScannerState() error {
	if a.Scanner == nil {
		return nil
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	if a.lifecyclePending {
		return errors.New("source cleanup pending")
	}
	sourceGenerations := a.IDStore.snapshotSourceGenerations(configuredSourceIDs(a.ConfigStore.Snapshot()))
	return a.Scanner.db.withWriteTx(func() error {
		enabled, err := a.Scanner.enabledLocked()
		if err != nil {
			return err
		}
		for source := range configuredSourceIDs(a.ConfigStore.Snapshot()) {
			run, err := a.Scanner.runLocked(source, true)
			if err != nil {
				return err
			}
			if run == nil {
				continue
			}
			if run.State != scanScanning && run.State != scanQueued {
				continue
			}
			if run.State == scanQueued && run.Recovering {
				continue
			}
			settings, err := a.Scanner.sourceLocked(source)
			if err != nil {
				return err
			}
			same := run.Generation == sourceGenerations[source]
			if !same {
				if err := a.Scanner.transitionSQL(run, scanStopped); err != nil {
					return err
				}
				continue
			}
			if !enabled || !settings.AllowScan {
				if err := a.Scanner.transitionSQL(run, scanPausedPermission); err != nil {
					return err
				}
				continue
			}
			run.Recovering = true
			if run.State != scanQueued {
				run.State = scanQueued
			}
			run.Revision++
			run.UpdatedAt = a.Scanner.now().UTC().Format(time.RFC3339Nano)
			if err := a.Scanner.saveRunSQL(*run); err != nil {
				return err
			}
		}
		return nil
	})
}

// Phase6 must persist MergeDiscoveryService identity first. This checkpoint
// write is a separate LAST operation and idempotent for replayed pages.
// Never call this on a page that failed identity persistence or validation.
func (a *App) scannerRecordCommittedPage(source, runID, library string, expectedOffset, nextOffset int64, itemCount int64, deltaTail ...int) error {
	if a.Scanner == nil {
		return errors.New("scanner unavailable")
	}
	if library == "" || expectedOffset < 0 || nextOffset <= expectedOffset || itemCount < 0 {
		return errScanTransition
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
		valid := run.Generation == sourceGeneration
		if !valid {
			return errScanStale
		}
		// A committed page must belong to a frozen target library.
		// Unknown and already completed libraries cannot accept more pages.
		membership, err := a.Scanner.db.prepare("SELECT state FROM scanner_run_libraries WHERE run_id=? AND library_id=?")
		if err != nil {
			return err
		}
		if err = membership.bindAll(runID, library); err != nil {
			membership.finalize()
			return err
		}
		found, err := membership.step()
		if err != nil {
			membership.finalize()
			return err
		}
		state := ""
		if found {
			state = membership.columnText(0)
		}
		membership.finalize()
		if !found || state == "completed" {
			return errScanTransition
		}
		stmt, err := a.Scanner.db.prepare(`SELECT next_start_index,items FROM scanner_checkpoints
  WHERE run_id=? AND library_id=?`)
		if err != nil {
			return err
		}
		if err = stmt.bindAll(runID, library); err != nil {
			stmt.finalize()
			return err
		}
		exists, err := stmt.step()
		if err != nil {
			stmt.finalize()
			return err
		}
		current := int64(0)
		currentItems := int64(0)
		if exists {
			current = stmt.columnInt64(0)
			currentItems = stmt.columnInt64(1)
		}
		stmt.finalize()
		if exists && current == nextOffset {
			return nil
		}
		if current != expectedOffset {
			return fmt.Errorf("%w: expected cursor %d, persisted %d", errScanTransition, expectedOffset, current)
		}
		checkpoint := scanLibraryCheckpoint{RunID: runID, SourceID: source, LibraryID: library,
			NextStartIndex: nextOffset, LastSuccessPage: expectedOffset, Items: currentItems + itemCount, State: run.State}
		data, err := json.Marshal(checkpoint)
		if err != nil {
			return err
		}
		if err := a.Scanner.db.execParams(`INSERT INTO scanner_checkpoints
    (run_id,source_id,library_id,next_start_index,last_success_page,items,state,payload)
    VALUES (?,?,?,?,?,?,?,?) ON CONFLICT(run_id,library_id) DO UPDATE SET
    next_start_index=excluded.next_start_index,last_success_page=excluded.last_success_page,
    items=excluded.items,state=excluded.state,payload=excluded.payload`,
			runID, source, library, nextOffset, expectedOffset, checkpoint.Items, run.State, string(data)); err != nil {
			return err
		}
		// Page and fallback tail-validation progress advance together so a
		// crash cannot reset the validation count and accidentally skip pages.
		if len(deltaTail) > 0 {
			if len(deltaTail) != 1 || run.Type != "delta" {
				return errScanTransition
			}
			if err := a.Scanner.db.execParams("UPDATE scanner_delta_windows SET tail_remaining=? WHERE run_id=? AND library_id=?", deltaTail[0], runID, library); err != nil {
				return err
			}
		}
		run.Pages++
		run.Items += itemCount
		run.Revision++
		run.UpdatedAt = a.Scanner.now().UTC().Format(time.RFC3339Nano)
		return a.Scanner.saveRunSQL(*run)
	})
}

// Source deletion takes the lifecycle exclusive gate, preventing this
// read from changing before scanner SQL commits.
func (a *App) scannerGenerationSnapshot(source string) int64 {
	a.IDStore.mu.RLock()
	defer a.IDStore.mu.RUnlock()
	return a.IDStore.sourceGenerationLocked(source)
}
