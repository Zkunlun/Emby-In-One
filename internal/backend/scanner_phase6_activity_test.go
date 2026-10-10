package backend

import (
	"net/http"
	"testing"
	"time"
)

func TestPhase6ActivityGateScopesMergedDetailAndIgnoresKeepAlive(t *testing.T) {
	a := task6HTTPMovie("movie-a")
	b := task6HTTPMovie("movie-b")
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		rows := f.list(t, "Movie")
		if len(rows) != 1 {
			t.Fatalf("expected one merged movie, got %d", len(rows))
		}
		id, _ := rows[0]["Id"].(string)
		when := time.Date(2026, 10, 10, 4, 0, 0, 0, time.UTC)
		f.app.scanActivity.now = func() time.Time { return when }
		f.app.scanActivity.mu.Lock()
		f.app.scanActivity.last = map[string]time.Time{}
		f.app.scanActivity.mu.Unlock()
		_ = f.request(t, http.MethodGet, "/Users/"+f.user+"/Items/"+id, nil, "")
		if f.app.scanActivity.quiet("server-a") || f.app.scanActivity.quiet("server-b") {
			t.Fatal("merged detail did not hold both authorized upstream gates")
		}
		when = when.Add(30 * time.Second)
		_ = f.request(t, http.MethodGet, "/Users/Me", nil, "")
		when = when.Add(31 * time.Second)
		if !f.app.scanActivity.quiet("server-a") || !f.app.scanActivity.quiet("server-b") {
			t.Fatal("user profile keepalive incorrectly renewed activity timeout")
		}
	})
}
