package backend

import (
	"net/http"
	"strings"
)

// resolvePlaybackDeviceID applies the frozen playback-device identity precedence.
// The final argument is the already-validated token-scoped fallback; lookup and
// persistence stay outside this selection primitive.
func resolvePlaybackDeviceID(headers http.Header, tokenCapturedDeviceID string) string {
	if direct := strings.TrimSpace(headers.Get("X-Emby-Device-Id")); direct != "" {
		return direct
	}

	for _, headerName := range []string{"X-Emby-Authorization", "Authorization"} {
		if deviceID := deviceIDFromAuthorizationHeader(headers.Get(headerName)); deviceID != "" {
			return deviceID
		}
	}

	return strings.TrimSpace(tokenCapturedDeviceID)
}

// playbackDeviceIDRequired reports whether this regular-user/server lifecycle is
// governed by the playback limiter but has no usable resolved DeviceID. Admin and
// non-limiter contexts remain exempt through playbackLimiterKey.
func (a *App) playbackDeviceIDRequired(reqCtx *RequestContext, serverID string) bool {
	_, applies := a.playbackLimiterKey(reqCtx, serverID)
	return applies && playbackDeviceID(reqCtx) == ""
}
