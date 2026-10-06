package backend

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// resolveRequestRouteID preserves each handler's missing/unavailable response
// while denying unauthorized media before any upstream request.
func (a *App) resolveRequestRouteID(w http.ResponseWriter, r *http.Request, virtualID string) (*routeResolution, bool) {
	resolved, err := a.resolveAuthorizedRouteID(requestContextFrom(r.Context()), virtualID)
	if errors.Is(err, errMediaAccessDenied) {
		writeJSON(w, http.StatusForbidden, map[string]any{"message": "Access denied"})
		return nil, false
	}
	return resolved, true
}

func (a *App) visibleWatchProgress(r *http.Request, virtualID string) (*WatchProgress, error) {
	var row *WatchProgress
	err := a.withVisibleWatchScope(requestContextFrom(r.Context()), false, func(scope mediaAccessScope) error {
		var err error
		row, err = a.WatchStore.GetVisibleProgress(scope, a.IDStore.CanonicalMergeID(virtualID))
		return err
	})
	return row, err
}

func (a *App) visibleWatchProgressBatch(r *http.Request, virtualIDs []string) (map[string]WatchProgress, error) {
	var rows map[string]WatchProgress
	err := a.withVisibleWatchScope(requestContextFrom(r.Context()), false, func(scope mediaAccessScope) error {
		var err error
		canonicalIDs := make([]string, 0, len(virtualIDs))
		canonical := map[string]string{}
		for _, id := range virtualIDs {
			key := a.IDStore.CanonicalMergeID(id)
			canonical[id] = key
			canonicalIDs = append(canonicalIDs, key)
		}
		var raw map[string]WatchProgress
		raw, err = a.WatchStore.GetVisibleProgressBatch(scope, canonicalIDs)
		if err == nil {
			rows = map[string]WatchProgress{}
			for id, key := range canonical {
				if row, ok := raw[key]; ok {
					rows[id] = row
				}
			}
		}
		return err
	})
	return rows, err
}

// Empty reads and failed reads are different. On failure omit personal fields;
// never inherit shared upstream state or manufacture "unplayed" filter matches.
func (a *App) logVisibleWatchReadError(err error) {
	if err != nil && a.Logger != nil {
		a.Logger.Warnf("Visible watch state read failed: %s", redactURLInError(err))
	}
}

func clearPersonalUserState(value any) {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			clearPersonalUserState(child)
		}
	case map[string]any:
		delete(typed, "UserData")
		if isUserDataMap(typed) {
			for _, key := range []string{"PlaybackPositionTicks", "Played", "IsFavorite", "PlayedPercentage",
				"LastPlayedDate", "PlayCount", "UnplayedItemCount", "Rating"} {
				delete(typed, key)
			}
		}
		for _, child := range typed {
			clearPersonalUserState(child)
		}
	}
}

// Keep the established default page size while honoring StartIndex and Limit=0.
func watchResponsePage(items []map[string]any, query url.Values) map[string]any {
	pageQuery := cloneValues(query)
	if pageQuery.Get("Limit") == "" {
		pageQuery.Set("Limit", "20")
	}
	return paginateItems(items, pageQuery)
}

// Filter rewritten source versions throughout a response using current grants.
// Source IDs have already been virtualized with an upstream-qualified namespace.
func (a *App) filterAuthorizedMediaSources(reqCtx *RequestContext, payload any) {
	switch typed := payload.(type) {
	case []any:
		for _, child := range typed {
			a.filterAuthorizedMediaSources(reqCtx, child)
		}
	case map[string]any:
		if raw, exists := typed["MediaSources"]; exists {
			kept := make([]any, 0)
			for _, source := range asItems(map[string]any{"Items": raw}) {
				id, _ := source["Id"].(string)
				resolved := a.IDStore.ResolveVirtualID(id)
				if resolved != nil && a.isServerAllowed(reqCtx, resolved.ServerID) {
					kept = append(kept, source)
				}
			}
			typed["MediaSources"] = kept
		}
		for key, child := range typed {
			if key != "MediaSources" {
				a.filterAuthorizedMediaSources(reqCtx, child)
			}
		}
	}
}

// mediaSourceSelection never chooses another server for an explicit version.
// Unknown virtual IDs are rejected; raw IDs require an existing, unique mapping
// within the selected item's source, then authoritative item membership.
type mediaSourceSelection struct {
	OriginalID     string
	VirtualID      string
	ItemOriginalID string
	ServerID       string
	Client         *UpstreamClient
}

func (a *App) selectAuthorizedMediaSource(r *http.Request, virtualItemID, sourceID string, preferred *routeResolution) (*mediaSourceSelection, error) {
	reqCtx := requestContextFrom(r.Context())
	if sourceID == "" || a.IDStore == nil {
		return nil, errMediaMappingMissing
	}
	mapped := a.IDStore.ResolveVirtualID(sourceID)
	virtualSourceID := sourceID
	if mapped == nil {
		// Do not pass a stale, unmapped virtual ID through to an arbitrary
		// upstream. Known raw IDs are accepted only with a source namespace.
		serverID := ""
		if preferred != nil {
			serverID = preferred.ServerID
		}
		var ok bool
		originalItem := resolvedOriginalIDForServer(a.IDStore.ResolveVirtualID(virtualItemID), serverID)
		virtualSourceID, mapped, ok = a.IDStore.ResolveMergeMediaSourceForItem(sourceID, serverID, originalItem)
		if !ok {
			return nil, errMediaMappingMissing
		}
	}
	serverID := mapped.ServerID
	if !a.isServerAllowed(reqCtx, serverID) {
		return nil, errMediaAccessDenied
	}
	item := a.IDStore.ResolveVirtualID(virtualItemID)
	itemOriginalID := resolvedOriginalIDForServer(item, serverID)
	if mapped.MediaItemID != "" {
		itemOriginalID = mapped.MediaItemID
	}
	if !a.IDStore.MergeMemberAllowed(virtualItemID, serverID, itemOriginalID, mapped.OriginalID) {
		return nil, errMediaMappingMissing
	}
	if itemOriginalID == "" {
		return nil, errMediaMappingMissing
	}
	client := a.Upstream.ClientByID(serverID)
	if client == nil || !client.IsOnline() {
		return nil, errMediaSourceUnavailable
	}
	selection := &mediaSourceSelection{OriginalID: mapped.OriginalID, VirtualID: virtualSourceID,
		ItemOriginalID: itemOriginalID, ServerID: serverID, Client: client}
	owner := playbackRouteOwner(reqCtx)
	if route, ok := a.playbackRoutes.MediaSource(owner, virtualSourceID); ok &&
		route.ServerID == serverID && route.ItemID == virtualItemID {
		return selection, nil
	}
	// A fresh authoritative list proves the relationship after cache expiry,
	// relogin, or when a client selected a version from the item detail screen.
	query := url.Values{"Fields": {"MediaSources"}, "UserId": {client.clientUserID()}}
	payload, err := client.RequestJSON(r.Context(), reqCtx, a.Identity, http.MethodGet,
		"/Users/"+client.clientUserID()+"/Items/"+itemOriginalID, query, nil)
	if err != nil {
		return nil, errMediaSourceUnavailable
	}
	data, ok := payload.(map[string]any)
	if !ok {
		return nil, errMediaMappingMissing
	}
	if id, _ := data["Id"].(string); id != "" && id != itemOriginalID {
		return nil, errMediaMappingMissing
	}
	found := false
	for _, source := range asItems(map[string]any{"Items": data["MediaSources"]}) {
		if id, _ := source["Id"].(string); id == mapped.OriginalID {
			found = true
			break
		}
	}
	if !found {
		return nil, errMediaMappingMissing
	}
	if !a.isServerAllowed(reqCtx, serverID) || !a.IDStore.MergeMemberAllowed(virtualItemID, serverID, itemOriginalID, mapped.OriginalID) {
		return nil, errMediaAccessDenied
	}
	a.rememberMediaMembership(reqCtx, virtualSourceID, virtualItemID, serverID)
	return selection, nil
}

func writeMediaSelectionError(w http.ResponseWriter, err error) {
	status := http.StatusNotFound
	if errors.Is(err, errMediaAccessDenied) {
		status = http.StatusForbidden
	} else if errors.Is(err, errMediaSourceUnavailable) {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, map[string]any{"message": "Media source is unavailable or not authorized"})
}

// Reject conflicting aliases or repeated selections before choosing a target.
func explicitMediaSourceID(query url.Values, body map[string]any) (string, error) {
	selected := ""
	accept := func(value string) error {
		if value == "" {
			return nil
		}
		if selected != "" && selected != value {
			return errMediaMappingMissing
		}
		selected = value
		return nil
	}
	for key, values := range query {
		if !strings.EqualFold(key, "MediaSourceId") {
			continue
		}
		if len(values) != 1 {
			return "", errMediaMappingMissing
		}
		if err := accept(values[0]); err != nil {
			return "", err
		}
	}
	for key, raw := range body {
		if !strings.EqualFold(key, "MediaSourceId") {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			return "", errMediaMappingMissing
		}
		if err := accept(value); err != nil {
			return "", err
		}
	}
	return selected, nil
}

func setSelectedMediaSource(query url.Values, body map[string]any, selection *mediaSourceSelection) {
	for key := range query {
		if strings.EqualFold(key, "MediaSourceId") {
			delete(query, key)
		}
	}
	query.Set("MediaSourceId", selection.OriginalID)
	for key := range body {
		if strings.EqualFold(key, "MediaSourceId") {
			body[key] = selection.OriginalID
		}
	}
}

// This filter runs before merging/grouping/paging, including responses that
// arrived after the source was removed from the current user's grants.
func (a *App) filterAllowedUpstreamResults(reqCtx *RequestContext, results []upstreamItemsResult) []upstreamItemsResult {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	scope := a.mediaAccessScopeLocked(reqCtx)
	kept := make([]upstreamItemsResult, 0, len(results))
	for _, result := range results {
		if result.Err == nil && scope.allows(result.ServerID) {
			kept = append(kept, result)
		}
	}
	return kept
}

// prepareFallbackMediaSelection only interprets classified media identity fields.
// Unrelated JSON and raw/non-JSON bodies keep their existing forwarding shape.
func (a *App) prepareFallbackMediaSelection(w http.ResponseWriter, r *http.Request, client *UpstreamClient, query url.Values, body any) bool {
	bodyMap, _ := body.(map[string]any)
	classified := r.URL.Query().Get("ItemId") != ""
	for _, segment := range strings.Split(r.URL.Path, "/") {
		classified = classified || fallbackResourceSegment(segment)
	}
	if bodyMap != nil {
		itemID, _ := bodyMap["ItemId"].(string)
		classified = classified || a.IDStore.ResolveVirtualID(itemID) != nil
	}
	if !classified {
		return a.isServerAllowed(requestContextFrom(r.Context()), client.ID)
	}
	sourceID, err := explicitMediaSourceID(r.URL.Query(), bodyMap)
	if err != nil {
		writeMediaSelectionError(w, err)
		return false
	}
	if sourceID == "" {
		return a.isServerAllowed(requestContextFrom(r.Context()), client.ID) &&
			a.qualifyFallbackBodyIDs(w, bodyMap, client.ID)
	}
	virtualItemID := ""
	segments := strings.Split(r.URL.Path, "/")
	for index := 1; index < len(segments); index++ {
		if fallbackResourceSegment(segments[index-1]) && a.IDStore.ResolveVirtualID(segments[index]) != nil {
			virtualItemID = segments[index]
			break
		}
	}
	if virtualItemID == "" {
		virtualItemID = r.URL.Query().Get("ItemId")
	}
	if virtualItemID == "" && bodyMap != nil {
		virtualItemID, _ = bodyMap["ItemId"].(string)
	}
	selection, err := a.selectAuthorizedMediaSource(r, virtualItemID, sourceID, &routeResolution{ServerID: client.ID})
	if err != nil || selection.ServerID != client.ID {
		if err == nil {
			err = errMediaAccessDenied
		}
		writeMediaSelectionError(w, err)
		return false
	}
	setSelectedMediaSource(query, bodyMap, selection)
	return a.qualifyFallbackBodyIDs(w, bodyMap, client.ID)
}

// Known classified body identities must use the chosen source namespace too.
// Unrelated/raw bodies never reach this helper.
func (a *App) qualifyFallbackBodyIDs(w http.ResponseWriter, body map[string]any, serverID string) bool {
	for key, raw := range body {
		itemField := strings.EqualFold(key, "ItemId")
		sessionField := strings.EqualFold(key, "PlaySessionId") || strings.EqualFold(key, "SessionId")
		if !itemField && !sessionField {
			continue
		}
		id, ok := raw.(string)
		if !ok {
			writeMediaSelectionError(w, errMediaMappingMissing)
			return false
		}
		if id == "" {
			continue
		}
		mapped := a.IDStore.ResolveVirtualID(id)
		if mapped == nil {
			// Preserve existing source-local raw session compatibility. Owner/item
			// proof for lifecycle writes is implemented separately in Phase3.
			if sessionField {
				continue
			}
			if _, _, known := a.IDStore.ResolveOriginalIDForServer(id, serverID); known {
				continue
			}
			writeMediaSelectionError(w, errMediaMappingMissing)
			return false
		}
		originalID := resolvedOriginalIDForServer(mapped, serverID)
		if originalID == "" || (sessionField && mapped.ServerID != serverID) {
			writeMediaSelectionError(w, errMediaAccessDenied)
			return false
		}
		body[key] = originalID
	}
	return true
}
