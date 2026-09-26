package backend

import (
	"net/http"
	"strings"
	"time"
)

// normalizeLocalUserDataPayload replaces shared-upstream user state with the
// current regular proxy user's local state anywhere a JSON response carries
// BaseItem/UserData objects. Administrators intentionally keep upstream state.
func (a *App) normalizeLocalUserDataPayload(r *http.Request, payload any) {
	if a.WatchStore == nil || !isRegularProxyUser(r) || payload == nil {
		return
	}
	reqCtx := requestContextFrom(r.Context())
	ids := make([]string, 0)
	seen := map[string]struct{}{}
	collectUserStateItemIDs(payload, &ids, seen)
	rows := a.WatchStore.GetProgressBatch(reqCtx.ProxyUser.UserID, ids)
	applyLocalUserStateRecursive(payload, rows)
}

func collectUserStateItemIDs(value any, ids *[]string, seen map[string]struct{}) {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			collectUserStateItemIDs(child, ids, seen)
		}
	case map[string]any:
		if ud, ok := typed["UserData"].(map[string]any); ok {
			id, _ := typed["Id"].(string)
			if itemID, _ := ud["ItemId"].(string); itemID != "" {
				id = itemID
			}
			appendUniqueStateID(id, ids, seen)
		} else if isBaseItemStateCandidate(typed) {
			// Some upstream endpoints omit UserData entirely when they were queried
			// without a user context. Still collect item-shaped IDs so an existing local
			// state can be materialized instead of disappearing from the response.
			id, _ := typed["Id"].(string)
			appendUniqueStateID(id, ids, seen)
		}
		if isUserDataMap(typed) {
			id, _ := typed["ItemId"].(string)
			appendUniqueStateID(id, ids, seen)
		}
		for key, child := range typed {
			if key == "UserData" {
				continue
			}
			collectUserStateItemIDs(child, ids, seen)
		}
	}
}

func appendUniqueStateID(id string, ids *[]string, seen map[string]struct{}) {
	if id == "" {
		return
	}
	if _, ok := seen[id]; ok {
		return
	}
	seen[id] = struct{}{}
	*ids = append(*ids, id)
}

func applyLocalUserStateRecursive(value any, rows map[string]WatchProgress) {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			applyLocalUserStateRecursive(child, rows)
		}
	case map[string]any:
		if ud, ok := typed["UserData"].(map[string]any); ok {
			id, _ := typed["Id"].(string)
			if itemID, _ := ud["ItemId"].(string); itemID != "" {
				id = itemID
			}
			applyUserDataState(ud, progressPtr(rows, id))
		} else if isBaseItemStateCandidate(typed) {
			id, _ := typed["Id"].(string)
			if row, ok := rows[id]; ok {
				ud := map[string]any{"ItemId": id}
				applyUserDataState(ud, &row)
				typed["UserData"] = ud
			}
		}
		if isUserDataMap(typed) {
			id, _ := typed["ItemId"].(string)
			applyUserDataState(typed, progressPtr(rows, id))
		}
		for key, child := range typed {
			if key == "UserData" {
				continue
			}
			applyLocalUserStateRecursive(child, rows)
		}
	}
}

func progressPtr(rows map[string]WatchProgress, id string) *WatchProgress {
	row, ok := rows[id]
	if !ok {
		return nil
	}
	copy := row
	return &copy
}

func isUserDataMap(m map[string]any) bool {
	if m == nil {
		return false
	}
	for _, key := range []string{"PlaybackPositionTicks", "Played", "IsFavorite", "PlayedPercentage", "LastPlayedDate", "PlayCount", "UnplayedItemCount", "Rating"} {
		if _, ok := m[key]; ok {
			return true
		}
	}
	return false
}

// isBaseItemStateCandidate deliberately requires both Id and Type before creating
// a missing UserData block. Generic fallback JSON often contains unrelated objects
// with an Id field; Type keeps the normalizer from manufacturing user state on them.
func isBaseItemStateCandidate(m map[string]any) bool {
	if m == nil {
		return false
	}
	id, _ := m["Id"].(string)
	itemType, _ := m["Type"].(string)
	return id != "" && itemType != ""
}

// normalizeLocalUserDataForItem is the single-item variant used by existing
// explicit handlers. It deliberately does not rewrite IDs; callers preserve
// their current response ordering and ID semantics.
func (a *App) normalizeLocalUserDataForItem(r *http.Request, virtualItemID string, payload any) {
	if a.WatchStore == nil || !isRegularProxyUser(r) || virtualItemID == "" || payload == nil {
		return
	}
	reqCtx := requestContextFrom(r.Context())
	row := a.WatchStore.GetProgress(reqCtx.ProxyUser.UserID, virtualItemID)
	normalizeExplicitUserState(payload, row)
}

func normalizeExplicitUserState(payload any, row *WatchProgress) {
	m, ok := payload.(map[string]any)
	if !ok {
		return
	}
	if isUserDataMap(m) {
		applyUserDataState(m, row)
	}
	if ud, ok := m["UserData"].(map[string]any); ok {
		applyUserDataState(ud, row)
	}
}

func applyUserDataState(ud map[string]any, progress *WatchProgress) {
	if ud == nil {
		return
	}
	// These fields are personal state too, but EIO does not yet maintain an
	// independent value for them. Removing them is safer than leaking the shared
	// upstream account's value into a regular user's response.
	delete(ud, "Rating")
	delete(ud, "PlayCount")
	delete(ud, "UnplayedItemCount")

	if progress == nil {
		ud["PlaybackPositionTicks"] = int64(0)
		ud["Played"] = false
		ud["IsFavorite"] = false
		ud["PlayedPercentage"] = float64(0)
		delete(ud, "LastPlayedDate")
		return
	}

	ud["PlaybackPositionTicks"] = progress.PositionTicks
	ud["Played"] = progress.Played
	ud["IsFavorite"] = progress.IsFavorite
	ud["PlayedPercentage"] = localPlayedPercentage(progress)
	if progress.LastPlayed > 0 {
		ud["LastPlayedDate"] = time.UnixMilli(progress.LastPlayed).UTC().Format(time.RFC3339Nano)
	} else {
		delete(ud, "LastPlayedDate")
	}
}

func localPlayedPercentage(progress *WatchProgress) float64 {
	if progress == nil {
		return 0
	}
	if progress.Played {
		return 100
	}
	if progress.RuntimeTicks <= 0 || progress.PositionTicks <= 0 {
		return 0
	}
	percentage := float64(progress.PositionTicks) * 100 / float64(progress.RuntimeTicks)
	if percentage < 0 {
		return 0
	}
	if percentage > 100 {
		return 100
	}
	return percentage
}

// ensureWatchRecordMetadata prevents explicit Played/Favorite/UserData writes
// from creating unroutable skeleton rows. The route identity is always stored;
// richer metadata is fetched only when the local row has not been enriched yet.
func (a *App) ensureWatchRecordMetadata(r *http.Request, reqCtx *RequestContext, virtualItemID string, resolved *routeResolution) error {
	if a.WatchStore == nil || reqCtx == nil || reqCtx.ProxyUser == nil || reqCtx.ProxyUser.Role == "admin" || virtualItemID == "" || resolved == nil {
		return nil
	}
	seed := &WatchProgress{
		ProxyUserID:    reqCtx.ProxyUser.UserID,
		VirtualItemID:  virtualItemID,
		ServerID:       resolved.ServerID,
		OriginalItemID: resolved.OriginalID,
	}
	existing := a.WatchStore.GetProgress(reqCtx.ProxyUser.UserID, virtualItemID)
	if existing == nil || existing.ItemType == "" || (existing.ItemType == "Episode" && existing.SeriesVirtualID == "") {
		a.enrichWatchProgressMetadata(r, reqCtx, seed, resolved.OriginalID, resolved.ServerID)
	}
	return a.WatchStore.UpsertMetadata(seed)
}

// parseLocalPlayedAt accepts both Emby's compact DatePlayed form and the
// RFC3339 form used by observed clients. Callers forward the original text
// upstream unchanged; this parser is only for the local timestamp.
func parseLocalPlayedAt(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.UnixMilli()
		}
	}
	if parsed, err := time.ParseInLocation("20060102150405", value, time.UTC); err == nil {
		return parsed.UnixMilli()
	}
	return 0
}
