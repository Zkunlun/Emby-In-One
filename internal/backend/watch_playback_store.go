package backend

import (
	"fmt"
	"time"
)

// RecordPlaybackProgress applies an automatic playback event in one SQLite
// statement. Unlike the generic RecordProgress API, it never clears Played.
// Explicit MarkPlayed/UpdatePosition operations keep their existing semantics.
//
// The caller must resolve event.Source and qualify duration/session candidates
// before calling. The supplied item type must also be known before completion.
// Metadata runtime/position/Played fields are not used as proof of completion;
// a bare historical duration has no media-version identity.
func (ws *WatchStore) RecordPlaybackProgress(metadata *WatchProgress, event playbackWatchEvent) error {
	return ws.recordPlaybackProgress(metadata, event, false)
}

// inherited keeps completion evidence on the admitted A event, while storing
// only the surviving B locator. It never labels A duration as B duration.
func (ws *WatchStore) recordPlaybackProgress(metadata *WatchProgress, event playbackWatchEvent, inherited bool) error {
	if ws == nil || ws.db == nil || metadata == nil ||
		metadata.ProxyUserID == "" || metadata.VirtualItemID == "" ||
		metadata.ServerID == "" || metadata.OriginalItemID == "" {
		return fmt.Errorf("watch playback: missing user or item route")
	}
	if event.Kind > playbackWatchStopped || !event.Source.valid() ||
		(!inherited && (event.Source.ServerID != metadata.ServerID ||
		event.Source.OriginalItemID != metadata.OriginalItemID)) {
		return fmt.Errorf("watch playback: invalid event source")
	}

	position := int64(0)
	hasPosition := event.Position.valid()
	if hasPosition {
		position = event.Position.Ticks
	}
	// Persist only the qualified duration for this event. If none is available,
	// keeping a historical version's runtime would also misstate PlayedPercentage.
	runtime := int64(0)
	if !inherited && event.Runtime.valid() && event.Runtime.Ticks > 0 {
		runtime = event.Runtime.Ticks
	}
	candidate := event.completionCandidate() && autoPlayedItemType(metadata.ItemType)
	insertPlayed := candidate
	insertPosition := position
	if insertPlayed {
		insertPosition = 0
	}
	// All manual watch mutations use this lock too. SQLite also serializes this
	// single upsert with writes from other stores on the shared connection.
	ws.mu.Lock()
	defer ws.mu.Unlock()
	now := time.Now().UnixMilli()
	lastPlayed := metadata.LastPlayed
	if lastPlayed <= 0 {
		lastPlayed = now
	}
	return ws.db.writeParams(`
		INSERT INTO user_watch_progress (
			proxy_user_id, virtual_item_id, server_id, original_item_id,
			item_type, series_virtual_id, series_original_id, series_name,
			parent_index_number, index_number, name, production_year, provider_tmdb,
			position_ticks, runtime_ticks, played, last_played, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(proxy_user_id, virtual_item_id) DO UPDATE SET
			position_ticks = CASE
				WHEN user_watch_progress.played = 1 OR ? = 1
				THEN 0
				WHEN ? = 1 THEN ?
				ELSE user_watch_progress.position_ticks
			END,
			runtime_ticks = excluded.runtime_ticks,
			played = CASE
				WHEN ? = 1
				THEN 1 ELSE user_watch_progress.played
			END,
			last_played = excluded.last_played,
			updated_at = excluded.updated_at,
			server_id = excluded.server_id,
			original_item_id = excluded.original_item_id,
			item_type = CASE WHEN excluded.item_type != '' THEN excluded.item_type ELSE user_watch_progress.item_type END,
			series_virtual_id = CASE WHEN excluded.series_virtual_id != '' THEN excluded.series_virtual_id ELSE user_watch_progress.series_virtual_id END,
			series_original_id = CASE WHEN excluded.series_original_id != '' THEN excluded.series_original_id ELSE user_watch_progress.series_original_id END,
			series_name = CASE WHEN excluded.series_name != '' THEN excluded.series_name ELSE user_watch_progress.series_name END,
			parent_index_number = CASE WHEN excluded.parent_index_number > 0 THEN excluded.parent_index_number ELSE user_watch_progress.parent_index_number END,
			index_number = CASE WHEN excluded.index_number > 0 THEN excluded.index_number ELSE user_watch_progress.index_number END,
			name = CASE WHEN excluded.name != '' THEN excluded.name ELSE user_watch_progress.name END,
			production_year = CASE WHEN excluded.production_year > 0 THEN excluded.production_year ELSE user_watch_progress.production_year END,
			provider_tmdb = CASE WHEN excluded.provider_tmdb != '' THEN excluded.provider_tmdb ELSE user_watch_progress.provider_tmdb END
	`,
		metadata.ProxyUserID, metadata.VirtualItemID, metadata.ServerID, metadata.OriginalItemID,
		metadata.ItemType, metadata.SeriesVirtualID, metadata.SeriesOriginalID, metadata.SeriesName,
		metadata.ParentIndexNumber, metadata.IndexNumber, metadata.Name, metadata.ProductionYear, metadata.ProviderTmdb,
		insertPosition, runtime, boolToInt(insertPlayed), lastPlayed, now,
		boolToInt(candidate), boolToInt(hasPosition), position, boolToInt(candidate),
	)
}
