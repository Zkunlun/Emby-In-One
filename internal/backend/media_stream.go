package backend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// streamRoute describes one media stream endpoint: the upstream path prefix
// ("/Videos" or "/Audio") and the label used in log lines.
type streamRoute struct {
	pathPrefix string
	label      string
}

var (
	videoStreamRoute = streamRoute{pathPrefix: "/Videos", label: "Stream"}
	audioStreamRoute = streamRoute{pathPrefix: "/Audio", label: "Audio stream"}
)

func (a *App) handleVideoProxy(w http.ResponseWriter, r *http.Request) {
	a.proxyStream(w, r, videoStreamRoute)
}

func (a *App) handleAudioProxy(w http.ResponseWriter, r *http.Request) {
	a.proxyStream(w, r, audioStreamRoute)
}

// proxyStream forwards a /Videos/{id}/... or /Audio/{id}/... request to the upstream
// that owns the item, keeping virtual IDs and the proxy token out of upstream URLs.
func (a *App) proxyStream(w http.ResponseWriter, r *http.Request, route streamRoute) {
	virtualItemID := r.PathValue("itemId")
	query := cloneValues(r.URL.Query())
	if a.Logger != nil {
		// The client's query may carry its own token; log only the safe shape.
		a.Logger.Debugf("%s request: itemId=%s, query=%s", route.label, virtualItemID, formatValuesForLog(query))
	}

	resolved, routeOK := a.resolveRequestRouteID(w, r, virtualItemID)
	if !routeOK {
		return
	}
	if resolved == nil {
		if a.Logger != nil {
			a.Logger.Warnf("%s: itemId=%s not found in mappings", route.label, virtualItemID)
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Item not found"})
		return
	}
	if !a.requireServerAccess(w, r, resolved) {
		return
	}

	rest := r.PathValue("rest")
	if rest == "" {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Stream not found"})
		return
	}
	// Subtitle/attachment path source IDs are explicit version selections too.
	pathSourceID := ""
	pathTail := ""
	if slash := strings.IndexByte(rest, '/'); slash > 0 {
		tail := rest[slash:]
		if strings.HasPrefix(tail, "/Subtitles/") || strings.HasPrefix(tail, "/Attachments/") {
			pathSourceID, pathTail = rest[:slash], tail
			querySourceID, err := explicitMediaSourceID(query, nil)
			if err != nil || (querySourceID != "" && querySourceID != pathSourceID) {
				writeMediaSelectionError(w, errMediaMappingMissing)
				return
			}
			query.Set("MediaSourceId", pathSourceID)
		}
	}
	if selected, err := explicitMediaSourceID(query, nil); err == nil && selected == "" {
		if err := a.prepareMergePlaybackItem(r, virtualItemID, resolved); err != nil {
			writeMediaSelectionError(w, err)
			return
		}
		if source := a.defaultMergeSource(virtualItemID, resolved.ServerID, resolved.OriginalID); source != "" {
			virtual, err := a.IDStore.GetMergeMediaSourceID(resolved.ServerID, resolved.OriginalID, source)
			if err != nil {
				writeMediaSelectionError(w, err)
				return
			}
			query.Set("MediaSourceId", virtual)
		}
	}
	client, originalID, ok := a.resolveStreamTarget(w, r, resolved, virtualItemID, query)
	if !ok {
		return
	}
	if pathSourceID != "" {
		rest = query.Get("MediaSourceId") + pathTail
	}
	if !a.isServerAllowed(requestContextFrom(r.Context()), client.ID) {
		writeMediaSelectionError(w, errMediaAccessDenied)
		return
	}

	upstreamPath := route.pathPrefix + "/" + originalID + "/" + rest
	reqCtx := requestContextFrom(r.Context())
	if a.Logger != nil {
		a.Logger.Infof("%s: %s/%s/%s -> [%s] %s",
			route.label, route.pathPrefix, virtualItemID, rest, client.Name, upstreamPath)
	}

	// Redirect mode: hand the client a direct upstream stream URL. Unknown/alive
	// lines keep the zero-wait fast path. Only an all-dead set enters request-scoped
	// recovery; cooldown-expired dead lines are probed concurrently without
	// credentials, and a line must be explicitly revived before it can receive a 302.
	if a.streamPlaybackMode(client) == "redirect" {
		playback, recovery := client.streamPlaybackAndRecoveryCandidates(time.Now())
		if len(playback) == 0 {
			if len(recovery) == 0 {
				if a.Logger != nil {
					a.Logger.Warnf("%s redirect unavailable: all stream lines are dead and cooling down", route.label)
				}
				writeJSON(w, http.StatusBadGateway, map[string]any{"message": "No available upstream stream line"})
				return
			}
			client.probeSelectedStreamBases(r.Context(), recovery, client.redirectRecoveryProbeTimeout())
			if r.Context().Err() != nil {
				return
			}
			if len(client.streamBaseCandidates()) == 0 {
				if a.Logger != nil {
					a.Logger.Warnf("%s redirect recovery failed: no stream line recovered", route.label)
				}
				writeJSON(w, http.StatusBadGateway, map[string]any{"message": "No available upstream stream line"})
				return
			}
		}

		if !a.isServerAllowed(reqCtx, client.ID) {
			writeMediaSelectionError(w, errMediaAccessDenied)
			return
		}
		redirectURL, err := client.BuildURL(upstreamPath, query, true, reqCtx)
		if err != nil {
			if !writePreparationError(w, err) {
				writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Failed to prepare upstream stream URL"})
			}
			return
		}
		if a.Logger != nil {
			a.Logger.Debugf("%s redirect: %s/%s/%s -> 302 %s", route.label, route.pathPrefix, virtualItemID, rest, formatOutboundURLForLog(redirectURL))
		}
		http.Redirect(w, r, redirectURL, http.StatusFound)
		return
	}

	a.forwardStream(w, r, client, upstreamPath, query, rest, virtualItemID)
}

// forwardStream performs the upstream request and copies the response back,
// rewriting HLS manifests so their URLs keep pointing at this proxy.
func (a *App) forwardStream(w http.ResponseWriter, r *http.Request, client *UpstreamClient, upstreamPath string, query url.Values, rest, virtualItemID string) {
	reqCtx := requestContextFrom(r.Context())
	if a.Logger != nil {
		a.Logger.Debugf("Stream request headers: Range=%q, Accept=%q, AE=%q",
			r.Header.Get("Range"), r.Header.Get("Accept"), r.Header.Get("Accept-Encoding"))
	}

	resp, err := client.Stream(r.Context(), reqCtx, a.Identity, upstreamPath, query, streamRequestHeaders(r))
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return // client disconnected or timed out — not a server error
		}
		if a.Logger != nil {
			a.Logger.Errorf("Stream error: itemId=%s upstream=%s: %s", virtualItemID, upstreamPath, redactURLInError(err))
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": err.Error()})
		return
	}
	defer resp.Body.Close()

	if !a.isServerAllowed(reqCtx, client.ID) {
		writeMediaSelectionError(w, errMediaAccessDenied)
		return
	}

	contentType := resp.Header.Get("Content-Type")
	if isPlaylistResponse(contentType, rest) {
		body, _ := io.ReadAll(resp.Body)
		proxyToken := ""
		if reqCtx != nil {
			proxyToken = reqCtx.ProxyToken
		}
		// The manifest's own response URL is the prepared one already sent, so the
		// base URL needs no second authentication read. A test-constructed response
		// has no Request, and falls back to preparing the URL once more.
		baseURL := ""
		if resp.Request != nil && resp.Request.URL != nil {
			baseURL = resp.Request.URL.String()
		} else {
			prepared, err := client.BuildURL(upstreamPath, query, true, reqCtx)
			if err != nil {
				if !writePreparationError(w, err) {
					writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Failed to prepare upstream stream URL"})
				}
				return
			}
			baseURL = prepared
		}
		manifest := RewriteM3U8ForItem(string(body), baseURL, virtualItemID, proxyToken)
		if source := query.Get("MediaSourceId"); source != "" {
			business, ok := streamBusinessPath(upstreamPath)
			parts := strings.Split(strings.TrimPrefix(business, "/"), "/")
			if !ok || len(parts) < 3 {
				writeMediaSelectionError(w, errMediaMappingMissing)
				return
			}
			qualified, err := a.IDStore.GetMergeMediaSourceID(client.ID, parts[1], source)
			if err != nil {
				writeMediaSelectionError(w, err)
				return
			}
			manifest = qualifyMergeManifestSources(manifest, virtualItemID, qualified)
		}
		w.Header().Set("Content-Type", "application/x-mpegURL")
		_, _ = io.WriteString(w, manifest)
		return
	}

	if a.Logger != nil {
		a.Logger.Infof("Stream upstream response: Status=%d, Type=%q, Len=%s, Encoding=%q, Range=%q",
			resp.StatusCode, contentType, resp.Header.Get("Content-Length"),
			resp.Header.Get("Content-Encoding"), resp.Header.Get("Content-Range"))
	}
	copyStreamResponseHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// resolveStreamTarget binds an explicit version to its authorized source/item.
// Unknown or revoked versions never fall back to another copy of the film.
func (a *App) resolveStreamTarget(w http.ResponseWriter, r *http.Request, resolved *routeResolution, virtualItemID string, query url.Values) (*UpstreamClient, string, bool) {
	sourceID, err := explicitMediaSourceID(query, nil)
	if err != nil {
		writeMediaSelectionError(w, err)
		return nil, "", false
	}
	if sourceID == "" {
		if !a.translateStreamSession(w, r, query, resolved.ServerID) {
			return nil, "", false
		}
		return resolved.Client, resolved.OriginalID, true
	}
	selection, err := a.selectAuthorizedMediaSource(r, virtualItemID, sourceID, resolved)
	if err != nil {
		writeMediaSelectionError(w, err)
		return nil, "", false
	}
	reqCtx := requestContextFrom(r.Context())
	routeOwner := playbackRouteOwner(reqCtx)
	previousServerID := resolved.ServerID
	previousPlaySessionID := query.Get("PlaySessionId")
	if mapped := a.IDStore.ResolveVirtualID(previousPlaySessionID); mapped != nil {
		previousPlaySessionID = mapped.OriginalID
	}
	if active, ok := a.playbackRoutes.Active(routeOwner, virtualItemID); ok {
		previousServerID = active.ServerID
		if active.PlaySessionID != "" {
			previousPlaySessionID = active.PlaySessionID
		}
	}
	targetPlaySessionID := ""
	clientPlaySessionID := ""
	if candidate, ok := a.playbackRoutes.MediaSource(routeOwner, selection.VirtualID); ok &&
		candidate.ServerID == selection.ServerID && candidate.ItemID == virtualItemID {
		targetPlaySessionID = candidate.PlaySessionID
		clientPlaySessionID = candidate.ClientPlaySessionID
	}
	setSelectedMediaSource(query, nil, selection)
	if targetPlaySessionID != "" {
		query.Set("PlaySessionId", targetPlaySessionID)
	} else if !a.translateStreamSession(w, r, query, selection.ServerID) {
		return nil, "", false
	}
	if previousServerID != selection.ServerID {
		if !a.switchPlaybackLease(w, r, selection.ServerID, previousServerID, virtualItemID,
			targetPlaySessionID, previousPlaySessionID, clientPlaySessionID) {
			return nil, "", false
		}
	} else {
		a.activatePlaybackRoute(reqCtx, virtualItemID, selection.ServerID, targetPlaySessionID, clientPlaySessionID)
	}
	return selection.Client, selection.ItemOriginalID, true
}

// A known upstream session cannot be sent to a different source. Raw session
// compatibility remains source-local; owner/item proof belongs to Phase3.
func (a *App) translateStreamSession(w http.ResponseWriter, r *http.Request, query url.Values, serverID string) bool {
	sessionID := query.Get("PlaySessionId")
	if mapped := a.IDStore.ResolveVirtualID(sessionID); mapped != nil {
		if mapped.ServerID != serverID || !a.isServerAllowed(requestContextFrom(r.Context()), mapped.ServerID) {
			writeMediaSelectionError(w, errMediaAccessDenied)
			return false
		}
		query.Set("PlaySessionId", mapped.OriginalID)
	}
	return true
}

// switchPlaybackLease moves the active route only after the target lease has been
// accepted. The target keeps its own upstream PlaySessionID; the previous lease is
// released only by the same resolved device and the previous exact session.
func (a *App) switchPlaybackLease(w http.ResponseWriter, r *http.Request, targetID, previousID, virtualItemID, targetPlaySessionID, previousPlaySessionID, clientPlaySessionID string) bool {
	reqCtx := requestContextFrom(r.Context())
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	if !a.mediaAccessScopeLocked(reqCtx).allows(targetID) {
		writeMediaSelectionError(w, errMediaAccessDenied)
		return false
	}
	routeOwner := playbackRouteOwner(reqCtx)
	limiterUserID, applies := a.playbackLimiterKey(reqCtx, targetID)
	if applies {
		deviceID := playbackDeviceID(reqCtx)
		if deviceID == "" {
			writePlaybackDeviceIDRequired(w)
			return false
		}
		if !a.PlaybackLimiter.Reserve(limiterUserID, targetID, deviceID, virtualItemID, targetPlaySessionID).Allowed {
			writePlaybackDeviceLimit(w)
			return false
		}
		if previousID != "" && previousID != targetID {
			a.PlaybackLimiter.Stop(limiterUserID, previousID, deviceID, previousPlaySessionID)
		}
	}
	a.playbackRoutes.RegisterOwner(routeOwner, reqCtx.ProxyUser.UserID)
	a.playbackRoutes.Activate(routeOwner, virtualItemID, targetID, targetPlaySessionID, clientPlaySessionID)
	a.IDStore.SetActiveStream(virtualItemID, targetID)
	return true
}

// streamPlaybackMode returns the effective playback mode for an upstream, falling
// back to the global setting.
func (a *App) streamPlaybackMode(client *UpstreamClient) string {
	if mode := client.Config.PlaybackMode; mode != "" {
		return mode
	}
	return a.ConfigStore.Snapshot().Playback.Mode
}

// resolvePlaySessionID rewrites a virtual PlaySessionId in place and reports the
// upstream server that owns it.
func (a *App) resolvePlaySessionID(query url.Values) (string, bool) {
	playSessionID := query.Get("PlaySessionId")
	if playSessionID == "" {
		return "", false
	}
	resolved := a.IDStore.ResolveVirtualID(playSessionID)
	if resolved == nil {
		return "", false
	}
	query.Set("PlaySessionId", resolved.OriginalID)
	return resolved.ServerID, true
}

// streamRequestHeaders forwards the client headers needed for seeking and
// partial content.
func streamRequestHeaders(r *http.Request) http.Header {
	headers := http.Header{}
	for _, name := range []string{"Range", "Accept", "Accept-Encoding", "Accept-Language"} {
		if value := r.Header.Get(name); value != "" {
			headers.Set(name, value)
		}
	}
	return headers
}

func isPlaylistResponse(contentType, rest string) bool {
	return strings.Contains(contentType, "mpegurl") || strings.HasSuffix(strings.ToLower(rest), ".m3u8")
}

// streamResponseHeaders are the upstream headers worth passing through. Chunked
// transfer encoding is skipped so the proxy sets its own framing.
var streamResponseHeaders = []string{
	"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges",
	"Cache-Control", "ETag", "Last-Modified", "Transfer-Encoding",
	"Content-Disposition", "Content-Encoding", "Date", "Server",
}

func copyStreamResponseHeaders(w http.ResponseWriter, resp *http.Response) {
	for _, name := range streamResponseHeaders {
		value := resp.Header.Get(name)
		if value == "" {
			continue
		}
		if strings.EqualFold(name, "Transfer-Encoding") && strings.Contains(strings.ToLower(value), "chunked") {
			continue
		}
		w.Header().Set(name, value)
	}
}

func (a *App) handleDeleteActiveEncodings(w http.ResponseWriter, r *http.Request) {
	query := cloneValues(r.URL.Query())
	serverID, found := a.resolvePlaySessionID(query)
	if !found {
		for _, client := range a.allowedClients(requestContextFrom(r.Context())) {
			_ = a.forwardNoContent(r, client, http.MethodDelete, "/Videos/ActiveEncodings", query, nil)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !a.isServerAllowed(requestContextFrom(r.Context()), serverID) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if client := a.Upstream.ClientByID(serverID); client != nil && client.IsOnline() {
		_ = a.forwardNoContent(r, client, http.MethodDelete, "/Videos/ActiveEncodings", query, nil)
	}
	w.WriteHeader(http.StatusNoContent)
}
