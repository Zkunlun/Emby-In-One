package backend

import (
	"net/http"
	"strings"
)

// Generic media fallback responses keep their existing source/query semantics,
// while item-shaped arrays use the same independent version projection.
func (a *App) rewriteVersionAwarePayload(r *http.Request, value any, server string) any {
	switch typed := value.(type) {
	case []any:
		media := len(typed) > 0
		for _, raw := range typed {
			item, ok := raw.(map[string]any)
			if !ok || !mergeHTTPItemShape(item) {
				media = false
				break
			}
		}
		if media {
			return toAnySlice(a.rewriteMergeResponseItems(r, asItems(typed), server, false))
		}
		for i, child := range typed {
			typed[i] = a.rewriteVersionAwarePayload(r, child, server)
		}
		return typed
	case map[string]any:
		if mergeHTTPItemShape(typed) {
			original, _ := typed["Id"].(string)
			for _, part := range strings.Split(r.URL.Path, "/") {
				mapped := a.IDStore.ResolveVirtualID(part)
				if mapped != nil && resolvedOriginalIDForServer(mapped, server) == original {
					return a.projectMergeItem(typed, server, part, a.clientFacingUserIDFor(r))
				}
			}
			rows := a.rewriteMergeResponseItems(r, []map[string]any{typed}, server, false)
			if len(rows) > 0 {
				return rows[0]
			}
			return map[string]any{}
		}
		// Rewrite scalar fields once; children are then handled with item context.
		cfg := a.ConfigStore.Snapshot()
		for key, child := range typed {
			switch child.(type) {
			case map[string]any, []any:
				if key == "UserData" || key == "ImageTags" || key == "BackdropImageTags" || key == "ParentBackdropImageTags" || key == "ImageBlurHashes" {
					wrapper := map[string]any{key: child}
					rewriteResponseIDs(wrapper, server, a.IDStore, cfg.Server.ID, a.clientFacingUserIDFor(r))
					typed[key] = wrapper[key]
				} else {
					typed[key] = a.rewriteVersionAwarePayload(r, child, server)
				}
			default:
				wrapper := map[string]any{key: child}
				rewriteResponseIDs(wrapper, server, a.IDStore, cfg.Server.ID, a.clientFacingUserIDFor(r))
				typed[key] = wrapper[key]
			}
		}
		return typed
	default:
		return value
	}
}
func mergeHTTPItemShape(item map[string]any) bool {
	id, _ := item["Id"].(string)
	kind, _ := item["Type"].(string)
	return id != "" && (kind == "Movie" || kind == "Episode" || kind == "Series" || kind == "Season")
}

func (a *App) projectNextEpisode(r *http.Request, raw map[string]any, server string, progress *WatchProgress) []map[string]any {
	if progress != nil && !progress.Played && progress.PositionTicks > 0 {
		candidate := newMergeCandidate(server, raw, nil, true)
		if candidate.Season.valid() && candidate.Episode.valid() && candidate.Season.Ticks == int64(progress.ParentIndexNumber) && candidate.Episode.Ticks == int64(progress.IndexNumber) {
			group := a.IDStore.ResolveMergeGroup(progress.VirtualItemID)
			if group == nil {
				return nil
			}
			found := false
			for _, instance := range mergeGroupInstances(group) {
				if instance.ServerID == server && instance.OriginalID == candidate.ItemID {
					found = true
				}
			}
			if !found {
				return nil
			}
			item := a.projectMergeItem(raw, server, a.IDStore.CanonicalMergeID(progress.VirtualItemID), a.clientFacingUserIDFor(r))
			if item == nil {
				return nil
			}
			return []map[string]any{item}
		}
	}
	return a.rewriteMergeResponseItems(r, []map[string]any{raw}, server, true)
}
