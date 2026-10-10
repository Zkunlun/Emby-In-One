package backend

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Delta discovers ONLY Movie and Series. Season/Episode updates remain passive
// on /Shows/{id}/Seasons and /Episodes, never active Scanner requests.
func (a *App) scannerRunDeltaLibrary(ctx context.Context, source, runID, lib, target string) error {
	w, exists, err := a.scannerDeltaWindow(source, runID, lib)
	if err != nil {
		return err
	}
	if !exists {
		for {
			w, err = a.scannerMakeDeltaWindow(ctx, source, runID, lib, target)
			if err == nil {
				break
			}
			var httpErr *scannerHTTPError
			if !errors.As(err, &httpErr) {
				return err
			}
			if httpErr.Status == 404 {
				gone, e := a.scannerMissingDeltaLibrary(ctx, source, runID, lib)
				if e != nil {
					return e
				}
				if gone {
					return nil
				}
			}
			grant, e := a.scannerGrant(source, runID)
			if e != nil {
				return e
			}
			handled := a.scannerHandleError(ctx, grant, err)
			if handled != nil {
				return handled
			}
		}
	}
	if w.Mode == "library_initial" {
		// An unrecognized library receives an independent initial Movie/Series
		// catalog. If it disappears mid-run, preserve any prior state and
		// require a complete /Views confirmation before marking inactive.
		err := a.scannerScanLibrary(ctx, source, runID, lib, w.Target)
		var missing *scannerHTTPError
		if errors.As(err, &missing) && missing.Status == 404 {
			gone, e := a.scannerMissingDeltaLibrary(ctx, source, runID, lib)
			if e != nil {
				return e
			}
			if gone {
				return nil
			}
		}
		return err
	}
	cutoff, valid := parseScannerTime(w.Cutoff)
	if !valid {
		return errors.New("delta missing frozen cutoff")
	}
	start, done, err := a.scannerCursor(source, runID, lib)
	if err != nil || done {
		return err
	}
	var previous string
	recent := make([]string, 0, 8)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		params := scannerDeltaParams(lib, w.Mode, cutoff, start)
		grant, err := a.scannerGrant(source, runID)
		if err != nil {
			return err
		}
		page, err := a.scannerDeltaFetch(ctx, source, runID, params)
		if err != nil {
			var upstream *scannerHTTPError
			if errors.As(err, &upstream) && upstream.Status == 404 {
				gone, e := a.scannerMissingDeltaLibrary(ctx, source, runID, lib)
				if e != nil {
					return e
				}
				if gone {
					return nil
				} // cursor remains unchanged
			}
			handled := a.scannerHandleError(ctx, grant, err)
			if handled == nil {
				continue
			}
			return handled
		}
		rows, err := a.scannerValidateDeltaPage(page, start)
		if err != nil {
			return err
		}
		if err := a.scannerResetFailure(source, runID); err != nil {
			return err
		}
		// Strict filtering: a server advertised as filter-capable must not return
		// records outside the frozen 5-minute overlap window.
		if w.Mode == "filtered" {
			for _, row := range rows {
				saved, ok := parseScannerTime(row["DateLastSaved"])
				if !ok || saved.Before(cutoff) {
					return errors.New("upstream delta filter semantics changed mid-run")
				}
			}
		}
		if w.Mode == "fallback" {
			// With no DateCreated timestamp the two-page old-tail condition
			// cannot be established. Fail visibly rather than crawling the
			// entire upstream library on every daily Delta run.
			for _, row := range rows {
				if _, ok := parseScannerTime(row["DateCreated"]); !ok {
					return errors.New("upstream fallback lacks reliable DateCreated")
				}
			}
		}
		var signature strings.Builder
		oldPage := len(rows) > 0
		observedChange := false
		for _, row := range rows {
			id, _ := row["Id"].(string)
			signature.WriteString(strconv.Itoa(len(id)))
			signature.WriteByte(':')
			signature.WriteString(id)
			signature.WriteByte('|')
			if w.Mode == "fallback" {
				created, ok := parseScannerTime(row["DateCreated"])
				if !ok || !created.Before(cutoff) {
					oldPage = false
				}
			}
			hash, unchanged, err := a.scannerFingerprintStatus(source, lib, row)
			if err != nil {
				return err
			}
			if !unchanged {
				observedChange = true
				if _, err := a.mergeDiscovery().registerScannerObservation(grant, row); err != nil {
					return err
				}
				if err := a.scannerRememberFingerprint(grant, lib, row, hash); err != nil {
					return err
				}
			}
		}
		// Bounded loop guard on servers ignoring StartIndex and lying about total.
		if len(rows) > 0 {
			fingerprint := signature.String()
			if fingerprint == previous {
				return errors.New("delta server returned identical consecutive pages")
			}
			for _, seen := range recent {
				if seen == fingerprint {
					return errors.New("delta server repeated page inside validation window")
				}
			}
			recent = append(recent, fingerprint)
			if len(recent) > 8 {
				recent = recent[1:]
			}
			previous = fingerprint
		}
		remaining := w.TailRemaining
		if w.Mode == "fallback" {
			if observedChange {
				remaining = -1
			}
			if oldPage && !observedChange {
				if remaining == -1 {
					remaining = 2
				} else if remaining > 0 {
					remaining--
				}
			}
		}
		if len(rows) > 0 {
			next := start + int64(len(rows))
			if err := a.scannerRecordCommittedPage(source, runID, lib, start, next, int64(len(rows)), remaining); err != nil {
				return err
			}
			start = next
			w.TailRemaining = remaining
		}
		if len(rows) < scannerPageSize || w.Mode == "fallback" && remaining == 0 {
			return a.scannerCompleteLibrary(source, runID, lib, w.Target)
		}
		if err := a.scannerDelay(ctx, scannerPageDelay()); err != nil {
			return err
		}
	}
}

func (a *App) scannerRunDelta(ctx context.Context, source, runID string) error {
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
	if run == nil || run.ID != runID || run.Type != "delta" {
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
		// An authorized inventory request discovers new libraries, while absent
		// ones stay in historical Scanner storage and are never deleted.
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
			// An authoritative empty inventory may mean all previously known
			// libraries disappeared. Preserve their historical cursor and map;
			// record each as inactive without scanning or deleting media.
			a.watchLifecycleMu.RLock()
			a.Scanner.mu.Lock()
			prior, e := a.Scanner.librariesLocked(source)
			a.Scanner.mu.Unlock()
			a.watchLifecycleMu.RUnlock()
			if e != nil {
				return e
			}
			old := []string{}
			for _, lib := range prior {
				if id, ok := lib["libraryId"].(string); ok && id != "" {
					old = append(old, id)
				}
			}
			if len(old) == 0 {
				return a.scannerFailMissingLibraries(source, runID)
			}
			if err := a.scannerSetRunLibraries(source, runID, old); err != nil {
				return err
			}
			for _, lib := range old {
				if err := a.scannerMarkRunLibraryInactive(source, runID, lib); err != nil {
					return err
				}
			}
			return a.scannerFinishRun(source, runID)
		}
		if err := a.scannerSetRunLibraries(source, runID, libs); err != nil {
			return err
		}
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
				err := a.scannerRunDeltaLibrary(runCtx, source, runID, lib, run.StartedAt)
				if err != nil {
					failures <- err
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
