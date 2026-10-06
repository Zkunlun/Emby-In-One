package backend

import "strings"

// Rows remain stored under their original IDs. Client reads choose one winner
// per user/canonical group, before state filtering, series folding and paging.
// A current canonical row wins a timestamp tie; old aliases remain auditable.
const mergedVisibleWatchTable = `(
 SELECT proxy_user_id, canonical_item_id AS virtual_item_id, server_id, original_item_id, item_type,
  canonical_series_id AS series_virtual_id, series_original_id, series_name,
  parent_index_number, index_number, name, production_year, provider_tmdb,
  position_ticks, runtime_ticks, played, is_favorite, last_played, updated_at
 FROM (
  SELECT w.*, COALESCE(alias.virtual_id,w.virtual_item_id) AS canonical_item_id,
   COALESCE(series_alias.virtual_id,w.series_virtual_id) AS canonical_series_id,
   ROW_NUMBER() OVER (
    PARTITION BY w.proxy_user_id, COALESCE(alias.virtual_id,w.virtual_item_id)
    ORDER BY w.updated_at DESC, (alias.alias_id IS NULL) DESC, w.virtual_item_id
   ) AS merge_row
  FROM user_watch_progress AS w
  LEFT JOIN media_merge_aliases AS alias ON alias.alias_id=w.virtual_item_id
  LEFT JOIN media_merge_aliases AS series_alias ON series_alias.alias_id=w.series_virtual_id
  WHERE w.proxy_user_id = ?
 ) WHERE merge_row=1
) AS w`

// Before an explicit write/event, materialize the deterministic winning state
// under the canonical ID. This happens lazily, never during startup/discovery,
// and never deletes or rewrites the alias row or manufactures timestamps.
func (ws *WatchStore) SeedMergeState(userID, id string) error {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	columns := strings.FieldsFunc(visibleWatchColumns, func(r rune) bool { return r == ',' })
	for i := range columns {
		columns[i] = strings.TrimSpace(columns[i])
	}
	var updates []string
	for _, column := range columns[1:] {
		updates = append(updates, column+"=excluded."+column)
	}
	query := `INSERT INTO user_watch_progress (proxy_user_id,` + visibleWatchColumns + `)
  SELECT proxy_user_id, ?, ` + strings.Join(columns[1:], ",") + `
  FROM user_watch_progress AS w
  WHERE proxy_user_id=? AND
   (virtual_item_id=? OR virtual_item_id IN (SELECT alias_id FROM media_merge_aliases WHERE virtual_id=?))
   AND EXISTS (SELECT 1 FROM media_merge_aliases WHERE virtual_id=?)
  ORDER BY updated_at DESC, (virtual_item_id=?) DESC, virtual_item_id
  LIMIT 1
  ON CONFLICT(proxy_user_id,virtual_item_id) DO UPDATE SET ` + strings.Join(updates, ",")
	return ws.db.withWriteTx(func() error { return ws.db.execParams(query, id, userID, id, id, id, id) })
}
