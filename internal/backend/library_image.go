package backend

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type indexedItem struct {
	Item     map[string]any
	ServerID string
	SortA    int
	SortB    int
}

func (a *App) registerLibraryAndImageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /Items/Counts", a.withCountsContext(a.requireCountsAuth(a.handleItemsCounts)))
	mux.HandleFunc("GET /Library/VirtualFolders", a.withContext(a.requireAuth(a.handleLibraryVirtualFolders)))
	mux.HandleFunc("GET /Library/SelectableRemoteLibraries", a.withContext(a.requireAuth(a.handleLibrarySelectableRemoteLibraries)))
	mux.HandleFunc("GET /Library/MediaFolders", a.withContext(a.requireAuth(a.handleLibraryMediaFolders)))
	for _, endpoint := range []string{"Genres", "MusicGenres", "Studios", "Persons", "Artists", "Artists/AlbumArtists"} {
		current := endpoint
		mux.HandleFunc("GET /"+current, a.withContext(a.requireAuth(func(w http.ResponseWriter, r *http.Request) {
			a.handleLibraryTaxonomy(w, r, current)
		})))
	}
	mux.HandleFunc("GET /Shows/{seriesId}/Seasons", a.withContext(a.requireAuth(a.handleShowsSeasons)))
	mux.HandleFunc("GET /Shows/{seriesId}/Episodes", a.withContext(a.requireAuth(a.handleShowsEpisodes)))
	mux.HandleFunc("GET /Search/Hints", a.withContext(a.requireAuth(a.handleSearchHints)))
	mux.HandleFunc("GET /Items/{itemId}/Images/{imageType}", a.withContext(a.handleItemImage))
	mux.HandleFunc("GET /Items/{itemId}/Images/{imageType}/{imageIndex}", a.withContext(a.handleItemImage))
	mux.HandleFunc("GET /Users/{userId}/Images/{imageType}", a.withContext(a.handleUserImageNotFound))
	mux.HandleFunc("GET /Users/{userId}/Images/{imageType}/{imageIndex}", a.withContext(a.handleUserImageNotFound))
}

func (a *App) handleLibraryVirtualFolders(w http.ResponseWriter, r *http.Request) {
	a.handleLibraryNamedArray(w, r, "/Library/VirtualFolders")
}

func (a *App) handleLibrarySelectableRemoteLibraries(w http.ResponseWriter, r *http.Request) {
	a.handleLibraryNamedArray(w, r, "/Library/SelectableRemoteLibraries")
}

func (a *App) handleLibraryNamedArray(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	reqCtx := requestContextFrom(r.Context())
	onlineClients := a.allowedClients(reqCtx)
	cfg := a.ConfigStore.Snapshot()
	multiSource := len(onlineClients) > 1
	hidden := a.hiddenLibrariesFor(reqCtx)

	groups := fanOutClients(onlineClients, func(c *UpstreamClient) []map[string]any {
		payload, err := c.RequestJSON(r.Context(), reqCtx, a.Identity, http.MethodGet, upstreamPath, cloneValues(r.URL.Query()), nil)
		if err != nil {
			return nil
		}
		items := asItems(payload)
		items = filterHiddenLibraryItems(items, c.ID, hidden)
		for _, item := range items {
			rewriteResponseIDs(item, c.ID, a.IDStore, cfg.Server.ID, a.clientFacingUserIDFor(r))
			if multiSource {
				if name, _ := item["Name"].(string); name != "" {
					item["Name"] = name + " (" + c.Name + ")"
				}
			}
		}
		return items
	})
	writeJSON(w, http.StatusOK, flattenItems(groups))
}

func (a *App) handleLibraryMediaFolders(w http.ResponseWriter, r *http.Request) {
	reqCtx := requestContextFrom(r.Context())
	clients := a.allowedClients(reqCtx)
	cfg := a.ConfigStore.Snapshot()
	hidden := a.hiddenLibrariesFor(reqCtx)

	groups := fanOutClients(clients, func(c *UpstreamClient) []map[string]any {
		payload, err := c.RequestJSON(r.Context(), reqCtx, a.Identity, http.MethodGet, "/Library/MediaFolders", cloneValues(r.URL.Query()), nil)
		if err != nil {
			return nil
		}
		items := asItems(payload)
		items = filterHiddenLibraryItems(items, c.ID, hidden)
		for _, item := range items {
			rewriteResponseIDs(item, c.ID, a.IDStore, cfg.Server.ID, a.clientFacingUserIDFor(r))
		}
		return items
	})
	results := flattenItems(groups)
	writeJSON(w, http.StatusOK, map[string]any{"Items": toAnySlice(results), "TotalRecordCount": len(results), "StartIndex": 0})
}

func (a *App) handleLibraryTaxonomy(w http.ResponseWriter, r *http.Request, endpoint string) {
	reqCtx := requestContextFrom(r.Context())
	clients := a.allowedClients(reqCtx)
	cfg := a.ConfigStore.Snapshot()

	groups := fanOutClients(clients, func(c *UpstreamClient) []map[string]any {
		query := cloneValues(r.URL.Query())
		query.Set("UserId", c.clientUserID())
		if hasBatchIDQuery(query) {
			translated, ok := translateBatchIDQueryForServer(query, c.ID, a.IDStore)
			if !ok {
				return nil
			}
			query = translated
		}
		payload, err := c.RequestJSON(r.Context(), reqCtx, a.Identity, http.MethodGet, "/"+endpoint, query, nil)
		if err != nil {
			return nil
		}
		items := asItems(payload)
		for _, item := range items {
			rewriteResponseIDs(item, c.ID, a.IDStore, cfg.Server.ID, a.clientFacingUserIDFor(r))
		}
		return items
	})
	results := flattenItems(groups)
	writeJSON(w, http.StatusOK, map[string]any{"Items": toAnySlice(results), "TotalRecordCount": len(results), "StartIndex": 0})
}
func (a *App) handleShowsSeasons(w http.ResponseWriter, r *http.Request) {
	a.handleMergeShows(w, r, "Season", "Seasons")
}
func (a *App) handleShowsEpisodes(w http.ResponseWriter, r *http.Request) {
	a.handleMergeShows(w, r, "Episode", "Episodes")
}

func (a *App) handleMergeShows(w http.ResponseWriter, r *http.Request, kind, endpoint string) {
	resolved, ok := a.resolveRequestRouteID(w, r, r.PathValue("seriesId"))
	if !ok {
		return
	}
	empty := map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0}
	if resolved == nil {
		writeJSON(w, http.StatusOK, empty)
		return
	}
	if !a.requireServerAccess(w, r, resolved) {
		return
	}
	reqCtx := requestContextFrom(r.Context())
	queryTemplate := cloneValues(r.URL.Query())
	filter, localFilter := a.prepareLocalUserFilter(w, r, queryTemplate)
	if !localFilter {
		requestMergedCandidateSet(queryTemplate)
	}
	full := requestMergeFields(queryTemplate)
	seasonID := firstQueryValue(queryTemplate, "SeasonId", "seasonId", "seasonid")
	if seasonID != "" {
		season, valid := a.resolveRequestRouteID(w, r, seasonID)
		if !valid {
			return
		}
		if season == nil {
			writeJSON(w, http.StatusOK, empty)
			return
		}
	}
	var results []upstreamItemsResult
	for _, inst := range a.collectAllowedInstances(reqCtx, resolved) {
		if !a.isServerAllowed(reqCtx, inst.ServerID) {
			continue
		}
		query := cloneValues(queryTemplate)
		query.Set("UserId", inst.Client.clientUserID())
		if seasonID != "" {
			raw := resolvedOriginalIDForServer(a.IDStore.ResolveVirtualID(seasonID), inst.ServerID)
			if raw == "" {
				continue
			}
			query.Del("seasonId")
			query.Del("seasonid")
			query.Set("SeasonId", raw)
		}
		payload, err := inst.Client.RequestJSON(r.Context(), reqCtx, a.Identity, http.MethodGet, "/Shows/"+inst.OriginalID+"/"+endpoint, query, nil)
		if err != nil || !a.isServerAllowed(reqCtx, inst.ServerID) {
			continue
		}
		if err := a.observeLegacyShowsParent(reqCtx, inst.ServerID, inst.OriginalID); err != nil {
			a.logMergeHTTPError(err)
			continue
		}
		items := asItems(payload)
		for _, item := range items {
			if _, exists := item["Type"]; !exists {
				item["Type"] = kind
			}
			if _, exists := item["SeriesId"]; !exists {
				item["SeriesId"] = inst.OriginalID
			}
		}
		results = append(results, upstreamItemsResult{ServerID: inst.ServerID, Items: items, FullSources: full, RequestScope: reqCtx})
	}
	results = a.hydrateMergeResults(r, results)
	items := a.mergeRoundRobinItems(results, a.clientFacingUserIDFor(r), reqCtx)
	sort.SliceStable(items, func(i, j int) bool {
		left, right := newMergeCandidate("", items[i], nil, false), newMergeCandidate("", items[j], nil, false)
		if left.Season.valid() != right.Season.valid() {
			return left.Season.valid()
		}
		if left.Season.Ticks != right.Season.Ticks {
			return left.Season.Ticks < right.Season.Ticks
		}
		if kind == "Episode" {
			if left.Episode.valid() != right.Episode.valid() {
				return left.Episode.valid()
			}
			return left.Episode.Ticks < right.Episode.Ticks
		}
		return false
	})
	if localFilter {
		var recency map[string]int64
		items, recency = a.filterItemsByLocalUserState(r, items, filter)
		if r.URL.Query().Get("SortBy") != "" {
			localItemSort(items, r.URL.Query(), recency)
		}
	}
	a.overlayLocalUserDataItems(r, items)
	if kind == "Season" {
		a.overlayLocalSeasonUnplayedCounts(r, resolved, items)
	}
	writeJSON(w, http.StatusOK, paginateItems(items, r.URL.Query()))
}

// Only 404/405 for the exact endpoint authorize compatibility fallback.
// Authentication, upstream server failures and transport errors are not a
// reason to switch endpoints or silently retry with another identity.
func searchEndpointUnsupported(err error, path string) bool {
	var httpErr *upstreamHTTPStatusError
	return errors.As(err, &httpErr) && httpErr.Path == path &&
		(httpErr.StatusCode == http.StatusNotFound || httpErr.StatusCode == http.StatusMethodNotAllowed)
}

func searchHintsEndpointUnsupported(err error) bool {
	return searchEndpointUnsupported(err, "/Search/Hints")
}

// A live search may add at most two lightweight compatibility probes per
// authorized upstream, not an unbounded library query. Never modify the
// client's original paging or search parameters in place.
func boundedSearchAlternativeQuery(original url.Values, upstreamUserID string) (url.Values, bool) {
	if strings.TrimSpace(original.Get("SearchTerm")) == "" {
		return nil, false
	}
	query := cloneValues(original)
	query.Set("UserId", upstreamUserID)
	query.Set("Recursive", "true")
	limit, err := strconv.Atoi(query.Get("Limit"))
	if err != nil || limit <= 0 {
		limit = 20
	} else if limit > 100 {
		limit = 100
	}
	query.Set("Limit", strconv.Itoa(limit))
	return query, true
}

// SearchHints can use ItemId while Items responses normally use Id. Before
// registration, normalize to a single upstream locator to prevent a conflicting
// alias from creating an extra mapping or leaking a raw upstream ID.
func normalizeSearchHintIDs(items []map[string]any) []map[string]any {
	for _, item := range items {
		if id := mergeWireItemID(item); id != "" {
			item["Id"] = id
			item["ItemId"] = id
		}
	}
	return items
}

func (a *App) handleSearchHints(w http.ResponseWriter, r *http.Request) {
	reqCtx := requestContextFrom(r.Context())
	clients := a.allowedClients(reqCtx)

	// SearchHints is an optional upstream compatibility surface. A successful
	// but empty/unrecognized response is not conclusive evidence that /Items
	// cannot find the requested work. Only nonblank search terms may trigger
	// one bounded alternative query on the same authorized upstream.
	perClient := fanOutClients(clients, func(c *UpstreamClient) *upstreamItemsResult {
		query := cloneValues(r.URL.Query())
		query.Set("UserId", c.clientUserID())
		fullSources := requestMergeFields(query)
		fallbackItems := func(reason string) *upstreamItemsResult {
			fallback, valid := boundedSearchAlternativeQuery(query, c.clientUserID())
			if !valid || r.Context().Err() != nil {
				return nil
			}
			if a.Logger != nil && a.noticeThrottle.allow("search-fallback:"+reason+":"+c.ID) {
				a.Logger.Infof("SearchHints compatibility: %s; checking bounded Items search alternatives", reason)
			}
			paths := []string{"/Items"}
			if c.clientUserID() != "" {
				paths = append(paths, "/Users/"+url.PathEscape(c.clientUserID())+"/Items")
			}
			for _, path := range paths {
				if r.Context().Err() != nil || !a.isServerAllowed(reqCtx, c.ID) {
					return nil
				}
				outbound := fallback
				if path != "/Items" {
					// This sibling route already scopes the user in its path.
					outbound = cloneValues(fallback)
					removePathScopedItemsUserQuery(outbound)
				}
				result, err := c.RequestJSON(r.Context(), reqCtx, a.Identity, http.MethodGet, path, outbound, nil)
				if err != nil {
					if searchEndpointUnsupported(err, path) {
						continue
					}
					// Never move to another endpoint after a 401/403, 5xx,
					// or transport error from either alternate endpoint.
					if a.Logger != nil && a.noticeThrottle.allow("search-fallback-error:"+c.ID) {
						a.Logger.Warnf("SearchHints compatibility: Items search failed (status/transport error; no query or credentials logged)")
					}
					return nil
				}
				items := asItems(result)
				if len(items) > 0 {
					return &upstreamItemsResult{ServerID: c.ID, Items: normalizeSearchHintIDs(items), FullSources: fullSources, RequestScope: reqCtx}
				}
			}
			return nil
		}
		payload, err := c.RequestJSON(r.Context(), reqCtx, a.Identity, http.MethodGet, "/Search/Hints", query, nil)
		if err != nil {
			if searchHintsEndpointUnsupported(err) {
				return fallbackItems("endpoint unsupported")
			}
			// Auth errors and upstream/server failures must never be hidden by
			// silently switching to another endpoint with weaker guarantees.
			if a.Logger != nil && a.noticeThrottle.allow("search-hints-error:"+c.ID) {
				a.Logger.Warnf("SearchHints upstream request failed; no fallback for authentication, transport or server errors")
			}
			return nil
		}
		block, ok := payload.(map[string]any)
		if !ok {
			return fallbackItems("unexpected response type")
		}
		items := asItems(map[string]any{"Items": block["SearchHints"]})
		if len(items) == 0 {
			items = asItems(payload)
		}
		if len(items) == 0 {
			return fallbackItems("empty or unrecognized response")
		}
		return &upstreamItemsResult{ServerID: c.ID, Items: normalizeSearchHintIDs(items), FullSources: fullSources, RequestScope: reqCtx}
	})

	collected := make([]upstreamItemsResult, 0, len(perClient))
	for _, result := range perClient {
		if result != nil {
			collected = append(collected, *result)
		}
	}
	collected = a.hydrateMergeResults(r, collected)
	merged := a.mergeRoundRobinItems(collected, a.clientFacingUserIDFor(r), reqCtx)

	// Some upstreams supply Id, others ItemId. Never return the upstream ID
	// in either field, including legacy/untyped fallback search entries.
	for _, item := range merged {
		if id, _ := item["Id"].(string); id != "" {
			item["ItemId"] = id
		}
	}
	a.overlayLocalUserDataItems(r, merged)
	writeJSON(w, http.StatusOK, map[string]any{
		"SearchHints":      toAnySlice(merged),
		"TotalRecordCount": len(merged),
	})
}

func (a *App) handleItemImage(w http.ResponseWriter, r *http.Request) {
	resolved := a.resolveRouteID(r.PathValue("itemId"))
	if ctx := requestContextFrom(r.Context()); ctx != nil && ctx.ProxyUser != nil {
		var routeOK bool
		resolved, routeOK = a.resolveRequestRouteID(w, r, r.PathValue("itemId"))
		if !routeOK {
			return
		}
	}
	if resolved == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	// Image URLs are embedded by clients and stay reachable without a token, so
	// the server access check only applies to authenticated requests.
	if reqCtx := requestContextFrom(r.Context()); reqCtx != nil && reqCtx.ProxyUser != nil && !a.isServerAllowed(reqCtx, resolved.ServerID) {
		writeJSON(w, http.StatusForbidden, map[string]any{"message": "Access denied"})
		return
	}
	imagePath := "/Items/" + resolved.OriginalID + "/Images/" + r.PathValue("imageType")
	if imageIndex := r.PathValue("imageIndex"); imageIndex != "" {
		imagePath += "/" + imageIndex
	}
	imageQuery := cloneValues(r.URL.Query())
	imageQuery.Del("api_key")
	imageQuery.Del("ApiKey")
	resp, err := resolved.Client.Stream(r.Context(), requestContextFrom(r.Context()), a.Identity, imagePath, imageQuery)
	if err != nil {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	} else {
		// An upstream outage or missing image must not be cached for a day.
		w.Header().Set("Cache-Control", "no-store")
	}
	for _, header := range []string{"Content-Type", "Content-Length", "ETag", "Last-Modified", "Content-Range"} {
		if value := resp.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (a *App) handleUserImageNotFound(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotFound)
}

func numericInt(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case float32:
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return int(parsed), true
		}
	case string:
		parsed, err := strconv.Atoi(typed)
		if err == nil {
			return parsed, true
		}
	}
	return 0, false
}

func toAnySlice(items []map[string]any) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}
