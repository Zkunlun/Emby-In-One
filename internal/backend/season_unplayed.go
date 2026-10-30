package backend

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// A season's local unplayed count needs the complete *visible* episode set.
// WatchStore alone only knows items the user has interacted with. If any
// upstream response is unavailable or incomplete, leave the field absent
// instead of copying the shared upstream account's value or guessing zero.
func (a *App) overlayLocalSeasonUnplayedCounts(r *http.Request, resolved *routeResolution, seasons []map[string]any) {
	if a.WatchStore == nil || !isRegularProxyUser(r) || resolved == nil || len(seasons) == 0 {
		return
	}
	// The unplayed count is optional compatibility metadata. Never delay
	// the real Season list on a slow or unavailable Episode endpoint.
	countCtx, cancel := context.WithTimeout(r.Context(), 2500*time.Millisecond)
	defer cancel()
	reqCtx := requestContextFrom(r.Context())
	instances := a.collectAllowedInstances(reqCtx, resolved)
	if len(instances) == 0 {
		return
	}

	// Only independent HTTP fetches run in workers. Episode validation, merge
	// provenance and local UserData projections remain deterministic and serial.
	type episodeScanResult struct {
		payload any
		err     error
		full    bool
	}
	requests := make([]url.Values, len(instances))
	fullSources := make([]bool, len(instances))
	for i, inst := range instances {
		if !a.isServerAllowed(reqCtx, inst.ServerID) {
			return
		}
		query := url.Values{
			"UserId":     {inst.Client.clientUserID()},
			"StartIndex": {"0"},
			"Limit":      {"5000"},
		}
		fullSources[i] = requestMergeFields(query)
		requests[i] = query
	}
	fetched := make([]episodeScanResult, len(instances))
	workers := len(instances)
	if workers > 4 {
		workers = 4
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				if countCtx.Err() != nil {
					fetched[index].err = countCtx.Err()
					continue
				}
				inst := instances[index]
				fetched[index].payload, fetched[index].err = inst.Client.RequestJSON(
					countCtx, reqCtx, a.Identity, http.MethodGet,
					"/Shows/"+inst.OriginalID+"/Episodes", requests[index], nil)
			}
		}()
	}
	for i := range instances {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	results := make([]upstreamItemsResult, 0, len(instances))
	for i, inst := range instances {
		if !a.isServerAllowed(reqCtx, inst.ServerID) || fetched[i].err != nil {
			return
		}
		payload, full := fetched[i].payload, fullSources[i]
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

	// These episodes are fetched only for local counts. Do not inherit the
	// public Seasons request's optional Fields (notably Path), which can
	// trigger unnecessary per-episode metadata hydration.
	results = a.hydrateMergeResults((&http.Request{}).WithContext(countCtx), results)
	if countCtx.Err() != nil {
		return
	}
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
