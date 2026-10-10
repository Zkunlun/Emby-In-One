package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const scannerPageSize = 60

var errScannerHold = errors.New("scanner paused or no longer authorized")

type scannerHTTPError struct {
	Status     int
	RetryAfter time.Time
}

func (e *scannerHTTPError) Error() string {
	return fmt.Sprintf("upstream scanner response %d", e.Status)
}

func (a *App) scannerFetch(ctx context.Context, c *UpstreamClient, path string, query url.Values) (map[string]any, error) {
	if c == nil {
		return nil, errScanStale
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	resp, err := c.doRequest(ctx, nil, http.MethodGet, path, query, nil, c.requestHeaders(nil, a.Identity), false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		retry := time.Time{}
		if raw := resp.Header.Get("Retry-After"); raw != "" {
			if sec, e := strconv.Atoi(raw); e == nil && sec > 0 {
				retry = time.Now().Add(time.Duration(sec) * time.Second)
			}
			if t, e := http.ParseTime(raw); e == nil && t.After(retry) {
				retry = t
			}
		}
		return nil, &scannerHTTPError{Status: resp.StatusCode, RetryAfter: retry}
	}
	// Consume at most one bounded JSON response; reject truncated or trailing
	// documents rather than trusting the first well-formed prefix.
	const maxBytes = 12 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxBytes {
		return nil, errors.New("scanner response too large")
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("scanner upstream JSON null")
	}
	return result, nil
}
func scannerItems(payload map[string]any) ([]map[string]any, error) {
	raw, ok := payload["Items"].([]any)
	if !ok {
		return nil, errors.New("scanner upstream missing Items list")
	}
	items := make([]map[string]any, 0, len(raw))
	for _, row := range raw {
		item, ok := row.(map[string]any)
		if !ok {
			return nil, errors.New("scanner item not an object")
		}
		items = append(items, item)
	}
	return items, nil
}
func (a *App) scannerDiscover(ctx context.Context, source string) ([]string, error) {
	client := a.Upstream.ClientByID(source)
	if client == nil || !client.IsOnline() {
		return nil, errScanDisabled
	}
	page, err := a.scannerFetch(ctx, client, "/Users/"+url.PathEscape(client.clientUserID())+"/Views", nil)
	if err != nil {
		return nil, err
	}
	rows, err := scannerItems(page)
	if err != nil {
		return nil, err
	}
	// /Views is treated as a complete inventory snapshot. A truncated
	// response must NOT freeze a partial library set and mark the run full.
	if totalRaw, ok := page["TotalRecordCount"]; ok {
		total, valid := numericInt(totalRaw)
		if !valid || total < 0 || total != len(rows) {
			return nil, errors.New("scanner /Views inventory truncated or inconsistent")
		}
	}
	if raw, ok := page["StartIndex"]; ok {
		start, valid := numericInt(raw)
		if !valid || start != 0 {
			return nil, errors.New("scanner /Views inventory is not first page")
		}
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, row := range rows {
		id, _ := row["Id"].(string)
		kind, _ := row["CollectionType"].(string)
		switch strings.ToLower(strings.TrimSpace(kind)) {
		case "music", "musicvideos", "photos", "books", "audiobooks", "livetv", "channels", "playlists", "games", "homevideos":
			continue
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}
func (a *App) scannerFrozenLibraries(source, runID string) ([]string, error) {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	stmt, err := a.Scanner.db.prepare("SELECT library_id FROM scanner_run_libraries WHERE source_id=? AND run_id=? ORDER BY library_id")
	if err != nil {
		return nil, err
	}
	defer stmt.finalize()
	if err = stmt.bindAll(source, runID); err != nil {
		return nil, err
	}
	ids := []string{}
	for {
		next, err := stmt.step()
		if err != nil {
			return nil, err
		}
		if !next {
			break
		}
		ids = append(ids, stmt.columnText(0))
	}
	return ids, nil
}
func (a *App) scannerCursor(source, runID, lib string) (int64, bool, error) {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	stmt, err := a.Scanner.db.prepare("SELECT state FROM scanner_run_libraries WHERE run_id=? AND source_id=? AND library_id=?")
	if err != nil {
		return 0, false, err
	}
	if err = stmt.bindAll(runID, source, lib); err != nil {
		stmt.finalize()
		return 0, false, err
	}
	found, err := stmt.step()
	state := ""
	if found {
		state = stmt.columnText(0)
	}
	stmt.finalize()
	if err != nil {
		return 0, false, err
	}
	if !found {
		return 0, false, errScanNotFound
	}
	if state == "completed" {
		return 0, true, nil
	}
	stmt, err = a.Scanner.db.prepare("SELECT next_start_index FROM scanner_checkpoints WHERE run_id=? AND source_id=? AND library_id=?")
	if err != nil {
		return 0, false, err
	}
	defer stmt.finalize()
	if err = stmt.bindAll(runID, source, lib); err != nil {
		return 0, false, err
	}
	found, err = stmt.step()
	if err != nil {
		return 0, false, err
	}
	if !found {
		return 0, false, nil
	}
	return stmt.columnInt64(0), false, nil
}
func (a *App) scannerGrant(source, runID string) (scanWriteGrant, error) {
	if a.Scanner == nil || a.scanActivity == nil || !a.scanActivity.quiet(source) {
		return scanWriteGrant{}, errScannerHold
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	if a.lifecyclePending || !configuredSourceIDs(a.ConfigStore.Snapshot())[source] {
		return scanWriteGrant{}, errScanStale
	}
	enabled, err := a.Scanner.enabledLocked()
	if err != nil {
		return scanWriteGrant{}, err
	}
	setting, err := a.Scanner.sourceLocked(source)
	if err != nil {
		return scanWriteGrant{}, err
	}
	run, err := a.Scanner.runLocked(source, true)
	if err != nil {
		return scanWriteGrant{}, err
	}
	if run == nil || run.ID != runID || run.State != scanScanning || !enabled || !setting.AllowScan || !a.scannerSourceOnline(source) || !a.scanActivity.quiet(source) {
		return scanWriteGrant{}, errScannerHold
	}
	if a.scannerGenerationSnapshot(source) != run.Generation {
		return scanWriteGrant{}, errScanStale
	}
	return scanWriteGrant{Source: source, RunID: runID, Generation: run.Generation}, nil
}
func (a *App) scannerDelay(ctx context.Context, d time.Duration) error {
	if a.Scanner != nil && a.Scanner.wait != nil {
		return a.Scanner.wait(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func scannerPageDelay() time.Duration { return time.Duration(5+rand.Intn(16)) * time.Second }
func scannerStagger() time.Duration   { return time.Duration(5+rand.Intn(6)) * time.Second }

func (a *App) scannerScanLibrary(ctx context.Context, source, runID, lib, safeWatermark string) error {
	start, done, err := a.scannerCursor(source, runID, lib)
	if err != nil || done {
		return err
	}
	c := a.Upstream.ClientByID(source)
	if c == nil {
		return errScanStale
	}
	endpoint := "/Users/" + url.PathEscape(c.clientUserID()) + "/Items"
	// Bounded repeated-page signatures catch an upstream that ignores
	// StartIndex while omitting response offset metadata. A loop must never
	// advance indefinitely using the same page of media identities.
	recentPageSignatures := make([]string, 0, 8)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		grant, release, err := a.scannerAcquirePageSlot(ctx, source, runID)
		if err != nil {
			return err
		}
		params := url.Values{"ParentId": {lib}, "Recursive": {"true"}, "IncludeItemTypes": {"Movie,Series"},
			"Limit": {"60"}, "StartIndex": {strconv.FormatInt(start, 10)}, "SortBy": {"SortName"},
			"SortOrder": {"Ascending"}, "Fields": {"ProviderIds,ProductionYear,DateCreated,DateLastSaved"}}
		payload, err := a.scannerFetch(ctx, c, endpoint, params)
		release()
		if err != nil {
			handled := a.scannerHandleError(ctx, grant, err)
			if handled == nil {
				continue
			}
			return handled
		}
		rows, err := scannerItems(payload)
		if err != nil {
			return err
		}
		if len(rows) > scannerPageSize {
			return errors.New("scanner page oversized")
		}
		if len(rows) > 0 {
			var sig strings.Builder
			for _, row := range rows {
				id, ok := row["Id"].(string)
				if !ok || id == "" {
					return errors.New("scanner page media item missing Id")
				}
				sig.WriteString(strconv.Itoa(len(id)))
				sig.WriteByte(':')
				sig.WriteString(id)
				kind, _ := row["Type"].(string)
				sig.WriteString(kind)
				sig.WriteByte('|')
			}
			signature := sig.String()
			for _, recent := range recentPageSignatures {
				if recent == signature {
					return errors.New("scanner upstream repeated an already-seen page")
				}
			}
			recentPageSignatures = append(recentPageSignatures, signature)
			if len(recentPageSignatures) > 8 {
				recentPageSignatures = recentPageSignatures[1:]
			}
		}
		if err := a.scannerResetFailure(source, runID); err != nil {
			return err
		}
		if raw, ok := payload["StartIndex"]; ok {
			if actual, valid := numericInt(raw); !valid || int64(actual) != start {
				return errors.New("scanner page offset mismatch")
			}
		}
		if raw, ok := payload["TotalRecordCount"]; ok {
			total, valid := numericInt(raw)
			next := start + int64(len(rows))
			if !valid || total < 0 || int64(total) < next || len(rows) < scannerPageSize && int64(total) > next {
				return errors.New("scanner upstream contradictory collection total")
			}
		}
		for _, row := range rows {
			kind, _ := row["Type"].(string)
			if kind != "Movie" && kind != "Series" {
				return fmt.Errorf("scanner source violated Movie/Series filter: %q", kind)
			}
			if _, err := a.mergeDiscovery().registerScannerObservation(grant, row); err != nil {
				return err
			}
			hash, err := scannerFingerprint(row)
			if err != nil {
				return err
			}
			if err := a.scannerRememberFingerprint(grant, lib, row, hash); err != nil {
				return err
			}
		}
		if len(rows) > 0 {
			next := start + int64(len(rows))
			if err := a.scannerRecordCommittedPage(source, runID, lib, start, next, int64(len(rows))); err != nil {
				return err
			}
			start = next
		}
		if len(rows) < scannerPageSize {
			return a.scannerCompleteLibrary(source, runID, lib, safeWatermark)
		}
		if err := a.scannerDelay(ctx, scannerPageDelay()); err != nil {
			return err
		}
	}
}

func (a *App) scannerRunFull(ctx context.Context, source, runID string) error {
	if a.Scanner == nil || a.scanActivity == nil {
		return errScanDisabled
	}
	a.watchLifecycleMu.RLock()
	a.Scanner.mu.Lock()
	run, err := a.Scanner.runLocked(source, true)
	a.Scanner.mu.Unlock()
	a.watchLifecycleMu.RUnlock()
	if err != nil {
		return err
	}
	if run == nil || run.ID != runID || (run.Type != "full" && run.Type != "force_full" && run.Type != "library_initial") {
		return errScanTransition
	}
	if !a.scanActivity.quiet(source) {
		return errScannerHold
	}
	if err := a.scannerRuntimeEvent(source, runID, "dispatch", time.Time{}); err != nil {
		return err
	}
	libs, err := a.scannerFrozenLibraries(source, runID)
	if err != nil {
		return err
	}
	if len(libs) == 0 {
		// Before /Views, independently check the scan-only double opt-in.
		a.watchLifecycleMu.RLock()
		a.Scanner.mu.Lock()
		enabled, e := a.Scanner.enabledLocked()
		settings, _ := a.Scanner.sourceLocked(source)
		a.Scanner.mu.Unlock()
		a.watchLifecycleMu.RUnlock()
		if e != nil {
			return e
		}
		if !enabled || !settings.AllowScan || !a.scannerSourceOnline(source) {
			return errScanDisabled
		}
		for {
			grant, e := a.scannerGrant(source, runID)
			if e != nil {
				return e
			}
			libs, err = a.scannerDiscover(ctx, source)
			if err == nil {
				break
			}
			handled := a.scannerHandleError(ctx, grant, err)
			if handled != nil {
				return handled
			}
		}
		if len(libs) == 0 {
			return a.scannerFailMissingLibraries(source, runID)
		}
		if err := a.scannerSetRunLibraries(source, runID, libs); err != nil {
			return err
		}
	}

	if !a.scanActivity.quiet(source) {
		_ = a.scannerRuntimeEvent(source, runID, "user_active", time.Time{})
		return errScannerHold
	}
	cap := 2
	for _, cfg := range a.ConfigStore.Snapshot().Upstream {
		if cfg.ID == source {
			if cfg.MaxConcurrent > 0 {
				cap = cfg.MaxConcurrent
			}
			break
		}
	}
	if cap > len(libs) {
		cap = len(libs)
	}
	if cap < 1 {
		cap = 1
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Buffer the frozen worklist: on soft pause workers exit while existing
	// in-flight pages finish naturally. No sender may deadlock on a closed gate.
	tasks := make(chan string, len(libs))
	for _, lib := range libs {
		tasks <- lib
	}
	close(tasks)
	failures := make(chan error, len(libs))
	var wg sync.WaitGroup
	for lane := 0; lane < cap; lane++ {
		wg.Add(1)
		go func(lane int) {
			defer wg.Done()
			for k := 0; k < lane; k++ {
				if err := a.scannerDelay(runCtx, scannerStagger()); err != nil {
					return
				}
			}
			for lib := range tasks {
				if err := a.scannerScanLibrary(runCtx, source, runID, lib, run.StartedAt); err != nil {
					select {
					case failures <- err:
					default:
					}
					if !errors.Is(err, errScannerHold) && !errors.Is(err, errScanStale) {
						cancel()
					}
					return
				}
			}
		}(lane)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return a.scannerFinishRun(source, runID)
}
