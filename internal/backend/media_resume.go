package backend

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func (a *App) handleUserItemsResume(w http.ResponseWriter, r *http.Request) {
	reqCtx := requestContextFrom(r.Context())
	// Non-admin users: serve resume from local WatchStore
	if a.WatchStore != nil && reqCtx != nil && reqCtx.ProxyUser != nil && reqCtx.ProxyUser.Role != "admin" {
		a.handleLocalResume(w, r, reqCtx)
		return
	}
	query := cloneValues(r.URL.Query())
	parentID := firstQueryValue(query, "ParentId", "parentId", "parentid")
	if parentID != "" {
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
		instances := a.collectAllowedInstances(requestContextFrom(r.Context()), resolved)
		originalIDs := map[string]struct{}{}
		for _, inst := range instances {
			originalIDs[inst.OriginalID] = struct{}{}
		}
		for _, inst := range instances {
			instQuery := cloneValues(query)
			instQuery.Set("ParentId", inst.OriginalID)
			instQuery.Set("UserId", inst.Client.clientUserID())
			instQuery.Del("parentId")
			instQuery.Del("parentid")
			payload, err := inst.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Users/"+inst.Client.clientUserID()+"/Items/Resume", instQuery, nil)
			if err != nil {
				continue
			}
			filtered := filterSeriesItems(asItems(payload), originalIDs)
			if len(filtered) > 0 {
				a.rewriteItems(filtered, inst.ServerID, a.clientFacingUserIDFor(r))
				writeJSON(w, http.StatusOK, map[string]any{"Items": filtered, "TotalRecordCount": len(filtered), "StartIndex": 0})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
		return
	}
	results := a.fetchItemsAcrossUpstreams(r.Context(), requestContextFrom(r.Context()), "/Users/%s/Items/Resume", query, nil)
	writeJSON(w, http.StatusOK, a.mergedItemsPayload(results, a.clientFacingUserIDFor(r), reqCtx))
}

// handleLocalResume serves resume items from local WatchStore for non-admin users.
// It fetches item metadata from upstream servers to build complete responses.
func (a *App) handleLocalResume(w http.ResponseWriter, r *http.Request, reqCtx *RequestContext) {
	query := cloneValues(r.URL.Query())
	parentID := firstQueryValue(query, "ParentId", "parentId", "parentid")
	if parentID != "" {
		if _, routeOK := a.resolveRequestRouteID(w, r, parentID); !routeOK {
			return
		}
	}
	var items []WatchProgress
	err := a.withVisibleWatchScope(reqCtx, true, func(scope mediaAccessScope) error {
		var err error
		items, err = a.WatchStore.GetVisibleResumeCandidates(scope, a.IDStore.CanonicalMergeID(parentID))
		return err
	})
	if err != nil {
		a.logVisibleWatchReadError(err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "Failed to read resume state"})
		return
	}
	enriched := a.enrichWatchItems(r, reqCtx, items)
	writeJSON(w, http.StatusOK, watchResponsePage(enriched, query))
}

// enrichWatchItems fetches fresh metadata from upstream for a list of WatchProgress items,
// groups by server, batch-fetches via GET /Items?Ids=..., overlays local UserData.
// When a recorded server is offline, items are remapped to an online OtherInstance via IDStore.
func (a *App) enrichWatchItems(r *http.Request, reqCtx *RequestContext, items []WatchProgress) []map[string]any {

	// Group by server ID, remapping offline servers to online alternatives
	type serverGroup struct {
		originalIDs []string
		watchItems  []WatchProgress
	}
	groups := map[string]*serverGroup{}
	for i := range items {
		resolved, err := a.resolveAuthorizedRouteID(reqCtx, items[i].VirtualItemID)
		if err != nil || resolved == nil {
			continue // all instances offline
		}
		serverID, originalID := resolved.ServerID, resolved.OriginalID
		items[i].ServerID = serverID
		items[i].OriginalItemID = originalID
		g, exists := groups[serverID]
		if !exists {
			g = &serverGroup{}
			groups[serverID] = g
		}
		g.originalIDs = append(g.originalIDs, originalID)
		g.watchItems = append(g.watchItems, items[i])
	}

	// Fetch metadata per server (all groups now point to online servers)
	type metadataKey struct {
		serverID   string
		originalID string
	}
	// Original item IDs belong to an upstream namespace, not a global one.
	fetched := map[metadataKey]map[string]any{}
	for serverID, g := range groups {
		client := a.Upstream.ClientByID(serverID)
		if client == nil || !client.IsOnline() || !a.isServerAllowed(reqCtx, serverID) {
			continue
		}
		q := url.Values{}
		q.Set("Fields", "BasicSyncInfo,CanDelete,PrimaryImageAspectRatio,Overview,DateCreated,MediaSources,Path,SortName,Studios,Taglines,Genres,CommunityRating,OfficialRating,CumulativeRunTimeTicks,Chapters,ProviderIds")
		for start := 0; start < len(g.originalIDs); start += maxBatchIDCount {
			end := start + maxBatchIDCount
			if end > len(g.originalIDs) {
				end = len(g.originalIDs)
			}
			if !a.isServerAllowed(reqCtx, serverID) {
				break
			}
			q.Set("Ids", joinComma(g.originalIDs[start:end]))
			payload, err := client.RequestJSON(r.Context(), reqCtx, a.Identity, http.MethodGet, "/Items", q, nil)
			if err != nil || !a.isServerAllowed(reqCtx, serverID) {
				continue
			}
			for _, item := range asItems(payload) {
				if id, _ := item["Id"].(string); id != "" {
					fetched[metadataKey{serverID: serverID, originalID: id}] = item
				}
			}
		}
	}

	// Build result in original order, then run the same local UserData normalizer
	// used by every other media response so percentage/history fields cannot leak.
	var result []map[string]any
	for _, wp := range items {
		if !a.isServerAllowed(reqCtx, wp.ServerID) {
			continue
		}
		item, ok := fetched[metadataKey{serverID: wp.ServerID, originalID: wp.OriginalItemID}]
		if !ok {
			continue
		}
		item = deepCloneMap(item)
		if err := a.observeMergeDetail(reqCtx, wp.VirtualItemID, wp.ServerID, wp.OriginalItemID, item, true); err != nil {
			a.logMergeHTTPError(err)
			continue
		}
		item = a.projectMergeItem(item, wp.ServerID, a.IDStore.CanonicalMergeID(wp.VirtualItemID), a.clientFacingUserIDFor(r))
		if item == nil {
			continue
		}
		if _, ok := item["UserData"].(map[string]any); !ok {
			item["UserData"] = map[string]any{}
		}
		result = append(result, item)
	}
	a.overlayLocalUserDataItems(r, result)
	return result
}

// resolveWatchItemServer returns the serverID and originalItemID to use for
// fetching metadata. If the item's recorded server is offline, it tries to find
// an online OtherInstance via IDStore.
func (a *App) resolveWatchItemServer(wp *WatchProgress) (serverID string, originalItemID string, ok bool) {
	client := a.Upstream.ClientByID(wp.ServerID)
	if client != nil && client.IsOnline() {
		return wp.ServerID, wp.OriginalItemID, true
	}
	resolved := a.IDStore.ResolveVirtualID(wp.VirtualItemID)
	if resolved == nil {
		return "", "", false
	}
	// Try IDStore primary (may differ from wp.ServerID if mapping was updated)
	if resolved.ServerID != wp.ServerID {
		alt := a.Upstream.ClientByID(resolved.ServerID)
		if alt != nil && alt.IsOnline() {
			return resolved.ServerID, resolved.OriginalID, true
		}
	}
	// Try OtherInstances
	for _, other := range resolved.OtherInstances {
		alt := a.Upstream.ClientByID(other.ServerID)
		if alt != nil && alt.IsOnline() {
			return other.ServerID, other.OriginalID, true
		}
	}
	return "", "", false
}

// joinComma joins strings with commas.
func joinComma(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	result := ss[0]
	for _, s := range ss[1:] {
		result += "," + s
	}
	return result
}

// queryInt extracts an integer from a url.Values query parameter.
func queryInt(values url.Values, key string) (int, bool) {
	s := strings.TrimSpace(values.Get(key))
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}
