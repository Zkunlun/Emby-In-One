package backend

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Existing raw WatchStore methods remain for internal metadata/state work.
// Client-facing reads use the scoped methods below, never row.server_id as
// authorization. Scope and query must be captured under withVisibleWatchScope.
const visibleWatchColumns = `virtual_item_id, server_id, original_item_id, item_type,
	series_virtual_id, series_original_id, series_name,
	parent_index_number, index_number, name, production_year, provider_tmdb,
	position_ticks, runtime_ticks, played, is_favorite, last_played, updated_at`

// A delimited hexadecimal set is one bound TEXT value, so large source sets do
// not exhaust SQLite's variable limit alongside a batch of item IDs. Delimiters
// cannot occur in encoded IDs. SQLite hex() and Go upper-case hex agree bytewise;
// instr() is exact membership, not substring matching of raw server IDs.
func watchScopeServerSet(serverIDs []string) string {
	var encoded strings.Builder
	encoded.WriteByte('|')
	seen := make(map[string]struct{}, len(serverIDs))
	for _, id := range serverIDs {
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		encoded.WriteString(strings.ToUpper(hex.EncodeToString([]byte(id))))
		encoded.WriteByte('|')
	}
	return encoded.String()
}

// Require a surviving primary mapping even when visibility comes from a
// secondary instance; an orphan additional row cannot resurrect deleted media.
// Both EXISTS lookups retain their virtual-ID index and use bound scope values.
const visibleWatchPredicate = `EXISTS (
	SELECT 1 FROM id_mappings AS mapping
	WHERE mapping.virtual_id = w.virtual_item_id AND (
		(mapping.original_id != '' AND instr(?, '|' || hex(mapping.server_id) || '|') > 0)
		OR EXISTS (
			SELECT 1 FROM id_additional_instances AS instance
			WHERE instance.virtual_id = mapping.virtual_id AND instance.original_id != ''
				AND instr(?, '|' || hex(instance.server_id) || '|') > 0
		)
	)
)`

func visibleWatchWhere(scope mediaAccessScope) (string, []any) {
	servers := watchScopeServerSet(scope.serverIDs)
	return "w.proxy_user_id = ? AND " + visibleWatchPredicate,
		[]any{scope.userID, scope.userID, servers, servers}
}

// readVisibleWatchSQL serializes this cross-table read with connection-scoped
// transactions. Lock order is WatchStore.mu -> db.writeMu; no store callbacks
// may execute while the database lock is held.
func (ws *WatchStore) readVisibleWatchSQL(scope mediaAccessScope, query string, args []any) ([]WatchProgress, error) {
	if scope.userID == "" || len(scope.serverIDs) == 0 {
		return []WatchProgress{}, nil
	}
	if ws == nil || ws.db == nil {
		return nil, fmt.Errorf("visible watch query: database unavailable")
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	var rows []WatchProgress
	err := ws.db.withWriteLock(func() error {
		stmt, err := ws.db.prepare(query)
		if err != nil {
			return err
		}
		defer stmt.finalize()
		if err := stmt.bindAll(args...); err != nil {
			return err
		}
		rows, err = ws.scanRows(scope.userID, stmt)
		return err
	})
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []WatchProgress{}
	}
	return rows, nil
}

// condition/order are fixed internal SQL fragments, never client-supplied text.
func (ws *WatchStore) visibleWatchRows(scope mediaAccessScope, condition, order string, values ...any) ([]WatchProgress, error) {
	where, args := visibleWatchWhere(scope)
	query := "SELECT " + visibleWatchColumns + " FROM " + mergedVisibleWatchTable + " WHERE " + where
	if condition != "" {
		query += " AND (" + condition + ")"
	}
	query += order
	args = append(args, values...)
	return ws.readVisibleWatchSQL(scope, query, args)
}

func (ws *WatchStore) GetVisibleFavoriteItems(scope mediaAccessScope) ([]WatchProgress, error) {
	return ws.visibleWatchRows(scope, "w.is_favorite = 1", " ORDER BY w.updated_at DESC, w.virtual_item_id")
}

func (ws *WatchStore) GetVisiblePlayedItems(scope mediaAccessScope) ([]WatchProgress, error) {
	return ws.visibleWatchRows(scope, "w.played = 1", " ORDER BY w.last_played DESC, w.virtual_item_id")
}

func (ws *WatchStore) GetVisibleResumableItems(scope mediaAccessScope) ([]WatchProgress, error) {
	return ws.visibleWatchRows(scope, "w.position_ticks > 0 AND w.played = 0", " ORDER BY w.last_played DESC, w.virtual_item_id")
}

// Filter before ROW_NUMBER and LIMIT: an unauthorized episode must not displace
// an authorized episode of the same series or consume the requested page.
func (ws *WatchStore) GetVisibleResumeItems(scope mediaAccessScope, limit int) ([]WatchProgress, error) {
	if limit <= 0 {
		limit = 20
	}
	return ws.visibleResumeRows(scope, "", limit)
}

// Read the authorized candidates before metadata enrichment and client paging.
// ParentId must be applied before series folding, never after an unrelated LIMIT.
func (ws *WatchStore) GetVisibleResumeCandidates(scope mediaAccessScope, parentID string) ([]WatchProgress, error) {
	return ws.visibleResumeRows(scope, parentID, 0)
}

func (ws *WatchStore) visibleResumeRows(scope mediaAccessScope, parentID string, limit int) ([]WatchProgress, error) {
	where, args := visibleWatchWhere(scope)
	if parentID != "" {
		where += " AND w.series_virtual_id = ?"
		args = append(args, parentID)
	}
	query := "SELECT " + visibleWatchColumns + ` FROM (
		SELECT w.*, ROW_NUMBER() OVER (
			PARTITION BY CASE
				WHEN w.item_type = 'Episode' AND w.series_virtual_id != '' THEN w.series_virtual_id
				WHEN w.item_type = 'Episode' AND w.series_name != '' THEN w.series_name
				ELSE w.virtual_item_id
			END
			ORDER BY w.last_played DESC, w.virtual_item_id
		) AS rn
		FROM ` + mergedVisibleWatchTable + ` WHERE ` + where + `
			AND w.position_ticks > 0 AND w.played = 0
	) WHERE rn = 1 ORDER BY last_played DESC, virtual_item_id`
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	return ws.readVisibleWatchSQL(scope, query, args)
}

func (ws *WatchStore) GetVisibleNextUpSeries(scope mediaAccessScope) ([]WatchProgress, error) {
	rows, err := ws.visibleWatchRows(scope,
		"w.item_type = 'Episode' AND w.series_name != '' AND (w.played = 1 OR w.position_ticks > 0)",
		" ORDER BY w.series_name, w.parent_index_number DESC, w.index_number DESC, w.virtual_item_id")
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	result := make([]WatchProgress, 0, len(rows))
	for _, row := range rows {
		key := row.SeriesVirtualID
		if key == "" {
			key = row.SeriesName
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, row)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].LastPlayed > result[j].LastPlayed })
	return result, nil
}

func (ws *WatchStore) GetVisibleNextUpForSeries(scope mediaAccessScope, seriesVirtualID string) (*WatchProgress, error) {
	if seriesVirtualID == "" {
		return nil, nil
	}
	rows, err := ws.visibleWatchRows(scope,
		"w.series_virtual_id = ? AND w.item_type = 'Episode' AND (w.played = 1 OR w.position_ticks > 0)",
		" ORDER BY w.parent_index_number DESC, w.index_number DESC, w.virtual_item_id LIMIT 1", seriesVirtualID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

func (ws *WatchStore) GetVisibleProgress(scope mediaAccessScope, virtualItemID string) (*WatchProgress, error) {
	if virtualItemID == "" {
		return nil, nil
	}
	rows, err := ws.visibleWatchRows(scope, "w.virtual_item_id = ?", "", virtualItemID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// Batch errors are explicit: callers must not reinterpret a failed query as
// "unplayed". Chunks all share one watch/database read lock and one scope.
func (ws *WatchStore) GetVisibleProgressBatch(scope mediaAccessScope, virtualItemIDs []string) (map[string]WatchProgress, error) {
	result := make(map[string]WatchProgress)
	if scope.userID == "" || len(scope.serverIDs) == 0 || len(virtualItemIDs) == 0 {
		return result, nil
	}
	if ws == nil || ws.db == nil {
		return nil, fmt.Errorf("visible watch batch: database unavailable")
	}
	ids := make([]string, 0, len(virtualItemIDs))
	seen := make(map[string]struct{}, len(virtualItemIDs))
	for _, id := range virtualItemIDs {
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	err := ws.db.withWriteLock(func() error {
		const chunkSize = 400
		for start := 0; start < len(ids); start += chunkSize {
			end := start + chunkSize
			if end > len(ids) {
				end = len(ids)
			}
			where, args := visibleWatchWhere(scope)
			query := "SELECT " + visibleWatchColumns + " FROM " + mergedVisibleWatchTable + " WHERE " + where +
				" AND w.virtual_item_id IN (" + strings.TrimSuffix(strings.Repeat("?,", end-start), ",") + ")"
			for _, id := range ids[start:end] {
				args = append(args, id)
			}
			rows, err := ws.scanVisibleWatchBatchChunk(scope.userID, query, args)
			if err != nil {
				return err
			}
			for _, row := range rows {
				result[row.VirtualItemID] = row
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Caller owns WatchStore.mu and db.writeMu; do not acquire either again.
func (ws *WatchStore) scanVisibleWatchBatchChunk(userID, query string, args []any) ([]WatchProgress, error) {
	stmt, err := ws.db.prepare(query)
	if err != nil {
		return nil, err
	}
	defer stmt.finalize()
	if err := stmt.bindAll(args...); err != nil {
		return nil, err
	}
	return ws.scanRows(userID, stmt)
}
