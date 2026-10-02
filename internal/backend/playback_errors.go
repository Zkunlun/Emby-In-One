package backend

import "net/http"

const (
	playbackDeviceLimitCode      = "PLAYBACK_DEVICE_LIMIT"
	playbackDeviceIDRequiredCode = "PLAYBACK_DEVICE_ID_REQUIRED"
)

// writePlaybackDeviceLimit reports the stable public error for a regular user
// attempting to use a second active playback device on the same upstream.
func writePlaybackDeviceLimit(w http.ResponseWriter) {
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"code":    playbackDeviceLimitCode,
		"message": "Playback is already active on another device",
	})
}

// writePlaybackDeviceIDRequired reports the stable fail-closed error when a
// regular playback request has no usable identity from any frozen DeviceID source.
func writePlaybackDeviceIDRequired(w http.ResponseWriter) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"code":    playbackDeviceIDRequiredCode,
		"message": "Playback DeviceID is required",
	})
}
