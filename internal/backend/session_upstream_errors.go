package backend

import (
	"context"
	"errors"
	"net"
	"net/http"
)

type sessionUpstreamFailureKind string

const (
	sessionUpstreamFailureTransport sessionUpstreamFailureKind = "transport"
	sessionUpstreamFailureRejected  sessionUpstreamFailureKind = "rejected"
	sessionUpstreamFailureTimeout   sessionUpstreamFailureKind = "timeout"
	sessionUpstreamFailureCanceled  sessionUpstreamFailureKind = "canceled"
)

const (
	upstreamSessionUnavailableCode = "UPSTREAM_SESSION_UNAVAILABLE"
	upstreamSessionFailedCode      = "UPSTREAM_SESSION_FAILED"
	upstreamSessionTimeoutCode     = "UPSTREAM_SESSION_TIMEOUT"
	upstreamSessionRejectedCode    = "UPSTREAM_SESSION_REJECTED"
)

// sessionUpstreamError carries only the structured failure category and the
// upstream status when one exists. Error intentionally omits the underlying
// transport text and upstream response body; callers may still use errors.Is
// through Unwrap when cancellation/deadline semantics matter.
type sessionUpstreamError struct {
	Kind           sessionUpstreamFailureKind
	UpstreamStatus int
	cause          error
}

func (e *sessionUpstreamError) Error() string {
	if e == nil {
		return ""
	}
	switch e.Kind {
	case sessionUpstreamFailureRejected:
		return "upstream session request was rejected"
	case sessionUpstreamFailureTimeout:
		return "upstream session request timed out"
	case sessionUpstreamFailureCanceled:
		return "upstream session request was canceled"
	default:
		return "upstream session request failed"
	}
}

func (e *sessionUpstreamError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func asSessionUpstreamError(err error) (*sessionUpstreamError, bool) {
	var sessionErr *sessionUpstreamError
	if errors.As(err, &sessionErr) {
		return sessionErr, true
	}
	return nil, false
}

// isSessionLifecyclePath limits strict diagnostic redaction to lifecycle calls.
// These routes must never log an upstream URL or the raw transport cause.
func isSessionLifecyclePath(path string) bool {
	switch path {
	case "/Sessions/Playing", "/Sessions/Playing/Progress", "/Sessions/Playing/Stopped":
		return true
	default:
		return false
	}
}

func classifySessionTransportError(err error) error {
	if err == nil {
		return nil
	}
	// Preparation errors already carry their own stable status/body contract and
	// must remain distinguishable from failures that happened during network I/O.
	if _, ok := asPreparationError(err); ok {
		return err
	}
	if errors.Is(err, context.Canceled) {
		return &sessionUpstreamError{Kind: sessionUpstreamFailureCanceled, cause: err}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &sessionUpstreamError{Kind: sessionUpstreamFailureTimeout, cause: err}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &sessionUpstreamError{Kind: sessionUpstreamFailureTimeout, cause: err}
	}
	return &sessionUpstreamError{Kind: sessionUpstreamFailureTransport, cause: err}
}

func newSessionUpstreamRejectedError(status int) error {
	return &sessionUpstreamError{
		Kind:           sessionUpstreamFailureRejected,
		UpstreamStatus: status,
	}
}

func writeSessionUpstreamUnavailable(w http.ResponseWriter) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"code":    upstreamSessionUnavailableCode,
		"message": "Upstream session service is unavailable",
	})
}

// writeSessionUpstreamError maps a classified network/upstream failure to the
// stable public session error contract. It deliberately exposes neither the
// upstream response body nor transport details. Cancellation uses the generic
// failed contract: on a real client disconnect the response is immaterial, and
// on an observable cancellation we must not report a false 204 success.
func writeSessionUpstreamError(w http.ResponseWriter, err error) bool {
	sessionErr, ok := asSessionUpstreamError(err)
	if !ok {
		return false
	}

	switch sessionErr.Kind {
	case sessionUpstreamFailureRejected:
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"code":    upstreamSessionRejectedCode,
			"message": "Upstream session request was rejected",
		})
	case sessionUpstreamFailureTimeout:
		writeJSON(w, http.StatusGatewayTimeout, map[string]any{
			"code":    upstreamSessionTimeoutCode,
			"message": "Upstream session request timed out",
		})
	case sessionUpstreamFailureCanceled, sessionUpstreamFailureTransport:
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"code":    upstreamSessionFailedCode,
			"message": "Upstream session request failed",
		})
	default:
		return false
	}
	return true
}
