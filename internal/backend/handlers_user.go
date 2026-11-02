package backend

import (
	"net/http"
	"time"
)

func (a *App) handleAuthenticateByName(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r, a.ConfigStore.Snapshot().Server.TrustProxy)
	if !a.loginLimiter.allowed(ip) {
		if a.Logger != nil {
			a.Logger.Warnf("Login rate limited: ip=%s", ip)
		}
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"message": "Too many failed login attempts, please try again later"})
		return
	}
	var body struct {
		Username string `json:"Username"`
		Pw       string `json:"Pw"`
		Password string `json:"Password"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"message": "Invalid request body"})
		return
	}
	password := body.Pw
	if password == "" {
		password = body.Password
	}
	// Authentication has no proxy token yet, so only the live request's first
	// three DeviceID sources participate. The issued token persists this value for
	// later fourth-priority playback fallback.
	loginDeviceID := resolvePlaybackDeviceID(r.Header, "")
	if a.Logger != nil {
		a.Logger.Infof("Login attempt: user=%q client=%q device=%q ip=%s",
			body.Username, r.Header.Get("X-Emby-Client"), r.Header.Get("X-Emby-Device-Name"), r.RemoteAddr)
		a.Logger.Debugf("Login headers: UA=%q DeviceId=%q Version=%q",
			r.Header.Get("User-Agent"), r.Header.Get("X-Emby-Device-Id"), r.Header.Get("X-Emby-Client-Version"))
	}

	// 1. Try admin match
	result, ok, err := a.Auth.Authenticate(body.Username, password, loginDeviceID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"message": err.Error()})
		return
	}
	if ok {
		a.loginLimiter.recordSuccess(ip)
		if token, _ := result["AccessToken"].(string); token != "" {
			if hasPassthroughIdentity(r.Header) {
				a.Identity.SetCaptured(token, r.Header)
			}
		}
		if a.Logger != nil {
			a.Logger.Infof("Login success: user=%q role=admin ip=%s", body.Username, r.RemoteAddr)
		}
		writeJSON(w, http.StatusOK, result)
		return
	}

	// 2. Try regular user match
	if a.UserStore != nil {
		user := a.UserStore.Authenticate(body.Username, password)
		if user != nil {
			hasPassword, stateErr := a.UserStore.hasPassword(user)
			if stateErr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "failed to read user password state"})
				return
			}
			response, token, authErr := a.authenticateCurrentUser(user, hasPassword, loginDeviceID)
			if authErr != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"message": authErr.Error()})
				return
			}
			if token != "" && hasPassthroughIdentity(r.Header) {
				a.Identity.SetCaptured(token, r.Header)
			}
			a.loginLimiter.recordSuccess(ip)
			if a.Logger != nil {
				a.Logger.Infof("Login success: user=%q role=user ip=%s", user.Username, r.RemoteAddr)
			}
			writeJSON(w, http.StatusOK, response)
			return
		}
	}

	// 3. No match
	a.loginLimiter.recordFailure(ip)
	if a.Logger != nil {
		a.Logger.Warnf("Login failed: user=%q ip=%s client=%q", body.Username, ip, r.Header.Get("X-Emby-Client"))
	}
	writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "Invalid username or password"})
}

func (a *App) handleUsersPublic(w http.ResponseWriter, r *http.Request) {
	cfg := a.ConfigStore.Snapshot()
	users := []map[string]any{{
		"Name":                      cfg.Admin.Username,
		"ServerId":                  cfg.Server.ID,
		"Id":                        a.clientFacingUserIDFor(r),
		"HasPassword":               true,
		"HasConfiguredPassword":     true,
		"HasConfiguredEasyPassword": false,
		"EnableAutoLogin":           false,
	}}
	if a.UserStore != nil {
		for _, user := range a.UserStore.List() {
			if user.Enabled {
				hasPassword, err := a.UserStore.hasPassword(user)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "failed to read user password state"})
					return
				}
				users = append(users, map[string]any{
					"Name":                      user.Username,
					"ServerId":                  cfg.Server.ID,
					"Id":                        user.ID,
					"HasPassword":               hasPassword,
					"HasConfiguredPassword":     hasPassword,
					"HasConfiguredEasyPassword": false,
					"EnableAutoLogin":           false,
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, users)
}

func (a *App) handleUserObject(w http.ResponseWriter, r *http.Request) {
	reqCtx := requestContextFrom(r.Context())
	if reqCtx != nil && reqCtx.ProxyUser != nil && reqCtx.ProxyUser.Role == "user" && a.UserStore != nil {
		user := a.UserStore.Get(reqCtx.ProxyUser.UserID)
		if user != nil && user.Enabled {
			hasPassword, err := a.UserStore.hasPassword(user)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "failed to read user password state"})
				return
			}
			writeJSON(w, http.StatusOK, a.Auth.BuildUserObjectForUser(user, hasPassword))
			return
		}
		if user == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"message": "User no longer exists"})
			return
		}
	}
	writeJSON(w, http.StatusOK, a.Auth.BuildUserObject())
}

func (a *App) handleUserViews(w http.ResponseWriter, r *http.Request) {
	reqCtx := requestContextFrom(r.Context())
	onlineClients := a.allowedClients(reqCtx)
	cfg := a.ConfigStore.Snapshot()
	multiSource := len(onlineClients) > 1
	hidden := a.hiddenLibrariesFor(reqCtx)

	// A slow source must not hold back every already-completed library.
	// Never mistake an all-failed request for a genuine empty success.
	groups, successes := a.collectUserViews(r, reqCtx, onlineClients, hidden,
		time.Duration(cfg.Timeouts.Global)*time.Millisecond)
	if r.Context().Err() != nil {
		return
	}
	if len(onlineClients) > 0 && successes == 0 {
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": "All available upstream Views requests failed"})
		return
	}
	allViews := make([]map[string]any, 0)
	for i, client := range onlineClients {
		for _, item := range groups[i] {
			if isHiddenLibraryItem(hidden, client.ID, item) {
				continue
			}
			rewritten := deepCloneMap(item)
			rewriteResponseIDs(rewritten, client.ID, a.IDStore, cfg.Server.ID, a.clientFacingUserIDFor(r))
			if multiSource {
				if name, _ := rewritten["Name"].(string); name != "" {
					rewritten["Name"] = name + " (" + client.Name + ")"
				}
			}
			allViews = append(allViews, rewritten)
		}
	}
	payload := map[string]any{
		"Items":            toAnySlice(allViews),
		"TotalRecordCount": len(allViews),
		"StartIndex":       0,
	}
	a.normalizeLocalUserDataPayload(r, payload)
	writeJSON(w, http.StatusOK, payload)
}

func (a *App) handleUserGroupingOptions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, []any{})
}

func (a *App) handleUserConfiguration(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleUserPolicy(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	token := extractToken(r)
	if token != "" {
		a.Auth.RevokeToken(token)
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}
