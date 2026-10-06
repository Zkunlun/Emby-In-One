package backend

import (
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
)

type countsQuery struct {
	userID string
}

// Only query spelling is normalized. Scope and credentials remain local;
// none of these fields is forwarded, captured, or installed in the cache.
func countsQueryName(name string) string {
	name = strings.ToLower(name)
	if name == "apikey" || name == "api_key" {
		return "api_key"
	}
	return name
}

func parseCountsQuery(raw string) (countsQuery, string) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return countsQuery{}, "INVALID_COUNTS_QUERY"
	}
	normalized := make(map[string]string, len(values))
	for name, entries := range values {
		key := countsQueryName(name)
		if len(entries) != 1 {
			return countsQuery{}, "INVALID_COUNTS_QUERY"
		}
		if _, duplicate := normalized[key]; duplicate {
			return countsQuery{}, "INVALID_COUNTS_QUERY"
		}
		normalized[key] = entries[0]
	}
	if value, supplied := normalized["userid"]; supplied && strings.TrimSpace(value) == "" {
		return countsQuery{}, "INVALID_COUNTS_QUERY"
	}
	for name := range normalized {
		switch name {
		case "userid", "api_key", "x-emby-client", "x-emby-device-name", "x-emby-device-id",
			"x-emby-client-version", "x-emby-language", "user-agent", "x-emby-token", "x-emby-authorization":
		default:
			return countsQuery{}, "COUNTS_FILTER_UNSUPPORTED"
		}
	}
	return countsQuery{userID: normalized["userid"]}, ""
}

// Retain extractToken's header/query/compound-header precedence while accepting
// the Counts contract's case-insensitive query credential spelling. Restore the
// exact original query before parameter validation; partial ParseQuery results
// cannot turn an invalid query into a successful response.
func (a *App) withCountsContext(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		originalQuery := r.URL.RawQuery
		values, _ := url.ParseQuery(originalQuery)
		token, occurrences := "", 0
		for name, entries := range values {
			if countsQueryName(name) != "api_key" {
				continue
			}
			occurrences += len(entries)
			if len(entries) == 1 {
				token = entries[0]
			}
		}
		prepared := r
		if occurrences == 1 && token != "" {
			prepared = r.Clone(r.Context())
			for name := range values {
				if countsQueryName(name) == "api_key" {
					delete(values, name)
				}
			}
			values.Set("api_key", token)
			prepared.URL.RawQuery = values.Encode()
		}
		a.withContext(func(w http.ResponseWriter, contextual *http.Request) {
			restored := contextual.Clone(contextual.Context())
			restored.URL.RawQuery = originalQuery
			next(w, restored)
		})(w, prepared)
	}
}

// Authentication is checked independently from lifecycle readiness. Existing
// routes keep requireAuth; Counts must distinguish authenticated pending=503
// from deleted/disabled/revoked credentials=401.
func (a *App) requireCountsAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.watchLifecycleMu.RLock()
		valid := a.countsIdentityValidLocked(requestContextFrom(r.Context()))
		a.watchLifecycleMu.RUnlock()
		if !valid {
			writeCountsJSON(w, r, http.StatusUnauthorized, map[string]string{"message": "Authentication required"})
			return
		}
		next(w, r)
	}
}

func (a *App) countsIdentityValidLocked(reqCtx *RequestContext) bool {
	return reqCtx != nil && reqCtx.ProxyUser != nil && reqCtx.ProxyUser.UserID != "" &&
		a.requestIdentityValidLocked(reqCtx)
}

func isCountsRoutePath(path string) bool {
	return path == "/Items/Counts" || path == "/emby/Items/Counts"
}

func setCountsResponseHeaders(w http.ResponseWriter) {
	headers := w.Header()
	headers.Set("Cache-Control", "private, no-store")
	headers.Del("ETag")
	headers.Del("Last-Modified")
	headers.Add("Vary", "Authorization, X-Emby-Token, X-Emby-Authorization")
}

type countsHeadWriter struct{ http.ResponseWriter }

func (w countsHeadWriter) Write(body []byte) (int, error) { return len(body), nil }

// Placed after prefix compatibility/CORS and before routing. A path guard
// handles other methods without a methodless ServeMux pattern that would
// conflict with GET /Items/{itemId}. HEAD is body-free, including early errors.
func (a *App) countsResponseMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isCountsRoutePath(r.URL.Path) {
			setCountsResponseHeaders(w)
			if r.Method == http.MethodHead {
				w = countsHeadWriter{w}
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				a.withCountsContext(a.requireCountsAuth(a.handleItemsCountsMethodNotAllowed))(w, r)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeCountsJSON(w http.ResponseWriter, r *http.Request, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		status = http.StatusServiceUnavailable
		body = []byte("{\"code\":\"COUNTS_UNAVAILABLE\",\"message\":\"媒体库统计暂不可用\"}")
	}
	body = append(body, '\n')
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Del("ETag")
	w.Header().Del("Last-Modified")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func writeCountsError(w http.ResponseWriter, r *http.Request, status int, code string) {
	message := "媒体库统计暂不可用"
	switch code {
	case "INVALID_COUNTS_QUERY":
		message = "统计查询参数无效"
	case "COUNTS_FILTER_UNSUPPORTED":
		message = "当前接口仅支持媒体库总数统计"
	case "COUNTS_USER_FORBIDDEN":
		message = "无权查询其他用户的统计"
	case "COUNTS_LIFECYCLE_PENDING":
		message = "媒体库统计生命周期清理尚未完成"
	case "COUNTS_OUT_OF_RANGE":
		message = "媒体库统计数量超出可安全表示范围"
	case "METHOD_NOT_ALLOWED":
		message = "当前接口仅支持GET和HEAD"
	}
	writeCountsJSON(w, r, status, map[string]string{"code": code, "message": message})
}

func (a *App) handleItemsCountsMethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	if !a.countsIdentityValidLocked(requestContextFrom(r.Context())) {
		writeCountsJSON(w, r, http.StatusUnauthorized, map[string]string{"message": "Authentication required"})
		return
	}
	w.Header().Set("Allow", "GET, HEAD")
	writeCountsError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED")
}

// Every dependency on this path is read-only. In particular it never invokes
// source synchronization, pending registration, Identity capture, Login,
// Counts collection, public-system probes, or a scheduler signal.
func (a *App) handleItemsCounts(w http.ResponseWriter, r *http.Request) {
	query, queryError := parseCountsQuery(r.URL.RawQuery)
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	reqCtx := requestContextFrom(r.Context())
	if !a.countsIdentityValidLocked(reqCtx) {
		writeCountsJSON(w, r, http.StatusUnauthorized, map[string]string{"message": "Authentication required"})
		return
	}
	if a.lifecyclePending {
		writeCountsError(w, r, http.StatusServiceUnavailable, "COUNTS_LIFECYCLE_PENDING")
		return
	}
	if queryError != "" {
		writeCountsError(w, r, http.StatusBadRequest, queryError)
		return
	}
	// An empty upstream auth snapshot deliberately excludes the upstream real
	// user alias accepted by ordinary proxy requests.
	if query.userID != "" && !IsCurrentUserAlias(query.userID, reqCtx, upstreamAuthSnapshot{}, reqCtx.Identifiers) {
		writeCountsError(w, r, http.StatusForbidden, "COUNTS_USER_FORBIDDEN")
		return
	}
	if a.ConfigStore == nil {
		writeCountsError(w, r, http.StatusServiceUnavailable, "COUNTS_UNAVAILABLE")
		return
	}
	scope := a.mediaAccessScopeLocked(reqCtx)
	if scope.userID != reqCtx.ProxyUser.UserID {
		writeCountsJSON(w, r, http.StatusUnauthorized, map[string]string{"message": "Authentication required"})
		return
	}
	result, code, release := a.readCountsScopeLocked(scope)
	if release != nil {
		defer release()
	}
	// Lifecycle publication is excluded by the read gate. Recheck credentials
	// at the response boundary; cache generations and all source auth/online
	// states are held stable by release until the response has been committed.
	if !a.countsIdentityValidLocked(reqCtx) {
		writeCountsJSON(w, r, http.StatusUnauthorized, map[string]string{"message": "Authentication required"})
		return
	}
	if a.lifecyclePending {
		writeCountsError(w, r, http.StatusServiceUnavailable, "COUNTS_LIFECYCLE_PENDING")
		return
	}
	if code != "" {
		writeCountsError(w, r, http.StatusServiceUnavailable, code)
		return
	}
	writeCountsJSON(w, r, http.StatusOK, result)
}

// Caller holds the lifecycle read gate. Lock direction is lifecycle -> cache
// -> clients, in sorted stable-ID order. Resolve pool/config BEFORE holding
// client locks: pool listener installation takes pool -> client and must not
// be acquired in reverse. No identity, grants, snapshots or caches are mutated.
func (a *App) readCountsScopeLocked(scope mediaAccessScope) (mediaCounts, string, func()) {
	if len(scope.serverIDs) == 0 {
		return mediaCounts{}, "", nil
	}
	if a.Upstream == nil {
		return mediaCounts{}, "COUNTS_UNAVAILABLE", nil
	}
	cfg := a.ConfigStore.Snapshot()
	configured := make(map[string]UpstreamConfig, len(cfg.Upstream))
	for _, source := range cfg.Upstream {
		configured[source.ID] = source
	}
	clients := make([]*UpstreamClient, 0, len(scope.serverIDs))
	for _, id := range scope.serverIDs {
		client := a.Upstream.ClientByID(id)
		if client == nil {
			return mediaCounts{}, "COUNTS_UNAVAILABLE", nil
		}
		clients = append(clients, client)
	}
	var cache *countsCache
	if a.mediaCounts != nil {
		cache = a.mediaCounts.cache
	}
	if cache != nil {
		cache.mu.RLock()
	}
	for _, client := range clients {
		client.mu.RLock()
	}
	release := func() {
		for i := len(clients) - 1; i >= 0; i-- {
			clients[i].mu.RUnlock()
		}
		if cache != nil {
			cache.mu.RUnlock()
		}
	}
	total := mediaCounts{}
	for _, client := range clients {
		source, exists := configured[client.ID]
		if !exists || client.retired || !reflect.DeepEqual(source, client.Config) {
			return mediaCounts{}, "COUNTS_UNAVAILABLE", release
		}
		if !client.Online {
			continue
		}
		if cache == nil || cache.closed || a.mediaCounts.closed.Load() {
			return mediaCounts{}, "COUNTS_UNAVAILABLE", release
		}
		proxyURL := ""
		if proxy := findProxy(cfg.Proxies, source.ProxyID); proxy != nil {
			proxyURL = proxy.URL
		}
		state := cache.states[client.ID]
		if state == nil || state.spec.Client != client || !state.hasSnapshot ||
			state.snapshot.DataGeneration != state.dataGeneration ||
			client.UserID == "" || client.AccessToken == "" ||
			client.countsLoginConfig != client.configuredCountsLoginBinding() ||
			state.spec.Auth.UserID != client.UserID ||
			state.spec.DataSignature != countsDataSignature(client, client.UserID, proxyURL, cache.key) {
			return mediaCounts{}, "COUNTS_UNAVAILABLE", release
		}
		// The account and generation are current. An old complete success remains
		// valid during same-account re-login, ordinary failure or rate limiting.
		if !state.snapshot.Value.validSource() {
			return mediaCounts{}, "COUNTS_OUT_OF_RANGE", release
		}
		value, err := total.add(state.snapshot.Value)
		if err != nil {
			return mediaCounts{}, "COUNTS_OUT_OF_RANGE", release
		}
		total = value
	}
	return total, "", release
}
