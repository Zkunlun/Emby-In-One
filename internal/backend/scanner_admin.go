package backend

import (
	"errors"
	"net/http"
	"strings"
)

func (a *App) handleScannerStatus(w http.ResponseWriter, r *http.Request) {
	if a.Scanner == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "scanner persistence unavailable"})
		return
	}
	status, err := a.scannerState("")
	if err != nil {
		writeScannerError(w, err)
		return
	}
	list := make([]any, 0)
	for _, source := range a.ConfigStore.Snapshot().Upstream {
		if source.ID == "" {
			continue
		}
		one, err := a.scannerState(source.ID)
		if err != nil {
			writeScannerError(w, err)
			return
		}
		list = append(list, one)
	}
	status["upstreams"] = list
	status["executorAvailable"] = a.Scanner.workerReady.Load()
	writeJSON(w, http.StatusOK, status)
}
func (a *App) handleScannerSourceStatus(w http.ResponseWriter, r *http.Request) {
	status, err := a.scannerState(r.PathValue("id"))
	if err != nil {
		writeScannerError(w, err)
		return
	}
	status["executorAvailable"] = a.Scanner != nil && a.Scanner.workerReady.Load()
	writeJSON(w, http.StatusOK, status)
}
func (a *App) handleScannerGlobalSetting(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ScanEnabled *bool `json:"scanEnabled"`
	}
	if err := decodeJSONBody(r, &input); err != nil || input.ScanEnabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "scanEnabled boolean required"})
		return
	}
	if err := a.setScannerGlobal(*input.ScanEnabled); err != nil {
		writeScannerError(w, err)
		return
	}
	a.handleScannerStatus(w, r)
}
func (a *App) handleScannerSourceSetting(w http.ResponseWriter, r *http.Request) {
	var input struct {
		AllowScan *bool `json:"allowScan"`
	}
	if err := decodeJSONBody(r, &input); err != nil || input.AllowScan == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "allowScan boolean required"})
		return
	}
	source := r.PathValue("id")
	if err := a.setScannerSource(source, *input.AllowScan); err != nil {
		writeScannerError(w, err)
		return
	}
	a.handleScannerSourceStatus(w, r)
}
func (a *App) handleScannerCommand(w http.ResponseWriter, r *http.Request) {
	action := strings.ToLower(r.PathValue("action"))
	if action == "force-full" {
		action = "force_full"
	}
	if action != "start" && action != "pause" && action != "resume" && action != "stop" && action != "force_full" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid scan command"})
		return
	}
	run, err := a.scannerCommand(r.PathValue("id"), action)
	if err != nil {
		writeScannerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "executorAvailable": false})
}
func writeScannerError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	message := "scanner control unavailable"
	switch {
	case errors.Is(err, errScanNotFound):
		code = http.StatusNotFound
		message = "upstream or scanner run not found"
	case errors.Is(err, errScanDisabled):
		code = http.StatusForbidden
		message = "global scan and upstream allowScan must both be enabled"
	case errors.Is(err, errScanConflict), errors.Is(err, errScanTransition), errors.Is(err, errScanStale):
		code = http.StatusConflict
		message = err.Error()
	case strings.Contains(err.Error(), "upstream unavailable"):
		code = http.StatusServiceUnavailable
		message = "upstream is offline or unauthenticated"
	}
	// Never return database errors, upstream credentials, tokens or raw SQL.
	writeJSON(w, code, map[string]any{"error": message})
}
