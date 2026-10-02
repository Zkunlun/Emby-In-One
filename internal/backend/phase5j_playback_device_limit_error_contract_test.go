package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPhase5JPlaybackDeviceLimitErrorContract(t *testing.T) {
	rr := httptest.NewRecorder()
	writePlaybackDeviceLimit(rr)

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429 body=%s", rr.Code, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error response: %v body=%s", err, rr.Body.String())
	}
	if got, _ := payload["code"].(string); got != playbackDeviceLimitCode {
		t.Fatalf("code=%q, want %s", got, playbackDeviceLimitCode)
	}
	if message, _ := payload["message"].(string); message == "" {
		t.Fatal("stable playback-device-limit response must include a message")
	}
}
