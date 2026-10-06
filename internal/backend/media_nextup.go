package backend

import (
	"net/http"
	"net/url"
	"strconv"
)

func (a *App) handleShowsNextUp(w http.ResponseWriter, r *http.Request) {
	reqCtx := requestContextFrom(r.Context())
	// Non-admin users: compute NextUp from local WatchStore
	if a.WatchStore != nil && reqCtx != nil && reqCtx.ProxyUser != nil && reqCtx.ProxyUser.Role != "admin" {
		a.handleLocalNextUp(w, r, reqCtx)
		return
	}
	query := cloneValues(r.URL.Query())
	seriesID := query.Get("SeriesId")
	if seriesID != "" {
		resolved, routeOK := a.resolveRequestRouteID(w, r, seriesID)
		if !routeOK {
			return
		}
		if resolved == nil {
			writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
			return
		}
		if !a.requireServerAccess(w, r, resolved) {
			return
		}
		instances := a.collectAllowedInstances(requestContextFrom(r.Context()), resolved)
		originalIDs := map[string]struct{}{}
		for _, inst := range instances {
			originalIDs[inst.OriginalID] = struct{}{}
		}
		for _, inst := range instances {
			instQuery := cloneValues(query)
			instQuery.Set("SeriesId", inst.OriginalID)
			requestMergeFields(instQuery)
			payload, err := inst.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet, "/Shows/NextUp", instQuery, nil)
			if err != nil {
				continue
			}
			filtered := filterSeriesItems(asItems(payload), originalIDs)
			if len(filtered) > 0 {
				for _, item := range filtered {
					if kind, _ := item["Type"].(string); kind == "" {
						item["Type"] = "Episode"
					}
				}
				filtered = a.rewriteMergeResponseItems(r, filtered, inst.ServerID, true)
				writeJSON(w, http.StatusOK, map[string]any{"Items": filtered, "TotalRecordCount": len(filtered), "StartIndex": 0})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0})
		return
	}
	results := a.fetchItemsAcrossUpstreams(r.Context(), requestContextFrom(r.Context()), "/Shows/NextUp", query, nil)
	writeJSON(w, http.StatusOK, a.mergedItemsPayload(results, a.clientFacingUserIDFor(r), reqCtx))
}

// handleLocalNextUp computes NextUp from local WatchStore for non-admin users.
// For each series the user has watched, queries upstream for the next unwatched episode.
func (a *App) handleLocalNextUp(w http.ResponseWriter, r *http.Request, reqCtx *RequestContext) {
	query := cloneValues(r.URL.Query())
	empty := map[string]any{"Items": []any{}, "TotalRecordCount": 0, "StartIndex": 0}

	// If SeriesId specified, only compute for that series
	seriesID := query.Get("SeriesId")
	if seriesID != "" {
		resolved, routeOK := a.resolveRequestRouteID(w, r, seriesID)
		if !routeOK {
			return
		}
		if resolved == nil {
			writeJSON(w, http.StatusOK, empty)
			return
		}
		if !a.requireServerAccess(w, r, resolved) {
			return
		}
		// Get the user's highest played episode for this series
		var seriesProgress *WatchProgress
		err := a.withVisibleWatchScope(reqCtx, true, func(scope mediaAccessScope) error {
			var err error
			seriesProgress, err = a.WatchStore.GetVisibleNextUpForSeries(scope, a.IDStore.CanonicalMergeID(seriesID))
			return err
		})
		if err != nil {
			a.logVisibleWatchReadError(err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "Failed to read next-up state"})
			return
		}
		if seriesProgress == nil {
			writeJSON(w, http.StatusOK, empty)
			return
		}
		nextEp := a.fetchNextEpisode(r, reqCtx, resolved.Client, resolved.OriginalID, resolved.ServerID, seriesProgress)
		if nextEp == nil || !a.isServerAllowed(reqCtx, resolved.ServerID) {
			writeJSON(w, http.StatusOK, empty)
			return
		}
		items := a.projectNextEpisode(r, nextEp, resolved.ServerID, seriesProgress)
		a.overlayLocalUserDataItems(r, items)
		writeJSON(w, http.StatusOK, watchResponsePage(items, query))
		return
	}

	// No SeriesId: compute for all series the user is watching
	var seriesList []WatchProgress
	err := a.withVisibleWatchScope(reqCtx, true, func(scope mediaAccessScope) error {
		var err error
		seriesList, err = a.WatchStore.GetVisibleNextUpSeries(scope)
		return err
	})
	if err != nil {
		a.logVisibleWatchReadError(err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "Failed to read next-up state"})
		return
	}
	if len(seriesList) == 0 {
		writeJSON(w, http.StatusOK, empty)
		return
	}

	var nextUpItems []map[string]any
	for _, sp := range seriesList {
		resolved, err := a.resolveAuthorizedRouteID(reqCtx, sp.SeriesVirtualID)
		if err != nil || resolved == nil {
			continue
		}
		serverID, seriesOrigID, client := resolved.ServerID, resolved.OriginalID, resolved.Client
		nextEp := a.fetchNextEpisode(r, reqCtx, client, seriesOrigID, serverID, &sp)
		if nextEp != nil && a.isServerAllowed(reqCtx, serverID) {
			items := a.projectNextEpisode(r, nextEp, serverID, &sp)
			a.overlayLocalUserDataItems(r, items)
			nextUpItems = append(nextUpItems, items...)
		}
	}

	writeJSON(w, http.StatusOK, watchResponsePage(nextUpItems, query))
}

// resolveSeriesServer returns an online server/client for a series watch entry.
// If the recorded server is offline, it tries OtherInstances via IDStore.
func (a *App) resolveSeriesServer(sp *WatchProgress) (serverID string, seriesOriginalID string, client *UpstreamClient) {
	c := a.Upstream.ClientByID(sp.ServerID)
	if c != nil && c.IsOnline() {
		return sp.ServerID, sp.SeriesOriginalID, c
	}
	if sp.SeriesVirtualID == "" {
		return "", "", nil
	}
	resolved := a.IDStore.ResolveVirtualID(sp.SeriesVirtualID)
	if resolved == nil {
		return "", "", nil
	}
	if resolved.ServerID != sp.ServerID {
		alt := a.Upstream.ClientByID(resolved.ServerID)
		if alt != nil && alt.IsOnline() {
			return resolved.ServerID, resolved.OriginalID, alt
		}
	}
	for _, other := range resolved.OtherInstances {
		alt := a.Upstream.ClientByID(other.ServerID)
		if alt != nil && alt.IsOnline() {
			return other.ServerID, other.OriginalID, alt
		}
	}
	return "", "", nil
}

// fetchNextEpisode queries the upstream for the next unwatched episode after
// the user's last played episode in a series.
func (a *App) fetchNextEpisode(r *http.Request, reqCtx *RequestContext, client *UpstreamClient, seriesOriginalID string, serverID string, lastPlayed *WatchProgress) map[string]any {
	if !a.isServerAllowed(reqCtx, serverID) {
		return nil
	}
	q := url.Values{}
	q.Set("Fields", "BasicSyncInfo,CanDelete,PrimaryImageAspectRatio,Overview,DateCreated,MediaSources,Path,SortName,Studios,Taglines,Genres,CommunityRating,OfficialRating,CumulativeRunTimeTicks,Chapters,ProviderIds")
	q.Set("UserId", client.clientUserID())
	q.Set("Season", strconv.Itoa(lastPlayed.ParentIndexNumber))
	q.Set("SortBy", "SortName")
	q.Set("SortOrder", "Ascending")

	payload, err := client.RequestJSON(r.Context(), reqCtx, a.Identity, http.MethodGet, "/Shows/"+seriesOriginalID+"/Episodes", q, nil)
	if err != nil {
		return nil
	}
	items := asItems(payload)
	// Find the first episode after the user's last played one
	foundCurrent := false
	var sameSeasonFallback map[string]any
	fallbackIndex := 0
	for _, ep := range items {
		parentIdx, _ := numericInt(ep["ParentIndexNumber"])
		idx, _ := numericInt(ep["IndexNumber"])

		if parentIdx == lastPlayed.ParentIndexNumber && idx == lastPlayed.IndexNumber {
			foundCurrent = true
			// If the current episode is not fully played yet (still in progress),
			// it IS the next-up item (user hasn't finished it)
			if !lastPlayed.Played && lastPlayed.PositionTicks > 0 {
				return ep
			}
			continue
		}
		if foundCurrent {
			return ep
		}
		// The remembered episode is not in this page — an upstream that renumbered the
		// season, or a page that stops before reaching it. Remember the first same-season
		// episode numbered after it instead of walking straight past the season.
		if parentIdx == lastPlayed.ParentIndexNumber && idx > lastPlayed.IndexNumber {
			if sameSeasonFallback == nil || idx < fallbackIndex {
				sameSeasonFallback = ep
				fallbackIndex = idx
			}
			continue
		}
		// Handle case where episodes are in a later season
		if parentIdx > lastPlayed.ParentIndexNumber {
			return ep
		}
	}
	if sameSeasonFallback != nil {
		return sameSeasonFallback
	}

	// If we didn't find a next episode in this season and the current one was played,
	// try the next season
	if lastPlayed.Played {
		if !a.isServerAllowed(reqCtx, client.ID) {
			return nil
		}
		q2 := url.Values{}
		q2.Set("Fields", q.Get("Fields"))
		q2.Set("UserId", client.clientUserID())
		q2.Set("Season", strconv.Itoa(lastPlayed.ParentIndexNumber+1))
		q2.Set("SortBy", "SortName")
		q2.Set("SortOrder", "Ascending")
		q2.Set("Limit", "1")

		payload2, err2 := client.RequestJSON(r.Context(), reqCtx, a.Identity, http.MethodGet, "/Shows/"+seriesOriginalID+"/Episodes", q2, nil)
		if err2 == nil {
			items2 := asItems(payload2)
			if len(items2) > 0 {
				return items2[0]
			}
		}
	}
	return nil
}
