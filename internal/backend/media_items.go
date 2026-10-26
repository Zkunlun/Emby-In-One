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
	// Multi-instance parents need all original parents, even on one server.
	if parent := firstQueryValue(query, "ParentId", "parentId", "parentid"); !hasBatchIDQuery(query) && parent != "" && !isRootMediaParent(parent) {
		if mapped := a.IDStore.ResolveVirtualID(parent); mapped != nil && len(mapped.OtherInstances) > 0 {
			resolved, ok := a.resolveRequestRouteID(w, r, parent)
			if !ok {
				return
			}
			if resolved == nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{"message": "No upstream for parent"})
				return
			}
			if !a.requireServerAccess(w, r, resolved) {
				return
			}
			if a.handleMultiInstanceParentItems(w, r, resolved, query) {
				return
			}
		}
	}
	if !hasBatchIDQuery(query) {
		filter, localFilter := a.prepareLocalUserFilter(w, r, query)
		if !localFilter {
			parent := firstQueryValue(query, "ParentId", "parentId", "parentid")
			if strings.TrimSpace(query.Get("SearchTerm")) != "" &&
				(parent == "" || parent == "0" || parent == "root") {
				// Some clients search via /Items rather than /Search/Hints.
				// Keep all results inside the same authorized merge/ID rewrite
				// pipeline instead of forwarding raw upstream item IDs.
				requestBoundedSearchCandidateSet(query, r.URL.Query())
				reqCtx := requestContextFrom(r.Context())
				results := a.fetchItemsAcrossUpstreams(r.Context(), reqCtx, "/Items", query, nil)
				if hidden := a.hiddenLibrariesFor(reqCtx); len(hidden) > 0 {
					dropHiddenLibraryViews(results, hidden)
				}
				merged := a.mergedItemsPayload(results, a.clientFacingUserIDFor(r), reqCtx)
				items := asItems(merged)
				a.writePagedUserItems(w, r, items)
				return
			}
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
		a.writePagedUserItems(w, r, kept)
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
		a.writePagedUserItems(w, r, kept)
		return
	}
	a.overlayLocalUserDataItems(r, items)
	writeJSON(w, http.StatusOK, merged)
}

// handleMultiInstanceParentItems merges all original parents before paging.
// The upstream endpoint follows the Hills-tested /Users/{id}/Items contract,
// even when the client-facing route is /Items?ParentId=...
func (a *App) handleMultiInstanceParentItems(w http.ResponseWriter, r *http.Request, resolved *routeResolution, query url.Values) bool {
	reqCtx := requestContextFrom(r.Context())
	instances := a.collectAllowedInstances(reqCtx, resolved)
	if len(instances) < 2 {
		return false
	}
	filter, localFilter := a.prepareLocalUserFilter(w, r, query)
	requestMergedCandidateSet(query)
	var results []upstreamItemsResult
	succeeded := 0
	for _, inst := range instances {
		if !a.isServerAllowed(reqCtx, inst.ServerID) {
			continue
		}
		q := cloneValues(query)
		q.Del("parentId")
		q.Del("parentid")
		q.Set("ParentId", inst.OriginalID)
		full := requestMergeFields(q)
		removePathScopedItemsUserQuery(q)
		path := "/Users/" + inst.Client.clientUserID() + "/Items"
		payload, err := inst.Client.RequestJSON(r.Context(), reqCtx, a.Identity, http.MethodGet, path, q, nil)
		if err != nil || !a.isServerAllowed(reqCtx, inst.ServerID) {
			continue
		}
		succeeded++
		results = append(results, upstreamItemsResult{ServerID: inst.ServerID, Items: asItems(payload), FullSources: full, RequestScope: reqCtx})
	}
	if succeeded == 0 {
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Upstream parent items request failed"})
		return true
	}
	results = a.hydrateMergeResults(r, results)
	items := a.mergeRoundRobinItems(results, a.clientFacingUserIDFor(r), reqCtx)
	if localFilter {
		var recency map[string]int64
		items, recency = a.filterItemsByLocalUserState(r, items, filter)
		localItemSort(items, r.URL.Query(), recency)
	}
	a.writePagedUserItems(w, r, items)
	return true
}
func (a *App) handleUserItems(w http.ResponseWriter, r *http.Request) {
	query := cloneValues(r.URL.Query())
	parentID := firstQueryValue(query, "ParentId", "parentId", "parentid")
	if parentID != "" && !isRootMediaParent(parentID) {
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
		if a.handleMultiInstanceParentItems(w, r, resolved, query) {
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
		removePathScopedItemsUserQuery(query)
		payload, err := resolved.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Users/"+resolved.Client.clientUserID()+"/Items", query, nil)
		if err != nil {
			// The upstream HTTP error may include its raw response body, which is
			// internal diagnostic data rather than a client-safe API message.
			writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Upstream items request failed"})
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
			a.writePagedUserItems(w, r, kept)
			return
		}
		a.overlayLocalUserDataItems(r, items)
		writeJSON(w, http.StatusOK, payload)
		return
	}
	clearRootMediaParent(query)
	filter, localFilter := a.prepareLocalUserFilter(w, r, query)
	if !localFilter {
		if strings.TrimSpace(query.Get("SearchTerm")) != "" {
			requestBoundedSearchCandidateSet(query, r.URL.Query())
		} else {
			requestMergedCandidateSet(query)
		}
	}
	results := a.fetchItemsAcrossUpstreams(r.Context(), requestContextFrom(r.Context()), "/Users/%s/Items", query, nil)
	if a.failAllMediaListUpstreams(w, r, results) {
		return
	}
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
			a.writePagedUserItems(w, r, kept)
			return
		}
		if len(asMaps) >= mergedItemsScanLimit {
			a.warnTruncatedMerge(len(asMaps))
		}
		a.writePagedUserItems(w, r, asMaps)
		return
	}
	writeJSON(w, http.StatusOK, merged)
}

func (a *App) handleUserItemsLatest(w http.ResponseWriter, r *http.Request) {
	query := cloneValues(r.URL.Query())
	filter, localFilter := a.prepareLocalUserFilter(w, r, query)
	parentID := firstQueryValue(query, "ParentId", "parentId", "parentid")
	if parentID != "" && !isRootMediaParent(parentID) {
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
		query.Del("parentId")
		query.Del("parentid")
		query.Set("UserId", resolved.Client.clientUserID())
		fullSources := requestMergeFields(query)
		payload, err := resolved.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Users/"+resolved.Client.clientUserID()+"/Items/Latest", query, nil)
		if err != nil {
			// Keep real gateway failures visible without disclosing the upstream
			// response body or source-specific internal diagnostics.
			writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Upstream latest items request failed"})
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
	clearRootMediaParent(query)
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
	if a.failAllMediaListUpstreams(w, r, collected) {
		return
	}
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
		// RequestJSON errors can embed full upstream responses and private details.
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Upstream similar items request failed"})
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
		a.writePagedUserItems(w, r, kept)
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
		// Upstream HTTP errors contain raw response bodies. Do not return them
		// to media clients; keep the genuine gateway failure status instead.
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Upstream theme media request failed"})
		return
	}
	if !a.requireServerAccess(w, r, resolved) {
		return
	}
	payload = a.rewriteVersionAwarePayload(r, payload, resolved.ServerID)
	a.normalizeLocalUserDataPayload(r, payload)
	writeJSON(w, http.StatusOK, payload)
}

// A /Users/{UserId}/Items endpoint already scopes its user in the path.
// Discard all case variants of the redundant Query alias; do not change
// /Items (whose Query UserId is meaningful) or other endpoints.
func removePathScopedItemsUserQuery(query url.Values) {
	for key := range query {
		if strings.EqualFold(key, "UserId") {
			delete(query, key)
		}
	}
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
				// /Items needs a real upstream Query UserId, while the
				// /Users/{id}/Items route already encodes it in its path.
				removePathScopedItemsUserQuery(serverQuery)
				if pathTemplate != "/Users/%s/Items" {
					serverQuery.Set("UserId", c.clientUserID())
				}
				if hasBatchIDQuery(serverQuery) {
					translated, ok := translateBatchIDQueryForServer(serverQuery, c.ID, a.IDStore)
					if !ok {
						return upstreamItemsResult{Err: errBatchQueryNotTranslatable}
					}
					serverQuery = translated
				}
				path := strings.Replace(pathTemplate, "%s", c.clientUserID(), 1)
				payload, err := c.RequestJSON(bgCtx, reqCtx, a.Identity, http.MethodGet, path, serverQuery, body)
				// Some Emby-compatible upstreams implement only one of the
				// /Items and /Users/{id}/Items search surfaces. Only root-level
				// nonblank SearchTerm queries may try the sibling endpoint.
				// Never fall back after auth/server/transport errors or for
				// ParentId-scoped browsing, which must stay within its library.
				parent := firstQueryValue(serverQuery, "ParentId", "parentId", "parentid")
				canTrySibling := (pathTemplate == "/Items" || pathTemplate == "/Users/%s/Items") &&
					strings.TrimSpace(serverQuery.Get("SearchTerm")) != "" &&
					(parent == "" || parent == "0" || parent == "root") &&
					c.clientUserID() != ""
				if canTrySibling &&
					((err == nil && len(asItems(payload)) == 0) || searchEndpointUnsupported(err, path)) &&
					bgCtx.Err() == nil && a.isServerAllowed(reqCtx, c.ID) {
					alternative := "/Items"
					if pathTemplate == "/Items" {
						alternative = "/Users/" + url.PathEscape(c.clientUserID()) + "/Items"
					}
					bounded, valid := boundedSearchAlternativeQuery(serverQuery, c.clientUserID())
					if valid {
						if pathTemplate == "/Items" {
							// /Items -> /Users/{id}/Items sibling fallback:
							// keep the real path user, never duplicate it.
							removePathScopedItemsUserQuery(bounded)
						}
						alternativePayload, alternativeErr := c.RequestJSON(bgCtx, reqCtx, a.Identity, http.MethodGet, alternative, bounded, nil)
						if alternativeErr == nil {
							payload, err = alternativePayload, nil
						} else if a.Logger != nil && a.noticeThrottle.allow("user-items-search-alternative:"+c.ID) {
							a.Logger.Warnf("Items search alternate endpoint failed (no query or credentials logged)")
						}
					}
				}
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

// Search pages only need the candidate prefix that could fill the requested
// window. Do not fetch the inherited 5000-item generic listing limit on each
// keystroke. Keep the legacy safety cap and merge-before-page semantics.
func requestBoundedSearchCandidateSet(query, requested url.Values) {
	start := 0
	if parsed, err := strconv.Atoi(requested.Get("StartIndex")); err == nil && parsed > 0 {
		start = parsed
	}
	pageSize := 20
	if parsed, err := strconv.Atoi(requested.Get("Limit")); err == nil && parsed > 0 {
		pageSize = parsed
	}
	candidateCount := mergedItemsScanLimit
	if start < mergedItemsScanLimit && pageSize < mergedItemsScanLimit-start {
		candidateCount = start + pageSize
	}
	query.Set("StartIndex", "0")
	query.Set("Limit", strconv.Itoa(candidateCount))
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

// Overlay only the rows the client will actually receive. The full candidate
// set is still used for cross-source merge, local filtering and accurate totals.
// Overlaying thousands of off-page rows wastes SQLite batch reads and heap.
func (a *App) writePagedUserItems(w http.ResponseWriter, r *http.Request, items []map[string]any) {
	page := paginateItems(items, r.URL.Query())
	a.overlayLocalUserDataItems(r, asItems(page))
	writeJSON(w, http.StatusOK, page)
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

// The Emby root may be expressed by omitted ParentId, 0, or root.
func isRootMediaParent(value string) bool {
	value = strings.TrimSpace(value)
	return value == "0" || strings.EqualFold(value, "root")
}

func clearRootMediaParent(query url.Values) {
	if isRootMediaParent(firstQueryValue(query, "ParentId", "parentId", "parentid")) {
		query.Del("ParentId")
		query.Del("parentId")
		query.Del("parentid")
	}
}

// Partial results are allowed; only an actual total failure among reachable,
// authorized sources becomes 502. Empty successful pages and zero accessible
// online sources retain their existing successful empty-list contract.
func (a *App) failAllMediaListUpstreams(w http.ResponseWriter, r *http.Request, successful []upstreamItemsResult) bool {
	if len(successful) > 0 || len(a.allowedClients(requestContextFrom(r.Context()))) == 0 {
		return false
	}
	if r.Context().Err() != nil {
		return true
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{"message": "All available upstream media requests failed"})
	return true
}

func firstQueryValue(values url.Values, keys ...string) string {
	for _, key := range keys {
		if value := values.Get(key); value != "" {
			return value
		}
	}
	return ""
}
