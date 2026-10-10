package backend

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Passive pages are finite HTTP requests, NOT a cap on the total collection.
// Source cursor always advances by the number of rows the source actually sent.
const passivePageSize = 128
const passiveDeepPageSize = 512

var errPassiveWindow = errors.New("invalid passive collection window")
var errPassivePage = errors.New("upstream passive pagination incomplete")

type passiveWindow struct {
	start, limit, needed int
	hasLimit             bool
}

func parsePassiveWindow(query url.Values) (passiveWindow, error) {
	var out passiveWindow
	for _, key := range []string{"StartIndex", "Limit"} {
		raw := strings.TrimSpace(query.Get(key))
		if raw == "" {
			continue
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			return out, errPassiveWindow
		}
		if key == "StartIndex" {
			out.start = v
		} else {
			out.limit = v
			out.hasLimit = true
		}
	}
	if out.hasLimit {
		if out.start > math.MaxInt-out.limit {
			return out, errPassiveWindow
		}
		out.needed = out.start + out.limit
	}
	return out, nil
}

type passivePageSource struct {
	serverID string
	request  func(context.Context, int, int) (any, error)
	// Completed source-qualified pieces are retained only for this HTTP request.
	items      []map[string]any
	cursor     int
	total      int
	totalKnown bool
	exhausted  bool
	previous   []map[string]any
}

// Source envelopes may omit TotalRecordCount (Latest). A contradictory total
// is an upstream error, never evidence that the source is exhausted.
func acceptPassivePage(source *passivePageSource, payload any, pageSize int) error {
	items := asItems(payload)
	if len(items) > pageSize {
		return fmt.Errorf("%w: source %s ignored limit", errPassivePage, source.serverID)
	}
	if block, ok := payload.(map[string]any); ok {
		if raw, exists := block["TotalRecordCount"]; exists {
			total, valid := numericInt(raw)
			if !valid || total < 0 || total < source.cursor+len(items) {
				return fmt.Errorf("%w: contradictory total for %s", errPassivePage, source.serverID)
			}
			if source.totalKnown && source.total != total {
				return fmt.Errorf("%w: total changed mid-request for %s", errPassivePage, source.serverID)
			}
			source.total, source.totalKnown = total, true
		}
		if raw, exists := block["StartIndex"]; exists {
			start, valid := numericInt(raw)
			if !valid || start != source.cursor {
				return fmt.Errorf("%w: nonprogressing start from %s", errPassivePage, source.serverID)
			}
		}
	}
	if len(items) == 0 && source.totalKnown && source.cursor < source.total {
		return fmt.Errorf("%w: zero-length page before total from %s", errPassivePage, source.serverID)
	}
	if len(items) > 0 && len(source.previous) == len(items) {
		same := true
		for i, item := range items {
			if itemID(item) == "" || itemID(item) != itemID(source.previous[i]) {
				same = false
				break
			}
		}
		if same {
			return fmt.Errorf("%w: repeated cursor page from %s", errPassivePage, source.serverID)
		}
	}
	source.previous = items
	source.items = append(source.items, items...)
	source.cursor += len(items)
	if source.totalKnown {
		source.exhausted = source.cursor >= source.total
	} else {
		source.exhausted = len(items) < pageSize
	}
	return nil
}

func (a *App) collectPassivePages(r *http.Request, sources []passivePageSource, query url.Values, filter func([]map[string]any) []map[string]any, user string) ([]map[string]any, bool, error) {
	window, err := parsePassiveWindow(query)
	if err != nil {
		return nil, false, err
	}
	if len(sources) == 0 {
		return []map[string]any{}, true, nil
	}
	// Global wait is bounded by a deadline rather than an arbitrary item ceiling;
	// a timed-out deep page fails explicitly rather than returning a false tail.
	timeout := 90 * time.Second
	if cfg := a.ConfigStore.Snapshot(); cfg.Timeouts.Global > 90000 {
		timeout = time.Duration(cfg.Timeouts.Global) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	reqCtx := requestContextFrom(r.Context())
	pageSize := passivePageSize
	if window.needed >= 2048 || !window.hasLimit {
		pageSize = passiveDeepPageSize
	}
	checkpoint := window.needed
	if checkpoint < pageSize {
		checkpoint = pageSize
	}
	currentEstimate := 0
	var merged []map[string]any
	var allExhausted bool
	for {
		allExhausted = true
		active := make([]int, 0, len(sources))
		for i := range sources {
			if !sources[i].exhausted {
				allExhausted = false
				active = append(active, i)
			}
		}
		if allExhausted {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}

		type outcome struct {
			i       int
			payload any
			err     error
		}
		ch := make(chan outcome, len(active))
		var wg sync.WaitGroup
		// bounded concurrency, avoiding unbounded fanout when users have many sources
		permits := make(chan struct{}, 4)
		for _, idx := range active {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				select {
				case permits <- struct{}{}:
				case <-ctx.Done():
					ch <- outcome{i: i, err: ctx.Err()}
					return
				}
				defer func() { <-permits }()
				source := &sources[i]
				if !a.isServerAllowed(reqCtx, source.serverID) {
					ch <- outcome{i: i, err: errMediaAccessDenied}
					return
				}
				payload, err := source.request(ctx, source.cursor, pageSize)
				if err == nil && !a.isServerAllowed(reqCtx, source.serverID) {
					err = errMediaAccessDenied
				}
				ch <- outcome{i: i, payload: payload, err: err}
			}(idx)
		}
		wg.Wait()
		close(ch)
		for result := range ch {
			if result.err != nil {
				return nil, false, fmt.Errorf("source %s page: %w", sources[result.i].serverID, result.err)
			}
			if err := acceptPassivePage(&sources[result.i], result.payload, pageSize); err != nil {
				return nil, false, err
			}
		}
		currentEstimate = 0
		for i := range sources {
			currentEstimate += len(sources[i].items)
		}
		allExhausted = true
		for i := range sources {
			if !sources[i].exhausted {
				allExhausted = false
			}
		}

		// For source-order collections, progressively resolve enough canonical
		// entries for the requested page. Explicit sortable collections require
		// verified full materialization to avoid misleading top-K/metadata shifts.
		requestedSort := strings.TrimSpace(query.Get("SortBy"))
		// Random is explicitly best-effort: a random first-page request must
		// not materialize the entire collection merely for an unprovable order.
		requiresExactSort := requestedSort != "" && !strings.EqualFold(requestedSort, "Random")
		if !allExhausted && (requiresExactSort || !window.hasLimit || window.limit == 0 || currentEstimate < checkpoint) {
			continue
		}
		merged = a.mergePassivePages(r, sources, user)
		if filter != nil {
			merged = filter(merged)
		}
		if len(merged) < window.needed && !allExhausted {
			// Overlap is unknown until canonical association; request more rows.
			checkpoint = currentEstimate + max(pageSize, len(merged)/2, window.needed-len(merged))
			continue
		}
		if !allExhausted {
			return merged, false, nil
		}
		break
	}
	if merged == nil {
		merged = a.mergePassivePages(r, sources, user)
		if filter != nil {
			merged = filter(merged)
		}
	}
	return merged, true, nil
}

func (a *App) mergePassivePages(r *http.Request, sources []passivePageSource, user string) []map[string]any {
	results := make([]upstreamItemsResult, 0, len(sources))
	reqCtx := requestContextFrom(r.Context())
	for _, source := range sources {
		if len(source.items) == 0 {
			continue
		}
		results = append(results, upstreamItemsResult{ServerID: source.serverID, Items: source.items,
			FullSources: requestMergeFields(r.URL.Query()), RequestScope: reqCtx})
	}
	if hidden := a.hiddenLibrariesFor(reqCtx); len(hidden) > 0 {
		dropHiddenLibraryViews(results, hidden)
	}
	results = a.hydrateMergeResults(r, results)
	return a.mergeRoundRobinItems(results, user, reqCtx)
}

func passiveSourceQuery(template url.Values, source *UpstreamClient, start, limit int) url.Values {
	q := cloneValues(template)
	q.Set("StartIndex", strconv.Itoa(start))
	q.Set("Limit", strconv.Itoa(limit))
	q.Set("UserId", source.clientUserID())
	return q
}

func (a *App) globalPassiveSources(r *http.Request, path string, query url.Values) []passivePageSource {
	reqCtx := requestContextFrom(r.Context())
	clients := a.allowedClients(reqCtx)
	result := make([]passivePageSource, 0, len(clients))
	for _, client := range clients {
		c := client
		result = append(result, passivePageSource{serverID: c.ID, request: func(ctx context.Context, start, limit int) (any, error) {
			q := passiveSourceQuery(query, c, start, limit)
			endpoint := strings.Replace(path, "%s", c.clientUserID(), 1)
			return c.RequestJSON(ctx, reqCtx, a.Identity, http.MethodGet, endpoint, q, nil)
		}})
	}
	return result
}

// A provisional numeric total keeps Emby clients requesting later pages.
// It is deliberately not presented as exact when sources are unfinished.
func passiveWindowPayload(items []map[string]any, query url.Values, complete bool) map[string]any {
	window, _ := parsePassiveWindow(query)
	total := len(items)
	if !complete {
		total = max(total, window.needed+1)
	}
	start := min(window.start, len(items))
	ending := len(items)
	if window.hasLimit {
		ending = min(ending, window.needed)
	}
	if ending < start {
		ending = start
	}
	return map[string]any{"Items": toAnySlice(items[start:ending]), "StartIndex": window.start, "TotalRecordCount": total}
}

func writePassivePageError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	if errors.Is(err, errPassiveWindow) {
		status = http.StatusBadRequest
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = http.StatusGatewayTimeout
	}
	if errors.Is(err, errMediaAccessDenied) {
		status = http.StatusForbidden
	}
	writeJSON(w, status, map[string]any{"message": "Unable to complete passive collection window"})
}

func passiveGlobalSort(items []map[string]any, query url.Values) {
	keys := splitQueryList(query.Get("SortBy"))
	if len(keys) == 0 {
		return
	}
	descending := strings.EqualFold(query.Get("SortOrder"), "Descending")
	for i := len(keys) - 1; i >= 0; i-- {
		less, ok := localItemLess(keys[i], descending)
		if !ok {
			continue
		}
		sort.SliceStable(items, func(x, y int) bool { return less(items[x], items[y]) })
	}
}
