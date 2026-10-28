package backend

import (
	"net/http"
	"net/url"
)

// A season's local unplayed count needs the complete *visible* episode set.
// WatchStore alone only knows items the user has interacted with. If any
// upstream response is unavailable or incomplete, leave the field absent
// instead of copying the shared upstream account's value or guessing zero.
func (a *App) overlayLocalSeasonUnplayedCounts(r *http.Request, resolved *routeResolution, seasons []map[string]any) {
	if a.WatchStore == nil || !isRegularProxyUser(r) || resolved == nil || len(seasons) == 0 {
		return
	}
	reqCtx := requestContextFrom(r.Context())
	instances := a.collectAllowedInstances(reqCtx, resolved)
	if len(instances) == 0 {
		return
	}

	results := make([]upstreamItemsResult, 0, len(instances))
	for _, inst := range instances {
		if !a.isServerAllowed(reqCtx, inst.ServerID) {
			return
		}
		query := url.Values{
			"UserId":     {inst.Client.clientUserID()},
			"StartIndex": {"0"},
			"Limit":      {"5000"},
		}
		full := requestMergeFields(query)
		payload, err := inst.Client.RequestJSON(r.Context(), reqCtx, a.Identity, http.MethodGet,
			"/Shows/"+inst.OriginalID+"/Episodes", query, nil)
		if err != nil || !a.isServerAllowed(reqCtx, inst.ServerID) {
			return
		}
		page, ok := payload.(map[string]any)
		if !ok {
			return
		}
		episodes := asItems(payload)
		total, valid := numericInt(page["TotalRecordCount"])
		if !valid || total != len(episodes) || total >= mergedItemsScanLimit {
			return
		}
		for _, episode := range episodes {
			if id, _ := episode["Id"].(string); id == "" {
				return
			}
			if itemType, _ := episode["Type"].(string); itemType == "" {
				episode["Type"] = "Episode"
			} else if itemType != "Episode" {
				return
			}
			if _, ok := episode["SeriesId"]; !ok {
				episode["SeriesId"] = inst.OriginalID
			}
			if id, _ := episode["SeasonId"].(string); id == "" {
				return
			}
		}
		results = append(results, upstreamItemsResult{
			ServerID: inst.ServerID, Items: episodes, FullSources: full, RequestScope: reqCtx,
		})
	}

	results = a.hydrateMergeResults(r, results)
	episodes := a.mergeRoundRobinItems(results, a.clientFacingUserIDFor(r), reqCtx)
	mergedIDs := make(map[string]bool, len(episodes))
	for _, episode := range episodes {
		if id, _ := episode["Id"].(string); id != "" {
			mergedIDs[a.IDStore.CanonicalMergeID(id)] = true
		}
	}
	// A failed merge projection must not silently reduce the episode total.
	for _, result := range results {
		for _, episode := range result.Items {
			rawID, _ := episode["Id"].(string)
			mapped := a.IDStore.ResolveMergeMember(result.ServerID, rawID, "")
			if mapped == "" || !mergedIDs[a.IDStore.CanonicalMergeID(mapped)] {
				return
			}
		}
	}
	ids := make([]string, 0, len(episodes))
	seasonCounts := make(map[string]int, len(seasons))
	for _, season := range seasons {
		if id, _ := season["Id"].(string); id != "" {
			seasonCounts[a.IDStore.CanonicalMergeID(id)] = 0
		}
	}
	for _, episode := range episodes {
		seasonID, _ := episode["SeasonId"].(string)
		episodeID, _ := episode["Id"].(string)
		if seasonID == "" || episodeID == "" {
			return
		}
		canonicalSeason := a.IDStore.CanonicalMergeID(seasonID)
		if _, found := seasonCounts[canonicalSeason]; !found {
			// An episode that cannot be assigned to a visible season makes
			// the local aggregate untrustworthy; do not output a partial count.
			return
		}
		seasonCounts[canonicalSeason]++
		ids = append(ids, episodeID)
	}

	progress, err := a.visibleWatchProgressBatch(r, ids)
	if err != nil {
		a.logVisibleWatchReadError(err)
		return
	}
	for _, episode := range episodes {
		episodeID, _ := episode["Id"].(string)
		seasonID, _ := episode["SeasonId"].(string)
		if progress[episodeID].Played {
			seasonCounts[a.IDStore.CanonicalMergeID(seasonID)]--
		}
	}
	for _, season := range seasons {
		id, _ := season["Id"].(string)
		if id == "" {
			continue
		}
		userData, ok := season["UserData"].(map[string]any)
		if !ok {
			userData = map[string]any{"ItemId": id}
			applyUserDataState(userData, nil)
			season["UserData"] = userData
		}
		userData["UnplayedItemCount"] = seasonCounts[a.IDStore.CanonicalMergeID(id)]
	}
}
