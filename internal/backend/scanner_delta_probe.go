package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// A guarded probe must PROVE that the server actually applies the date filter.
// Absence of strong evidence (including Date header / DateLastSaved) means
// fallback, NEVER silent acceptance of a purportedly supported parameter.
func (a *App) scannerProbeDateSample(ctx context.Context, source, runID, lib string) (map[string]any, time.Time, error) {
	client := a.Upstream.ClientByID(source)
	if client == nil {
		return nil, time.Time{}, errScanStale
	}
	grant, release, err := a.scannerAcquirePageSlot(ctx, source, runID)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer release()
	_ = grant
	query := url.Values{"ParentId": {lib}, "Recursive": {"true"}, "IncludeItemTypes": {"Movie,Series"},
		"SortBy": {"DateLastSaved"}, "SortOrder": {"Descending"}, "Limit": {"1"}, "StartIndex": {"0"},
		"Fields": {"DateLastSaved,DateCreated,ProviderIds,ProductionYear"}}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	resp, err := client.doRequest(ctx, nil, http.MethodGet, "/Users/"+url.PathEscape(client.clientUserID())+"/Items", query, nil, client.requestHeaders(nil, a.Identity), false)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, time.Time{}, &scannerHTTPError{Status: resp.StatusCode}
	}
	serverTime, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return nil, time.Time{}, nil
	}
	if delta := time.Since(serverTime); delta > 2*time.Minute || delta < -2*time.Minute {
		return nil, time.Time{}, nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (12<<20)+1))
	if err != nil {
		return nil, time.Time{}, err
	}
	if len(raw) > 12<<20 {
		return nil, time.Time{}, errors.New("scanner probe response too large")
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, time.Time{}, err
	}
	rows, err := scannerItems(result)
	if err != nil {
		return nil, time.Time{}, err
	}
	if len(rows) != 1 {
		return nil, time.Time{}, nil
	}
	recent, valid := parseScannerTime(rows[0]["DateLastSaved"])
	if !valid || recent.After(serverTime.Add(2*time.Minute)) {
		return nil, time.Time{}, nil
	}
	return rows[0], serverTime.UTC(), nil
}
func scannerDeltaParams(lib, mode string, cutoff time.Time, start int64) url.Values {
	params := url.Values{"ParentId": {lib}, "Recursive": {"true"}, "IncludeItemTypes": {"Movie,Series"},
		"Limit": {"60"}, "StartIndex": {strconv.FormatInt(start, 10)},
		"Fields": {"ProviderIds,ProductionYear,DateCreated,DateLastSaved"}}
	if mode == "filtered" {
		params.Set("SortBy", "DateLastSaved")
		params.Set("SortOrder", "Ascending")
		params.Set("MinDateLastSaved", cutoff.UTC().Format(time.RFC3339Nano))
	} else {
		params.Set("SortBy", "DateCreated")
		params.Set("SortOrder", "Descending")
	}
	return params
}
func (a *App) scannerDeltaFetch(ctx context.Context, source, runID string, params url.Values) (map[string]any, error) {
	client := a.Upstream.ClientByID(source)
	if client == nil {
		return nil, errScanStale
	}
	_, release, err := a.scannerAcquirePageSlot(ctx, source, runID)
	if err != nil {
		return nil, err
	}
	defer release()
	payload, err := a.scannerFetch(ctx, client, "/Users/"+url.PathEscape(client.clientUserID())+"/Items", params)
	return payload, err
}
func (a *App) scannerProbeFilter(ctx context.Context, source, runID, lib string) (bool, error) {
	baseline, serverTime, err := a.scannerProbeDateSample(ctx, source, runID, lib)
	if err != nil {
		return false, err
	}
	if baseline == nil {
		return false, nil
	}
	saved, _ := parseScannerTime(baseline["DateLastSaved"])
	id, _ := baseline["Id"].(string)
	if id == "" {
		return false, nil
	}
	// Positive control MUST include a known row. Negative control MUST be
	// empty; an upstream ignoring MinDateLastSaved fails the negative control.
	for _, probe := range []struct {
		cutoff   time.Time
		expected bool
	}{
		{saved.Add(-time.Second), true},
		{serverTime.Add(10 * time.Minute), false},
	} {
		if err := a.scannerDelay(ctx, scannerPageDelay()); err != nil {
			return false, err
		}
		q := scannerDeltaParams(lib, "filtered", probe.cutoff, 0)
		q.Set("Limit", "1")
		q.Set("SortOrder", "Descending")
		page, err := a.scannerDeltaFetch(ctx, source, runID, q)
		if err != nil {
			var httpErr *scannerHTTPError
			if errors.As(err, &httpErr) && httpErr.Status >= 400 && httpErr.Status < 500 && httpErr.Status != 401 && httpErr.Status != 403 && httpErr.Status != 429 {
				return false, nil // parameter rejected: fallback
			}
			return false, err
		}
		rows, err := scannerItems(page)
		if err != nil {
			return false, nil
		}
		if probe.expected {
			if len(rows) == 0 {
				return false, nil
			}
			got, _ := rows[0]["Id"].(string)
			if got != id {
				return false, nil
			}
		} else if len(rows) != 0 {
			return false, nil
		}
	}
	return true, nil
}
func (a *App) scannerMakeDeltaWindow(ctx context.Context, source, runID, lib, target string) (scannerDeltaWindow, error) {
	if w, ok, err := a.scannerDeltaWindow(source, runID, lib); err != nil || ok {
		return w, err
	}
	cursor, initialized, err := a.scannerCommittedLibrary(source, lib)
	if err != nil {
		return scannerDeltaWindow{}, err
	}
	w := scannerDeltaWindow{Mode: "library_initial", Target: target, TailRemaining: -1}
	if initialized && cursor != "" {
		cutoff, err := scannerOverlap(cursor)
		if err != nil {
			return scannerDeltaWindow{}, err
		}
		w.Mode = "fallback"
		w.Cutoff = cutoff.Format(time.RFC3339Nano)
		reliable, err := a.scannerProbeFilter(ctx, source, runID, lib)
		if err != nil {
			return scannerDeltaWindow{}, err
		}
		if reliable {
			w.Mode = "filtered"
		}
	}
	if err := a.scannerPutDeltaWindow(source, runID, lib, w); err != nil {
		return scannerDeltaWindow{}, err
	}
	return w, nil
}
func (a *App) scannerValidateDeltaPage(payload map[string]any, start int64) ([]map[string]any, error) {
	rows, err := scannerItems(payload)
	if err != nil {
		return nil, err
	}
	if len(rows) > scannerPageSize {
		return nil, errors.New("oversized delta page")
	}
	if raw, ok := payload["StartIndex"]; ok {
		if n, valid := numericInt(raw); !valid || int64(n) != start {
			return nil, errors.New("delta offset mismatch")
		}
	}
	if raw, ok := payload["TotalRecordCount"]; ok {
		n, valid := numericInt(raw)
		if !valid || n < 0 || int64(n) < start+int64(len(rows)) || len(rows) < scannerPageSize && int64(n) > start+int64(len(rows)) {
			return nil, errors.New("contradictory delta page count")
		}
	}
	for _, item := range rows {
		id, ok := item["Id"].(string)
		kind, _ := item["Type"].(string)
		if !ok || id == "" || (kind != "Movie" && kind != "Series") {
			return nil, fmt.Errorf("invalid delta media id/type")
		}
	}
	return rows, nil
}
