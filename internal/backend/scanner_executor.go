package backend

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// This is an execution coordinator, never a source of scan authority.
// Admin-started/recovered Full and scheduled Delta runs both require
// explicit persisted global and per-source authorization.
func (a *App) scannerWorkerLoop(ctx context.Context) {
	if a.Scanner == nil || a.scanActivity == nil {
		return
	}
	a.Scanner.workerReady.Store(true)
	defer a.Scanner.workerReady.Store(false)
	defer a.Scanner.workerWG.Wait()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		a.scannerDispatchQueued(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (a *App) scannerDispatchQueued(ctx context.Context) {
	if a.Scanner == nil {
		return
	}
	for _, source := range a.ConfigStore.Snapshot().Upstream {
		if source.ID == "" {
			continue
		}
		id := source.ID
		a.watchLifecycleMu.RLock()
		a.Scanner.mu.Lock()
		if a.Scanner.working == nil {
			a.Scanner.working = map[string]bool{}
		}
		if a.Scanner.working[id] || a.lifecyclePending {
			a.Scanner.mu.Unlock()
			a.watchLifecycleMu.RUnlock()
			continue
		}
		run, err := a.Scanner.runLocked(id, true)
		a.Scanner.mu.Unlock()
		a.watchLifecycleMu.RUnlock()
		if err != nil || run == nil {
			continue
		}
		if run.Type != "full" && run.Type != "force_full" && run.Type != "library_initial" && run.Type != "delta" {
			continue
		}
		if !a.scanActivity.quiet(id) {
			continue
		}
		if run.State == scanPausedPermission && run.LastError == "http401" && a.scannerSourceOnline(id) {
			// Wait for upstream re-authentication debounce, rather than retrying
			// a stale online session every two seconds after HTTP 401.
			pausedAt, e := time.Parse(time.RFC3339Nano, run.UpdatedAt)
			if e == nil && time.Since(pausedAt) >= 30*time.Second {
				if _, e = a.scannerCommand(id, "resume"); e == nil {
					run.State = scanQueued
				}
			}
		}
		if run.State == scanPausedActivity {
			if err := a.scannerRuntimeEvent(id, run.ID, "activity_clear", time.Time{}); err != nil {
				continue
			}
			run.State = scanQueued
		}
		if run.State == scanBackoff || run.State == scanCircuitOpen {
			a.watchLifecycleMu.RLock()
			a.Scanner.mu.Lock()
			circuit, e := a.Scanner.circuitLocked(id)
			a.Scanner.mu.Unlock()
			a.watchLifecycleMu.RUnlock()
			if e != nil {
				continue
			}
			reason, _ := circuit["lastError"].(string)
			when, _ := circuit["retryAt"].(string)
			retry, e := time.Parse(time.RFC3339Nano, when)
			if e != nil || time.Now().Before(retry) {
				continue
			}
			if run.State == scanCircuitOpen && reason != "retryable" {
				continue
			} // 403 requires admin
			if _, e := a.scannerCommand(id, "resume"); e != nil {
				continue
			}
			run.State = scanQueued
		}
		if run.State != scanQueued {
			continue
		}
		if !a.scannerSourceOnline(id) {
			continue
		}
		a.Scanner.mu.Lock()
		if a.Scanner.working[id] {
			a.Scanner.mu.Unlock()
			continue
		}
		a.Scanner.working[id] = true
		a.Scanner.mu.Unlock()
		a.Scanner.workerWG.Add(1)
		go func(id, runID string) {
			defer a.Scanner.workerWG.Done()
			defer func() { a.Scanner.mu.Lock(); delete(a.Scanner.working, id); a.Scanner.mu.Unlock() }()
			var err error
			if run.Type == "delta" {
				err = a.scannerRunDelta(ctx, id, runID)
			} else {
				err = a.scannerRunFull(ctx, id, runID)
			}
			if err == nil {
				return
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) && ctx.Err() != nil {
				return
			}
			if errors.Is(err, errScannerHold) {
				if !a.scanActivity.quiet(id) {
					_ = a.scannerRuntimeEvent(id, runID, "user_active", time.Time{})
				} else if !a.scannerSourceOnline(id) {
					_ = a.scannerRuntimeEvent(id, runID, "http401", time.Time{})
				}
				return
			}
			// Backoff / circuit / revoked grant has already been committed by the
			// classified handler. Never override those states with a generic failure.
			a.watchLifecycleMu.RLock()
			a.Scanner.mu.Lock()
			active, e := a.Scanner.runLocked(id, true)
			a.Scanner.mu.Unlock()
			a.watchLifecycleMu.RUnlock()
			if e == nil && active != nil && active.ID == runID &&
				(active.State == scanScanning || active.State == scanQueued) {
				_ = a.scannerRuntimeEvent(id, runID, "failed", time.Time{})
			}
			if a.Logger != nil {
				a.Logger.Warnf("scanner run %s on source %s stopped: %v", runID, id, err)
			}
		}(id, run.ID)
	}
}

// Every successful page resets the per-source consecutive transport-error
// count. Errors from one library affect all library lanes on that upstream.
func (a *App) scannerResetFailure(source, runID string) error {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	run, err := a.Scanner.runLocked(source, true)
	if err != nil {
		return err
	}
	if run == nil || run.ID != runID {
		return errScanStale
	}
	return a.Scanner.db.writeParams("UPDATE scanner_circuits SET failure_count=0,last_error='' WHERE source_id=?", source)
}
func (a *App) scannerRetryable(grant scanWriteGrant) (bool, error) {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	if a.lifecyclePending {
		return false, errScanStale
	}
	generation := a.scannerGenerationSnapshot(grant.Source)
	if generation != grant.Generation {
		return false, errScanStale
	}
	breaker := false
	err := a.Scanner.db.withWriteTx(func() error {
		run, e := a.Scanner.runLocked(grant.Source, true)
		if e != nil {
			return e
		}
		if run == nil || run.ID != grant.RunID || run.State != scanScanning {
			return errScanStale
		}
		old, e := a.Scanner.circuitLocked(grant.Source)
		if e != nil {
			return e
		}
		count := 0
		if n, ok := old["failureCount"].(int); ok {
			count = n
		}
		count++
		breaker = count >= 5
		status := "closed"
		retry := ""
		if breaker {
			status = "open"
			retry = a.Scanner.now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
		}
		if e := a.Scanner.db.execParams("INSERT INTO scanner_circuits(source_id,failure_count,state,retry_at,last_error) VALUES(?,?,?,?,?) ON CONFLICT(source_id) DO UPDATE SET failure_count=excluded.failure_count,state=excluded.state,retry_at=excluded.retry_at,last_error=excluded.last_error",
			grant.Source, count, status, retry, "retryable"); e != nil {
			return e
		}
		if breaker {
			return a.Scanner.transitionSQL(run, scanCircuitOpen)
		}
		return nil
	})
	return breaker, err
}
func (a *App) scannerHandleError(ctx context.Context, grant scanWriteGrant, err error) error {
	var bad *scannerHTTPError
	if errors.As(err, &bad) {
		switch bad.Status {
		case 401:
			if e := a.scannerRuntimeEvent(grant.Source, grant.RunID, "http401", time.Time{}); e != nil {
				return e
			}
			return errScannerHold
		case 403:
			if e := a.scannerRuntimeEvent(grant.Source, grant.RunID, "http403", time.Time{}); e != nil {
				return e
			}
			return errScannerHold
		case 429:
			if e := a.scannerRuntimeEvent(grant.Source, grant.RunID, "http429", bad.RetryAfter); e != nil {
				return e
			}
			return errScannerHold
		default:
			if bad.Status < 500 || bad.Status > 599 {
				return bad
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	tripped, e := a.scannerRetryable(grant)
	if e != nil {
		return e
	}
	if tripped {
		return errScannerHold
	}
	delay := 5 * time.Second
	if bad != nil && bad.Status == 503 && bad.RetryAfter.After(time.Now().Add(delay)) {
		delay = time.Until(bad.RetryAfter) // RFC Retry-After overrides local 5s
	}
	if e := a.scannerDelay(ctx, delay); e != nil {
		return e
	}
	return nil // caller retries the identical request cursor
}
func (a *App) scannerFailMissingLibraries(source, runID string) error {
	// Preserve the run as a visible failure rather than silently declaring an
	// empty collection fully scanned.
	if e := a.scannerRuntimeEvent(source, runID, "failed", time.Time{}); e != nil {
		return e
	}
	return fmt.Errorf("no supported Movie/Series libraries on %s", source)
}

// Source authorization capacity is also a LIVE HTTP-page concurrency ceiling.
// If MaxConcurrent shrinks while lanes are running, their already-in-flight
// pages finish; no new page is issued until the active count drops below cap.
func (a *App) scannerAcquirePageSlot(ctx context.Context, source, runID string) (scanWriteGrant, func(), error) {
	if a.Scanner == nil {
		return scanWriteGrant{}, nil, errScanDisabled
	}
	for {
		grant, err := a.scannerGrant(source, runID)
		if err != nil {
			return scanWriteGrant{}, nil, err
		}
		cap := 2
		available := false
		for _, cfg := range a.ConfigStore.Snapshot().Upstream {
			if cfg.ID == source {
				available = true
				if cfg.MaxConcurrent > 0 {
					cap = cfg.MaxConcurrent
				}
				break
			}
		}
		if !available {
			return scanWriteGrant{}, nil, errScanStale
		}
		a.Scanner.mu.Lock()
		if a.Scanner.activePages == nil {
			a.Scanner.activePages = map[string]int{}
		}
		acquired := a.Scanner.activePages[source] < cap
		if acquired {
			a.Scanner.activePages[source]++
		}
		a.Scanner.mu.Unlock()
		if acquired {
			release := func() {
				a.Scanner.mu.Lock()
				a.Scanner.activePages[source]--
				if a.Scanner.activePages[source] <= 0 {
					delete(a.Scanner.activePages, source)
				}
				a.Scanner.mu.Unlock()
			}
			// Authorization can be revoked while waiting for an available slot.
			if grant, err = a.scannerGrant(source, runID); err != nil {
				release()
				return scanWriteGrant{}, nil, err
			}
			return grant, release, nil
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return scanWriteGrant{}, nil, ctx.Err()
		case <-timer.C:
		}
	}
}
