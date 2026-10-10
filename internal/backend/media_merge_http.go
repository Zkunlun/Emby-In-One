package backend

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

func requestMergeFields(query url.Values) bool {
	fields := strings.Split(query.Get("Fields"), ",")
	for _, required := range []string{"ProviderIds", "MediaSources"} {
		found := false
		for _, field := range fields {
			if strings.EqualFold(field, required) {
				found = true
			}
		}
		if !found {
			fields = append(fields, required)
		}
	}
	query.Set("Fields", strings.Trim(strings.Join(fields, ","), ","))
	for key, values := range query {
		if strings.EqualFold(key, "MediaSourceId") && len(values) > 0 && values[0] != "" {
			return false
		}
	}
	return true
}

// Complete metadata is requested only for items already encountered by this
// request. No startup/library traversal or third-party ID lookup is introduced.
func (a *App) hydrateMergeResults(r *http.Request, results []upstreamItemsResult) []upstreamItemsResult {
	reqCtx := requestContextFrom(r.Context())
	cfg := a.ConfigStore.Snapshot()
	timeout := time.Duration(cfg.Timeouts.Global) * time.Millisecond
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	for i := range results {
		result := &results[i]
		if !a.isServerAllowed(reqCtx, result.ServerID) {
			continue
		}
		client := a.Upstream.ClientByID(result.ServerID)
		if client == nil || !client.IsOnline() {
			continue
		}
		if result.Parents == nil {
			result.Parents = map[string]*mergeSeriesEvidence{}
		}
		if result.CompleteItems == nil {
			result.CompleteItems = map[string]bool{}
		}
		quality := sampleMergeQuality(result.Items)
		missing := []string{}
		seen := map[string]bool{}
		for _, item := range result.Items {
			id, _ := item["Id"].(string)
			kind, _ := item["Type"].(string)
			if parent := newMergeSeriesEvidence(result.ServerID, item); parent != nil {
				result.Parents[id] = parent
			}
			sources, _ := item["MediaSources"].([]any)
			if id != "" && (kind == "" || ((kind == "Movie" || kind == "Episode") && len(sources) == 0)) && !seen[id] {
				missing = append(missing, id)
				seen[id] = true
			}
		}
		quality["need_metadata_hydrate"] = len(missing)
		quality["metadata_hydrate_batches"] = (len(missing) + maxBatchIDCount - 1) / maxBatchIDCount
		metadata := a.fetchEncounteredMergeMetadata(withSampleSource(ctx, sampleSourceHydrate), reqCtx, client, missing)
		for _, item := range result.Items {
			id, _ := item["Id"].(string)
			if data := metadata[id]; data != nil {
				for key, value := range data {
					item[key] = value
				}
				result.CompleteItems[id] = true
			}
		}
		missing = nil
		seen = map[string]bool{}
		for _, item := range result.Items {
			kind, _ := item["Type"].(string)
			if kind != "Season" && kind != "Episode" {
				continue
			}
			parentID, _ := item["SeriesId"].(string)
			if parentID == "" && kind == "Season" {
				parentID, _ = item["ParentId"].(string)
			}
			if parentID == "" || result.Parents[parentID] != nil || seen[parentID] {
				continue
			}
			seen[parentID] = true
			// A saved, source-qualified parent member remains proof through metadata edits.
			owner := a.IDStore.ResolveMergeMember(result.ServerID, parentID, "")
			if group := a.IDStore.ResolveMergeGroup(owner); group != nil && group.MediaType == "Series" {
				for _, member := range group.Members {
					if member.Ref.ServerID == result.ServerID && member.Ref.ItemID == parentID {
						result.Parents[parentID] = &mergeSeriesEvidence{ServerID: result.ServerID, SeriesID: parentID, Identity: member.Identity}
						break
					}
				}
			}
			if result.Parents[parentID] == nil {
				missing = append(missing, parentID)
			}
		}
		quality["need_parent_hydrate"] = len(missing)
		quality["parent_hydrate_batches"] = (len(missing) + maxBatchIDCount - 1) / maxBatchIDCount
		a.SampleCollector.RecordMergeQuality(reqCtx, client.Name, quality)
		for id, item := range a.fetchEncounteredMergeMetadata(withSampleSource(ctx, sampleSourceParentHydrate), reqCtx, client, missing) {
			if parent := newMergeSeriesEvidence(result.ServerID, item); parent != nil {
				result.Parents[id] = parent
			}
		}
	}
	var parents []upstreamItemsResult
	for _, result := range results {
		entry := upstreamItemsResult{ServerID: result.ServerID}
		ids := make([]string, 0, len(result.Parents))
		for id := range result.Parents {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			parent := result.Parents[id]
			providers := map[string]any{}
			for key, value := range parent.Identity.ProviderIDs {
				providers[key] = value
			}
			body := map[string]any{"Id": id, "Type": "Series", "Name": parent.Identity.Name, "ProviderIds": providers}
			if parent.Identity.YearValid {
				body["ProductionYear"] = parent.Identity.Year
			}
			entry.Items = append(entry.Items, body)
		}
		if len(entry.Items) > 0 {
			parents = append(parents, entry)
		}
	}
	if len(parents) > 0 {
		a.mergeHTTPItems(parents, a.clientFacingUserIDFor(r), reqCtx)
	}
	return results
}

func (a *App) fetchEncounteredMergeMetadata(ctx context.Context, reqCtx *RequestContext, client *UpstreamClient, ids []string) map[string]map[string]any {
	metadata := map[string]map[string]any{}
	for start := 0; start < len(ids); start += maxBatchIDCount {
		if ctx.Err() != nil || !a.isServerAllowed(reqCtx, client.ID) {
			break
		}
		end := start + maxBatchIDCount
		if end > len(ids) {
			end = len(ids)
		}
		requested := map[string]bool{}
		for _, id := range ids[start:end] {
			requested[id] = true
		}
		q := url.Values{"Ids": {strings.Join(ids[start:end], ",")}, "UserId": {client.clientUserID()}}
		requestMergeFields(q)
		payload, err := client.RequestJSON(ctx, reqCtx, a.Identity, http.MethodGet, "/Items", q, nil)
		if err != nil || !a.isServerAllowed(reqCtx, client.ID) {
			continue
		}
		for _, item := range asItems(payload) {
			id, _ := item["Id"].(string)
			if requested[id] {
				metadata[id] = item
			}
		}
	}
	return metadata
}

func mergeResultCandidate(result upstreamItemsResult, item map[string]any) mergeCandidate {
	parentID, _ := item["SeriesId"].(string)
	if parentID == "" && item["Type"] == "Season" {
		parentID, _ = item["ParentId"].(string)
	}
	id, _ := item["Id"].(string)
	candidate := newMergeCandidate(result.ServerID, item, result.Parents[parentID], result.FullSources || result.CompleteItems[id])
	candidate.ObservationOrigin = "passive_list"
	return candidate
}

func (a *App) invalidateMergedWatchAliases(ids []string) {
	if len(ids) == 0 {
		return
	}
	changed := map[string]bool{}
	for _, id := range ids {
		changed[id] = true
	}
	a.watchPlayback.mu.Lock()
	defer a.watchPlayback.mu.Unlock()
	for key := range a.watchPlayback.shared {
		if changed[key.ItemID] {
			delete(a.watchPlayback.shared, key)
		}
	}
	for key := range a.watchPlayback.entries {
		if changed[key.VirtualItemID] {
			delete(a.watchPlayback.entries, key)
		}
	}
}

// Compatibility wrappers retain HTTP call sites while all mutation policy,
// lifecycle checks and cache publication live in the shared service.
func (a *App) associateHTTPMerge(reqCtx *RequestContext, left, right mergeCandidate, sourceLeft, sourceRight, target string) (string, error) {
	return a.mergeDiscovery().associate(reqCtx, left, right, sourceLeft, sourceRight, target)
}

func (a *App) registerHTTPMerge(reqCtx *RequestContext, candidate mergeCandidate, target string) (string, error) {
	return a.mergeDiscovery().register(reqCtx, candidate, target, true)
}

func (a *App) bindHTTPMergePlaceholder(reqCtx *RequestContext, id string, candidate mergeCandidate) error {
	_, err := a.mergeDiscovery().register(reqCtx, candidate, id, true)
	return err
}

type httpMergeUnit struct {
	Candidate mergeCandidate
	SourceID  string
	VirtualID string
	Item      map[string]any
	ServerID  string
}

func (a *App) mergeHTTPItems(results []upstreamItemsResult, clientUserID string, reqCtx *RequestContext) []map[string]any {
	cfg := a.ConfigStore.Snapshot()
	results = append([]upstreamItemsResult(nil), results...)
	order := map[string]int{}
	for i, server := range cfg.Upstream {
		order[server.ID] = i
	}
	sort.SliceStable(results, func(i, j int) bool { return order[results[i].ServerID] < order[results[j].ServerID] })
	var units []httpMergeUnit
	index := map[string][]int{}
	maxLen := 0
	for _, result := range results {
		if len(result.Items) > maxLen {
			maxLen = len(result.Items)
		}
	}
	for row := 0; row < maxLen; row++ {
		for _, result := range results {
			if row >= len(result.Items) || (reqCtx != nil && !a.isServerAllowed(reqCtx, result.ServerID)) {
				continue
			}
			item := result.Items[row]
			candidate := mergeResultCandidate(result, item)
			kind := candidate.Identity.Type
			if candidate.ItemID == "" {
				continue
			}
			if kind == "" {
				// Failed type hydration must not mint a new whole-item legacy exception.
				if _, _, known := a.IDStore.ResolveOriginalIDForServer(candidate.ItemID, result.ServerID); !known {
					continue
				}
			}
			if kind != "Movie" && kind != "Episode" && kind != "Series" && kind != "Season" {
				units = append(units, httpMergeUnit{Item: deepCloneMap(item), ServerID: result.ServerID, VirtualID: a.IDStore.GetOrCreateVirtualID(candidate.ItemID, result.ServerID)})
				continue
			}
			unit := httpMergeUnit{Candidate: candidate, Item: deepCloneMap(item), ServerID: result.ServerID}
			hints := candidate.lookupKeys()
			if candidate.Parent != nil {
				parent := a.IDStore.ResolveMergeMember(candidate.Parent.ServerID, candidate.Parent.SeriesID, "")
				if key := candidate.savedParentLookupKey(parent); key != "" {
					hints = append(hints, key)
				}
			}
			matches := map[int]bool{}
			for _, hint := range hints {
				for _, other := range index[hint] {
					matches[other] = true
				}
			}
			ordered := make([]int, 0, len(matches))
			for other := range matches {
				ordered = append(ordered, other)
			}
			sort.Ints(ordered)
			for _, other := range ordered {
				previous := units[other]
				target := a.IDStore.CanonicalMergeID(previous.VirtualID)
				id, err := a.associateHTTPMerge(reqCtx, previous.Candidate, candidate, "", "", target)
				if err == nil {
					unit.VirtualID = id
					break
				}
			}
			if unit.VirtualID == "" {
				id, err := a.registerHTTPMerge(reqCtx, candidate, "")
				if err != nil {
					a.logMergeHTTPError(err)
					continue
				}
				unit.VirtualID = id
			}
			current := len(units)
			units = append(units, unit)
			for _, hint := range hints {
				index[hint] = append(index[hint], current)
			}
		}
	}
	// Fold after all associations so an absorbed ID cannot leave a second tile.
	merged := []map[string]any{}
	seen := map[string]int{}
	servers := map[string]string{}
	for _, unit := range units {
		if unit.VirtualID == "" || (reqCtx != nil && !a.isServerAllowed(reqCtx, unit.ServerID)) {
			continue
		}
		id := a.IDStore.CanonicalMergeID(unit.VirtualID)
		item := a.projectMergeItem(unit.Item, unit.ServerID, id, clientUserID)
		if item == nil {
			continue
		}
		if pos, ok := seen[id]; ok {
			if a.IDStore.MergeGroupTrust(id) != mergeTrustTrusted {
				continue // do not blend conflicting sources or metadata
			}
			combined := mergeProjectedSources(merged[pos]["MediaSources"], item["MediaSources"])
			if isBetterMetadata(merged[pos], servers[id], item, unit.ServerID, cfg) {
				merged[pos] = item
				servers[id] = unit.ServerID
			}
			if len(combined) > 0 {
				merged[pos]["MediaSources"] = combined
				merged[pos]["MediaSourceCount"] = len(combined)
			}
			continue
		}
		seen[id] = len(merged)
		servers[id] = unit.ServerID
		merged = append(merged, item)
	}
	return merged
}

// Virtual source IDs include both upstream and original item identity.
func mergeProjectedSources(values ...any) []any {
	var sources []any
	seen := map[string]bool{}
	for _, value := range values {
		for _, source := range asItems(value) {
			id, _ := source["Id"].(string)
			if id != "" && !seen[id] {
				sources = append(sources, source)
				seen[id] = true
			}
		}
	}
	return sources
}

func (a *App) logMergeHTTPError(err error) {
	if a.Logger != nil {
		a.Logger.Warnf("Version association: %s", redactURLInError(err))
	}
}

// Explicit virtual item/source fields are restored after generic rewriting,
// which must never remint a group or fall back from a multi-version raw item.
func (a *App) projectMergeItem(raw map[string]any, server, id, user string) map[string]any {
	item := deepCloneMap(raw)
	original, _ := raw["Id"].(string)
	var sources []any
	_, hasSources := raw["MediaSources"]
	for _, source := range asItems(map[string]any{"Items": raw["MediaSources"]}) {
		sourceID, _ := source["Id"].(string)
		if sourceID == "" || !a.IDStore.MergeMemberAllowed(id, server, original, sourceID) {
			continue
		}
		virtual, err := a.IDStore.GetMergeMediaSourceID(server, original, sourceID)
		if err != nil {
			a.logMergeHTTPError(err)
			return nil
		}
		copy := deepCloneMap(source)
		copy["Id"] = virtual
		copy["ItemId"] = id
		sources = append(sources, copy)
	}
	delete(item, "MediaSources")
	delete(item, "Id")
	self := map[string]bool{}
	for field := range simpleIDFields {
		if value, _ := item[field].(string); value == original && original != "" {
			self[field] = true
			delete(item, field)
		}
	}
	if ud, ok := item["UserData"].(map[string]any); ok {
		delete(ud, "ItemId")
	}
	cfg := a.ConfigStore.Snapshot()
	rewriteResponseIDs(item, server, a.IDStore, cfg.Server.ID, user)
	item["Id"] = id
	for field := range self {
		item[field] = id
	}
	if ud, ok := item["UserData"].(map[string]any); ok {
		ud["ItemId"] = id
	}
	if a.IDStore.MergeGroupTrust(id) != mergeTrustTrusted {
		sources = nil // no automatic cross-source version projection
	}
	if hasSources {
		item["MediaSources"] = sources
		item["MediaSourceCount"] = len(sources)
	}
	if group := a.IDStore.ResolveMergeGroup(id); group != nil && group.Policy == mergePolicyExact && len(sources) == 1 {
		source := sources[0].(map[string]any)
		if runtime := mergePositiveTicks(source, "RunTimeTicks"); runtime.valid() {
			item["RunTimeTicks"] = runtime.Ticks
		}
	}
	return item
}

// Used by single-source endpoints: the acquisition is still demand driven and
// shares the same version model, without inventing cross-server requests.
func (a *App) rewriteMergeResponseItems(r *http.Request, items []map[string]any, server string, complete bool) []map[string]any {
	results := []upstreamItemsResult{{ServerID: server, Items: items, FullSources: complete, RequestScope: requestContextFrom(r.Context())}}
	results = a.hydrateMergeResults(r, results)
	return a.mergeRoundRobinItems(results, a.clientFacingUserIDFor(r), requestContextFrom(r.Context()))
}
