package backend

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPhase7CSessionTransportFailureClassification(t *testing.T) {
	prep := newMissingAuthStateError("body.UserId")
	if got := classifySessionTransportError(prep); got != prep {
		t.Fatalf("preparation error identity changed: got=%T %v want original", got, got)
	}

	cases := []struct {
		name string
		err  error
		kind sessionUpstreamFailureKind
	}{
		{name: "transport", err: errors.New("dial tcp secret.example:443: refused"), kind: sessionUpstreamFailureTransport},
		{name: "timeout", err: fmt.Errorf("wrapped: %w", context.DeadlineExceeded), kind: sessionUpstreamFailureTimeout},
		{name: "canceled", err: fmt.Errorf("wrapped: %w", context.Canceled), kind: sessionUpstreamFailureCanceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			classified := classifySessionTransportError(tc.err)
			se, ok := asSessionUpstreamError(classified)
			if !ok || se.Kind != tc.kind {
				t.Fatalf("classified=%#v kind=%q want=%q", classified, func() sessionUpstreamFailureKind {
					if se == nil {
						return ""
					}
					return se.Kind
				}(), tc.kind)
			}
			if tc.kind == sessionUpstreamFailureTimeout && !errors.Is(classified, context.DeadlineExceeded) {
				t.Fatal("timeout classification lost DeadlineExceeded unwrap")
			}
			if tc.kind == sessionUpstreamFailureCanceled && !errors.Is(classified, context.Canceled) {
				t.Fatal("canceled classification lost context.Canceled unwrap")
			}
			if strings.Contains(classified.Error(), "secret.example") {
				t.Fatalf("classified error leaked transport detail: %q", classified.Error())
			}
		})
	}
}

func TestPhase7CRejectedSessionErrorKeepsStatusWithoutBody(t *testing.T) {
	err := newSessionUpstreamRejectedError(http.StatusForbidden)
	se, ok := asSessionUpstreamError(err)
	if !ok || se.Kind != sessionUpstreamFailureRejected || se.UpstreamStatus != http.StatusForbidden {
		t.Fatalf("rejected classification=%#v", err)
	}
	if strings.Contains(err.Error(), "403") {
		t.Fatalf("public error text should not embed upstream response/status detail: %q", err.Error())
	}
}

func TestPhase7CForwardNoContentTreatsAny2xxAsConfirmedAndSanitizesNon2xx(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		wantErr    bool
		wantKind   sessionUpstreamFailureKind
		wantStatus int
	}{
		{name: "200 text body", status: http.StatusOK, body: "OK"},
		{name: "204 empty", status: http.StatusNoContent},
		{name: "299 text body", status: 299, body: "accepted"},
		{name: "401 rejected", status: http.StatusUnauthorized, body: "upstream-token=secret", wantErr: true, wantKind: sessionUpstreamFailureRejected, wantStatus: http.StatusUnauthorized},
		{name: "500 rejected", status: http.StatusInternalServerError, body: "database secret", wantErr: true, wantKind: sessionUpstreamFailureRejected, wantStatus: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := phase7BSessionUpstream(t, tc.status, tc.body)
			defer upstream.Close()
			withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, _ http.Handler) {
				req := httptest.NewRequest(http.MethodPost, "/Sessions/Playing/Progress", nil)
				err := app.forwardNoContent(req, app.Upstream.ClientByID("server-a"), http.MethodPost, "/Sessions/Playing/Progress", nil, map[string]any{"ItemId": "item-a"})
				if !tc.wantErr {
					if err != nil {
						t.Fatalf("2xx confirmation returned error: %v", err)
					}
					return
				}
				se, ok := asSessionUpstreamError(err)
				if !ok || se.Kind != tc.wantKind || se.UpstreamStatus != tc.wantStatus {
					t.Fatalf("session error=%#v want kind=%q status=%d", err, tc.wantKind, tc.wantStatus)
				}
				if tc.body != "" && strings.Contains(err.Error(), tc.body) {
					t.Fatalf("upstream response body leaked through error: %q", err.Error())
				}
			})
		})
	}
}
