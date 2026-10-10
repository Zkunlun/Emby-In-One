package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Phase 5 manages durable control-plane data ONLY: no HTTP upstream scanner.
const (
	scanPending          = "initial_scan_pending"
	scanIdle             = "idle"
	scanQueued           = "queued"
	scanScanning         = "scanning"
	scanPausedActivity   = "paused_user_activity"
	scanPausedAdmin      = "paused_admin"
	scanPausedPermission = "paused_permission"
	scanBackoff          = "backoff"
	scanCircuitOpen      = "circuit_open"
	scanCompleted        = "completed"
	scanFailed           = "failed"
	scanStopped          = "stopped"
)

var (
	errScanDisabled   = errors.New("scanner disabled or upstream not allowlisted")
	errScanConflict   = errors.New("nonterminal scanner run already exists")
	errScanTransition = errors.New("invalid scanner transition")
	errScanNotFound   = errors.New("scanner run or source not found")
	errScanStale      = errors.New("scanner source generation changed")
)

type scanRun struct {
	ID         string `json:"id"`
	SourceID   string `json:"sourceId"`
	Type       string `json:"type"`
	State      string `json:"state"`
	Generation int64  `json:"sourceGeneration"`
	Revision   int64  `json:"revision"`
	StartedAt  string `json:"startedAt"`
	UpdatedAt  string `json:"updatedAt"`
	LastError  string `json:"lastError,omitempty"`
	Pages      int64  `json:"pages"`
	Items      int64  `json:"items"`
	Recovering bool   `json:"recovering,omitempty"`
}
type scanSourceSettings struct {
	SourceID             string `json:"sourceId"`
	AllowScan            bool   `json:"allowScan"`
	InitialFullCompleted bool   `json:"initialFullScanCompleted"`
	LastCompletedAt      string `json:"lastCompletedAt,omitempty"`
}
type scanLibraryCheckpoint struct {
	RunID             string `json:"runId"`
	SourceID          string `json:"sourceId"`
	LibraryID         string `json:"libraryId"`
	NextStartIndex    int64  `json:"nextStartIndex"`
	LastSuccessPage   int64  `json:"lastSuccessPage"`
	Items             int64  `json:"items"`
	State             string `json:"state"`
	CommittedCursor   string `json:"committedCursor,omitempty"`
	FullSafeWatermark string `json:"fullSafeWatermark,omitempty"`
}

// Lifecycle gate precedes scanner.mu; DB write lock is acquired last.
// Upstream deletion holds the lifecycle exclusive gate for atomic cleanup.
type ScannerControl struct {
	mu          sync.Mutex
	db          *sqliteDB
	now         func() time.Time
	wait        func(context.Context, time.Duration) error
	working     map[string]bool
	activePages map[string]int
	workerReady atomic.Bool
	workerWG    sync.WaitGroup
}

func initScannerSchema(db *sqliteDB) error {
	if db == nil {
		return errors.New("scanner requires persistent sqlite")
	}
	return db.withWriteTx(func() error {
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS scanner_meta(key TEXT PRIMARY KEY,value TEXT NOT NULL)`,
			`INSERT OR IGNORE INTO scanner_meta(key,value) VALUES('scan_enabled','0')`,
			`CREATE TABLE IF NOT EXISTS scanner_sources(
    source_id TEXT PRIMARY KEY,allow_scan INTEGER NOT NULL DEFAULT 0 CHECK(allow_scan IN (0,1)),
    initial_full_completed INTEGER NOT NULL DEFAULT 0 CHECK(initial_full_completed IN (0,1)),
    last_completed_at TEXT NOT NULL DEFAULT '')`,
			`CREATE TABLE IF NOT EXISTS scanner_runs(
    run_id TEXT PRIMARY KEY,source_id TEXT NOT NULL,run_type TEXT NOT NULL
     CHECK(run_type IN ('full','delta','force_full','library_initial')),
    state TEXT NOT NULL,generation INTEGER NOT NULL,
    revision INTEGER NOT NULL CHECK(revision>0),payload TEXT NOT NULL)`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_scanner_one_active_source ON scanner_runs(source_id)
     WHERE state IN ('queued','scanning','paused_user_activity','paused_admin',
      'paused_permission','backoff','circuit_open')`,
			`CREATE TABLE IF NOT EXISTS scanner_libraries(
    source_id TEXT NOT NULL,library_id TEXT NOT NULL,
    committed_cursor TEXT NOT NULL DEFAULT '',initial_completed INTEGER NOT NULL DEFAULT 0,
    full_safe_watermark TEXT NOT NULL DEFAULT '',capability TEXT NOT NULL DEFAULT 'unknown',
    fingerprint TEXT NOT NULL DEFAULT '',inactive INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY(source_id,library_id))`,
			`CREATE TABLE IF NOT EXISTS scanner_run_libraries (
    run_id TEXT NOT NULL,source_id TEXT NOT NULL,library_id TEXT NOT NULL,
    state TEXT NOT NULL DEFAULT 'queued',
    pending_watermark TEXT NOT NULL DEFAULT '',PRIMARY KEY(run_id,library_id))`,
			`CREATE TABLE IF NOT EXISTS scanner_checkpoints(
    run_id TEXT NOT NULL,source_id TEXT NOT NULL,library_id TEXT NOT NULL,
    next_start_index INTEGER NOT NULL CHECK(next_start_index>=0),
    last_success_page INTEGER NOT NULL DEFAULT -1,items INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL DEFAULT 'queued',payload TEXT NOT NULL,
    PRIMARY KEY(run_id,library_id))`,
			`CREATE TABLE IF NOT EXISTS scanner_fingerprints(
    source_id TEXT NOT NULL,item_id TEXT NOT NULL,library_id TEXT NOT NULL,
    fingerprint TEXT NOT NULL,PRIMARY KEY(source_id,item_id))`,
			`CREATE TABLE IF NOT EXISTS scanner_delta_windows (
    run_id TEXT NOT NULL,source_id TEXT NOT NULL,library_id TEXT NOT NULL,
    mode TEXT NOT NULL CHECK(mode IN ('filtered','fallback','library_initial')),
    cutoff TEXT NOT NULL DEFAULT '',target_watermark TEXT NOT NULL,
    tail_remaining INTEGER NOT NULL DEFAULT -1,
    PRIMARY KEY(run_id,library_id))`,
			`CREATE TABLE IF NOT EXISTS scanner_circuits(
    source_id TEXT PRIMARY KEY,failure_count INTEGER NOT NULL DEFAULT 0,
    state TEXT NOT NULL DEFAULT 'closed',retry_at TEXT NOT NULL DEFAULT '',
    last_error TEXT NOT NULL DEFAULT '')`,
			`CREATE TABLE IF NOT EXISTS scanner_schedule_slots(
    source_id TEXT NOT NULL,schedule_date TEXT NOT NULL,
    trigger_type TEXT NOT NULL DEFAULT 'auto_delta',run_id TEXT NOT NULL DEFAULT '',
    reason TEXT NOT NULL DEFAULT '',
    PRIMARY KEY(source_id,schedule_date,trigger_type))`,
		} {
			if err := db.exec(q); err != nil {
				return fmt.Errorf("scanner migration: %w", err)
			}
		}
		return nil
	})
}
func newScannerControl(db *sqliteDB) (*ScannerControl, error) {
	if err := initScannerSchema(db); err != nil {
		return nil, err
	}
	return &ScannerControl{db: db, now: time.Now}, nil
}
func (s *ScannerControl) enabledLocked() (bool, error) {
	stmt, err := s.db.prepare(`SELECT value FROM scanner_meta WHERE key='scan_enabled'`)
	if err != nil {
		return false, err
	}
	defer stmt.finalize()
	row, err := stmt.step()
	if err != nil {
		return false, err
	}
	if !row {
		return false, errors.New("scanner enable setting missing")
	}
	return stmt.columnText(0) == "1", nil
}
func (s *ScannerControl) sourceLocked(source string) (scanSourceSettings, error) {
	out := scanSourceSettings{SourceID: source}
	stmt, err := s.db.prepare(`SELECT allow_scan,initial_full_completed,last_completed_at FROM scanner_sources WHERE source_id=?`)
	if err != nil {
		return out, err
	}
	defer stmt.finalize()
	if err := stmt.bindAll(source); err != nil {
		return out, err
	}
	row, err := stmt.step()
	if err != nil {
		return out, err
	}
	if row {
		out.AllowScan = stmt.columnInt(0) == 1
		out.InitialFullCompleted = stmt.columnInt(1) == 1
		out.LastCompletedAt = stmt.columnText(2)
	}
	return out, nil
}
func (s *ScannerControl) runLocked(source string, active bool) (*scanRun, error) {
	q := `SELECT payload FROM scanner_runs WHERE source_id=? ORDER BY rowid DESC LIMIT 1`
	if active {
		q = `SELECT payload FROM scanner_runs WHERE source_id=? AND state IN
 ('queued','scanning','paused_user_activity','paused_admin','paused_permission','backoff','circuit_open')
 LIMIT 1`
	}
	stmt, err := s.db.prepare(q)
	if err != nil {
		return nil, err
	}
	defer stmt.finalize()
	if err := stmt.bindAll(source); err != nil {
		return nil, err
	}
	row, err := stmt.step()
	if err != nil {
		return nil, err
	}
	if !row {
		return nil, nil
	}
	var run scanRun
	if err := json.Unmarshal([]byte(stmt.columnText(0)), &run); err != nil {
		return nil, err
	}
	return &run, nil
}
func (s *ScannerControl) saveRunSQL(run scanRun) error {
	data, err := json.Marshal(run)
	if err != nil {
		return err
	}
	return s.db.execParams(`INSERT INTO scanner_runs(run_id,source_id,run_type,state,generation,revision,payload)
 VALUES(?,?,?,?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET
 state=excluded.state,revision=excluded.revision,payload=excluded.payload`,
		run.ID, run.SourceID, run.Type, run.State, run.Generation, run.Revision, string(data))
}
func (s *ScannerControl) transitionSQL(run *scanRun, state string) error {
	if run.State == state {
		return nil
	}
	run.State = state
	run.Revision++
	run.UpdatedAt = s.now().UTC().Format(time.RFC3339Nano)
	return s.saveRunSQL(*run)
}
func (s *ScannerControl) createRunSQL(source, typ string, generation int64) (*scanRun, error) {
	now := s.now().UTC().Format(time.RFC3339Nano)
	run := scanRun{ID: randomHex(16), SourceID: source, Type: typ, State: scanQueued,
		Generation: generation, Revision: 1, StartedAt: now, UpdatedAt: now}
	if err := s.saveRunSQL(run); err != nil {
		return nil, err
	}
	return &run, nil
}

// Must run inside the source-delete lifecycle transaction, not a later cleanup.
func removeScannerSourceSQL(db *sqliteDB, source string) error {
	for _, q := range []string{
		`DELETE FROM scanner_run_libraries WHERE source_id=?`,
		`DELETE FROM scanner_checkpoints WHERE source_id=?`,
		`DELETE FROM scanner_delta_windows WHERE source_id=?`,
		`DELETE FROM scanner_fingerprints WHERE source_id=?`,
		`DELETE FROM scanner_libraries WHERE source_id=?`,
		`DELETE FROM scanner_runs WHERE source_id=?`,
		`DELETE FROM scanner_circuits WHERE source_id=?`,
		`DELETE FROM scanner_schedule_slots WHERE source_id=?`,
		`DELETE FROM scanner_sources WHERE source_id=?`,
	} {
		if err := db.execParams(q, source); err != nil {
			return err
		}
	}
	return nil
}
