package backend

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func (a *App) handleItemsCollection(w http.ResponseWriter, r *http.Request) {
	query := cloneValues(r.URL.Query())
	if !hasBatchIDQuery(query) {
		filter, localFilter := a.prepareLocalUserFilter(w, r, query)
		if !localFilter {
			a.handleFallbackProxy(w, r)
			return
		}
		// A regular user's state filter cannot be forwarded to the shared upstream
		// account. Fetch the candidate set across allowed servers, then filter/page it
		// locally using virtual IDs.
		results := a.fetchItemsAcrossUpstreams(r.Context(), requestContextFrom(r.Context()), "/Items", query, nil)
		merged := a.mergedItemsPayload(results, a.clientFacingUserIDFor(r), requestContextFrom(r.Context()))
		items := asItems(merged)
		kept, recency := a.filterItemsByLocalUserState(r, items, filter)
		localItemSort(kept, r.URL.Query(), recency)
		a.overlayLocalUserDataItems(r, kept)
		writeJSON(w, http.StatusOK, paginateItems(kept, r.URL.Query()))
		return
	}
	// The candidate set is the ID list the client sent, so a user-state filter can be
	// answered exactly here: no scan limit and no upstream paging to undo.
	filter, localFilter := a.localUserStateIntersect(w, r, query)
	reqCtx := requestContextFrom(r.Context())
	clients := a.allowedClients(reqCtx)
	if len(clients) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
		return
	}

	cfg := a.ConfigStore.Snapshot()
	globalTimeout := time.Duration(cfg.Timeouts.Global) * time.Millisecond
	if globalTimeout <= 0 {
		globalTimeout = 15 * time.Second
	}

	tasks := make([]upstreamTask, len(clients))
	for i, client := range clients {
		c := client
		tasks[i] = upstreamTask{
			index: i,
			fn: func(bgCtx context.Context) upstreamItemsResult {
				serverQuery, ok := translateBatchIDQueryForServer(query, c.ID, a.IDStore)
				if !ok {
					return upstreamItemsResult{Err: errBatchQueryNotTranslatable}
				}
				fullSources := requestMergeFields(serverQuery)
				payload, err := c.RequestJSON(bgCtx, reqCtx, a.Identity, http.MethodGet, "/Items", serverQuery, nil)
				if err != nil {
					return upstreamItemsResult{Err: err}
				}
				return upstreamItemsResult{ServerID: c.ID, Items: asItems(payload), FullSources: fullSources, RequestScope: reqCtx}
			},
		}
	}

	collected := a.aggregateUpstreams(r.Context(), aggregationConfig{
		gracePeriod:   time.Duration(cfg.Timeouts.SearchGracePeriod) * time.Millisecond,
		globalTimeout: globalTimeout,
	}, tasks)
	collected = a.hydrateMergeResults(r, collected)
	merged := a.mergedItemsPayload(collected, a.clientFacingUserIDFor(r), reqCtx)
	items := a.filterRequestedMergeItems(query, asItems(merged))
	merged["Items"] = items
	merged["TotalRecordCount"] = len(items)
	if localFilter {
		kept, recency := a.filterItemsByLocalUserState(r, items, filter)
		localItemSort(kept, r.URL.Query(), recency)
		a.overlayLocalUserDataItems(r, kept)
		writeJSON(w, http.StatusOK, paginateItems(kept, r.URL.Query()))
		return
	}
	a.overlayLocalUserDataItems(r, items)
	writeJSON(w, http.StatusOK, merged)
}

func (a *App) handleUserItems(w http.ResponseWriter, r *http.Request) {
	query := cloneValues(r.URL.Query())
	parentID := firstQueryValue(query, "ParentId", "parentId", "parentid")
	if parentID != "" && parentID != "0" && parentID != "root" {
		resolved, routeOK := a.resolveRequestRouteID(w, r, parentID)
		if !routeOK {
			return
		}
		if resolved == nil {
			writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
			return
		}
		if !a.requireServerAccess(w, r, resolved) {
			return
		}
		query.Set("ParentId", resolved.OriginalID)
		query.Del("parentId")
		query.Del("parentid")
		// A user-state filter is answered from the local record: the upstream still
		// scopes the query to the requested container, but it no longer decides which
		// of that container's items the user sees.
		filter, localFilter := a.prepareLocalUserFilter(w, r, query)
		fullSources := requestMergeFields(query)
		payload, err := resolved.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Users/"+resolved.Client.clientUserID()+"/Items", query, nil)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"message": err.Error()})
			return
		}
		if !a.requireServerAccess(w, r, resolved) {
			return
		}
		items := a.rewriteMergeResponseItems(r, asItems(payload), resolved.ServerID, fullSources)
		if block, ok := payload.(map[string]any); ok {
			block["Items"] = toAnySlice(items)
		}
		if localFilter {
			kept, recency := a.filterItemsByLocalUserState(r, items, filter)
			localItemSort(kept, r.URL.Query(), recency)
			a.overlayLocalUserDataItems(r, kept)
			writeJSON(w, http.StatusOK, paginateItems(kept, r.URL.Query()))
			return
		}
		a.overlayLocalUserDataItems(r, items)
		writeJSON(w, http.StatusOK, payload)
		return
	}
	filter, localFilter := a.prepareLocalUserFilter(w, r, query)
	if !localFilter {
		requestMergedCandidateSet(query)
	}
	results := a.fetchItemsAcrossUpstreams(r.Context(), requestContextFrom(r.Context()), "/Users/%s/Items", query, nil)
	// Root-level listings include the library views themselves; some clients
	// browse the root through this endpoint instead of /Users/{id}/Views, so
	// hidden libraries must be dropped here too. Content items are never
	// touched — dropHiddenLibraryViews matches on the library item types only.
	if hidden := a.hiddenLibrariesFor(requestContextFrom(r.Context())); len(hidden) > 0 {
		dropHiddenLibraryViews(results, hidden)
	}
	merged := a.mergedItemsPayload(results, a.clientFacingUserIDFor(r), requestContextFrom(r.Context()))
	if items, ok := merged["Items"].([]any); ok {
		asMaps := make([]map[string]any, 0, len(items))
		for _, item := range items {
			if m, ok := item.(map[string]any); ok {
				asMaps = append(asMaps, m)
			}
		}
		if localFilter {
			kept, recency := a.filterItemsByLocalUserState(r, asMaps, filter)
			localItemSort(kept, r.URL.Query(), recency)
			a.overlayLocalUserDataItems(r, kept)
			writeJSON(w, http.StatusOK, paginateItems(kept, r.URL.Query()))
			return
		}
		if len(asMaps) >= mergedItemsScanLimit {
			a.warnTruncatedMerge(len(asMaps))
		}
		a.overlayLocalUserDataItems(r, asMaps)
		writeJSON(w, http.StatusOK, paginateItems(asMaps, r.URL.Query()))
		return
	}
	writeJSON(w, http.StatusOK, merged)
}

func (a *App) handleUserItemsLatest(w http.ResponseWriter, r *http.Request) {
	query := cloneValues(r.URL.Query())
	filter, localFilter := a.prepareLocalUserFilter(w, r, query)
	parentID := query.Get("ParentId")
	if parentID != "" {
		resolved, routeOK := a.resolveRequestRouteID(w, r, parentID)
		if !routeOK {
			return
		}
		if resolved == nil {
			writeJSON(w, http.StatusOK, []any{})
			return
		}
		if !a.requireServerAccess(w, r, resolved) {
			return
		}
		query.Set("ParentId", resolved.OriginalID)
		query.Set("UserId", resolved.Client.clientUserID())
		fullSources := requestMergeFields(query)
		payload, err := resolved.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Users/"+resolved.Client.clientUserID()+"/Items/Latest", query, nil)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"message": err.Error()})
			return
		}
		if !a.requireServerAccess(w, r, resolved) {
			return
		}
		items := a.rewriteMergeResponseItems(r, asItems(payload), resolved.ServerID, fullSources)
		if localFilter {
			var recency map[string]int64
			items, recency = a.filterItemsByLocalUserState(r, items, filter)
			localItemSort(items, r.URL.Query(), recency)
			items = asItems(paginateItems(items, r.URL.Query()))
		}
		a.overlayLocalUserDataItems(r, items)
		writeJSON(w, http.StatusOK, items)
		return
	}
	reqCtx := requestContextFrom(r.Context())
	clients := a.allowedClients(reqCtx)
	cfg := a.ConfigStore.Snapshot()
	globalTimeout := time.Duration(cfg.Timeouts.Global) * time.Millisecond
	if globalTimeout <= 0 {
		globalTimeout = 15 * time.Second
	}

	tasks := make([]upstreamTask, len(clients))
	for i, client := range clients {
		c := client
		tasks[i] = upstreamTask{
			index: i,
			fn: func(bgCtx context.Context) upstreamItemsResult {
				instQuery := cloneValues(query)
				instQuery.Set("UserId", c.clientUserID())
				fullSources := requestMergeFields(instQuery)
				payload, err := c.RequestJSON(bgCtx, reqCtx, a.Identity, http.MethodGet, "/Users/"+c.clientUserID()+"/Items/Latest", instQuery, nil)
				if err != nil {
					return upstreamItemsResult{Err: err}
				}
				if !a.isServerAllowed(reqCtx, c.ID) {
					return upstreamItemsResult{Err: errMediaAccessDenied}
				}
				return upstreamItemsResult{ServerID: c.ID, Items: asItems(payload), FullSources: fullSources, RequestScope: reqCtx}
			},
		}
	}

	collected := a.aggregateUpstreams(r.Context(), aggregationConfig{
		gracePeriod:   time.Duration(cfg.Timeouts.LatestGracePeriod) * time.Millisecond,
		globalTimeout: globalTimeout,
	}, tasks)
	collected = a.hydrateMergeResults(r, collected)
	allItems := a.mergeRoundRobinItems(collected, a.clientFacingUserIDFor(r), reqCtx)
	if localFilter {
		var recency map[string]int64
		allItems, recency = a.filterItemsByLocalUserState(r, allItems, filter)
		localItemSort(allItems, r.URL.Query(), recency)
		allItems = asItems(paginateItems(allItems, r.URL.Query()))
	}
	a.overlayLocalUserDataItems(r, allItems)
	writeJSON(w, http.StatusOK, allItems)
}

func (a *App) handleUserItemByID(w http.ResponseWriter, r *http.Request) {
	a.handleMergeItemDetail(w, r, true)
}

func (a *App) handleItemByID(w http.ResponseWriter, r *http.Request) {
	a.handleMergeItemDetail(w, r, false)
}

func (a *App) handleItemSimilar(w http.ResponseWriter, r *http.Request) {
	resolved, routeOK := a.resolveRequestRouteID(w, r, r.PathValue("itemId"))
	if !routeOK {
		return
	}
	if resolved == nil {
		writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0})
		return
	}
	if !a.requireServerAccess(w, r, resolved) {
		return
	}
	query := cloneValues(r.URL.Query())
	query.Set("UserId", resolved.Client.clientUserID())
	filter, localFilter := a.prepareLocalUserFilter(w, r, query)
	payload, err := resolved.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Items/"+resolved.OriginalID+"/Similar", query, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": err.Error()})
		return
	}
	if !a.requireServerAccess(w, r, resolved) {
		return
	}
	items := a.rewriteMergeResponseItems(r, asItems(payload), resolved.ServerID, false)
	if page, ok := payload.(map[string]any); ok {
		page["Items"] = toAnySlice(items)
	}
	if localFilter {
		kept, recency := a.filterItemsByLocalUserState(r, items, filter)
		localItemSort(kept, r.URL.Query(), recency)
		a.overlayLocalUserDataItems(r, kept)
		writeJSON(w, http.StatusOK, paginateItems(kept, r.URL.Query()))
		return
	}
	a.overlayLocalUserDataItems(r, items)
	writeJSON(w, http.StatusOK, payload)
}

func (a *App) handleItemThemeMedia(w http.ResponseWriter, r *http.Request) {
	resolved, routeOK := a.resolveRequestRouteID(w, r, r.PathValue("itemId"))
	if !routeOK {
		return
	}
	if resolved == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ThemeVideosResult":     map[string]any{"Items": []any{}, "TotalRecordCount": 0},
			"ThemeSongsResult":      map[string]any{"Items": []any{}, "TotalRecordCount": 0},
			"SoundtrackSongsResult": map[string]any{"Items": []any{}, "TotalRecordCount": 0},
		})
		return
	}
	if !a.requireServerAccess(w, r, resolved) {
		return
	}
	query := cloneValues(r.URL.Query())
	query.Set("UserId", resolved.Client.clientUserID())
	payload, err := resolved.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Items/"+resolved.OriginalID+"/ThemeMedia", query, nil)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": err.Error()})
		return
	}
	if !a.requireServerAccess(w, r, resolved) {
		return
	}
	payload = a.rewriteVersionAwarePayload(r, payload, resolved.ServerID)
	a.normalizeLocalUserDataPayload(r, payload)
	writeJSON(w, http.StatusOK, payload)
}

func (a *App) fetchItemsAcrossUpstreams(ctx context.Context, reqCtx *RequestContext, pathTemplate string, query url.Values, body any) []upstreamItemsResult {
	clients := a.allowedClients(reqCtx)
	if len(clients) == 0 {
		return nil
	}
	cfg := a.ConfigStore.Snapshot()
	globalTimeout := time.Duration(cfg.Timeouts.Global) * time.Millisecond
	if globalTimeout <= 0 {
		globalTimeout = 15 * time.Second
	}

	tasks := make([]upstreamTask, len(clients))
	for i, client := range clients {
		c := client
		tasks[i] = upstreamTask{
			index: i,
			fn: func(bgCtx context.Context) upstreamItemsResult {
				serverQuery := cloneValues(query)
				fullSources := requestMergeFields(serverQuery)
				// Clients only ever hold EIO's virtual user ID, which no upstream knows.
				serverQuery.Set("UserId", c.clientUserID())
				if hasBatchIDQuery(serverQuery) {
					translated, ok := translateBatchIDQueryForServer(serverQuery, c.ID, a.IDStore)
					if !ok {
						return upstreamItemsResult{Err: errBatchQueryNotTranslatable}
					}
					serverQuery = translated
				}
				payload, err := c.RequestJSON(bgCtx, reqCtx, a.Identity, http.MethodGet, strings.Replace(pathTemplate, "%s", c.clientUserID(), 1), serverQuery, body)
				if err != nil {
					return upstreamItemsResult{Err: err}
				}
				items := asItems(payload)
				if pathTemplate == "/Shows/NextUp" {
					for _, item := range items {
						if kind, _ := item["Type"].(string); kind == "" {
							item["Type"] = "Episode"
						}
					}
				}
				return upstreamItemsResult{ServerID: c.ID, Items: items, FullSources: fullSources, RequestScope: reqCtx}
			},
		}
	}

	results := a.aggregateUpstreams(ctx, aggregationConfig{
		gracePeriod:   time.Duration(cfg.Timeouts.SearchGracePeriod) * time.Millisecond,
		globalTimeout: globalTimeout,
	}, tasks)
	return a.hydrateMergeResults((&http.Request{}).WithContext(ctx), results)
}

// getItemKey generates a deduplication key for an item.
// Returns empty string if the item cannot be deduplicated.
func getItemKey(item map[string]any) string {
	// Priority 1: TMDB ID
	if providerIDs, ok := item["ProviderIds"].(map[string]any); ok {
		if tmdb, ok := providerIDs["Tmdb"].(string); ok && tmdb != "" {
			return "tmdb:" + tmdb
		}
	}
	// Priority 2: Movie/Series by Name + Year
	itemType, _ := item["Type"].(string)
	if itemType == "Movie" || itemType == "Series" {
		name, _ := item["Name"].(string)
		year := ""
		if y, ok := numericInt(item["ProductionYear"]); ok {
			year = strconv.Itoa(y)
		}
		return "name:" + strings.ToLower(name) + ":" + year
	}
	// Priority 3: Episode by SeriesName + Season:Episode
	if itemType == "Episode" {
		seriesName, _ := item["SeriesName"].(string)
		parentIdx, okP := numericInt(item["ParentIndexNumber"])
		idx, okE := numericInt(item["IndexNumber"])
		if seriesName != "" && okP && okE {
			return "ep:" + strings.ToLower(seriesName) + ":S" + strconv.Itoa(parentIdx) + "E" + strconv.Itoa(idx)
		}
	}
	return ""
}

// containsChinese checks if a string contains CJK Unified Ideographs (U+4E00–U+9FA5).
func containsChinese(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FA5 {
			return true
		}
	}
	return false
}

// isBetterMetadata returns true if the candidate item from candidateServerID has
// better metadata than the existing item from existingServerID, using the V1.2
// 4-level priority: priorityMetadata flag → Chinese in Overview → longer
// Overview → earlier upstream order.
func isBetterMetadata(existing map[string]any, existingServerID string, candidate map[string]any, candidateServerID string, cfg Config) bool {
	// 1. priorityMetadata flag
	existingPriority := false
	candidatePriority := false
	existingOrder := len(cfg.Upstream)
	candidateOrder := len(cfg.Upstream)
	for idx, u := range cfg.Upstream {
		if u.ID == existingServerID {
			existingPriority = u.PriorityMetadata
			existingOrder = idx
		}
		if u.ID == candidateServerID {
			candidatePriority = u.PriorityMetadata
			candidateOrder = idx
		}
	}
	if candidatePriority && !existingPriority {
		return true
	}
	if existingPriority && !candidatePriority {
		return false
	}

	// 2. Chinese in Overview
	existingOverview, _ := existing["Overview"].(string)
	candidateOverview, _ := candidate["Overview"].(string)
	hasChinese1 := containsChinese(existingOverview)
	hasChinese2 := containsChinese(candidateOverview)
	if hasChinese2 && !hasChinese1 {
		return true
	}
	if hasChinese1 && !hasChinese2 {
		return false
	}

	// 3. Longer Overview
	if len(candidateOverview) > len(existingOverview) {
		return true
	}
	if len(existingOverview) > len(candidateOverview) {
		return false
	}

	// 4. Lower server index (order in cfg.Upstream)
	return candidateOrder < existingOrder
}

func (a *App) mergedItemsPayload(results []upstreamItemsResult, clientUserID string, requestScopes ...*RequestContext) map[string]any {
	merged := a.mergeRoundRobinItems(results, clientUserID, requestScopes...)
	return map[string]any{
		"Items":            toAnySlice(merged),
		"TotalRecordCount": len(merged),
		"StartIndex":       0,
	}
}

// Demand discovery uses precise version membership, never whole-item source projection.
func (a *App) mergeRoundRobinItems(results []upstreamItemsResult, clientUserID string, requestScopes ...*RequestContext) []map[string]any {
	var reqCtx *RequestContext
	if len(requestScopes) > 0 {
		reqCtx = requestScopes[0]
		results = a.filterAllowedUpstreamResults(reqCtx, results)
	}
	return a.mergeHTTPItems(results, clientUserID, reqCtx)
}

// mergedItemsScanLimit caps how many items one merged (no ParentId) item query may pull
// from each upstream.
const mergedItemsScanLimit = 5000

// requestMergedCandidateSet asks the upstreams for the candidate set instead of the
// client's page. Paging belongs to the proxy on this path: the answers are merged and
// deduplicated before the page is cut, so a window forwarded upstream would be cut
// twice — once there, once by paginateItems — and the merged total would be the page
// size rather than the library size, telling the client there is no page 2.
func requestMergedCandidateSet(query url.Values) {
	query.Set("StartIndex", "0")
	query.Set("Limit", strconv.Itoa(mergedItemsScanLimit))
}

// warnTruncatedMerge reports that the merged candidate set hit the scan cap, so the
// reported total is a lower bound and the tail of the library is unreachable.
func (a *App) warnTruncatedMerge(count int) {
	if a.Logger == nil || !a.noticeThrottle.allow("merged-scan") {
		return
	}
	a.Logger.Warnf("merged item query scanned %d upstream items (limit %d): the total and the tail of the list may be truncated",
		count, mergedItemsScanLimit)
}

func paginateItems(merged []map[string]any, query url.Values) map[string]any {
	totalCount := len(merged)
	startIndex := 0
	if s := query.Get("StartIndex"); s != "" {
		if v, err := strconv.Atoi(s); err == nil && v >= 0 {
			startIndex = v
		}
	}
	if startIndex > len(merged) {
		startIndex = len(merged)
	}
	merged = merged[startIndex:]
	if lim := query.Get("Limit"); lim != "" {
		if v, err := strconv.Atoi(lim); err == nil && v >= 0 && v < len(merged) {
			merged = merged[:v]
		}
	}
	return map[string]any{
		"Items":            toAnySlice(merged),
		"TotalRecordCount": totalCount,
		"StartIndex":       startIndex,
	}
}

func firstQueryValue(values url.Values, keys ...string) string {
	for _, key := range keys {
		if value := values.Get(key); value != "" {
			return value
		}
	}
	return ""
}
