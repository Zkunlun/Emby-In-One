package backend

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const playbackWatchMetadataTimeout = 2 * time.Second

func watchSession(reqCtx *RequestContext, source playbackWatchSource, playSessionID string) playbackWatchSession {
	return playbackWatchSession{
		ProxyUserID: reqCtx.ProxyUser.UserID, PlaybackDeviceID: playbackDeviceID(reqCtx),
		PlaySessionID: playSessionID, Source: source,
	}
}

func (a *App) watchItemIdentity(itemID, serverID string) (string, string) {
	if a.IDStore == nil {
		return "", ""
	}
	if resolved := a.IDStore.ResolveVirtualID(itemID); resolved != nil {
		return a.IDStore.CanonicalMergeID(itemID), resolvedOriginalIDForServer(resolved, serverID)
	}
	virtualID, resolved, ok := a.IDStore.ResolveOriginalIDForServer(itemID, serverID)
	if !ok {
		groups := a.IDStore.MergeGroupsForItem(serverID, itemID)
		if len(groups) != 1 {
			return "", ""
		}
		resolved = a.IDStore.ResolveVirtualID(groups[0])
		if resolved == nil {
			return "", ""
		}
		return groups[0], resolvedOriginalIDForServer(resolved, serverID)
	}
	return a.IDStore.CanonicalMergeID(virtualID), resolved.OriginalID
}

func (a *App) watchItemOriginalID(itemID, serverID string) string {
	_, originalID := a.watchItemIdentity(itemID, serverID)
	return originalID
}

// The event has already passed its route's upstream-success/terminal gate.
// Its source is the actual translated source, never a newly guessed upstream.
func (a *App) recordPlaybackWatchEvent(r *http.Request, virtualItemID, playSessionID string, event playbackWatchEvent) {
	reqCtx := requestContextFrom(r.Context())
	if a.WatchStore == nil || reqCtx == nil || reqCtx.ProxyUser == nil ||
		reqCtx.ProxyUser.Role == "admin" || virtualItemID == "" || !event.Source.valid() {
		return
	}
	admission := playbackWatchAdmissionFrom(r)
	if admission == nil || event.Kind != admission.Kind ||
		!samePlaybackWatchIdentity(watchSession(reqCtx, event.Source, playSessionID), admission.Session) {
		return
	}
	virtualItemID = admission.SharedKey.ItemID
	snapshot := admission.Snapshot
	session := admission.Session
	if event.Source.MediaSourceID == "" {
		event.Source.MediaSourceID = session.Source.MediaSourceID
	}
	if session.Source.MediaSourceID != "" && event.Source.MediaSourceID != session.Source.MediaSourceID {
		return
	}
	session.Source = event.Source
	metadata := snapshot.Value.Metadata
	metadata.ProxyUserID, metadata.VirtualItemID = reqCtx.ProxyUser.UserID, virtualItemID
	metadata.ServerID, metadata.OriginalItemID = event.Source.ServerID, event.Source.OriginalItemID
	metadata.LastPlayed, metadata.UpdatedAt = 0, 0
	// Reuse only route-matching type/series metadata. Historical runtime is never
	// imported here: the table has no media-version identity.
	if metadata.ItemType == "" {
		if stored := a.WatchStore.GetProgress(metadata.ProxyUserID, virtualItemID); stored != nil &&
			stored.ServerID == metadata.ServerID && stored.OriginalItemID == metadata.OriginalItemID {
			metadata.ItemType = stored.ItemType
			metadata.Name, metadata.ProductionYear, metadata.ProviderTmdb = stored.Name, stored.ProductionYear, stored.ProviderTmdb
			metadata.SeriesVirtualID, metadata.SeriesOriginalID, metadata.SeriesName = stored.SeriesVirtualID, stored.SeriesOriginalID, stored.SeriesName
			metadata.ParentIndexNumber, metadata.IndexNumber = stored.ParentIndexNumber, stored.IndexNumber
		}
	}
	candidate := snapshot.Value.Runtime
	if event.Kind == playbackWatchStarted && !event.Failed && session.identified() && candidate.matches(event.Source) {
		if !a.confirmPlaybackWatchStarted(reqCtx, admission, session) &&
			configuredSourceIDs(a.ConfigStore.Snapshot())[event.Source.ServerID] {
			return
		}
	}
	runtime := resolvePlaybackRuntime(event.Source, event.Runtime, candidate)
	live := event.Live || snapshot.Value.Live
	// Reported duration alone does not prove that this version belongs to the film.
	if (metadata.ItemType == "" || !runtime.valid() || !candidate.matches(event.Source)) && a.watchPlayback.claimFetch(snapshot, event.Kind) {
		fetched := metadata
		fetchedRuntime, fetchedLive, success := a.fetchPlaybackWatchMetadata(r, reqCtx, &fetched, event.Source)
		a.watchPlayback.finishFetch(snapshot, fetched, fetchedRuntime, fetchedLive, success)
		if success {
			metadata = fetched
			live = live || fetchedLive
			if fetchedRuntime.Source.valid() {
				candidate = fetchedRuntime
			}
			runtime = resolvePlaybackRuntime(event.Source, event.Runtime, candidate)
		}
	}
	// A uniquely identified metadata source can qualify a client that omitted
	// MediaSourceId, including an exact same-session final-position fallback.
	if event.Source.MediaSourceID == "" && candidate.matches(event.Source) &&
		candidate.SingleMediaSource && candidate.Source.MediaSourceID != "" {
		event.Source.MediaSourceID = candidate.Source.MediaSourceID
		session.Source = event.Source
	}
	if !session.identified() || !candidate.matches(event.Source) {
		return // No version/session proof: do not guess a shared writer.
	}
	event.Runtime, event.Live = runtime, live
	event.Position = resolvePlaybackPosition(event.Kind, event.Position, session, snapshot.Value.Position)
	// Recheck current authorization and surviving identity after all network I/O.
	// Phase4 management takes the write side of this gate before publishing changes.
	a.watchLifecycleMu.RLock()
	scope := a.mediaAccessScopeLocked(reqCtx)
	_, originalID := a.watchItemIdentity(virtualItemID, event.Source.ServerID)
	var err error
	if scope.allows(event.Source.ServerID) && originalID == event.Source.OriginalItemID && a.IDStore.MergeMemberAllowed(virtualItemID, event.Source.ServerID, originalID, event.Source.MediaSourceID) {
		err = a.watchPlayback.commitAdmitted(admission, metadata, event, session, func() error {
			if err := a.WatchStore.SeedMergeState(metadata.ProxyUserID, virtualItemID); err != nil {
				return err
			}
			return a.WatchStore.RecordPlaybackProgress(&metadata, event)
		})
	} else if inherited, ok := a.inheritedWatchTargetLocked(reqCtx, admission, metadata, event); ok {
		err = a.watchPlayback.commitInherited(admission, event, session, func() error {
			return a.WatchStore.recordPlaybackProgress(&inherited, event, true)
		})
	}
	a.watchLifecycleMu.RUnlock()
	if err != nil && a.Logger != nil {
		a.Logger.Warnf("WatchStore playback record error: %s", redactURLInError(err))
	}
}

func (a *App) fetchPlaybackWatchMetadata(r *http.Request, reqCtx *RequestContext, metadata *WatchProgress, source playbackWatchSource) (playbackRuntimeCandidate, bool, bool) {
	empty := playbackRuntimeCandidate{}
	if a.Upstream == nil {
		return empty, false, false
	}
	client := a.Upstream.ClientByID(source.ServerID)
	if client == nil || !client.IsOnline() || !a.isServerAllowed(reqCtx, source.ServerID) {
		return empty, false, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), playbackWatchMetadataTimeout)
	defer cancel()
	query := url.Values{"Fields": {"ProviderIds,MediaSources"}}
	payload, err := client.RequestJSON(ctx, reqCtx, a.Identity, http.MethodGet,
		"/Users/"+client.clientUserID()+"/Items/"+source.OriginalItemID, query, nil)
	if err != nil {
		return empty, false, false
	}
	item, ok := payload.(map[string]any)
	if !ok {
		return empty, false, false
	}
	if id, _ := item["Id"].(string); id != "" && id != source.OriginalItemID {
		return empty, false, false
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	if !a.mediaAccessScopeLocked(reqCtx).allows(source.ServerID) ||
		a.watchItemOriginalID(metadata.VirtualItemID, source.ServerID) != source.OriginalItemID {
		return empty, false, false
	}
	a.populateWatchProgressMetadata(metadata, item, source.ServerID)
	sources := asItems(map[string]any{"Items": item["MediaSources"]})
	for _, mediaSource := range sources {
		if itemID, _ := mediaSource["ItemId"].(string); itemID != "" && itemID != source.OriginalItemID {
			continue
		}
		id, _ := mediaSource["Id"].(string)
		if id == "" {
			continue
		}
		if (source.MediaSourceID != "" && id == source.MediaSourceID) ||
			(source.MediaSourceID == "" && len(sources) == 1) {
			selected := source
			selected.MediaSourceID = id
			ticks := playbackTicksFromBody(mediaSource, "RunTimeTicks")
			// With exactly one identified version, the item's runtime can fill a
			// missing source runtime. Multiple versions cannot use that fallback.
			if (!ticks.valid() || ticks.Ticks <= 0) && len(sources) == 1 {
				ticks = playbackTicksFromBody(item, "RunTimeTicks")
			}
			candidate := playbackRuntimeCandidate{Source: selected, SingleMediaSource: len(sources) == 1}
			if ticks.valid() {
				candidate.Ticks = ticks.Ticks
			}
			return candidate, playbackWatchMediaLive(mediaSource), true
		}
	}
	// No media-source list is not proof that an item has only one version.
	return empty, false, true
}

func playbackWatchMediaLive(mediaSource map[string]any) bool {
	infinite, _ := mediaSource["IsInfiniteStream"].(bool)
	id, _ := mediaSource["LiveStreamId"].(string)
	return infinite || id != ""
}

func (a *App) rememberPlaybackInfoWatch(reqCtx *RequestContext, virtualItemID, serverID, originalItemID, playSessionID string, mediaSources []map[string]any) {
	if reqCtx == nil || reqCtx.ProxyUser == nil || reqCtx.ProxyUser.Role == "admin" || a.WatchStore == nil {
		return
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	if !a.mediaAccessScopeLocked(reqCtx).allows(serverID) || a.watchItemOriginalID(virtualItemID, serverID) != originalItemID {
		return
	}
	virtualItemID = a.IDStore.CanonicalMergeID(virtualItemID)
	metadata := WatchProgress{ProxyUserID: reqCtx.ProxyUser.UserID, VirtualItemID: virtualItemID,
		ServerID: serverID, OriginalItemID: originalItemID}
	if stored := a.WatchStore.GetProgress(metadata.ProxyUserID, virtualItemID); stored != nil &&
		stored.ServerID == serverID && stored.OriginalItemID == originalItemID {
		metadata = *stored
		metadata.RuntimeTicks, metadata.PositionTicks = 0, 0
	}
	for _, mediaSource := range mediaSources {
		if itemID, _ := mediaSource["ItemId"].(string); itemID != "" && itemID != originalItemID {
			continue
		}
		id, _ := mediaSource["Id"].(string)
		if id == "" {
			continue
		}
		source := playbackWatchSource{ServerID: serverID, OriginalItemID: originalItemID, MediaSourceID: id}
		candidate := playbackRuntimeCandidate{Source: source, SingleMediaSource: len(mediaSources) == 1}
		ticks := playbackTicksFromBody(mediaSource, "RunTimeTicks")
		if ticks.valid() {
			candidate.Ticks = ticks.Ticks
		}
		session := watchSession(reqCtx, source, playSessionID)
		a.watchPlayback.remember(playbackWatchCacheKey{Session: session, VirtualItemID: virtualItemID},
			metadata, candidate, playbackWatchMediaLive(mediaSource))
		if len(mediaSources) == 1 {
			session.Source.MediaSourceID = ""
			a.watchPlayback.remember(playbackWatchCacheKey{Session: session, VirtualItemID: virtualItemID},
				metadata, candidate, playbackWatchMediaLive(mediaSource))
		}
	}
}

func (a *App) mutateLocalPlayed(userID, virtualItemID string, played bool, playedAt int64, contexts ...*RequestContext) error {
	virtualItemID = a.IDStore.CanonicalMergeID(virtualItemID)
	if len(contexts) > 0 {
		return a.authenticatedWatchMutation(contexts[0], virtualItemID, func() error {
			return a.mutateLocalPlayed(userID, virtualItemID, played, playedAt)
		})
	}
	return a.watchPlayback.mutate(userID, virtualItemID, true, func() error {
		if err := a.WatchStore.SeedMergeState(userID, virtualItemID); err != nil {
			return err
		}
		return a.WatchStore.MarkPlayedAt(userID, virtualItemID, played, playedAt)
	})
}

// Group explicit UserData operations under the cache-reset guard while preserving
// the old false -> position -> favorite -> true order and response messages.
func (a *App) mutateLocalUserData(userID, virtualItemID string, played *bool, position *int64, runtime int64, favorite *bool, playedAt int64, contexts ...*RequestContext) error {
	virtualItemID = a.IDStore.CanonicalMergeID(virtualItemID)
	if len(contexts) > 0 {
		return a.authenticatedWatchMutation(contexts[0], virtualItemID, func() error {
			return a.mutateLocalUserData(userID, virtualItemID, played, position, runtime, favorite, playedAt)
		})
	}
	return a.watchPlayback.mutate(userID, virtualItemID, played != nil || position != nil, func() error {
		if err := a.WatchStore.SeedMergeState(userID, virtualItemID); err != nil {
			return err
		}
		if played != nil && !*played {
			if err := a.WatchStore.MarkPlayedAt(userID, virtualItemID, false, playedAt); err != nil {
				return fmt.Errorf("Failed to update local watched state")
			}
		}
		if position != nil {
			if err := a.WatchStore.UpdatePositionAt(userID, virtualItemID, *position, runtime, playedAt); err != nil {
				return fmt.Errorf("Failed to update local playback position")
			}
		}
		if favorite != nil {
			if err := a.WatchStore.SetFavorite(userID, virtualItemID, *favorite); err != nil {
				return fmt.Errorf("Failed to update local favorite state")
			}
		}
		if played != nil && *played {
			if err := a.WatchStore.MarkPlayedAt(userID, virtualItemID, true, playedAt); err != nil {
				return fmt.Errorf("Failed to update local watched state")
			}
		}
		return nil
	})
}
