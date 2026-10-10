package backend

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// Activity is per-source and can only be recorded from an authenticated client
// HTTP request. Scanner/background services never pass through this path.
type scanActivityGate struct {
	mu       sync.Mutex
	last     map[string]time.Time
	now      func() time.Time
	quietFor time.Duration
}

func newScanActivityGate() *scanActivityGate {
	return &scanActivityGate{last: map[string]time.Time{}, now: time.Now, quietFor: 60 * time.Second}
}
func (g *scanActivityGate) note(sources []string) {
	if g == nil {
		return
	}
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, source := range sources {
		if source != "" {
			g.last[source] = now
		}
	}
}
func (g *scanActivityGate) quiet(source string) bool {
	if g == nil {
		return false
	} // fail closed if gate unavailable
	g.mu.Lock()
	defer g.mu.Unlock()
	last, ok := g.last[source]
	return !ok || !g.now().Before(last.Add(g.quietFor))
}
func scanInteractivePath(path string) bool {
	if strings.HasPrefix(path, "/admin/") || strings.HasPrefix(path, "/System/") || strings.HasPrefix(path, "/Health") ||
		strings.HasPrefix(path, "/Notifications") || path == "/" {
		return false
	}
	if strings.HasPrefix(path, "/Users/") {
		// /Users/Me and user-profile polling are not media activity. A client
		// that polls those routes must not starve the Scanner indefinitely.
		return strings.Contains(path, "/Items") || strings.Contains(path, "/Views") ||
			strings.Contains(path, "/FavoriteItems") || strings.Contains(path, "/PlayedItems")
	}
	if path == "/Items" || path == "/Search" {
		return true
	}
	for _, part := range []string{"/Items/", "/Shows/", "/Videos/", "/Audio/", "/Images/", "/Search/", "/Sessions/Playing", "/Sessions/Stopped", "/PlaybackInfo", "/LiveTv/"} {
		if strings.HasPrefix(path, part) {
			return true
		}
	}
	return false
}

// Exact known-item and library routes pause only the involved upstream.
// Global search/root fanout pauses every source that the client may query.
func (a *App) observeScanClientActivity(req *http.Request, scope *RequestContext) {
	if a.scanActivity == nil || scope == nil || scope.ProxyUser == nil || !scanInteractivePath(req.URL.Path) {
		return
	}
	a.watchLifecycleMu.RLock()
	access := a.mediaAccessScopeLocked(scope)
	a.watchLifecycleMu.RUnlock()
	if len(access.serverIDs) == 0 {
		return
	}
	target := ""
	for _, key := range []string{"ParentId", "parentId", "parentid"} {
		if id := req.URL.Query().Get(key); id != "" && id != "0" && !strings.EqualFold(id, "root") {
			if resolved := a.IDStore.ResolveVirtualID(id); resolved != nil {
				target = resolved.ServerID
			}
			break
		}
	}
	if target != "" && access.allows(target) {
		// ParentId is an actual source-owned library, not a merged work.
		a.scanActivity.note([]string{target})
		return
	}
	for _, part := range strings.Split(strings.Trim(req.URL.Path, "/"), "/") {
		if resolved := a.IDStore.ResolveVirtualID(part); resolved != nil {
			// Detail, playback and Shows can query EVERY known authorized
			// instance, not only the primary source. Pause them together.
			sources := []string{}
			if access.allows(resolved.ServerID) {
				sources = append(sources, resolved.ServerID)
			}
			for _, other := range resolved.OtherInstances {
				if access.allows(other.ServerID) && !containsString(sources, other.ServerID) {
					sources = append(sources, other.ServerID)
				}
			}
			if len(sources) > 0 {
				a.scanActivity.note(sources)
				return
			}
		}
	}
	// Unknown locator or global multi-source request: conservatively pause all
	// authorized sources; never block actual user requests.
	a.scanActivity.note(access.serverIDs)
}
