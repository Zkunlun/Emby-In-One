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
	return resolved.OriginalID
}

func (a *App) translateSessionBodyIDs(reqCtx *RequestContext, body map[string]any) (string, bool) {
	rawItemID, _ := body["ItemId"].(string)
	rawMediaSourceID, _ := body["MediaSourceId"].(string)
	rawPlaySessionID, _ := body["PlaySessionId"].(string)

	resolve := func(value string) *ResolvedID {
		if value == "" {
			return nil
		}
		resolved := a.IDStore.ResolveVirtualID(value)
		if resolved == nil {
			resolved = a.IDStore.ResolveByOriginalID(value)
		}
		return resolved
	}
	itemResolved := resolve(rawItemID)
	mediaResolved := resolve(rawMediaSourceID)
	playResolved := resolve(rawPlaySessionID)

	owner := playbackRouteOwner(reqCtx)
	serverID := ""
	targetPlaySessionID := ""

	// Item and MediaSource routes are overwritten by later PlaybackInfo calls.
	// Only an explicit session match may translate a client session onto a route's
	// upstream session; a delayed old event must never acquire the latest session.
	routeMatchesSession := func(route playbackRouteEntry) bool {
		if rawPlaySessionID == "" {
			return true
		}
		if route.ClientPlaySessionID != "" {
			if rawPlaySessionID == route.ClientPlaySessionID {
				return true
			}
			if clientSession := resolve(route.ClientPlaySessionID); clientSession != nil && playResolved != nil &&
				clientSession.ServerID == playResolved.ServerID && clientSession.OriginalID == playResolved.OriginalID {
				return true
			}
		}
		if playResolved != nil {
			return playResolved.ServerID == route.ServerID && playResolved.OriginalID == route.PlaySessionID
		}
		return rawPlaySessionID == route.PlaySessionID
	}

	// A selected MediaSource is the strongest evidence of the server actually
	// serving bytes. PlaybackInfo records that source's own upstream PlaySessionID.
	if mediaResolved != nil {
		serverID = mediaResolved.ServerID
		if route, ok := a.playbackRoutes.MediaSource(owner, rawMediaSourceID); ok && route.ServerID == serverID && routeMatchesSession(route) {
			targetPlaySessionID = route.PlaySessionID
		}
	}
	// Some clients omit MediaSourceId from check-ins after opening the stream. Use
	// the request-owner-scoped active route instead of the old process-global item route.
	if serverID == "" && rawItemID != "" {
		if route, ok := a.playbackRoutes.Active(owner, rawItemID); ok {
			serverID = route.ServerID
			if routeMatchesSession(route) {
				targetPlaySessionID = route.PlaySessionID
			}
		}
	}
	if targetPlaySessionID == "" && playResolved != nil {
		// Without a proven cross-upstream alias, the explicit session retains its
		// own server namespace even when the latest item/media route has moved.
		serverID = playResolved.ServerID
		targetPlaySessionID = playResolved.OriginalID
	}
	if serverID == "" && itemResolved != nil {
		serverID = itemResolved.ServerID
	}
	if serverID == "" && rawItemID != "" {
		// Legacy last resort for callers that do not yet have request-scoped route state.
		if active, ok := a.IDStore.GetActiveStream(rawItemID); ok {
			serverID = active
		}
	}

	if itemResolved != nil {
		body["ItemId"] = resolvedOriginalIDForServer(itemResolved, serverID)
	}
	if mediaResolved != nil {
		body["MediaSourceId"] = mediaResolved.OriginalID
	}
	if rawPlaySessionID != "" {
		if targetPlaySessionID != "" {
			body["PlaySessionId"] = targetPlaySessionID
		} else if playResolved != nil {
			body["PlaySessionId"] = playResolved.OriginalID
		}
	}

	if a.Logger != nil {
		a.Logger.Debugf("Session translation: TargetServer=%s, MediaSourceId=%v, ItemId=%v, PlaySessionId=%v",
			serverID, body["MediaSourceId"], body["ItemId"], body["PlaySessionId"])
	}
	return serverID, serverID != ""
}

// recordSessionToWatchStore writes playback progress to the local WatchStore
// for non-admin users. virtualItemID is the pre-translation virtual ID.
// isStopped indicates whether the playback has ended (Stopped event).
func (a *App) recordSessionToWatchStore(r *http.Request, virtualItemID string, body map[string]any, serverID string, isStopped bool) {
	if a.WatchStore == nil || virtualItemID == "" {
		return
	}
	reqCtx := requestContextFrom(r.Context())
	if reqCtx == nil || reqCtx.ProxyUser == nil || reqCtx.ProxyUser.Role == "admin" {
		return
	}
	positionTicks, _ := numericInt64(body["PositionTicks"])
	runtimeTicks, _ := numericInt64(body["RunTimeTicks"])
	originalItemID, _ := body["ItemId"].(string)

	p := &WatchProgress{
		ProxyUserID:    reqCtx.ProxyUser.UserID,
		VirtualItemID:  virtualItemID,
		ServerID:       serverID,
		OriginalItemID: originalItemID,
		PositionTicks:  positionTicks,
		RuntimeTicks:   runtimeTicks,
	}

	// Auto-mark played if stopped near end (>= 90% of runtime)
	if isStopped && runtimeTicks > 0 && positionTicks > 0 {
		ratio := float64(positionTicks) / float64(runtimeTicks)
		if ratio >= 0.90 {
			p.Played = true
			p.PositionTicks = 0
		}
	}

	// Enrich with item metadata if not already stored
	existing := a.WatchStore.GetProgress(reqCtx.ProxyUser.UserID, virtualItemID)
	if existing == nil || existing.ItemType == "" {
		a.enrichWatchProgressMetadata(r, reqCtx, p, originalItemID, serverID)
	}

	if err := a.WatchStore.RecordProgress(p); err != nil {
		if a.Logger != nil {
			a.Logger.Warnf("WatchStore record error: %s", redactURLInError(err))
		}
	}
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
	a.recordSessionToWatchStore(r, virtualItemID, body, serverID, true)
	a.stopPlaybackLease(requestContextFrom(r.Context()), serverID, playSessionID)
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
	serverID, found := a.translateSessionBodyIDs(requestContextFrom(r.Context()), body)
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Cannot determine target server"})
		return
	}
	if !a.isServerAllowed(requestContextFrom(r.Context()), serverID) {
		writeJSON(w, http.StatusForbidden, map[string]any{"message": "Access denied"})
		return
	}
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
	a.recordSessionToWatchStore(r, virtualItemID, body, serverID, false)
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
	serverID, found := a.translateSessionBodyIDs(requestContextFrom(r.Context()), body)
	if !found {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !a.isServerAllowed(requestContextFrom(r.Context()), serverID) {
		writeJSON(w, http.StatusForbidden, map[string]any{"message": "Access denied"})
		return
	}
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
	a.recordSessionToWatchStore(r, virtualItemID, body, serverID, false)
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
	if a.playbackDeviceIDRequired(requestContextFrom(r.Context()), serverID) {
		// A local Stopped event still updates per-user progress, but an unidentified
		// request must never release a device-owned lease or be reported upstream as
		// if its playback identity were authoritative.
		a.recordSessionToWatchStore(r, virtualItemID, body, serverID, true)
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
	resolved := a.resolveRouteID(r.PathValue("itemId"))
	if resolved == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Item not found"})
		return
	}
	if !a.requireServerAccess(w, r, resolved) {
		return
	}
	query := cloneValues(r.URL.Query())
	a.translateMediaSourceQuery(query)
	path := "/Users/" + resolved.Client.clientUserID() + "/PlayingItems/" + resolved.OriginalID
	_ = a.forwardNoContent(r, resolved.Client, method, path, query, nil)
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
	resolved := a.resolveRouteID(virtualItemID)
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
			if err := a.WatchStore.MarkPlayedAt(reqCtx.ProxyUser.UserID, virtualItemID, played, playedAt); err != nil {
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
	resolved := a.resolveRouteID(virtualItemID)
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
			// A UserData payload may carry Played=false together with a non-zero
			// PlaybackPositionTicks. Apply the unplayed transition first so the explicit
			// position that follows is preserved as the new in-progress state. Played=true
			// stays last because a completed item must finish at position zero.
			if playedValue != nil && !*playedValue {
				if err := a.WatchStore.MarkPlayedAt(reqCtx.ProxyUser.UserID, virtualItemID, false, localPlayedAt); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "Failed to update local watched state"})
					return
				}
			}
			if positionValue != nil {
				if err := a.WatchStore.UpdatePositionAt(reqCtx.ProxyUser.UserID, virtualItemID, *positionValue, runtimeValue, localPlayedAt); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "Failed to update local playback position"})
					return
				}
			}
			if favoriteValue != nil {
				if err := a.WatchStore.SetFavorite(reqCtx.ProxyUser.UserID, virtualItemID, *favoriteValue); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "Failed to update local favorite state"})
					return
				}
			}
			if playedValue != nil && *playedValue {
				if err := a.WatchStore.MarkPlayedAt(reqCtx.ProxyUser.UserID, virtualItemID, true, localPlayedAt); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "Failed to update local watched state"})
					return
				}
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
	resolved := a.resolveRouteID(virtualItemID)
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
			if err := a.WatchStore.SetFavorite(reqCtx.ProxyUser.UserID, virtualItemID, true); err != nil {
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
	resolved := a.resolveRouteID(virtualItemID)
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
			if err := a.WatchStore.SetFavorite(reqCtx.ProxyUser.UserID, virtualItemID, false); err != nil {
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
	if a.WatchStore == nil || !isRegularProxyUser(r) || len(items) == 0 {
		return
	}
	reqCtx := requestContextFrom(r.Context())
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if id, _ := item["Id"].(string); id != "" {
			ids = append(ids, id)
		}
	}
	rows := a.WatchStore.GetProgressBatch(reqCtx.ProxyUser.UserID, ids)
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
	}
	if isUserDataMap(item) {
		applyUserDataState(item, row)
	}
}

func (a *App) overlayLocalUserData(r *http.Request, virtualItemID string, payload any) {
	a.normalizeLocalUserDataForItem(r, virtualItemID, payload)
}
