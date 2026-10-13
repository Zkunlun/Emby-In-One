package backend

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var fallbackVirtualIDPattern = regexp.MustCompile(`(?i)[a-f0-9]{32}`)

func (a *App) handleFallbackProxy(w http.ResponseWriter, r *http.Request) {
	a.handleFallbackProxyWithWatchEvent(w, r, nil)
}

// Only the dedicated legacy lifecycle routes supply a watch event.
func (a *App) handleFallbackProxyWithWatchEvent(w http.ResponseWriter, r *http.Request, watchKind *playbackWatchEventKind) {
	reqCtx := requestContextFrom(r.Context())
	targetClient, rewrittenPath, serverID, query, ambiguous := a.resolveFallbackTarget(r, reqCtx)
	if watchKind != nil {
		// Dedicated legacy lifecycle routes use exact session aliases rather
		// than the generic fallback's independent source/session constraints.
		resolved, sessionQuery, found := a.resolveLegacyWatchTarget(r)
		if found {
			targetClient, serverID, query = resolved.Client, resolved.ServerID, sessionQuery
			segments := strings.Split(r.URL.Path, "/")
			for index, segment := range segments {
				if index > 0 && strings.EqualFold(segments[index-1], "PlayingItems") && segment == r.PathValue("itemId") {
					segments[index] = resolved.OriginalID
				}
			}
			rewrittenPath, ambiguous = strings.Join(segments, "/"), false
		} else {
			targetClient, ambiguous = nil, false
		}
	}
	if targetClient == nil {
		if ambiguous {
			if a.Logger != nil {
				a.Logger.Warnf("Fallback: ambiguous target for %s %s", r.Method, r.URL.Path)
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Ambiguous upstream target"})
			return
		}
		if a.Logger != nil {
			a.Logger.Warnf("Fallback: no upstream available for %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"message": "No upstream servers available"})
		return
	}
	if a.Logger != nil {
		a.Logger.Debugf("Fallback: %s %s → [%s] %s", r.Method, r.URL.Path, targetClient.Name, rewrittenPath)
	}

	// The user segment and the api_key are handled by the shared URL preparation
	// layer in doRequest, which normalizes the current user for supported
	// endpoints and keeps an unclassified value as the client sent it. A blanket
	// string replacement here would also rewrite unrelated text.

	body, err := decodeFallbackBody(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Invalid request body"})
		return
	}
	if watchKind != nil {
		r = a.admitLegacyWatchRequest(r, serverID, query, *watchKind)
		if _, available := a.sessionUpstreamClient(serverID); !available {
			if *watchKind == playbackWatchStopped {
				a.recordLegacyWatchEvent(r, serverID, query, *watchKind)
				w.WriteHeader(http.StatusNoContent)
			} else {
				writeSessionUpstreamUnavailable(w)
			}
			return
		}
	}
	if !a.prepareFallbackMediaSelection(w, r, targetClient, query, body) {
		if watchKind != nil && *watchKind == playbackWatchStopped {
			sessionID, _ := legacyPlaySessionID(query)
			a.stopAdmittedPlaybackLease(r, serverID, sessionID)
		}
		return
	}
	resp, err := a.performUpstreamRequest(r, targetClient, r.Method, rewrittenPath, query, body)
	if watchKind != nil && (*watchKind == playbackWatchStopped ||
		(err == nil && resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices)) {
		a.recordLegacyWatchEvent(r, serverID, query, *watchKind)
	}
	if err != nil {
		if writePreparationError(w, err) {
			return
		}
		if a.Logger != nil {
			a.Logger.Errorf("Fallback error: %s %s - %s", r.Method, r.URL.Path, redactURLInError(err))
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Upstream request failed"})
		return
	}
	defer resp.Body.Close()

	if !a.isServerAllowed(reqCtx, serverID) {
		writeMediaSelectionError(w, errMediaAccessDenied)
		return
	}

	// Content-Length is deliberately not copied here. Every buffered path below
	// re-serializes the payload (JSON is re-encoded after ID rewriting, HTML errors are
	// replaced), so the upstream length no longer describes what we are about to write.
	// net/http honours an explicitly set Content-Length verbatim: a longer body gets
	// silently truncated and a shorter one leaves the client waiting for bytes that
	// never arrive. Each path below is responsible for its own length: the byte-exact
	// passthrough re-copies it, the buffered paths let net/http compute it.
	copySelectedHeaders(w.Header(), resp.Header, []string{"Content-Type", "Content-Range", "Accept-Ranges", "Cache-Control", "ETag", "Last-Modified", "Content-Disposition"})
	contentType := resp.Header.Get("Content-Type")

	// For successful responses with binary (non-text, non-JSON) content types,
	// stream directly to avoid buffering large image/audio/video bodies.
	// Text types and error responses are always buffered for ID rewriting / HTML sanitisation.
	mediaLower := strings.ToLower(contentType)
	if resp.StatusCode < http.StatusBadRequest && contentType != "" &&
		!isJSONContentType(contentType) && !strings.HasPrefix(mediaLower, "text/") {
		// io.Copy forwards the upstream bytes untouched, so the upstream length still
		// applies and can be passed through for exact framing.
		if contentLength := resp.Header.Get("Content-Length"); contentLength != "" {
			w.Header().Set("Content-Length", contentLength)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Failed to read upstream response"})
		return
	}
	if resp.StatusCode >= http.StatusBadRequest {
		if a.Logger != nil {
			a.Logger.Debugf("Fallback upstream error: %d for %s %s", resp.StatusCode, r.Method, r.URL.Path)
		}
		if looksLikeHTMLDocument(bodyBytes, contentType) {
			w.Header().Del("Content-Length")
			writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Upstream returned HTML error page"})
			return
		}
	}
	if looksLikeJSON(bodyBytes) {
		var payload any
		if err := json.Unmarshal(bodyBytes, &payload); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Invalid upstream JSON"})
			return
		}
		if !a.isServerAllowed(reqCtx, serverID) {
			writeMediaSelectionError(w, errMediaAccessDenied)
			return
		}
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			payload = a.rewriteVersionAwarePayload(r, payload, serverID)
		} else {
			cfg := a.ConfigStore.Snapshot()
			rewriteResponseIDs(payload, serverID, a.IDStore, cfg.Server.ID, a.clientFacingUserIDFor(r))
		}
		a.normalizeLocalUserDataPayload(r, payload)
		writeJSON(w, resp.StatusCode, payload)
		return
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(bodyBytes)
}

func (a *App) resolveFallbackTarget(r *http.Request, reqCtx *RequestContext) (*UpstreamClient, string, string, url.Values, bool) {
	query := cloneValues(r.URL.Query())
	segments := strings.Split(r.URL.Path, "/")
	type constraint struct {
		id             string
		mapping        *ResolvedID
		explicitSource bool
	}
	constraints := make([]constraint, 0)
	add := func(id string, explicitSource bool) bool {
		if id == "" {
			return true
		}
		mapping := a.IDStore.ResolveVirtualID(id)
		if mapping == nil {
			// Unmapped explicit sources cannot fall back to the only online
			// server. Raw source IDs are qualified and validated below.
			if explicitSource && !a.IDStore.ContainsOriginalID(id) {
				return false
			}
			return true
		}
		constraints = append(constraints, constraint{id: id, mapping: mapping, explicitSource: explicitSource})
		return true
	}
	for index, segment := range segments {
		if index > 0 && strings.EqualFold(segments[index-1], "Users") {
			continue // user identity is normalized at the outbound boundary
		}
		if !add(segment, false) {
			return nil, r.URL.Path, "", query, false
		}
		if index > 0 && fallbackResourceSegment(segments[index-1]) &&
			fallbackVirtualIDPattern.MatchString(segment) && a.IDStore.ResolveVirtualID(segment) == nil {
			// A classified unknown resource ID must not be guessed on B.
			return nil, r.URL.Path, "", query, false
		}
	}
	for key, values := range query {
		explicitSource := strings.EqualFold(key, "MediaSourceId") || strings.EqualFold(key, "PlaySessionId") || strings.EqualFold(key, "SessionId")
		_, simple := simpleIDFields[key]
		if !simple && !explicitSource && !isBatchIDQueryKey(key) {
			continue
		}
		if strings.EqualFold(key, "UserId") {
			continue
		}
		for _, value := range values {
			for _, id := range strings.Split(value, ",") {
				if !add(strings.TrimSpace(id), explicitSource) {
					return nil, r.URL.Path, "", query, false
				}
			}
		}
	}
	online := a.allowedClients(reqCtx)
	var target *UpstreamClient
	for _, client := range online {
		matches := true
		for _, required := range constraints {
			if required.explicitSource {
				matches = required.mapping.ServerID == client.ID
			} else {
				matches = resolvedOriginalIDForServer(required.mapping, client.ID) != ""
			}
			if !matches {
				break
			}
		}
		if matches {
			if target == nil {
				target = client
			}
			// With no route evidence, retain the existing ambiguity failure.
			if len(constraints) == 0 && target != client {
				return nil, r.URL.Path, "", query, true
			}
		}
	}
	if target == nil {
		return nil, r.URL.Path, "", query, false
	}
	for index, segment := range segments {
		if index > 0 && strings.EqualFold(segments[index-1], "Users") {
			continue
		}
		if mapping := a.IDStore.ResolveVirtualID(segment); mapping != nil {
			originalID := resolvedOriginalIDForServer(mapping, target.ID)
			if originalID == "" {
				return nil, r.URL.Path, "", query, false
			}
			segments[index] = originalID
		}
	}
	for key, values := range query {
		_, simple := simpleIDFields[key]
		if !simple && !isBatchIDQueryKey(key) && !strings.EqualFold(key, "MediaSourceId") &&
			!strings.EqualFold(key, "PlaySessionId") && !strings.EqualFold(key, "SessionId") {
			continue
		}
		if strings.EqualFold(key, "UserId") {
			continue
		}
		for index, value := range values {
			parts := strings.Split(value, ",")
			for position, id := range parts {
				if mapping := a.IDStore.ResolveVirtualID(strings.TrimSpace(id)); mapping != nil {
					parts[position] = resolvedOriginalIDForServer(mapping, target.ID)
				}
			}
			values[index] = strings.Join(parts, ",")
		}
	}
	return target, strings.Join(segments, "/"), target.ID, query, false
}

func fallbackResourceSegment(segment string) bool {
	switch strings.ToLower(segment) {
	case "items", "videos", "audio", "shows", "playingitems":
		return true
	default:
		return false
	}
}

// decodeFallbackBody reads the client body once. A JSON-declared body is decoded
// into a structure; anything else keeps its original bytes untouched, because
// trimming or re-encoding a non-JSON payload would corrupt binary or signed
// content. The trim is used only to answer "was this body empty?".
func decodeFallbackBody(r *http.Request) (any, error) {
	if r.Body == nil || r.Method == http.MethodGet || r.Method == http.MethodHead {
		return nil, nil
	}
	defer r.Body.Close()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	if !isExplicitJSONContentType(r.Header.Get("Content-Type")) {
		return &rawRequestBody{data: raw, contentType: r.Header.Get("Content-Type")}, nil
	}
	var payload any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (a *App) performUpstreamRequest(r *http.Request, client *UpstreamClient, method, path string, query url.Values, body any) (*http.Response, error) {
	reqCtx := requestContextFrom(r.Context())
	return client.doRequest(r.Context(), reqCtx, method, path, query, body, client.requestHeaders(reqCtx, a.Identity), false)
}

// playbackLimiterKey reports whether the single-device playback lease applies to
// this regular user/server pair. Admins are exempt, and unresolved users or
// upstreams do not participate. Device identity is deliberately not derived here:
// the HTTP boundary resolves DeviceID and passes that explicit value to the limiter.
func (a *App) playbackLimiterKey(reqCtx *RequestContext, serverID string) (userID string, ok bool) {
	if reqCtx == nil || reqCtx.ProxyUser == nil || reqCtx.ProxyUser.Role == "admin" {
		return "", false
	}
	if a.PlaybackLimiter == nil || serverID == "" {
		return "", false
	}
	cfg := a.ConfigStore.Snapshot()
	found := false
	for _, u := range cfg.Upstream {
		if u.ID == serverID {
			found = true
			break
		}
	}
	if !found {
		return "", false
	}
	return reqCtx.ProxyUser.UserID, true
}

func isJSONContentType(contentType string) bool {
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = contentType
	}
	return strings.Contains(strings.ToLower(mediaType), "json")
}

func isExplicitJSONContentType(contentType string) bool {
	return isJSONContentType(contentType)
}

func copySelectedHeaders(dst, src http.Header, keys []string) {
	for _, key := range keys {
		if value := src.Get(key); value != "" {
			dst.Set(key, value)
		}
	}
}

func looksLikeJSON(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false
	}
	return trimmed[0] == '{' || trimmed[0] == '['
}

func looksLikeHTMLDocument(body []byte, contentType string) bool {
	mediaType := strings.ToLower(strings.TrimSpace(contentType))
	if mediaType != "" {
		parsed, _, err := mime.ParseMediaType(mediaType)
		if err == nil {
			mediaType = parsed
		}
		if mediaType == "text/html" || mediaType == "application/xhtml+xml" {
			return true
		}
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false
	}
	leading := strings.ToLower(string(trimmed[:minInt(len(trimmed), 32)]))
	return strings.HasPrefix(leading, "<!doctype") || strings.HasPrefix(leading, "<html")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
