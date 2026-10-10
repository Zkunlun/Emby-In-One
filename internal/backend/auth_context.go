package backend

import (
	"context"
	"net/http"
	"strings"
)

type requestContextKey struct{}

type RequestContext struct {
	Headers    http.Header
	ProxyToken string
	ProxyUser  *tokenInfo
	// TraceID links one client request to all EIO→upstream requests it triggers while sample capture is active.
	TraceID string
	// PlaybackDeviceID is the already-resolved playback-device identity consumed by
	// lifecycle handlers. withContext resolves it once from live headers and the
	// validated token-scoped fallback.
	PlaybackDeviceID string
	// LegacyProxyUserID is the single global virtual user ID that older responses
	// handed to every client. It is kept only so requests that still carry it are
	// recognised as the current user; the identity of the request itself always
	// comes from ProxyUser.
	LegacyProxyUserID string
	// Identifiers is the read-only signing/registration view for this request. It
	// is built once and shared by the identity predicates.
	Identifiers *IdentifierLookup
	// Frozen before any upstream I/O. Every passive fan-out and late drain
	// uses this same source identity snapshot to reject delete/recreate ABA.
	SourceGenerations        map[string]int64
	SourceGenerationCaptured bool
}

func (a *App) withContext(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := extractToken(r)
		var proxyUser *tokenInfo
		if token != "" {
			proxyUser = a.Auth.ValidateToken(token)
		}
		tokenDeviceID := ""
		if proxyUser != nil {
			tokenDeviceID = proxyUser.DeviceID
		}
		// Keep the epoch snapshot stable across upstream I/O; normal Reload does
		// not change it. Acquire no network resources under the lifecycle gate.
		var epochs map[string]int64
		a.watchLifecycleMu.RLock()
		if a.IDStore != nil && a.ConfigStore != nil {
			epochs = a.IDStore.snapshotSourceGenerations(configuredSourceIDs(a.ConfigStore.Snapshot()))
		}
		a.watchLifecycleMu.RUnlock()
		ctx := context.WithValue(r.Context(), requestContextKey{}, &RequestContext{
			Headers:                  r.Header.Clone(),
			TraceID:                  sampleTraceFromContext(r.Context()),
			ProxyToken:               token,
			ProxyUser:                proxyUser,
			PlaybackDeviceID:         resolvePlaybackDeviceID(r.Header, tokenDeviceID),
			LegacyProxyUserID:        a.Auth.ProxyUserID(),
			Identifiers:              a.newRequestIdentifierLookup(),
			SourceGenerations:        epochs,
			SourceGenerationCaptured: true,
		})
		a.observeScanClientActivity(r, requestContextFrom(ctx))
		next(w, r.WithContext(ctx))
	}
}

func (a *App) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if reqCtx := requestContextFrom(r.Context()); !a.currentRequestAuthorized(reqCtx) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Authentication required"})
			return
		}
		next(w, r)
	}
}

func (a *App) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqCtx := requestContextFrom(r.Context())
		if !a.currentRequestAuthorized(reqCtx) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Authentication required"})
			return
		}
		if reqCtx.ProxyUser.Role != "admin" {
			writeJSON(w, http.StatusForbidden, map[string]any{"message": "需要管理员权限"})
			return
		}
		next(w, r)
	}
}

func requestContextFrom(ctx context.Context) *RequestContext {
	reqCtx, _ := ctx.Value(requestContextKey{}).(*RequestContext)
	return reqCtx
}

// playbackDeviceID is the single lifecycle read boundary for an already-resolved
// device identity. HTTP/token extraction stays at withContext; limiter call sites
// consume only RequestContext.PlaybackDeviceID.
func playbackDeviceID(reqCtx *RequestContext) string {
	if reqCtx == nil {
		return ""
	}
	return reqCtx.PlaybackDeviceID
}

// allowedClients returns only the online upstream clients that the
// current user is permitted to access. Admin users see all online servers.
func (a *App) allowedClients(reqCtx *RequestContext) []*UpstreamClient {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	scope := a.mediaAccessScopeLocked(reqCtx)
	clients := make([]*UpstreamClient, 0, len(scope.onlineServerIDs))
	if a.Upstream == nil {
		return clients
	}
	// Preserve configured source order for aggregation and default selection.
	for _, client := range a.Upstream.OnlineClients() {
		if scope.allows(client.ID) {
			clients = append(clients, client)
		}
	}
	return clients
}

// isServerAllowed checks whether the current user is allowed to access the
// upstream server with the given ID. Admin users may access every server;
// regular users may access only servers explicitly listed in AllowedServers.
func (a *App) isServerAllowed(reqCtx *RequestContext, serverID string) bool {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	return a.mediaAccessScopeLocked(reqCtx).allows(serverID)
}

// requireServerAccess writes a 403 response and returns false when the current
// user is not allowed to access the upstream server that owns resolved.
// A nil resolution returns true so each caller keeps its own not-found response.
func (a *App) requireServerAccess(w http.ResponseWriter, r *http.Request, resolved *routeResolution) bool {
	if resolved == nil || a.isServerAllowed(requestContextFrom(r.Context()), resolved.ServerID) {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]any{"message": "Access denied"})
	return false
}

func extractToken(r *http.Request) string {
	if token := r.Header.Get("X-Emby-Token"); token != "" {
		return token
	}
	if token := r.URL.Query().Get("api_key"); token != "" {
		return token
	}
	if token := r.URL.Query().Get("ApiKey"); token != "" {
		return token
	}
	for _, headerName := range []string{"X-Emby-Authorization", "Authorization"} {
		if auth := r.Header.Get(headerName); auth != "" {
			if token := extractTokenFromAuthHeader(auth); token != "" {
				return token
			}
		}
	}
	return ""
}

func extractTokenFromAuthHeader(header string) string {
	if parsed, ok := parseAuthorizationIdentityStrict(header); ok {
		// An absent or ambiguous compound Token cannot regain credentials through
		// the permissive legacy substring fallback.
		return authorizationIdentityParameter(parsed, "Token")
	}
	// Compatibility fallback for legacy/non-standard clients whose authorization
	// string is not a valid compound header but historically still exposed Token=.
	return extractTokenFromAuthHeaderLegacy(header)
}

func extractTokenFromAuthHeaderLegacy(header string) string {
	for _, marker := range []string{"Token=\"", "Token="} {
		idx := strings.Index(header, marker)
		if idx < 0 {
			continue
		}
		rest := header[idx+len(marker):]
		if marker == "Token=\"" {
			if end := strings.Index(rest, "\""); end >= 0 {
				return rest[:end]
			}
		}
		end := strings.IndexAny(rest, ", ")
		if end >= 0 {
			return rest[:end]
		}
		return rest
	}
	return ""
}
