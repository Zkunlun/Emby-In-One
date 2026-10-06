package backend

import (
	"net/http"
	"net/url"
	"strings"
)

func (a *App) handleUserPlayingItemProgress(w http.ResponseWriter, r *http.Request) {
	kind := playbackWatchProgress
	a.handleFallbackProxyWithWatchEvent(w, r, &kind)
}

func (a *App) handleUserPlayingItemStopCompat(w http.ResponseWriter, r *http.Request) {
	kind := playbackWatchStopped
	a.handleFallbackProxyWithWatchEvent(w, r, &kind)
}

func legacyPlaySessionID(query url.Values) (string, bool) {
	selected := ""
	for key, values := range query {
		if !strings.EqualFold(key, "PlaySessionId") {
			continue
		}
		if len(values) != 1 {
			return "", false
		}
		if values[0] == "" {
			continue
		}
		if selected != "" && selected != values[0] {
			return "", false
		}
		selected = values[0]
	}
	return selected, true
}

// Reuse the same exact session-alias translation as modern check-ins. The
// returned item, version and session all belong to one selected upstream.
func (a *App) resolveLegacyWatchTarget(r *http.Request) (*routeResolution, url.Values, bool) {
	query := cloneValues(r.URL.Query())
	mediaID, err := explicitMediaSourceID(query, nil)
	sessionID, ok := legacyPlaySessionID(query)
	if err != nil || !ok {
		return nil, query, false
	}
	body := map[string]any{"ItemId": r.PathValue("itemId"), "MediaSourceId": mediaID, "PlaySessionId": sessionID}
	serverID, found := a.translateSessionBodyIDs(requestContextFrom(r.Context()), body)
	if !found {
		return nil, query, false
	}
	client := a.Upstream.ClientByID(serverID)
	if client == nil {
		return nil, query, false
	}
	originalItemID, _ := body["ItemId"].(string)
	for key := range query {
		if strings.EqualFold(key, "MediaSourceId") || strings.EqualFold(key, "PlaySessionId") {
			delete(query, key)
		}
	}
	if mediaID != "" {
		query.Set("MediaSourceId", body["MediaSourceId"].(string))
	}
	if sessionID != "" {
		query.Set("PlaySessionId", body["PlaySessionId"].(string))
	}
	return &routeResolution{OriginalID: originalItemID, ServerID: serverID, Client: client}, query, true
}

func (a *App) legacyWatchEvent(r *http.Request, serverID string, query url.Values, kind playbackWatchEventKind) (string, string, playbackWatchEvent, bool) {
	virtualItemID, originalItemID := a.watchItemIdentity(r.PathValue("itemId"), serverID)
	empty := playbackWatchEvent{}
	mediaID, err := explicitMediaSourceID(query, nil)
	playSessionID, ok := legacyPlaySessionID(query)
	if originalItemID == "" || err != nil || !ok {
		return "", "", empty, false
	}
	if mapped := a.IDStore.ResolveVirtualID(mediaID); mapped != nil {
		if mapped.ServerID != serverID {
			return "", "", empty, false
		}
		mediaID = mapped.OriginalID
	}
	if mapped := a.IDStore.ResolveVirtualID(playSessionID); mapped != nil {
		if mapped.ServerID != serverID {
			return "", "", empty, false
		}
		playSessionID = mapped.OriginalID
	}
	event := playbackWatchEventFromQuery(kind, query)
	event.Source = playbackWatchSource{ServerID: serverID, OriginalItemID: originalItemID, MediaSourceID: mediaID}
	return virtualItemID, playSessionID, event, true
}

func (a *App) admitLegacyWatchRequest(r *http.Request, serverID string, query url.Values, kind playbackWatchEventKind) *http.Request {
	itemID, sessionID, event, ok := a.legacyWatchEvent(r, serverID, query, kind)
	if !ok {
		return a.admitPlaybackWatchRequest(r, "", "", playbackWatchEvent{Kind: kind})
	}
	return a.admitPlaybackWatchRequest(r, itemID, sessionID, event)
}

func (a *App) recordLegacyWatchEvent(r *http.Request, serverID string, query url.Values, kind playbackWatchEventKind) {
	itemID, sessionID, event, ok := a.legacyWatchEvent(r, serverID, query, kind)
	if ok {
		a.recordPlaybackWatchEvent(r, itemID, sessionID, event)
	}
	if kind == playbackWatchStopped {
		// Shared-state rejection does not stop the exact old source lease from
		// ending. A's final event must never release B's current lease.
		a.stopAdmittedPlaybackLease(r, serverID, sessionID)
	}
}
