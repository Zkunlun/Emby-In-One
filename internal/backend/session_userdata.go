package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
)

func (a *App) registerSessionAndUserStateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /Sessions/Playing", a.withContext(a.requireAuth(a.handleSessionPlaying)))
	mux.HandleFunc("POST /Sessions/Playing/Progress", a.withContext(a.requireAuth(a.handleSessionPlayingProgress)))
	mux.HandleFunc("POST /Sessions/Playing/Stopped", a.withContext(a.requireAuth(a.handleSessionPlayingStopped)))
	mux.HandleFunc("POST /Sessions/Capabilities", a.withContext(a.requireAuth(a.handleSessionsCapabilities)))
	mux.HandleFunc("POST /Sessions/Capabilities/Full", a.withContext(a.requireAuth(a.handleSessionsCapabilitiesFull)))
	mux.HandleFunc("POST /Users/{userId}/PlayingItems/{itemId}", a.withContext(a.requireAuth(a.handleUserPlayingItemStart)))
	mux.HandleFunc("DELETE /Users/{userId}/PlayingItems/{itemId}", a.withContext(a.requireAuth(a.handleUserPlayingItemStop)))
	mux.HandleFunc("POST /Users/{userId}/PlayingItems/{itemId}/Progress", a.withContext(a.requireAuth(a.handleUserPlayingItemProgress)))
	mux.HandleFunc("POST /Users/{userId}/PlayingItems/{itemId}/Delete", a.withContext(a.requireAuth(a.handleUserPlayingItemStopCompat)))
	mux.HandleFunc("POST /Users/{userId}/Items/{itemId}/UserData", a.withContext(a.requireAuth(a.handleUserItemUserData)))
	mux.HandleFunc("POST /Users/{userId}/PlayedItems/{itemId}", a.withContext(a.requireAuth(a.handlePlayedItemAdd)))
	mux.HandleFunc("DELETE /Users/{userId}/PlayedItems/{itemId}", a.withContext(a.requireAuth(a.handlePlayedItemRemove)))
	mux.HandleFunc("POST /Users/{userId}/PlayedItems/{itemId}/Delete", a.withContext(a.requireAuth(a.handlePlayedItemRemoveCompat)))
	mux.HandleFunc("POST /Users/{userId}/FavoriteItems/{itemId}", a.withContext(a.requireAuth(a.handleFavoriteItemAdd)))
	mux.HandleFunc("DELETE /Users/{userId}/FavoriteItems/{itemId}", a.withContext(a.requireAuth(a.handleFavoriteItemRemove)))
}

func decodeOptionalJSON(r *http.Request) (any, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func resolvedOriginalIDForServer(resolved *ResolvedID, serverID string) string {
	if resolved == nil {
		return ""
	}
	if resolved.ServerID == serverID || serverID == "" {
		return resolved.OriginalID
	}
	for _, other := range resolved.OtherInstances {
		if other.ServerID == serverID {
			return other.OriginalID
		}
	}
	return "" // No instance on this source: never forward another source's raw ID.
}

func (a *App) translateSessionBodyIDs(reqCtx *RequestContext, body map[string]any) (string, bool) {
	rawItemID, _ := body["ItemId"].(string)
	rawMediaSourceID, _ := body["MediaSourceId"].(string)
	rawPlaySessionID, _ := body["PlaySessionId"].(string)
	if a.IDStore == nil || rawItemID == "" {
		return "", false
	}
	item := a.IDStore.ResolveVirtualID(rawItemID)
	media := a.IDStore.ResolveVirtualID(rawMediaSourceID)
	play := a.IDStore.ResolveVirtualID(rawPlaySessionID)
	owner := playbackRouteOwner(reqCtx)
	routeMatchesSession := func(route playbackRouteEntry) bool {
		if rawPlaySessionID == "" {
			return false // Never acquire the latest session from a missing identity.
		}
		if rawPlaySessionID == route.PlaySessionID || rawPlaySessionID == route.ClientPlaySessionID {
			return true
		}
		if play == nil {
			alias := a.IDStore.ResolveVirtualID(route.ClientPlaySessionID)
			return alias != nil && rawPlaySessionID == alias.OriginalID
		}
		if play.ServerID == route.ServerID && play.OriginalID == route.PlaySessionID {
			return true
		}
		alias := a.IDStore.ResolveVirtualID(route.ClientPlaySessionID)
		return alias != nil && alias.ServerID == play.ServerID && alias.OriginalID == play.OriginalID
	}
	serverID, targetSessionID := "", ""
	virtualSourceID := rawMediaSourceID
	if media != nil {
		serverID = media.ServerID
		if route, ok := a.playbackRoutes.MediaSource(owner, rawMediaSourceID); ok &&
			route.ItemID == rawItemID && route.ServerID == serverID && routeMatchesSession(route) {
			targetSessionID = route.PlaySessionID
		}
	}
	if serverID == "" && rawMediaSourceID == "" {
		if route, ok := a.playbackRoutes.Active(owner, rawItemID); ok && routeMatchesSession(route) {
			serverID, targetSessionID = route.ServerID, route.PlaySessionID
		}
	}
	if serverID == "" && play != nil {
		serverID, targetSessionID = play.ServerID, play.OriginalID
	}
	if serverID == "" && item != nil {
		if resolved, err := a.resolveAuthorizedSessionRouteID(reqCtx, rawItemID); err == nil && resolved != nil {
			serverID = resolved.ServerID
		}
	}
	if serverID == "" && item == nil {
		// A raw item has no global namespace. Accept only one proven mapping
		// among this request's authorized sources, never the oldest global ID.
		for _, client := range a.allowedClients(reqCtx) {
			if _, _, known := a.IDStore.ResolveOriginalIDForServer(rawItemID, client.ID); !known {
				continue
			}
			if rawMediaSourceID != "" {
				if _, _, known := a.IDStore.ResolveOriginalIDForServer(rawMediaSourceID, client.ID); !known {
					continue
				}
			}
			if serverID != "" {
				return "", false
			}
			serverID = client.ID
		}
	}
	if serverID == "" || !a.isServerAllowed(reqCtx, serverID) {
		return "", false
	}
	if item == nil {
		if media != nil && media.MediaItemID == rawItemID {
			if group := a.IDStore.ResolveMergeMember(serverID, rawItemID, media.OriginalID); group != "" {
				item = a.IDStore.ResolveVirtualID(group)
			}
		}
		if item == nil {
			if groups := a.IDStore.MergeGroupsForItem(serverID, rawItemID); len(groups) == 1 {
				item = a.IDStore.ResolveVirtualID(groups[0])
			}
		}
		if item == nil {
			_, mapped, ok := a.IDStore.ResolveOriginalIDForServer(rawItemID, serverID)
			if !ok {
				return "", false
			}
			item = mapped
		}
	}
	originalItemID := resolvedOriginalIDForServer(item, serverID)
	if media != nil && media.MediaItemID != "" {
		originalItemID = media.MediaItemID
	}
	if originalItemID == "" {
		return "", false
	}
	if rawMediaSourceID != "" {
		if media == nil {
			var known bool
			virtualSourceID, media, known = a.IDStore.ResolveMergeMediaSourceForItem(rawMediaSourceID, serverID, originalItemID)
			if !known {
				return "", false
			}
			if route, ok := a.playbackRoutes.MediaSource(owner, virtualSourceID); ok &&
				route.ItemID == rawItemID && route.ServerID == serverID && routeMatchesSession(route) {
				targetSessionID = route.PlaySessionID
			}
		}
		if media.ServerID != serverID {
			return "", false
		}
		groupID := rawItemID
		if a.IDStore.ResolveVirtualID(rawItemID) == nil {
			groupID = a.IDStore.ResolveMergeMember(serverID, originalItemID, media.OriginalID)
		}
		if (media.MediaItemID != "" && media.MediaItemID != originalItemID) || !a.IDStore.MergeMemberAllowed(groupID, serverID, originalItemID, media.OriginalID) {
			return "", false
		}
		body["MediaSourceId"] = media.OriginalID
	}
	if targetSessionID == "" && play != nil {
		// An explicit source/session pair can cross namespaces only through a
		// proven owner/item-scoped PlaybackInfo alias, never the latest route.
		if play.ServerID != serverID {
			return "", false
		}
		targetSessionID = play.OriginalID
	}
	body["ItemId"] = originalItemID
	if rawPlaySessionID != "" && targetSessionID != "" {
		body["PlaySessionId"] = targetSessionID
	}
	return serverID, true
}

// recordSessionWatchEvent consumes IDs after the existing session translation.
func (a *App) recordSessionWatchEvent(r *http.Request, virtualItemID string, body map[string]any, serverID string, kind playbackWatchEventKind) {
	event, playSessionID := sessionWatchEvent(body, serverID, kind)
	a.recordPlaybackWatchEvent(r, virtualItemID, playSessionID, event)
}

// enrichWatchProgressMetadata fetches item details from upstream and populates
// metadata fields on the WatchProgress (type, series info, name, etc.).
func (a *App) enrichWatchProgressMetadata(r *http.Request, reqCtx *RequestContext, p *WatchProgress, originalItemID string, serverID string) {
	client := a.Upstream.ClientByID(serverID)
	if client == nil || !client.IsOnline() || originalItemID == "" {
		return
	}
	q := url.Values{}
	q.Set("Fields", "ProviderIds")
	payload, err := client.RequestJSON(r.Context(), reqCtx, a.Identity, http.MethodGet, "/Users/"+client.clientUserID()+"/Items/"+originalItemID, q, nil)
	if err != nil {
		return
	}
	item, ok := payload.(map[string]any)
	if !ok {
		return
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	if !a.mediaAccessScopeLocked(reqCtx).allows(serverID) || a.watchItemOriginalID(p.VirtualItemID, serverID) != originalItemID {
		return
	}
	a.populateWatchProgressMetadata(p, item, serverID)
}

func (a *App) populateWatchProgressMetadata(p *WatchProgress, item map[string]any, serverID string) {
	p.ItemType, _ = item["Type"].(string)
	p.Name, _ = item["Name"].(string)
	if year, ok := numericInt(item["ProductionYear"]); ok {
		p.ProductionYear = year
	}
	if providerIDs, ok := item["ProviderIds"].(map[string]any); ok {
		p.ProviderTmdb, _ = providerIDs["Tmdb"].(string)
	}
	if rt, ok := numericInt64(item["RunTimeTicks"]); ok && rt > 0 && p.RuntimeTicks == 0 {
		p.RuntimeTicks = rt
	}

	// Episode-specific: series info
	if p.ItemType == "Episode" {
		p.SeriesName, _ = item["SeriesName"].(string)
		if parentIdx, ok := numericInt(item["ParentIndexNumber"]); ok {
			p.ParentIndexNumber = parentIdx
		}
		if idx, ok := numericInt(item["IndexNumber"]); ok {
			p.IndexNumber = idx
		}
		// Map seriesId to virtual
		if seriesID, _ := item["SeriesId"].(string); seriesID != "" {
			p.SeriesOriginalID = seriesID
			p.SeriesVirtualID = a.IDStore.GetOrCreateVirtualID(seriesID, serverID)
		}
	}
}

// numericInt64 extracts an int64 from a JSON number (float64) or returns 0.
func numericInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(math.Round(n)), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	}
	return 0, false
}

func (a *App) translateMediaSourceQuery(values url.Values) {
	if mediaSourceID := values.Get("MediaSourceId"); mediaSourceID != "" {
		if resolved := a.IDStore.ResolveVirtualID(mediaSourceID); resolved != nil {
			values.Set("MediaSourceId", resolved.OriginalID)
		}
	}
}

func (a *App) performUpstream(ctx *http.Request, client *UpstreamClient, method, path string, query url.Values, body any) (*http.Response, error) {
	reqCtx := requestContextFrom(ctx.Context())
	return client.doRequest(ctx.Context(), reqCtx, method, path, query, body, client.requestHeaders(reqCtx, a.Identity), false)
}

func readUpstreamJSONOrNoContent(resp *http.Response) (int, any, error) {
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, nil, fmt.Errorf("upstream request failed: %s %s", resp.Status, string(bytes.TrimSpace(payload)))
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || resp.StatusCode == http.StatusNoContent {
		return resp.StatusCode, nil, nil
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, payload, nil
}

func (a *App) forwardNoContent(r *http.Request, client *UpstreamClient, method, path string, query url.Values, body any) error {
	resp, err := a.performUpstream(r, client, method, path, query, body)
	if err != nil {
		return classifySessionTransportError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return newSessionUpstreamRejectedError(resp.StatusCode)
	}
	// Session/no-content calls use the HTTP status as the upstream confirmation.
	// Some Emby-compatible servers return a small non-JSON body with 200; that is
	// still a successful session update and must not be reclassified as a failure.
	return nil
}

func (a *App) forwardJSONOrNoContent(r *http.Request, client *UpstreamClient, method, path string, query url.Values, body any) (int, any, error) {
	resp, err := a.performUpstream(r, client, method, path, query, body)
	if err != nil {
		return 0, nil, err
	}
	return readUpstreamJSONOrNoContent(resp)
}

func asBodyMap(payload any) (map[string]any, bool) {
	if payload == nil {
		return map[string]any{}, true
	}
	body, ok := payload.(map[string]any)
	return body, ok
}

func (a *App) heartbeatPlaybackLease(reqCtx *RequestContext, serverID string) bool {
	limiterUserID, ok := a.playbackLimiterKey(reqCtx, serverID)
	if !ok {
		return false
	}
	deviceID := playbackDeviceID(reqCtx)
	if deviceID == "" {
		return false
	}
	return a.PlaybackLimiter.Heartbeat(limiterUserID, serverID, deviceID)
}

// stopPlaybackLease releases only the lease owned by the resolved lifecycle
// device and the exact upstream PlaySessionID. Device identity comes from the
// request context; callers pass the session after virtual-ID translation.
func (a *App) stopPlaybackLease(reqCtx *RequestContext, serverID, playSessionID string) bool {
	limiterUserID, ok := a.playbackLimiterKey(reqCtx, serverID)
	if !ok {
		return false
	}
	deviceID := playbackDeviceID(reqCtx)
	if deviceID == "" {
		return false
	}
	return a.PlaybackLimiter.Stop(limiterUserID, serverID, deviceID, playSessionID)
}

// sessionUpstreamClient centralizes the lifecycle availability gate. A missing
// client and a configured-but-offline client are the same availability state to
// Playing/Progress; Stopped consumes the same gate but applies its own local
// finalization semantics.
func (a *App) sessionUpstreamClient(serverID string) (*UpstreamClient, bool) {
	if a == nil || a.Upstream == nil {
		return nil, false
	}
	client := a.Upstream.ClientByID(serverID)
	if client == nil {
		return nil, false
	}
	// Missing UserID must reach outbound preparation so its original 503
	// kind/field response and authentication recovery remain intact. IsOnline
	// includes UserID readiness and would incorrectly classify that failure as
	// offline, hiding it behind Stopped's best-effort 204 response.
	client.mu.RLock()
	available := client.Online && client.AccessToken != ""
	client.mu.RUnlock()
	return client, available
}

// finalizeStoppedPlayback applies the local half of a Stopped event exactly once.
// It intentionally runs even when outbound request preparation failed: the client
// has ended playback locally, so watch progress must be persisted and only the
// matching device/session lease may be released.
func (a *App) finalizeStoppedPlayback(r *http.Request, virtualItemID string, body map[string]any, serverID, playSessionID string) {
	a.recordSessionWatchEvent(r, virtualItemID, body, serverID, playbackWatchStopped)
	a.stopAdmittedPlaybackLease(r, serverID, playSessionID)
}

// handleStoppedPreparationError preserves the preparation error response while
// still finalizing the local Stopped lifecycle. Non-preparation errors are left to
// the existing best-effort upstream-error path.
func (a *App) handleStoppedPreparationError(w http.ResponseWriter, r *http.Request, virtualItemID string, body map[string]any, serverID, playSessionID string, err error) bool {
	status, ok := preparationErrorStatus(err)
	if !ok {
		return false
	}
	a.finalizeStoppedPlayback(r, virtualItemID, body, serverID, playSessionID)
	writeJSON(w, status, preparationErrorBody(err))
	return true
}

func (a *App) handleSessionPlaying(w http.ResponseWriter, r *http.Request) {
	payload, err := decodeOptionalJSON(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Invalid JSON body"})
		return
	}
	body, ok := asBodyMap(payload)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Invalid session payload"})
		return
	}
	virtualItemID, _ := body["ItemId"].(string) // capture before translation
	mediaID, _ := body["MediaSourceId"].(string)
	if a.sessionTargetAccessDenied(requestContextFrom(r.Context()), virtualItemID, mediaID) {
		writeJSON(w, http.StatusForbidden, map[string]any{"message": "Access denied"})
		return
	}
	serverID, found := a.translateSessionBodyIDs(requestContextFrom(r.Context()), body)
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Cannot determine target server"})
		return
	}
	if !a.isServerAllowed(requestContextFrom(r.Context()), serverID) {
		writeJSON(w, http.StatusForbidden, map[string]any{"message": "Access denied"})
		return
	}
	r = a.admitSessionWatchRequest(r, virtualItemID, body, serverID, playbackWatchStarted)
	if a.playbackDeviceIDRequired(requestContextFrom(r.Context()), serverID) {
		writePlaybackDeviceIDRequired(w)
		return
	}
	client, available := a.sessionUpstreamClient(serverID)
	if !available {
		writeSessionUpstreamUnavailable(w)
		return
	}
	if err := a.forwardNoContent(r, client, http.MethodPost, "/Sessions/Playing", nil, body); err != nil {
		// A preparation failure means the request never left the process, so the
		// client gets the preparation status and no local progress is recorded for
		// a play event that was never announced upstream.
		if status, ok := preparationErrorStatus(err); ok {
			writeJSON(w, status, preparationErrorBody(err))
			return
		}
		if a.Logger != nil {
			a.Logger.Warnf("Sessions/Playing upstream error (server %s): %v", serverID, err)
		}
		if !writeSessionUpstreamError(w, err) {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"code":    upstreamSessionFailedCode,
				"message": "Upstream session request failed",
			})
		}
		return
	}

	// Local watch state and the lease heartbeat commit only after the upstream
	// has confirmed the Playing event with a 2xx response.
	a.recordSessionWatchEvent(r, virtualItemID, body, serverID, playbackWatchStarted)
	a.heartbeatPlaybackLease(requestContextFrom(r.Context()), serverID)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleSessionPlayingProgress(w http.ResponseWriter, r *http.Request) {
	payload, err := decodeOptionalJSON(r)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body, ok := asBodyMap(payload)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	virtualItemID, _ := body["ItemId"].(string)
	mediaID, _ := body["MediaSourceId"].(string)
	if a.sessionTargetAccessDenied(requestContextFrom(r.Context()), virtualItemID, mediaID) {
		writeJSON(w, http.StatusForbidden, map[string]any{"message": "Access denied"})
		return
	}
	serverID, found := a.translateSessionBodyIDs(requestContextFrom(r.Context()), body)
	if !found {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !a.isServerAllowed(requestContextFrom(r.Context()), serverID) {
		writeJSON(w, http.StatusForbidden, map[string]any{"message": "Access denied"})
		return
	}
	r = a.admitSessionWatchRequest(r, virtualItemID, body, serverID, playbackWatchProgress)
	if a.playbackDeviceIDRequired(requestContextFrom(r.Context()), serverID) {
		writePlaybackDeviceIDRequired(w)
		return
	}
	client, available := a.sessionUpstreamClient(serverID)
	if !available {
		writeSessionUpstreamUnavailable(w)
		return
	}
	if err := a.forwardNoContent(r, client, http.MethodPost, "/Sessions/Playing/Progress", nil, body); err != nil {
		if status, ok := preparationErrorStatus(err); ok {
			writeJSON(w, status, preparationErrorBody(err))
			return
		}
		if a.Logger != nil {
			a.Logger.Warnf("Sessions/Playing/Progress upstream error (server %s): %v", serverID, err)
		}
		if !writeSessionUpstreamError(w, err) {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"code":    upstreamSessionFailedCode,
				"message": "Upstream session request failed",
			})
		}
		return
	}

	// Local watch state and the lease heartbeat commit only after the upstream
	// has confirmed the Progress event with a 2xx response.
	a.recordSessionWatchEvent(r, virtualItemID, body, serverID, playbackWatchProgress)
	a.heartbeatPlaybackLease(requestContextFrom(r.Context()), serverID)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleSessionPlayingStopped(w http.ResponseWriter, r *http.Request) {
	payload, err := decodeOptionalJSON(r)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body, ok := asBodyMap(payload)
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	virtualItemID, _ := body["ItemId"].(string)
	mediaID, _ := body["MediaSourceId"].(string)
	if a.sessionTargetAccessDenied(requestContextFrom(r.Context()), virtualItemID, mediaID) {
		writeJSON(w, http.StatusForbidden, map[string]any{"message": "Access denied"})
		return
	}
	serverID, found := a.translateSessionBodyIDs(requestContextFrom(r.Context()), body)
	if !found {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	playSessionID, _ := body["PlaySessionId"].(string)
	if !a.isServerAllowed(requestContextFrom(r.Context()), serverID) {
		writeJSON(w, http.StatusForbidden, map[string]any{"message": "Access denied"})
		return
	}
	r = a.admitSessionWatchRequest(r, virtualItemID, body, serverID, playbackWatchStopped)
	if a.playbackDeviceIDRequired(requestContextFrom(r.Context()), serverID) {
		// Preserve the terminal handler shape. Without an admitted device/session
		// identity, the shared writer skips this event and no lease is released.
		a.recordSessionWatchEvent(r, virtualItemID, body, serverID, playbackWatchStopped)
		writePlaybackDeviceIDRequired(w)
		return
	}
	client, available := a.sessionUpstreamClient(serverID)
	if !available {
		// Stopped is a client-observed terminal event. Even when the upstream is
		// unavailable, preserve the final local progress and release only the exact
		// matching device/session lease instead of stranding it until stale timeout.
		a.finalizeStoppedPlayback(r, virtualItemID, body, serverID, playSessionID)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := a.forwardNoContent(r, client, http.MethodPost, "/Sessions/Playing/Stopped", nil, body); err != nil {
		if a.handleStoppedPreparationError(w, r, virtualItemID, body, serverID, playSessionID, err) {
			return
		}
		if a.Logger != nil {
			a.Logger.Warnf("Sessions/Playing/Stopped upstream error (server %s): %v", serverID, err)
		}
	}
	a.finalizeStoppedPlayback(r, virtualItemID, body, serverID, playSessionID)
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleSessionsCapabilities(w http.ResponseWriter, r *http.Request) {
	a.broadcastCapabilities(w, r, "/Sessions/Capabilities", true)
}

func (a *App) handleSessionsCapabilitiesFull(w http.ResponseWriter, r *http.Request) {
	a.broadcastCapabilities(w, r, "/Sessions/Capabilities/Full", false)
}

// broadcastCapabilities relays a capabilities broadcast to every allowed
// upstream. Each upstream gets its own copy of the body, so preparing one
// upstream's identity can never modify the body another upstream will receive.
// A preparation failure is reported with its own status instead of being hidden
// behind the best-effort 204.
func (a *App) broadcastCapabilities(w http.ResponseWriter, r *http.Request, path string, forwardQuery bool) {
	body, err := decodeOptionalJSON(r)
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	reqCtx := requestContextFrom(r.Context())
	var query url.Values
	if forwardQuery {
		query = cloneValues(r.URL.Query())
	}
	for _, client := range a.allowedClients(reqCtx) {
		if err := a.forwardNoContent(r, client, http.MethodPost, path, query, body); err != nil {
			if status, ok := preparationErrorStatus(err); ok {
				writeJSON(w, status, preparationErrorBody(err))
				return
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleUserPlayingItemStart(w http.ResponseWriter, r *http.Request) {
	a.handleUserPlayingItem(w, r, http.MethodPost)
}

func (a *App) handleUserPlayingItemStop(w http.ResponseWriter, r *http.Request) {
	a.handleUserPlayingItem(w, r, http.MethodDelete)
}

func (a *App) handleUserPlayingItem(w http.ResponseWriter, r *http.Request, method string) {
	mediaID, _ := explicitMediaSourceID(r.URL.Query(), nil)
	if a.sessionTargetAccessDenied(requestContextFrom(r.Context()), r.PathValue("itemId"), mediaID) {
		writeJSON(w, http.StatusForbidden, map[string]any{"message": "Access denied"})
		return
	}
	resolved, query, found := a.resolveLegacyWatchTarget(r)
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Cannot determine authorized playback source"})
		return
	}
	kind := playbackWatchStarted
	if method == http.MethodDelete {
		kind = playbackWatchStopped
	}
	// Ordering is captured before version qualification or lifecycle network I/O.
	r = a.admitLegacyWatchRequest(r, resolved.ServerID, query, kind)
	if kind == playbackWatchStopped {
		// Also finish the captured lease on a version/preparation rejection. The
		// revision fence makes a repeated successful finalization harmless.
		sessionID, _ := legacyPlaySessionID(query)
		defer a.stopAdmittedPlaybackLease(r, resolved.ServerID, sessionID)
	}
	if _, available := a.sessionUpstreamClient(resolved.ServerID); !available {
		if kind == playbackWatchStopped {
			a.recordLegacyWatchEvent(r, resolved.ServerID, query, kind)
			w.WriteHeader(http.StatusNoContent)
		} else {
			writeSessionUpstreamUnavailable(w)
		}
		return
	}
	if sourceID, err := explicitMediaSourceID(r.URL.Query(), nil); err != nil {
		writeMediaSelectionError(w, err)
		return
	} else if sourceID != "" {
		selection, err := a.selectAuthorizedMediaSource(r, r.PathValue("itemId"), sourceID, resolved)
		if err != nil {
			writeMediaSelectionError(w, err)
			return
		}
		if selection.ServerID != resolved.ServerID {
			writeMediaSelectionError(w, errMediaAccessDenied)
			return
		}
		setSelectedMediaSource(query, nil, selection)
	}
	path := "/Users/" + resolved.Client.clientUserID() + "/PlayingItems/" + resolved.OriginalID
	err := a.forwardNoContent(r, resolved.Client, method, path, query, nil)
	if err == nil || kind == playbackWatchStopped {
		a.recordLegacyWatchEvent(r, resolved.ServerID, query, kind)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handlePlayedItemAdd(w http.ResponseWriter, r *http.Request) {
	a.handlePlayedItemState(w, r, http.MethodPost, true, false)
}

func (a *App) handlePlayedItemRemove(w http.ResponseWriter, r *http.Request) {
	a.handlePlayedItemState(w, r, http.MethodDelete, false, false)
}

func (a *App) handlePlayedItemRemoveCompat(w http.ResponseWriter, r *http.Request) {
	a.handlePlayedItemState(w, r, http.MethodPost, false, true)
}

func (a *App) handlePlayedItemState(w http.ResponseWriter, r *http.Request, method string, played, deleteCompat bool) {
	virtualItemID := r.PathValue("itemId")
	resolved, routeOK := a.resolveRequestRouteID(w, r, virtualItemID)
	if !routeOK {
		return
	}
	if resolved == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Item not found"})
		return
	}
	if !a.requireServerAccess(w, r, resolved) {
		return
	}

	// Hills sends the standard PlayedItems mutation with an empty JSON body
	// (Content-Type: application/json, Content-Length: 0). decodeOptionalJSON
	// intentionally represents that as nil. Preserve the declared media type
	// when forwarding the empty body so this dedicated route keeps the client's
	// wire shape while translating the user/item IDs and outbound credentials.
	contentType := r.Header.Get("Content-Type")
	body, err := decodeOptionalJSON(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Invalid JSON body"})
		return
	}
	if body == nil && contentType != "" {
		body = rawRequestBody{data: nil, contentType: contentType}
	}

	path := fmt.Sprintf("/Users/%s/PlayedItems/%s", resolved.Client.clientUserID(), resolved.OriginalID)
	if deleteCompat {
		path += "/Delete"
	}
	query := cloneValues(r.URL.Query())
	status, payload, err := a.forwardJSONOrNoContent(r, resolved.Client, method, path, query, body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"message": err.Error()})
		return
	}

	if a.WatchStore != nil {
		if reqCtx := requestContextFrom(r.Context()); reqCtx != nil && reqCtx.ProxyUser != nil && reqCtx.ProxyUser.Role != "admin" {
			if err := a.ensureWatchRecordMetadata(r, reqCtx, virtualItemID, resolved); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "Failed to prepare local watched state"})
				return
			}
			playedAt := parseLocalPlayedAt(query.Get("DatePlayed"))
			if err := a.mutateLocalPlayed(reqCtx.ProxyUser.UserID, virtualItemID, played, playedAt, reqCtx); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "Failed to update local watched state"})
				return
			}
		}
	}

	if payload == nil {
		if status == 0 {
			status = http.StatusNoContent
		}
		w.WriteHeader(status)
		return
	}
	a.overlayLocalUserData(r, virtualItemID, payload)
	cfg := a.ConfigStore.Snapshot()
	rewriteResponseIDs(payload, resolved.ServerID, a.IDStore, cfg.Server.ID, a.clientFacingUserIDFor(r))
	writeJSON(w, status, payload)
}

func (a *App) handleUserItemUserData(w http.ResponseWriter, r *http.Request) {
	virtualItemID := r.PathValue("itemId")
	resolved, routeOK := a.resolveRequestRouteID(w, r, virtualItemID)
	if !routeOK {
		return
	}
	if resolved == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Item not found"})
		return
	}
	if !a.requireServerAccess(w, r, resolved) {
		return
	}
	body, err := decodeOptionalJSON(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Invalid JSON body"})
		return
	}
	var playedValue *bool
	var favoriteValue *bool
	var positionValue *int64
	var runtimeValue int64
	var localPlayedAt int64
	if bodyMap, ok := body.(map[string]any); ok {
		if played, ok := bodyMap["Played"].(bool); ok {
			playedValue = &played
		}
		if favorite, ok := bodyMap["IsFavorite"].(bool); ok {
			favoriteValue = &favorite
		}
		if position, ok := numericInt64(bodyMap["PlaybackPositionTicks"]); ok {
			positionValue = &position
		}
		if runtime, ok := numericInt64(bodyMap["RunTimeTicks"]); ok {
			runtimeValue = runtime
		} else if runtime, ok := numericInt64(bodyMap["RuntimeTicks"]); ok {
			runtimeValue = runtime
		}
		if text, _ := bodyMap["LastPlayedDate"].(string); text != "" {
			localPlayedAt = parseLocalPlayedAt(text)
		}
	}
	status, payload, err := a.forwardJSONOrNoContent(r, resolved.Client, http.MethodPost, fmt.Sprintf("/Users/%s/Items/%s/UserData", resolved.Client.clientUserID(), resolved.OriginalID), nil, body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"message": err.Error()})
		return
	}
	// Dual-write only after the upstream accepted the change. The local record is
	// seeded with route/item metadata first so explicit mutations never create a
	// server-less skeleton that Resume/NextUp cannot resolve later.
	if a.WatchStore != nil {
		if reqCtx := requestContextFrom(r.Context()); reqCtx != nil && reqCtx.ProxyUser != nil && reqCtx.ProxyUser.Role != "admin" {
			if positionValue != nil || playedValue != nil || favoriteValue != nil {
				if err := a.ensureWatchRecordMetadata(r, reqCtx, virtualItemID, resolved); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "Failed to prepare local user state"})
					return
				}
			}
			if err := a.mutateLocalUserData(reqCtx.ProxyUser.UserID, virtualItemID,
				playedValue, positionValue, runtimeValue, favoriteValue, localPlayedAt, reqCtx); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"message": err.Error()})
				return
			}
		}
	}
	if payload == nil {
		if status == 0 {
			status = http.StatusNoContent
		}
		w.WriteHeader(status)
		return
	}
	// Overlay local UserData for non-admin users
	a.overlayLocalUserData(r, virtualItemID, payload)
	cfg := a.ConfigStore.Snapshot()
	rewriteResponseIDs(payload, resolved.ServerID, a.IDStore, cfg.Server.ID, a.clientFacingUserIDFor(r))
	writeJSON(w, status, payload)
}

func (a *App) handleFavoriteItemAdd(w http.ResponseWriter, r *http.Request) {
	virtualItemID := r.PathValue("itemId")
	resolved, routeOK := a.resolveRequestRouteID(w, r, virtualItemID)
	if !routeOK {
		return
	}
	if resolved == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Item not found"})
		return
	}
	if !a.requireServerAccess(w, r, resolved) {
		return
	}
	body, err := decodeOptionalJSON(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Invalid JSON body"})
		return
	}
	status, payload, err := a.forwardJSONOrNoContent(r, resolved.Client, http.MethodPost, fmt.Sprintf("/Users/%s/FavoriteItems/%s", resolved.Client.clientUserID(), resolved.OriginalID), nil, body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"message": err.Error()})
		return
	}
	// Dual-write favorite after upstream success; metadata seeding prevents a
	// favorite-only first interaction from becoming an unroutable skeleton row.
	if a.WatchStore != nil {
		if reqCtx := requestContextFrom(r.Context()); reqCtx != nil && reqCtx.ProxyUser != nil && reqCtx.ProxyUser.Role != "admin" {
			if err := a.ensureWatchRecordMetadata(r, reqCtx, virtualItemID, resolved); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "Failed to prepare local favorite state"})
				return
			}
			if err := a.mutateLocalFavorite(reqCtx, virtualItemID, true); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "Failed to update local favorite state"})
				return
			}
		}
	}
	if payload == nil {
		if status == 0 {
			status = http.StatusNoContent
		}
		w.WriteHeader(status)
		return
	}
	a.overlayLocalUserData(r, virtualItemID, payload)
	cfg := a.ConfigStore.Snapshot()
	rewriteResponseIDs(payload, resolved.ServerID, a.IDStore, cfg.Server.ID, a.clientFacingUserIDFor(r))
	writeJSON(w, status, payload)
}

func (a *App) handleFavoriteItemRemove(w http.ResponseWriter, r *http.Request) {
	virtualItemID := r.PathValue("itemId")
	resolved, routeOK := a.resolveRequestRouteID(w, r, virtualItemID)
	if !routeOK {
		return
	}
	if resolved == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Item not found"})
		return
	}
	if !a.requireServerAccess(w, r, resolved) {
		return
	}
	body, err := decodeOptionalJSON(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Invalid JSON body"})
		return
	}
	status, payload, err := a.forwardJSONOrNoContent(r, resolved.Client, http.MethodDelete, fmt.Sprintf("/Users/%s/FavoriteItems/%s", resolved.Client.clientUserID(), resolved.OriginalID), nil, body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"message": err.Error()})
		return
	}
	// Dual-write favorite removal after upstream success.
	if a.WatchStore != nil {
		if reqCtx := requestContextFrom(r.Context()); reqCtx != nil && reqCtx.ProxyUser != nil && reqCtx.ProxyUser.Role != "admin" {
			if err := a.ensureWatchRecordMetadata(r, reqCtx, virtualItemID, resolved); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "Failed to prepare local favorite state"})
				return
			}
			if err := a.mutateLocalFavorite(reqCtx, virtualItemID, false); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "Failed to update local favorite state"})
				return
			}
		}
	}
	if payload == nil {
		if status == 0 {
			status = http.StatusNoContent
		}
		w.WriteHeader(status)
		return
	}
	a.overlayLocalUserData(r, virtualItemID, payload)
	cfg := a.ConfigStore.Snapshot()
	rewriteResponseIDs(payload, resolved.ServerID, a.IDStore, cfg.Server.ID, a.clientFacingUserIDFor(r))
	writeJSON(w, status, payload)
}

// These compatibility wrappers keep existing explicit handlers small while all
// regular-user UserData semantics live in user_state.go.
func (a *App) overlayLocalUserDataItems(r *http.Request, items []map[string]any) {
	for _, item := range items {
		a.filterAuthorizedMediaSources(requestContextFrom(r.Context()), item)
	}
	if a.WatchStore == nil || !isRegularProxyUser(r) || len(items) == 0 {
		return
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if id, _ := item["Id"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	rows, err := a.visibleWatchProgressBatch(r, ids)
	if err != nil {
		a.logVisibleWatchReadError(err)
		for _, item := range items {
			clearPersonalUserState(item)
		}
		return
	}
	for _, item := range items {
		id, _ := item["Id"].(string)
		applyUserDataStateToItem(item, progressPtr(rows, id))
	}
}

func applyUserDataStateToItem(item map[string]any, row *WatchProgress) {
	if item == nil {
		return
	}
	if ud, ok := item["UserData"].(map[string]any); ok {
		applyUserDataState(ud, row)
	} else if row != nil && isBaseItemStateCandidate(item) {
		// Lists and explicit item responses need the same missing-UserData
		// behavior as the recursive fallback normalizer.
		ud := map[string]any{}
		if id, _ := item["Id"].(string); id != "" {
			ud["ItemId"] = id
		}
		applyUserDataState(ud, row)
		item["UserData"] = ud
	}
	if isUserDataMap(item) {
		applyUserDataState(item, row)
	}
}

func (a *App) overlayLocalUserData(r *http.Request, virtualItemID string, payload any) {
	a.normalizeLocalUserDataForItem(r, virtualItemID, payload)
}
