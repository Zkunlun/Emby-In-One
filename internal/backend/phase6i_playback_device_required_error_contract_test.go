package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPhase6IPlaybackDeviceIDRequiredErrorContract(t *testing.T) {
	rr := httptest.NewRecorder()
	writePlaybackDeviceIDRequired(rr)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusBadRequest, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rr.Body.String())
	}
	if got, _ := payload["code"].(string); got != "PLAYBACK_DEVICE_ID_REQUIRED" {
		t.Fatalf("code = %q, want PLAYBACK_DEVICE_ID_REQUIRED", got)
	}
	if message, _ := payload["message"].(string); message == "" {
		t.Fatal("stable playback DeviceID-required error must include a non-empty message")
	}
}
