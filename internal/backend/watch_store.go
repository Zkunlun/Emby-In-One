package backend

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// WatchProgress stores per-proxy-user watch state for a single item.
type WatchProgress struct {
	ProxyUserID       string
	VirtualItemID     string
	ServerID          string
	OriginalItemID    string
	ItemType          string // "Movie", "Episode", etc.
	SeriesVirtualID   string // virtual ID of parent series (episodes only)
	SeriesOriginalID  string // upstream original ID of parent series
	SeriesName        string
	ParentIndexNumber int // season number
	IndexNumber       int // episode number
	Name              string
	ProductionYear    int
	ProviderTmdb      string
	PositionTicks     int64
	RuntimeTicks      int64
	Played            bool
	IsFavorite        bool
	LastPlayed        int64 // Unix milliseconds; true playback time only
	UpdatedAt         int64 // Unix milliseconds; any local user-state mutation
}

// WatchStore manages per-user watch history in SQLite.
type WatchStore struct {
	db     *sqliteDB
	mu     sync.Mutex
	logger *Logger
}

// NewWatchStore initializes the watch_progress table on the shared DB.
func NewWatchStore(db *sqliteDB, logger *Logger) (*WatchStore, error) {
	if db == nil {
		return nil, fmt.Errorf("database is nil")
	}
	// Phase 3 intentionally does not migrate legacy per-user watch state. If an
	// older table survives an upgrade (including the server_index migration path),
	// discard it and recreate the approved schema rather than mixing old semantics
	// with the new LastPlayed/UpdatedAt contract.
	hasUpdatedAt, err := tableHasColumn(db, "user_watch_progress", "updated_at")
	if err != nil {
		return nil, fmt.Errorf("watch store schema probe: %w", err)
	}
	if !hasUpdatedAt {
		if err := db.exec(`DROP TABLE IF EXISTS user_watch_progress;`); err != nil {
			return nil, fmt.Errorf("watch store reset legacy schema: %w", err)
		}
	}
	if err := db.exec(`
		CREATE TABLE IF NOT EXISTS user_watch_progress (
			proxy_user_id TEXT NOT NULL,
			virtual_item_id TEXT NOT NULL,
			server_id TEXT NOT NULL,
			original_item_id TEXT NOT NULL DEFAULT '',
			item_type TEXT NOT NULL DEFAULT '',
			series_virtual_id TEXT NOT NULL DEFAULT '',
			series_original_id TEXT NOT NULL DEFAULT '',
			series_name TEXT NOT NULL DEFAULT '',
			parent_index_number INTEGER NOT NULL DEFAULT 0,
			index_number INTEGER NOT NULL DEFAULT 0,
			name TEXT NOT NULL DEFAULT '',
			production_year INTEGER NOT NULL DEFAULT 0,
			provider_tmdb TEXT NOT NULL DEFAULT '',
			position_ticks INTEGER NOT NULL DEFAULT 0,
			runtime_ticks INTEGER NOT NULL DEFAULT 0,
			played INTEGER NOT NULL DEFAULT 0,
			is_favorite INTEGER NOT NULL DEFAULT 0,
			last_played INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (proxy_user_id, virtual_item_id)
		);
		CREATE INDEX IF NOT EXISTS idx_watch_user ON user_watch_progress(proxy_user_id);
		CREATE INDEX IF NOT EXISTS idx_watch_user_series ON user_watch_progress(proxy_user_id, series_name);
	`); err != nil {
		return nil, fmt.Errorf("watch store schema: %w", err)
	}

	// Migrate dirty data: mark items as played if they were watched >= 90%
	if err := db.exec(`
		UPDATE user_watch_progress
		SET played = 1
		WHERE position_ticks > 0
		  AND runtime_ticks > 0
		  AND played = 0
		  AND CAST(position_ticks AS REAL) / runtime_ticks >= 0.9
	`); err != nil {
		if logger != nil {
			logger.Warnf("watch store migration warning: %v", err)
		}
	}

	return &WatchStore{db: db, logger: logger}, nil
}

// RecordProgress upserts watch progress for a user+item. Metadata fields are
// only updated when non-empty (allows progressive enrichment).
func (ws *WatchStore) RecordProgress(p *WatchProgress) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	now := time.Now().UnixMilli()
	lastPlayed := p.LastPlayed
	if lastPlayed <= 0 {
		lastPlayed = now
	}
	updatedAt := p.UpdatedAt
	if updatedAt <= 0 {
		updatedAt = now
	}
	return ws.db.writeParams(`
		INSERT INTO user_watch_progress (
			proxy_user_id, virtual_item_id, server_id, original_item_id,
			item_type, series_virtual_id, series_original_id, series_name,
			parent_index_number, index_number, name, production_year, provider_tmdb,
			position_ticks, runtime_ticks, played, is_favorite, last_played, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(proxy_user_id, virtual_item_id) DO UPDATE SET
			position_ticks = excluded.position_ticks,
			runtime_ticks = CASE WHEN excluded.runtime_ticks > 0 THEN excluded.runtime_ticks ELSE runtime_ticks END,
			played = excluded.played,
			is_favorite = CASE WHEN excluded.is_favorite = 1 THEN 1 ELSE is_favorite END,
			last_played = excluded.last_played,
			updated_at = excluded.updated_at,
			server_id = CASE WHEN excluded.server_id != '' THEN excluded.server_id ELSE server_id END,
			original_item_id = CASE WHEN excluded.original_item_id != '' THEN excluded.original_item_id ELSE original_item_id END,
			item_type = CASE WHEN excluded.item_type != '' THEN excluded.item_type ELSE item_type END,
			series_virtual_id = CASE WHEN excluded.series_virtual_id != '' THEN excluded.series_virtual_id ELSE series_virtual_id END,
			series_original_id = CASE WHEN excluded.series_original_id != '' THEN excluded.series_original_id ELSE series_original_id END,
			series_name = CASE WHEN excluded.series_name != '' THEN excluded.series_name ELSE series_name END,
			parent_index_number = CASE WHEN excluded.parent_index_number > 0 THEN excluded.parent_index_number ELSE parent_index_number END,
			index_number = CASE WHEN excluded.index_number > 0 THEN excluded.index_number ELSE index_number END,
			name = CASE WHEN excluded.name != '' THEN excluded.name ELSE name END,
			production_year = CASE WHEN excluded.production_year > 0 THEN excluded.production_year ELSE production_year END,
			provider_tmdb = CASE WHEN excluded.provider_tmdb != '' THEN excluded.provider_tmdb ELSE provider_tmdb END
	`,
		p.ProxyUserID, p.VirtualItemID, p.ServerID, p.OriginalItemID,
		p.ItemType, p.SeriesVirtualID, p.SeriesOriginalID, p.SeriesName,
		p.ParentIndexNumber, p.IndexNumber, p.Name, p.ProductionYear, p.ProviderTmdb,
		p.PositionTicks, p.RuntimeTicks, boolToInt(p.Played), boolToInt(p.IsFavorite), lastPlayed, updatedAt,
	)
}

// UpsertMetadata ensures a user+item record has routable media metadata without
// changing playback/favorite state or manufacturing a playback timestamp.
func (ws *WatchStore) UpsertMetadata(p *WatchProgress) error {
	if p == nil || p.ProxyUserID == "" || p.VirtualItemID == "" {
		return fmt.Errorf("watch metadata: missing user or item id")
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	now := time.Now().UnixMilli()
	return ws.db.writeParams(`
		INSERT INTO user_watch_progress (
			proxy_user_id, virtual_item_id, server_id, original_item_id,
			item_type, series_virtual_id, series_original_id, series_name,
			parent_index_number, index_number, name, production_year, provider_tmdb,
			runtime_ticks, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(proxy_user_id, virtual_item_id) DO UPDATE SET
			server_id = CASE WHEN excluded.server_id != '' THEN excluded.server_id ELSE server_id END,
			original_item_id = CASE WHEN excluded.original_item_id != '' THEN excluded.original_item_id ELSE original_item_id END,
			item_type = CASE WHEN excluded.item_type != '' THEN excluded.item_type ELSE item_type END,
			series_virtual_id = CASE WHEN excluded.series_virtual_id != '' THEN excluded.series_virtual_id ELSE series_virtual_id END,
			series_original_id = CASE WHEN excluded.series_original_id != '' THEN excluded.series_original_id ELSE series_original_id END,
			series_name = CASE WHEN excluded.series_name != '' THEN excluded.series_name ELSE series_name END,
			parent_index_number = CASE WHEN excluded.parent_index_number > 0 THEN excluded.parent_index_number ELSE parent_index_number END,
			index_number = CASE WHEN excluded.index_number > 0 THEN excluded.index_number ELSE index_number END,
			name = CASE WHEN excluded.name != '' THEN excluded.name ELSE name END,
			production_year = CASE WHEN excluded.production_year > 0 THEN excluded.production_year ELSE production_year END,
			provider_tmdb = CASE WHEN excluded.provider_tmdb != '' THEN excluded.provider_tmdb ELSE provider_tmdb END,
			runtime_ticks = CASE WHEN excluded.runtime_ticks > 0 THEN excluded.runtime_ticks ELSE runtime_ticks END,
			updated_at = CASE WHEN updated_at > 0 THEN updated_at ELSE excluded.updated_at END
	`, p.ProxyUserID, p.VirtualItemID, p.ServerID, p.OriginalItemID,
		p.ItemType, p.SeriesVirtualID, p.SeriesOriginalID, p.SeriesName,
		p.ParentIndexNumber, p.IndexNumber, p.Name, p.ProductionYear, p.ProviderTmdb,
		p.RuntimeTicks, now)
}

// UpdatePosition updates playback position and records a true playback timestamp.
func (ws *WatchStore) UpdatePosition(proxyUserID, virtualItemID string, positionTicks, runtimeTicks int64) error {
	return ws.UpdatePositionAt(proxyUserID, virtualItemID, positionTicks, runtimeTicks, 0)
}

func (ws *WatchStore) UpdatePositionAt(proxyUserID, virtualItemID string, positionTicks, runtimeTicks, playedAt int64) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	now := time.Now().UnixMilli()
	if playedAt <= 0 {
		playedAt = now
	}
	return ws.db.writeParams(`
		UPDATE user_watch_progress SET
			position_ticks = ?,
			runtime_ticks = CASE WHEN ? > 0 THEN ? ELSE runtime_ticks END,
			last_played = ?,
			updated_at = ?
		WHERE proxy_user_id = ? AND virtual_item_id = ?
	`, positionTicks, runtimeTicks, runtimeTicks, playedAt, now, proxyUserID, virtualItemID)
}

// MarkPlayed sets the played status for a user+item. Marking played records a
// playback time; marking unplayed preserves playback history and resets resume state.
func (ws *WatchStore) MarkPlayed(proxyUserID, virtualItemID string, played bool) error {
	return ws.MarkPlayedAt(proxyUserID, virtualItemID, played, 0)
}

func (ws *WatchStore) MarkPlayedAt(proxyUserID, virtualItemID string, played bool, playedAt int64) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	now := time.Now().UnixMilli()
	if played && playedAt <= 0 {
		playedAt = now
	}
	return ws.db.writeParams(`
		INSERT INTO user_watch_progress (proxy_user_id, virtual_item_id, server_id, position_ticks, played, last_played, updated_at)
		VALUES (?, ?, '', 0, ?, ?, ?)
		ON CONFLICT(proxy_user_id, virtual_item_id) DO UPDATE SET
			position_ticks = 0,
			played = excluded.played,
			last_played = CASE WHEN excluded.played = 1 THEN excluded.last_played ELSE last_played END,
			updated_at = excluded.updated_at
	`, proxyUserID, virtualItemID, boolToInt(played), playedAt, now)
}

// SetFavorite changes only favorite state and updated_at. It must never create a
// synthetic LastPlayedDate for an item the user has never played.
func (ws *WatchStore) SetFavorite(proxyUserID, virtualItemID string, favorite bool) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	now := time.Now().UnixMilli()
	return ws.db.writeParams(`
		INSERT INTO user_watch_progress (proxy_user_id, virtual_item_id, server_id, is_favorite, updated_at)
		VALUES (?, ?, '', ?, ?)
		ON CONFLICT(proxy_user_id, virtual_item_id) DO UPDATE SET
			is_favorite = excluded.is_favorite,
			updated_at = excluded.updated_at
	`, proxyUserID, virtualItemID, boolToInt(favorite), now)
}

// GetResumeItems returns items with position > 0 and not fully played, ordered by last_played desc.
func (ws *WatchStore) GetResumeItems(proxyUserID string, limit int) ([]WatchProgress, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if limit <= 0 {
		limit = 20
	}
	stmt, err := ws.db.prepare(`
		SELECT virtual_item_id, server_id, original_item_id, item_type,
			series_virtual_id, series_original_id, series_name,
			parent_index_number, index_number, name, production_year, provider_tmdb,
			position_ticks, runtime_ticks, played, is_favorite, last_played, updated_at
		FROM (
			SELECT *, ROW_NUMBER() OVER (
				PARTITION BY CASE
					WHEN item_type = 'Episode' AND series_virtual_id != '' THEN series_virtual_id
					WHEN item_type = 'Episode' AND series_name != '' THEN series_name
					ELSE virtual_item_id
				END
				ORDER BY last_played DESC
			) AS rn
			FROM user_watch_progress
			WHERE proxy_user_id = ? AND position_ticks > 0 AND played = 0
		)
		WHERE rn = 1
		ORDER BY last_played DESC
		LIMIT ?
	`)
	if err != nil {
		return nil, err
	}
	defer stmt.finalize()
	if err := stmt.bindAll(proxyUserID, limit); err != nil {
		return nil, err
	}
	return ws.scanRows(proxyUserID, stmt)
}

// GetFavoriteItems returns all favorited items for a user.
func (ws *WatchStore) GetFavoriteItems(proxyUserID string) ([]WatchProgress, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	stmt, err := ws.db.prepare(`
		SELECT virtual_item_id, server_id, original_item_id, item_type,
			series_virtual_id, series_original_id, series_name,
			parent_index_number, index_number, name, production_year, provider_tmdb,
			position_ticks, runtime_ticks, played, is_favorite, last_played, updated_at
		FROM user_watch_progress
		WHERE proxy_user_id = ? AND is_favorite = 1
		ORDER BY updated_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer stmt.finalize()
	if err := stmt.bindAll(proxyUserID); err != nil {
		return nil, err
	}
	return ws.scanRows(proxyUserID, stmt)
}

// GetPlayedItems returns every item the user has marked as played.
func (ws *WatchStore) GetPlayedItems(proxyUserID string) ([]WatchProgress, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	stmt, err := ws.db.prepare(`
		SELECT virtual_item_id, server_id, original_item_id, item_type,
			series_virtual_id, series_original_id, series_name,
			parent_index_number, index_number, name, production_year, provider_tmdb,
			position_ticks, runtime_ticks, played, is_favorite, last_played, updated_at
		FROM user_watch_progress
		WHERE proxy_user_id = ? AND played = 1
		ORDER BY last_played DESC
	`)
	if err != nil {
		return nil, err
	}
	defer stmt.finalize()
	if err := stmt.bindAll(proxyUserID); err != nil {
		return nil, err
	}
	return ws.scanRows(proxyUserID, stmt)
}

// GetResumableItems returns the items the user is part-way through. It is the same
// predicate as GetResumeItems without the one-episode-per-series fold, because a
// list filter has to see every matching item to page and count them.
func (ws *WatchStore) GetResumableItems(proxyUserID string) ([]WatchProgress, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	stmt, err := ws.db.prepare(`
		SELECT virtual_item_id, server_id, original_item_id, item_type,
			series_virtual_id, series_original_id, series_name,
			parent_index_number, index_number, name, production_year, provider_tmdb,
			position_ticks, runtime_ticks, played, is_favorite, last_played, updated_at
		FROM user_watch_progress
		WHERE proxy_user_id = ? AND position_ticks > 0 AND played = 0
		ORDER BY last_played DESC
	`)
	if err != nil {
		return nil, err
	}
	defer stmt.finalize()
	if err := stmt.bindAll(proxyUserID); err != nil {
		return nil, err
	}
	return ws.scanRows(proxyUserID, stmt)
}

// GetProgress returns the watch progress for a specific user+item, or nil if not found.
func (ws *WatchStore) GetProgress(proxyUserID, virtualItemID string) *WatchProgress {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	stmt, err := ws.db.prepare(`
		SELECT virtual_item_id, server_id, original_item_id, item_type,
			series_virtual_id, series_original_id, series_name,
			parent_index_number, index_number, name, production_year, provider_tmdb,
			position_ticks, runtime_ticks, played, is_favorite, last_played, updated_at
		FROM user_watch_progress
		WHERE proxy_user_id = ? AND virtual_item_id = ?
	`)
	if err != nil {
		return nil
	}
	defer stmt.finalize()
	if err := stmt.bindAll(proxyUserID, virtualItemID); err != nil {
		return nil
	}
	rows, _ := ws.scanRows(proxyUserID, stmt)
	if len(rows) == 0 {
		return nil
	}
	return &rows[0]
}

// GetNextUpSeries returns one row per series the user has played episodes of,

// GetProgressBatch returns local rows for the requested virtual IDs. The query is
// chunked to stay comfortably below SQLite's parameter limit.
func (ws *WatchStore) GetProgressBatch(proxyUserID string, virtualItemIDs []string) map[string]WatchProgress {
	result := make(map[string]WatchProgress)
	if proxyUserID == "" || len(virtualItemIDs) == 0 {
		return result
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	const chunkSize = 400
	for start := 0; start < len(virtualItemIDs); start += chunkSize {
		end := start + chunkSize
		if end > len(virtualItemIDs) {
			end = len(virtualItemIDs)
		}
		ids := virtualItemIDs[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		query := `SELECT virtual_item_id, server_id, original_item_id, item_type,
			series_virtual_id, series_original_id, series_name,
			parent_index_number, index_number, name, production_year, provider_tmdb,
			position_ticks, runtime_ticks, played, is_favorite, last_played, updated_at
			FROM user_watch_progress WHERE proxy_user_id = ? AND virtual_item_id IN (` + placeholders + `)`
		stmt, err := ws.db.prepare(query)
		if err != nil {
			continue
		}
		args := make([]any, 0, len(ids)+1)
		args = append(args, proxyUserID)
		for _, id := range ids {
			args = append(args, id)
		}
		if err := stmt.bindAll(args...); err == nil {
			if rows, err := ws.scanRows(proxyUserID, stmt); err == nil {
				for _, row := range rows {
					result[row.VirtualItemID] = row
				}
			}
		}
		stmt.finalize()
	}
	return result
}

// GetNextUpSeries returns one row per series the user has played episodes of,
// with the most-recently-played episode per series.
func (ws *WatchStore) GetNextUpSeries(proxyUserID string) ([]WatchProgress, error) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	stmt, err := ws.db.prepare(`
		SELECT virtual_item_id, server_id, original_item_id, item_type,
			series_virtual_id, series_original_id, series_name,
			parent_index_number, index_number, name, production_year, provider_tmdb,
			position_ticks, runtime_ticks, played, is_favorite, last_played, updated_at
		FROM user_watch_progress
		WHERE proxy_user_id = ? AND item_type = 'Episode' AND series_name != ''
			AND (played = 1 OR position_ticks > 0)
		ORDER BY series_name, parent_index_number DESC, index_number DESC
	`)
	if err != nil {
		return nil, err
	}
	defer stmt.finalize()
	if err := stmt.bindAll(proxyUserID); err != nil {
		return nil, err
	}
	all, err := ws.scanRows(proxyUserID, stmt)
	if err != nil {
		return nil, err
	}
	// Deduplicate: keep only the highest-numbered episode per series. The series identity
	// is the virtual ID, not the display name — two different series can share a name, and
	// folding them together would hide one of them from Continue Watching.
	seen := map[string]bool{}
	result := make([]WatchProgress, 0)
	for _, row := range all {
		key := row.SeriesVirtualID
		if key == "" {
			key = row.SeriesName
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, row)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].LastPlayed > result[j].LastPlayed })
	return result, nil
}

// GetNextUpForSeries returns the highest-numbered episode the user has watched
// (played or in-progress) for a specific series, identified by virtual series ID.
func (ws *WatchStore) GetNextUpForSeries(proxyUserID, seriesVirtualID string) *WatchProgress {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	stmt, err := ws.db.prepare(`
		SELECT virtual_item_id, server_id, original_item_id, item_type,
			series_virtual_id, series_original_id, series_name,
			parent_index_number, index_number, name, production_year, provider_tmdb,
			position_ticks, runtime_ticks, played, is_favorite, last_played, updated_at
		FROM user_watch_progress
		WHERE proxy_user_id = ? AND series_virtual_id = ? AND item_type = 'Episode'
			AND (played = 1 OR position_ticks > 0)
		ORDER BY parent_index_number DESC, index_number DESC
		LIMIT 1
	`)
	if err != nil {
		return nil
	}
	defer stmt.finalize()
	if err := stmt.bindAll(proxyUserID, seriesVirtualID); err != nil {
		return nil
	}
	rows, _ := ws.scanRows(proxyUserID, stmt)
	if len(rows) == 0 {
		return nil
	}
	return &rows[0]
}

// DeleteUser removes all watch data for a proxy user.
func (ws *WatchStore) DeleteUser(proxyUserID string) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.db.writeParams(`DELETE FROM user_watch_progress WHERE proxy_user_id = ?`, proxyUserID)
}

// DeleteVirtualItems removes state only for media identities that no longer have
// any upstream instance. Server deletion must not erase a surviving virtual item.
func (ws *WatchStore) DeleteVirtualItems(virtualItemIDs []string) error {
	if len(virtualItemIDs) == 0 {
		return nil
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	const chunkSize = 400
	for start := 0; start < len(virtualItemIDs); start += chunkSize {
		end := start + chunkSize
		if end > len(virtualItemIDs) {
			end = len(virtualItemIDs)
		}
		ids := virtualItemIDs[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		args := make([]any, 0, len(ids))
		for _, id := range ids {
			args = append(args, id)
		}
		if err := ws.db.writeParams(`DELETE FROM user_watch_progress WHERE virtual_item_id IN (`+placeholders+`)`, args...); err != nil {
			return err
		}
	}
	return nil
}

// DeleteServerData is retained for compatibility with callers outside the upstream
// deletion flow. New server deletion code must use DeleteVirtualItems with the
// orphan list returned by IDStore.
func (ws *WatchStore) DeleteServerData(serverID string) error {
	if serverID == "" {
		return nil
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.db.writeParams(`DELETE FROM user_watch_progress WHERE server_id = ?`, serverID)
}

func (ws *WatchStore) scanRows(proxyUserID string, stmt *sqliteStmt) ([]WatchProgress, error) {
	var results []WatchProgress
	for {
		hasRow, err := stmt.step()
		if err != nil {
			return nil, err
		}
		if !hasRow {
			break
		}
		results = append(results, WatchProgress{
			ProxyUserID:       proxyUserID,
			VirtualItemID:     stmt.columnText(0),
			ServerID:          stmt.columnText(1),
			OriginalItemID:    stmt.columnText(2),
			ItemType:          stmt.columnText(3),
			SeriesVirtualID:   stmt.columnText(4),
			SeriesOriginalID:  stmt.columnText(5),
			SeriesName:        stmt.columnText(6),
			ParentIndexNumber: stmt.columnInt(7),
			IndexNumber:       stmt.columnInt(8),
			Name:              stmt.columnText(9),
			ProductionYear:    stmt.columnInt(10),
			ProviderTmdb:      stmt.columnText(11),
			PositionTicks:     stmt.columnInt64(12),
			RuntimeTicks:      stmt.columnInt64(13),
			Played:            stmt.columnInt(14) != 0,
			IsFavorite:        stmt.columnInt(15) != 0,
			LastPlayed:        stmt.columnInt64(16),
			UpdatedAt:         stmt.columnInt64(17),
		})
	}
	return results, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
