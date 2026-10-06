package backend

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"strings"
)

type playbackInfoLeaseReservation struct {
	Applies  bool
	UserID   string
	ServerID string
	DeviceID string
	Result   playbackLeaseResult
}

// commitPlaybackInfoLease commits the successful upstream's PlaySessionID onto
// the lease that will own the playback. If PlaybackInfo fell back to another
// upstream, reserve that target first and only retire a source provisional lease
// when this request created it and still owns its exact revision.
func (a *App) commitPlaybackInfoLease(lease *playbackInfoLeaseReservation, itemID, serverID, playSessionID string) bool {
	if lease == nil || !lease.Applies {
		return true
	}

	sourceServerID := lease.ServerID
	sourceResult := lease.Result
	if serverID != sourceServerID {
		target := a.PlaybackLimiter.Reserve(lease.UserID, serverID, lease.DeviceID, itemID, "")
		if !target.Allowed {
			if sourceResult.Created {
				a.PlaybackLimiter.RollbackReservation(lease.UserID, sourceServerID, lease.DeviceID, sourceResult.Revision)
			}
			return false
		}
		lease.ServerID = serverID
		lease.Result = target
	}

	if playSessionID != "" {
		committed := a.PlaybackLimiter.Reserve(lease.UserID, lease.ServerID, lease.DeviceID, itemID, playSessionID)
		if !committed.Allowed {
			if lease.Result.Created {
				a.PlaybackLimiter.RollbackReservation(lease.UserID, lease.ServerID, lease.DeviceID, lease.Result.Revision)
			}
			if sourceServerID != lease.ServerID && sourceResult.Created {
				a.PlaybackLimiter.RollbackReservation(lease.UserID, sourceServerID, lease.DeviceID, sourceResult.Revision)
			}
			return false
		}
		lease.Result = committed
	}

	if sourceServerID != lease.ServerID && sourceResult.Created {
		a.PlaybackLimiter.RollbackReservation(lease.UserID, sourceServerID, lease.DeviceID, sourceResult.Revision)
	}
	return true
}

func (a *App) handlePlaybackInfo(w http.ResponseWriter, r *http.Request) {
	resolved, routeOK := a.resolveRequestRouteID(w, r, r.PathValue("itemId"))
	if !routeOK {
		return
	}
	if resolved == nil {
		if a.Logger != nil {
			a.Logger.Warnf("PlaybackInfo: itemId=%s not found in mappings", r.PathValue("itemId"))
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"message": "Item not found"})
		return
	}
	reqCtx := requestContextFrom(r.Context())
	if !a.requireServerAccess(w, r, resolved) {
		return
	}
	if err := a.prepareMergePlaybackItem(r, r.PathValue("itemId"), resolved); err != nil {
		writeMediaSelectionError(w, err)
		return
	}
	query := cloneValues(r.URL.Query())
	body := map[string]any{}
	if r.Method == http.MethodPost && r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	sourceID, err := explicitMediaSourceID(query, body)
	if err != nil {
		writeMediaSelectionError(w, err)
		return
	}
	var selected *mediaSourceSelection
	if sourceID != "" {
		selected, err = a.selectAuthorizedMediaSource(r, r.PathValue("itemId"), sourceID, resolved)
		if err != nil {
			writeMediaSelectionError(w, err)
			return
		}
		resolved = &routeResolution{OriginalID: selected.ItemOriginalID, ServerID: selected.ServerID, Client: selected.Client}
		setSelectedMediaSource(query, body, selected)
	}
	instances := a.collectAllowedInstances(reqCtx, resolved)
	if a.Logger != nil {
		a.Logger.Debugf("PlaybackInfo: itemId=%s → server=[%s] originalId=%s, instances=%d",
			r.PathValue("itemId"), resolved.Client.Name, resolved.OriginalID, len(instances))
	}
	// Keep the complete provisional reservation context for the rest of this
	// PlaybackInfo request. Device identity is consumed only from the centralized
	// RequestContext boundary, and Result carries the revision needed by commit/rollback.
	lease := playbackInfoLeaseReservation{ServerID: resolved.ServerID}
	if limiterUserID, ok := a.playbackLimiterKey(reqCtx, resolved.ServerID); ok {
		deviceID := playbackDeviceID(reqCtx)
		if deviceID == "" {
			writePlaybackDeviceIDRequired(w)
			return
		}
		lease.Applies = true
		lease.UserID = limiterUserID
		lease.DeviceID = deviceID
		if !a.publishPlaybackState(reqCtx, lease.ServerID, func() {
			lease.Result = a.PlaybackLimiter.Reserve(lease.UserID, lease.ServerID, lease.DeviceID, r.PathValue("itemId"), "")
		}) {
			writeMediaSelectionError(w, errMediaAccessDenied)
			return
		}
		if !lease.Result.Allowed {
			writePlaybackDeviceLimit(w)
			return
		}
	}

	// Remove proxy token from query before forwarding to upstream
	query.Del("api_key")
	query.Del("ApiKey")

	var base map[string]any
	baseServerID := ""
	basePlaySessionID := ""
	clientPlaySessionID := ""
	allMediaSources := []map[string]any{}
	for _, inst := range instances {
		if !a.isServerAllowed(reqCtx, inst.ServerID) {
			continue
		}
		// The client only ever holds EIO's virtual user ID, and Emby prefers a UserId in the
		// query or body over the one the request was authenticated as. Forwarding the virtual
		// ID made the upstream look up a user that does not exist there and answer 500
		// (NullReferenceException), which surfaced as a 502 to the client.
		instQuery := cloneValues(query)
		instQuery.Set("UserId", inst.Client.clientUserID())
		if selected == nil {
			if source := a.defaultMergeSource(r.PathValue("itemId"), inst.ServerID, inst.OriginalID); source != "" {
				instQuery.Set("MediaSourceId", source)
			}
		}
		instBody := deepCloneMap(body)
		instBody["UserId"] = inst.Client.clientUserID()
		if source := instQuery.Get("MediaSourceId"); source != "" {
			instBody["MediaSourceId"] = source
		}
		payload, err := inst.Client.RequestJSON(r.Context(), requestContextFrom(r.Context()), a.Identity, r.Method, "/Items/"+inst.OriginalID+"/PlaybackInfo", instQuery, instBody)
		if err != nil {
			if a.Logger != nil {
				a.Logger.Warnf("PlaybackInfo: server=[%s] originalId=%s failed: %s", inst.Client.Name, inst.OriginalID, redactURLInError(err))
			}
			continue
		}
		data, ok := payload.(map[string]any)
		if !ok || !a.isServerAllowed(reqCtx, inst.ServerID) {
			continue
		}
		explicitSource := ""
		if selected != nil {
			explicitSource = selected.OriginalID
		}
		if err := a.observeMergePlaybackSources(reqCtx, r.PathValue("itemId"), inst.ServerID, inst.OriginalID, data); err != nil {
			a.logMergeHTTPError(err)
			continue
		}
		filtered := a.mergePlaybackSources(r.PathValue("itemId"), inst.ServerID, inst.OriginalID, asItems(map[string]any{"Items": data["MediaSources"]}), explicitSource)
		if len(filtered) == 0 {
			continue
		}
		data["MediaSources"] = toAnySlice(filtered)
		instPlaySessionID, _ := data["PlaySessionId"].(string)
		a.rememberPlaybackInfoWatch(reqCtx, r.PathValue("itemId"), inst.ServerID, inst.OriginalID,
			instPlaySessionID, asItems(map[string]any{"Items": data["MediaSources"]}))
		virtualPlaySessionID := instPlaySessionID
		if instPlaySessionID != "" {
			virtualPlaySessionID = a.IDStore.GetOrCreateVirtualID(instPlaySessionID, inst.ServerID)
		}
		if base == nil {
			base = deepCloneMap(data)
			baseServerID = inst.ServerID
			basePlaySessionID = instPlaySessionID
			clientPlaySessionID = virtualPlaySessionID
		}
		for _, raw := range asItems(map[string]any{"Items": data["MediaSources"]}) {
			mediaSource := deepCloneMap(raw)
			originalMSID, _ := mediaSource["Id"].(string)
			virtualMSID := originalMSID
			if originalMSID != "" {
				virtualMSID, err = a.IDStore.GetMergeMediaSourceID(inst.ServerID, inst.OriginalID, originalMSID)
				if err != nil {
					a.logMergeHTTPError(err)
					continue
				}
				mediaSource["Id"] = virtualMSID
			}
			a.rememberSourceRoute(reqCtx, virtualMSID, r.PathValue("itemId"), inst.ServerID, instPlaySessionID, clientPlaySessionID)
			// MediaSource.ItemId refers to the item whose PlaybackInfo was requested. Keep it
			// on the same virtual item identity exposed to the client; leaving the upstream
			// ItemId here makes clients that construct external-subtitle URLs from this field
			// address an ID EIO cannot resolve.
			mediaSource["ItemId"] = r.PathValue("itemId")
			if directURL, ok := mediaSource["DirectStreamUrl"].(string); ok && directURL != "" {
				// Extract container from URL, stripping query string first
				// Node.js uses regex /\.([a-z0-9]+)(?:\?|$)/i — path.Ext doesn't stop at '?'
				cleanURL := directURL
				if qIdx := strings.IndexByte(cleanURL, '?'); qIdx >= 0 {
					cleanURL = cleanURL[:qIdx]
				}
				container := strings.TrimPrefix(path.Ext(cleanURL), ".")
				if container == "" {
					if rawContainer, ok := mediaSource["Container"].(string); ok {
						container = rawContainer
					}
				}
				if container == "" {
					container = "mp4"
				}
				proxyURL := url.Values{}
				proxyURL.Set("MediaSourceId", virtualMSID)
				if virtualPlaySessionID != "" {
					proxyURL.Set("PlaySessionId", virtualPlaySessionID)
				}
				proxyURL.Set("Static", "true")
				if reqCtx := requestContextFrom(r.Context()); reqCtx != nil && reqCtx.ProxyToken != "" {
					proxyURL.Set("api_key", reqCtx.ProxyToken)
				}
				mediaSource["DirectStreamUrl"] = "/Videos/" + r.PathValue("itemId") + "/stream." + container + "?" + proxyURL.Encode()
			}
			if transcodingURL, ok := mediaSource["TranscodingUrl"].(string); ok && transcodingURL != "" {
				parsed, err := url.Parse(transcodingURL)
				if err == nil {
					if parsed.Scheme == "" {
						parsed, _ = url.Parse(inst.Client.StreamBaseURL + transcodingURL)
					}
					proxyPath := parsed.Path
					proxyPath = strings.Replace(proxyPath, "/Videos/"+inst.OriginalID+"/", "/Videos/"+r.PathValue("itemId")+"/", 1)
					proxyPath = strings.Replace(proxyPath, "/Audio/"+inst.OriginalID+"/", "/Audio/"+r.PathValue("itemId")+"/", 1)
					queryValues := parsed.Query()
					queryValues.Del("api_key")
					queryValues.Del("ApiKey")
					if originalMSID != "" {
						queryValues.Set("MediaSourceId", virtualMSID)
					}
					if virtualPlaySessionID != "" {
						queryValues.Set("PlaySessionId", virtualPlaySessionID)
					}
					if reqCtx := requestContextFrom(r.Context()); reqCtx != nil && reqCtx.ProxyToken != "" {
						queryValues.Set("api_key", reqCtx.ProxyToken)
					}
					mediaSource["TranscodingUrl"] = proxyPath + "?" + queryValues.Encode()
				}
			}
			// Rewrite HTTP paths by exact path segment. MediaSource IDs can contain the
			// item ID (for example mediasource_15511), so whole-string replacement can
			// corrupt the MediaSource segment before it is virtualised.
			if protocol, _ := mediaSource["Protocol"].(string); protocol == "Http" {
				if msPath, ok := mediaSource["Path"].(string); ok && msPath != "" {
					mediaSource["Path"] = rewriteMediaPathIDs(msPath, inst.OriginalID, r.PathValue("itemId"), originalMSID, virtualMSID)
				}
			}
			if rawStreams, ok := mediaSource["MediaStreams"].([]any); ok {
				for _, rawStream := range rawStreams {
					if stream, ok := rawStream.(map[string]any); ok {
						if deliveryURL, ok := stream["DeliveryUrl"].(string); ok && deliveryURL != "" {
							deliveryURL = rewriteMediaPathIDs(deliveryURL, inst.OriginalID, r.PathValue("itemId"), originalMSID, virtualMSID)
							if parsed, err := url.Parse(deliveryURL); err == nil {
								queryValues := parsed.Query()
								queryValues.Del("api_key")
								queryValues.Del("ApiKey")
								if virtualPlaySessionID != "" {
									queryValues.Set("PlaySessionId", virtualPlaySessionID)
								}
								if reqCtx != nil && reqCtx.ProxyToken != "" {
									queryValues.Set("api_key", reqCtx.ProxyToken)
								}
								parsed.RawQuery = queryValues.Encode()
								// DeliveryUrl must resolve back through EIO. Some upstreams return an
								// absolute CDN URL; keeping that authority while replacing its token with
								// EIO's token makes clients bypass EIO and fail authentication upstream.
								parsed.Scheme = ""
								parsed.Host = ""
								parsed.User = nil
								parsed.Opaque = ""
								deliveryURL = parsed.String()
							}
							stream["DeliveryUrl"] = deliveryURL
						}
					}
				}
			}
			allMediaSources = append(allMediaSources, mediaSource)
		}
	}
	if base == nil || len(allMediaSources) == 0 {
		if a.Logger != nil {
			a.Logger.Errorf("PlaybackInfo: all upstream requests failed for itemId=%s", r.PathValue("itemId"))
		}
		// Roll back only a provisional lease this PlaybackInfo request actually
		// created, and only while its exact revision is still current. A failed
		// same-device retry reuses an older committed lease (Created=false), while a
		// later Reserve advances Revision and makes an older rollback harmless.
		if lease.Applies && lease.Result.Created {
			a.PlaybackLimiter.RollbackReservation(lease.UserID, lease.ServerID, lease.DeviceID, lease.Result.Revision)
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": "Failed to fetch playback info from upstream"})
		return
	}
	if !a.isServerAllowed(reqCtx, baseServerID) {
		if lease.Applies && lease.Result.Created {
			a.PlaybackLimiter.RollbackReservation(lease.UserID, lease.ServerID, lease.DeviceID, lease.Result.Revision)
		}
		writeMediaSelectionError(w, errMediaAccessDenied)
		return
	}
	if !a.publishPlaybackInfoLease(reqCtx, &lease, r.PathValue("itemId"), baseServerID, basePlaySessionID) {
		writePlaybackDeviceLimit(w)
		return
	}
	// The first successful PlaybackInfo response owns the top-level PlaySessionId.
	// Track that server for later session routing instead of assuming the primary
	// mapping server answered successfully.
	if len(allMediaSources) > 0 {
		if virtualItemID := r.PathValue("itemId"); virtualItemID != "" {
			a.activatePlaybackRoute(reqCtx, virtualItemID, baseServerID, basePlaySessionID, clientPlaySessionID)
		}
	}
	if a.Logger != nil {
		a.Logger.Debugf("PlaybackInfo: returning %d MediaSources for itemId=%s", len(allMediaSources), r.PathValue("itemId"))
	}
	cfg := a.ConfigStore.Snapshot()
	// Rewrite top-level fields (excluding MediaSources which were already virtualised per-server above)
	delete(base, "MediaSources")
	rewriteResponseIDs(base, baseServerID, a.IDStore, cfg.Server.ID, a.clientFacingUserIDFor(r))
	base["MediaSources"] = make([]any, 0, len(allMediaSources))
	for _, mediaSource := range allMediaSources {
		base["MediaSources"] = append(base["MediaSources"].([]any), mediaSource)
	}
	a.filterAuthorizedMediaSources(reqCtx, base)
	writeJSON(w, http.StatusOK, base)
}

func deepCloneMap(source map[string]any) map[string]any {
	encoded, _ := json.Marshal(source)
	var decoded map[string]any
	_ = json.Unmarshal(encoded, &decoded)
	return decoded
}

// rewriteMediaPathIDs virtualises item and media-source IDs only when they are
// complete URL path segments. This avoids substring collisions such as item
// "15511" inside media source "mediasource_15511" while preserving query data.
func rewriteMediaPathIDs(raw, originalItemID, virtualItemID, originalMSID, virtualMSID string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	segments := strings.Split(parsed.Path, "/")
	changed := false
	for i, segment := range segments {
		switch {
		case originalMSID != "" && virtualMSID != "" && segment == originalMSID:
			segments[i] = virtualMSID
			changed = true
		case originalItemID != "" && virtualItemID != "" && segment == originalItemID:
			segments[i] = virtualItemID
			changed = true
		}
	}
	if !changed {
		return raw
	}
	parsed.Path = strings.Join(segments, "/")
	parsed.RawPath = ""
	return parsed.String()
}

// resolveMediaSourceInPath resolves a virtual MediaSourceId embedded as the first path
// segment when followed by /Subtitles/ or /Attachments/ (e.g. "{msId}/Subtitles/0/Stream.srt").
func resolveMediaSourceInPath(rest string, idStore *IDStore) string {
	slash := strings.IndexByte(rest, '/')
	if slash <= 0 {
		return rest
	}
	firstSeg := rest[:slash]
	after := rest[slash:] // includes leading '/'
	if !strings.HasPrefix(after, "/Subtitles/") && !strings.HasPrefix(after, "/Attachments/") {
		return rest
	}
	if resolved := idStore.ResolveVirtualID(firstSeg); resolved != nil {
		return resolved.OriginalID + after
	}
	return rest
}
