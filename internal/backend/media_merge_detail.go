package backend

import (
	"context"
	"net/http"
	"time"
)

func (a *App) observeMergeDetail(reqCtx *RequestContext, id, server, original string, data map[string]any, full bool) error {
	if returned, _ := data["Id"].(string); returned != "" && returned != original {
		return errMediaMappingMissing
	}
	data["Id"] = original
	group := a.IDStore.ResolveMergeGroup(id)
	if group != nil && data["Type"] == nil && group.MediaType != "" {
		data["Type"] = group.MediaType
	}
	candidate := newMergeCandidate(server, data, nil, full)
	if kind := candidate.Identity.Type; kind != "Movie" && kind != "Episode" && kind != "Series" && kind != "Season" {
		return nil
	}
	return a.bindHTTPMergePlaceholder(reqCtx, id, candidate)
}

func (a *App) handleMergeItemDetail(w http.ResponseWriter, r *http.Request, userPath bool) {
	id := r.PathValue("itemId")
	resolved, ok := a.resolveRequestRouteID(w, r, id)
	if !ok {
		return
	}
	if resolved == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Item not found"})
		return
	}
	if !a.requireServerAccess(w, r, resolved) {
		return
	}
	reqCtx := requestContextFrom(r.Context())
	instances := a.collectAllowedInstances(reqCtx, resolved)
	sourceID, err := explicitMediaSourceID(r.URL.Query(), nil)
	if err != nil {
		writeMediaSelectionError(w, err)
		return
	}
	var selected *mediaSourceSelection
	if sourceID != "" {
		if err := a.prepareMergePlaybackItem(r, id, resolved); err != nil {
			writeMediaSelectionError(w, err)
			return
		}
		selected, err = a.selectAuthorizedMediaSource(r, id, sourceID, resolved)
		if err != nil {
			writeMediaSelectionError(w, err)
			return
		}
		instances = []seriesInstance{{ServerID: selected.ServerID, OriginalID: selected.ItemOriginalID, Client: selected.Client}}
	}
	cfg := a.ConfigStore.Snapshot()
	timeout := time.Duration(cfg.Timeouts.Global) * time.Millisecond
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	fetch := func(batch []seriesInstance) []upstreamItemsResult {
		tasks := make([]upstreamTask, len(batch))
		for i, inst := range batch {
			instance := inst
			tasks[i] = upstreamTask{index: i, fn: func(ctx context.Context) upstreamItemsResult {
				if !a.isServerAllowed(reqCtx, instance.ServerID) {
					return upstreamItemsResult{Err: errMediaAccessDenied}
				}
				query := cloneValues(r.URL.Query())
				if selected != nil {
					setSelectedMediaSource(query, nil, selected)
				}
				query.Set("UserId", instance.Client.clientUserID())
				full := requestMergeFields(query)
				endpoint := "/Items/" + instance.OriginalID
				if userPath {
					endpoint = "/Users/" + instance.Client.clientUserID() + endpoint
				}
				payload, err := instance.Client.RequestJSON(ctx, reqCtx, a.Identity, http.MethodGet, endpoint, query, nil)
				if err != nil {
					return upstreamItemsResult{Err: err}
				}
				data, valid := payload.(map[string]any)
				if !valid {
					return upstreamItemsResult{Err: errMediaMappingMissing}
				}
				if returned, _ := data["Id"].(string); returned != "" && returned != instance.OriginalID {
					return upstreamItemsResult{Err: errMediaMappingMissing}
				}
				data["Id"] = instance.OriginalID
				return upstreamItemsResult{ServerID: instance.ServerID, Items: []map[string]any{data}, FullSources: full, RequestScope: reqCtx}
			}}
		}
		return a.aggregateUpstreams(r.Context(), aggregationConfig{gracePeriod: time.Duration(cfg.Timeouts.MetadataGracePeriod) * time.Millisecond, globalTimeout: timeout}, tasks)

	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	r = r.WithContext(ctx)
	var results []upstreamItemsResult
	seenInstances := map[AdditionalInstance]bool{}
	pending := instances
	for len(pending) > 0 && ctx.Err() == nil {
		for _, inst := range pending {
			seenInstances[AdditionalInstance{ServerID: inst.ServerID, OriginalID: inst.OriginalID}] = true
		}
		for _, result := range fetch(pending) {
			if len(result.Items) == 0 || !a.isServerAllowed(reqCtx, result.ServerID) {
				continue
			}
			raw := result.Items[0]
			original, _ := raw["Id"].(string)
			if err := a.observeMergeDetail(reqCtx, id, result.ServerID, original, raw, result.FullSources); err != nil {
				a.logMergeHTTPError(err)
				continue
			}
			results = append(results, result)
		}
		if selected != nil {
			break
		}
		updated, err := a.resolveAuthorizedRouteID(reqCtx, id)
		if err != nil || updated == nil {
			break
		}
		pending = nil
		for _, inst := range a.collectAllowedInstances(reqCtx, updated) {
			if !seenInstances[AdditionalInstance{ServerID: inst.ServerID, OriginalID: inst.OriginalID}] {
				pending = append(pending, inst)
				instances = append(instances, inst)
			}
		}
	}
	var base map[string]any
	baseServer := ""
	var sources []any
	for _, result := range results {
		if len(result.Items) == 0 || !a.isServerAllowed(reqCtx, result.ServerID) {
			continue
		}
		raw := result.Items[0]
		projected := a.projectMergeItem(raw, result.ServerID, id, a.clientFacingUserIDFor(r))
		if projected == nil {
			continue
		}
		if selected != nil {
			var only []any
			for _, source := range asItems(projected["MediaSources"]) {
				if source["Id"] == selected.VirtualID {
					only = append(only, source)
				}
			}
			projected["MediaSources"] = only
		}
		if base == nil || isBetterMetadata(base, baseServer, projected, result.ServerID, cfg) {
			base = projected
			baseServer = result.ServerID
		}
		for _, source := range asItems(map[string]any{"Items": projected["MediaSources"]}) {
			if virtual, _ := source["Id"].(string); virtual != "" {
				a.rememberMediaMembership(reqCtx, virtual, id, result.ServerID)
			}
			if len(instances) > 1 {
				if client := a.Upstream.ClientByID(result.ServerID); client != nil {
					name, _ := source["Name"].(string)
					if name == "" {
						name = "Version"
					}
					source["Name"] = name + " [" + client.Name + "]"
				}
			}
			sources = append(sources, source)
		}
	}
	if base == nil || !a.isServerAllowed(reqCtx, baseServer) {
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Upstream request failed"})
		return
	}
	if len(sources) > 0 || base["MediaSources"] != nil {
		base["MediaSources"] = mergeProjectedSources(sources)
		base["MediaSourceCount"] = len(asItems(base["MediaSources"]))
	}
	a.overlayLocalUserData(r, id, base)
	writeJSON(w, http.StatusOK, base)
}

func (a *App) prepareMergePlaybackItem(r *http.Request, id string, resolved *routeResolution) error {
	group := a.IDStore.ResolveMergeGroup(id)
	if group == nil || group.MediaType == "" || (group.MediaType != "Movie" && group.MediaType != "Episode") {
		return nil
	}
	if group.Policy == mergePolicyWork {
		for _, member := range group.Members {
			if member.Ref.ServerID == resolved.ServerID && member.Ref.ItemID == resolved.OriginalID && member.Ref.MediaSourceID != "" {
				return nil
			}
		}
	}
	query := cloneValues(r.URL.Query())
	query.Del("MediaSourceId")
	query.Del("mediaSourceId")
	requestMergeFields(query)
	payload, err := resolved.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, http.MethodGet,
		"/Users/"+resolved.Client.clientUserID()+"/Items/"+resolved.OriginalID, query, nil)
	if err != nil {
		return errMediaSourceUnavailable
	}
	data, ok := payload.(map[string]any)
	if !ok {
		return errMediaMappingMissing
	}
	if err := a.observeMergeDetail(requestContextFrom(r.Context()), id, resolved.ServerID, resolved.OriginalID, data, true); err != nil {
		return err
	}
	updated, err := a.resolveAuthorizedRouteID(requestContextFrom(r.Context()), id)
	if err != nil {
		return err
	}
	if updated == nil {
		return errMediaMappingMissing
	}
	*resolved = *updated
	return nil
}

// PlaybackInfo may reveal additional versions after an earlier partial list.
func (a *App) observeMergePlaybackSources(reqCtx *RequestContext, id, server, item string, data map[string]any) error {
	group := a.IDStore.ResolveMergeGroup(id)
	if group == nil || group.MediaType == "" {
		return nil
	}
	for _, member := range group.Members {
		if member.Ref.ServerID != server || member.Ref.ItemID != item {
			continue
		}
		raw := map[string]any{"Id": item, "Type": group.MediaType, "MediaSources": data["MediaSources"]}
		candidate := newMergeCandidate(server, raw, nil, false)
		candidate.Identity = member.Identity
		candidate.Parent = member.Parent
		candidate.Season = member.Season
		candidate.Episode = member.Episode
		return a.bindHTTPMergePlaceholder(reqCtx, id, candidate)
	}
	return nil
}

func (a *App) mergePlaybackSources(id, server, item string, raw []map[string]any, explicit string) []map[string]any {
	kept := []map[string]any{}
	for _, source := range raw {
		sourceID, _ := source["Id"].(string)
		if explicit != "" && sourceID != explicit {
			continue
		}
		if a.IDStore.MergeMemberAllowed(id, server, item, sourceID) {
			kept = append(kept, source)
		}
	}
	return kept
}

// Negotiation must be requested for the chosen group's actual version, even
// when the client omitted MediaSourceId and the upstream item's default differs.
func (a *App) defaultMergeSource(id, server, item string) string {
	group := a.IDStore.ResolveMergeGroup(id)
	if group == nil || group.Policy != mergePolicyExact {
		return ""
	}
	for _, member := range group.Members {
		if member.Ref.ServerID == server && member.Ref.ItemID == item && member.Ref.MediaSourceID != "" {
			return member.Ref.MediaSourceID
		}
	}
	return ""
}
