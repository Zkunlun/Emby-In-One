package backend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// A delta target is fixed to this run's start, not the wall clock after a
// possibly multi-hour scan. The saved window survives a process restart.
type scannerDeltaWindow struct {
	Mode          string
	Cutoff        string
	Target        string
	TailRemaining int
}

func parseScannerTime(raw any) (time.Time, bool) {
	s, ok := raw.(string)
	if !ok || s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.9999999", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
func scannerOverlap(committed string) (time.Time, error) {
	t, ok := parseScannerTime(committed)
	if !ok {
		return time.Time{}, fmt.Errorf("invalid committed delta cursor")
	}
	return t.Add(-5 * time.Minute), nil
}
func scannerFingerprint(item map[string]any) (string, error) {
	if item == nil {
		return "", errors.New("empty scanner row")
	}
	core := map[string]any{}
	for _, key := range []string{"Type", "Name", "ProviderIds", "ProductionYear", "DateLastSaved", "DateCreated"} {
		if value, ok := item[key]; ok {
			core[key] = value
		}
	}
	data, err := json.Marshal(core)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// All fingerprint reads/writes are isolated under source lifecycle, scanner
// control and SQLite. A scanner must NEVER mark a fingerprint committed before
// the corresponding Phase3 identity write has succeeded.
func (a *App) scannerFingerprintStatus(source, lib string, item map[string]any) (string, bool, error) {
	id, _ := item["Id"].(string)
	if id == "" {
		return "", false, errors.New("scanner item missing id")
	}
	hash, err := scannerFingerprint(item)
	if err != nil {
		return "", false, err
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	stmt, err := a.Scanner.db.prepare("SELECT fingerprint,library_id FROM scanner_fingerprints WHERE source_id=? AND item_id=?")
	if err != nil {
		return "", false, err
	}
	defer stmt.finalize()
	if err = stmt.bindAll(source, id); err != nil {
		return "", false, err
	}
	found, err := stmt.step()
	if err != nil {
		return "", false, err
	}
	return hash, found && stmt.columnText(0) == hash && stmt.columnText(1) == lib, nil
}

func (a *App) scannerRememberFingerprint(grant scanWriteGrant, lib string, item map[string]any, hash string) error {
	id, _ := item["Id"].(string)
	if id == "" || lib == "" || hash == "" {
		return errScanTransition
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	if a.lifecyclePending || !configuredSourceIDs(a.ConfigStore.Snapshot())[grant.Source] {
		return errScanStale
	}
	if grant.Generation != a.scannerGenerationSnapshot(grant.Source) {
		return errScanStale
	}
	return a.Scanner.db.withWriteTx(func() error {
		run, err := a.Scanner.runLocked(grant.Source, true)
		if err != nil {
			return err
		}
		if run == nil || run.ID != grant.RunID || run.Generation != grant.Generation {
			return errScanStale
		}
		switch run.State {
		case scanScanning, scanPausedAdmin, scanPausedActivity, scanPausedPermission:
		default:
			return errScanStale
		}
		return a.Scanner.db.execParams("INSERT INTO scanner_fingerprints(source_id,item_id,library_id,fingerprint) VALUES(?,?,?,?) ON CONFLICT(source_id,item_id) DO UPDATE SET library_id=excluded.library_id,fingerprint=excluded.fingerprint", grant.Source, id, lib, hash)
	})
}
func (a *App) scannerDeltaWindow(source, runID, lib string) (scannerDeltaWindow, bool, error) {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	stmt, err := a.Scanner.db.prepare("SELECT mode,cutoff,target_watermark,tail_remaining FROM scanner_delta_windows WHERE source_id=? AND run_id=? AND library_id=?")
	if err != nil {
		return scannerDeltaWindow{}, false, err
	}
	defer stmt.finalize()
	if err = stmt.bindAll(source, runID, lib); err != nil {
		return scannerDeltaWindow{}, false, err
	}
	found, err := stmt.step()
	if err != nil {
		return scannerDeltaWindow{}, false, err
	}
	if !found {
		return scannerDeltaWindow{}, false, nil
	}
	return scannerDeltaWindow{Mode: stmt.columnText(0), Cutoff: stmt.columnText(1), Target: stmt.columnText(2), TailRemaining: stmt.columnInt(3)}, true, nil
}
func (a *App) scannerPutDeltaWindow(source, runID, lib string, w scannerDeltaWindow) error {
	if w.Target == "" || lib == "" || (w.Mode != "fallback" && w.Mode != "filtered" && w.Mode != "library_initial") {
		return errScanTransition
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	if a.lifecyclePending {
		return errScanStale
	}
	return a.Scanner.db.withWriteTx(func() error {
		run, err := a.Scanner.runLocked(source, true)
		if err != nil {
			return err
		}
		if run == nil || run.ID != runID || run.Type != "delta" || run.State != scanScanning {
			return errScanTransition
		}
		// Initialization cannot overwrite a checkpoint from a prior process.
		stmt, err := a.Scanner.db.prepare("SELECT mode,cutoff,target_watermark FROM scanner_delta_windows WHERE run_id=? AND library_id=?")
		if err != nil {
			return err
		}
		if err = stmt.bindAll(runID, lib); err != nil {
			stmt.finalize()
			return err
		}
		found, err := stmt.step()
		if err != nil {
			stmt.finalize()
			return err
		}
		if found {
			same := stmt.columnText(0) == w.Mode && stmt.columnText(1) == w.Cutoff && stmt.columnText(2) == w.Target
			stmt.finalize()
			if same {
				return nil
			}
			return errScanTransition
		}
		stmt.finalize()
		return a.Scanner.db.execParams("INSERT INTO scanner_delta_windows(run_id,source_id,library_id,mode,cutoff,target_watermark,tail_remaining) VALUES(?,?,?,?,?,?,?)", runID, source, lib, w.Mode, w.Cutoff, w.Target, w.TailRemaining)
	})
}
func (a *App) scannerCommittedLibrary(source, lib string) (string, bool, error) {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	stmt, err := a.Scanner.db.prepare("SELECT committed_cursor,initial_completed FROM scanner_libraries WHERE source_id=? AND library_id=?")
	if err != nil {
		return "", false, err
	}
	defer stmt.finalize()
	if err = stmt.bindAll(source, lib); err != nil {
		return "", false, err
	}
	found, err := stmt.step()
	if err != nil {
		return "", false, err
	}
	if !found {
		return "", false, nil
	}
	return stmt.columnText(0), stmt.columnInt(1) == 1, nil
}

func (a *App) scannerMarkRunLibraryInactive(source, runID, lib string) error {
	if a.Scanner == nil {
		return errScanDisabled
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	if a.lifecyclePending {
		return errScanStale
	}
	generation := a.scannerGenerationSnapshot(source)
	return a.Scanner.db.withWriteTx(func() error {
		run, err := a.Scanner.runLocked(source, true)
		if err != nil {
			return err
		}
		if run == nil || run.ID != runID || run.Generation != generation || run.State != scanScanning {
			return errScanTransition
		}
		stmt, err := a.Scanner.db.prepare("SELECT state FROM scanner_run_libraries WHERE run_id=? AND library_id=?")
		if err != nil {
			return err
		}
		if err = stmt.bindAll(runID, lib); err != nil {
			stmt.finalize()
			return err
		}
		found, err := stmt.step()
		state := ""
		if found {
			state = stmt.columnText(0)
		}
		stmt.finalize()
		if err != nil {
			return err
		}
		if !found || state == "completed" {
			return errScanTransition
		}
		if state == "inactive" {
			return nil
		}
		return a.Scanner.db.execParams("UPDATE scanner_run_libraries SET state='inactive',pending_watermark='' WHERE run_id=? AND library_id=?", runID, lib)
	})
}
func (a *App) scannerMissingDeltaLibrary(ctx context.Context, source, runID, lib string) (bool, error) {
	if _, err := a.scannerGrant(source, runID); err != nil {
		return false, err
	}
	libs, err := a.scannerDiscover(ctx, source)
	if err != nil {
		return false, err
	}
	for _, current := range libs {
		if current == lib {
			return false, nil
		}
	}
	return true, a.scannerMarkRunLibraryInactive(source, runID, lib)
}
