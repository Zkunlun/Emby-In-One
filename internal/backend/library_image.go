package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
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
	requestMergeFields(queryTemplate)
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
	sources := make([]passivePageSource, 0)
	for _, instance := range a.collectAllowedInstances(reqCtx, resolved) {
		inst := instance
		if !a.isServerAllowed(reqCtx, inst.ServerID) {
			continue
		}
		query := cloneValues(queryTemplate)
		if seasonID != "" {
			raw := resolvedOriginalIDForServer(a.IDStore.ResolveVirtualID(seasonID), inst.ServerID)
			if raw == "" {
				continue
			}
			query.Del("seasonId")
			query.Del("seasonid")
			query.Set("SeasonId", raw)
		}
		sources = append(sources, passivePageSource{serverID: inst.ServerID, request: func(ctx context.Context, start, limit int) (any, error) {
			q := passiveSourceQuery(query, inst.Client, start, limit)
			payload, err := inst.Client.RequestJSON(ctx, reqCtx, a.Identity, http.MethodGet, "/Shows/"+inst.OriginalID+"/"+endpoint, q, nil)
			if err != nil {
				return nil, err
			}
			if err = a.observeLegacyShowsParent(reqCtx, inst.ServerID, inst.OriginalID); err != nil {
				return nil, err
			}
			block, ok := payload.(map[string]any)
			if !ok {
				return payload, nil
			}
			items := asItems(block)
			for _, item := range items {
				if _, ok := item["Type"]; !ok {
					item["Type"] = kind
				}
				if _, ok := item["SeriesId"]; !ok {
					item["SeriesId"] = inst.OriginalID
				}
			}
			block["Items"] = toAnySlice(items)
			return block, nil
		}})
	}
	filtered := func(items []map[string]any) []map[string]any {
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
		return items
	}
	items, complete, err := a.collectPassivePages(r, sources, queryTemplate, filtered, a.clientFacingUserIDFor(r))
	if err != nil {
		writePassivePageError(w, err)
		return
	}
	page := passiveWindowPayload(items, r.URL.Query(), complete)
	a.overlayLocalUserDataItems(r, asItems(page))
	writeJSON(w, http.StatusOK, page)
}

func (a *App) handleSearchHints(w http.ResponseWriter, r *http.Request) {
	reqCtx := requestContextFrom(r.Context())
	clients := a.allowedClients(reqCtx)

	// A failed client returns nil so its group is simply absent from the merge.
	perClient := fanOutClients(clients, func(c *UpstreamClient) *upstreamItemsResult {
		query := cloneValues(r.URL.Query())
		query.Set("UserId", c.clientUserID())
		fullSources := requestMergeFields(query)
		payload, err := c.RequestJSON(r.Context(), reqCtx, a.Identity, http.MethodGet, "/Search/Hints", query, nil)
		if err != nil {
			return nil
		}
		block, ok := payload.(map[string]any)
		if !ok {
			return nil
		}
		items := asItems(map[string]any{"Items": block["SearchHints"]})
		if len(items) == 0 {
			items = asItems(payload)
		}
		return &upstreamItemsResult{ServerID: c.ID, Items: items, FullSources: fullSources, RequestScope: reqCtx}
	})

	collected := make([]upstreamItemsResult, 0, len(perClient))
	for _, result := range perClient {
		if result != nil {
			collected = append(collected, *result)
		}
	}
	collected = a.hydrateMergeResults(r, collected)
	merged := a.mergeRoundRobinItems(collected, a.clientFacingUserIDFor(r), reqCtx)

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
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Cache-Control", "public, max-age=86400")
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
