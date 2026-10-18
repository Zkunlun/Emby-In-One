package backend

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// Regression target: deleting a source with many independent whole-item mappings
// should not execute per-virtual-row cleanup SQL. SQLite fixture and HTTP handler
// are completely isolated from production; this is not a real upstream delete.
func TestPhase2DeleteLargeSourceAvoidsPerItemSQL(t *testing.T) {
	withHomeLibraryApp(t, func(app *App, handler http.Handler, _ *atomic.Int32) {
		source := app.Upstream.Clients()[0].ID
		const items = 20000
		if err := app.IDStore.db.withWriteTx(func() error {
			for i := 0; i < items; i++ {
				id := fmt.Sprintf("phase2-delete-virtual-%06d", i)
				original := fmt.Sprintf("fixture-source-item-%06d", i)
				if err := app.IDStore.db.execParams("INSERT INTO id_mappings(virtual_id,original_id,server_id) VALUES(?,?,?)", id, original, source); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		unlock := app.lockLifecycleStores()
		err := app.reloadLifecycleStoresLocked(true)
		unlock()
		if err != nil {
			t.Fatal(err)
		}
		token := loginToken(t, handler, "secret")
		started := time.Now()
		rr := doJSONRequest(t, handler, http.MethodDelete, "/admin/api/upstream/"+source, nil, token)
		elapsed := time.Since(started)
		if rr.Code != http.StatusOK {
			t.Fatalf("delete status=%d body=%s", rr.Code, rr.Body.String())
		}
		var count int
		stmt, err := app.IDStore.db.prepare("SELECT COUNT(*) FROM id_mappings")
		if err != nil {
			t.Fatal(err)
		}
		defer stmt.finalize()
		if row, err := stmt.step(); err != nil {
			t.Fatal(err)
		} else if row {
			count = stmt.columnInt(0)
		}
		if count != 0 {
			t.Fatalf("removed source has %d leftover mappings", count)
		}
		t.Logf("synthetic_delete_20k_elapsed=%s", elapsed)
		if elapsed > 900*time.Millisecond {
			t.Fatalf("20k single-source delete took %s, expected batched cleanup <900ms", elapsed)
		}
	})
}
