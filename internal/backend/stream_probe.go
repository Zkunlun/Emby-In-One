package backend

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"
)

// streamLivenessProbePath is the path liveness probes are issued against. It is
// deliberately inside the /Videos/ route family the line is actually used for:
// a line whose reverse proxy only forwards /Videos/ and /Audio/ (a common
// self-hosted split-tunnel setup) answers 403/404 here, and that answer is
// exactly what proves the whole line — proxy hop included — is up. The probe
// never depends on any Emby endpoint being exposed at the root.
const streamLivenessProbePath = "/Videos/probe"

// redirectRecoveryProbeCap prevents a client playback request from inheriting an
// arbitrarily large administrator health-check timeout. Recovery probes still
// honor a smaller configured timeout, but never block redirect playback beyond
// this per-probe ceiling; selected bases are checked concurrently.
const redirectRecoveryProbeCap = 5 * time.Second

// streamProbeTimeout bounds one liveness probe. It shares the health-check
// timeout when one is configured and falls back to 10s otherwise.
func (c *UpstreamClient) streamProbeTimeout() time.Duration {
	if c.timeouts.HealthCheck > 0 {
		return time.Duration(c.timeouts.HealthCheck) * time.Millisecond
	}
	return 10 * time.Second
}

func (c *UpstreamClient) redirectRecoveryProbeTimeout() time.Duration {
	timeout := c.streamProbeTimeout()
	if timeout > redirectRecoveryProbeCap {
		return redirectRecoveryProbeCap
	}
	return timeout
}

// probeStreamBases applies the background-health candidate policy, then delegates
// the actual concurrent checks to probeSelectedStreamBases. Keeping selection and
// execution separate lets request-scoped recovery probe an explicit subset later
// without changing periodic health-check semantics.
func (c *UpstreamClient) probeStreamBases(ctx context.Context) {
	bases := c.streamBaseProbeCandidates(time.Now())
	c.probeSelectedStreamBases(ctx, bases, c.streamProbeTimeout())
}

// probeSelectedStreamBases checks exactly the supplied stream bases in parallel
// and updates their liveness marks. Authentication/not-found responses still prove
// a route reachable; transport failures and explicit gateway-unavailability
// responses (502/503/504) mark a base dead.
//
// The probe uses the client's own transport (including its outbound proxy) but no
// credentials: the request carries no token, so nothing is exposed to a line that
// might not even be the administrator's own. Candidate selection belongs to the
// caller; this executor deliberately supports a single explicit base as well.
func (c *UpstreamClient) probeSelectedStreamBases(ctx context.Context, bases []string, timeout time.Duration) {
	if len(bases) == 0 {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = c.streamProbeTimeout()
	}
	client := &http.Client{
		Transport:     c.transport,
		Timeout:       timeout,
		CheckRedirect: redirectPolicy(c.Config.FollowRedirects),
	}
	var wg sync.WaitGroup
	for _, base := range bases {
		base := base
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-ctx.Done():
				return
			default:
			}
			generation := c.beginStreamBaseObservation(base)
			if probeOneStreamBase(ctx, client, strings.TrimRight(base, "/")+streamLivenessProbePath) {
				if c.markStreamBaseAliveObservation(base, generation) && c.logger != nil {
					c.logger.Debugf("[%s] Stream line alive: %s", c.Name, base)
				}
			} else {
				if c.markStreamBaseFailedObservation(base, generation) && c.logger != nil {
					c.logger.Warnf("[%s] Stream line probe failed, marked dead; revalidation suppressed for %s: %s",
						c.Name, streamFailureCooldown, base)
				}
			}
		}()
	}
	wg.Wait()
}

// probeOneStreamBase issues one liveness request. Transport failures are dead;
// HTTP responses use the shared stream-unavailable classifier so background
// health checks and proxy playback can share one definition of a broken line.
// The URL is built from the administrator-configured base plus a fixed path, so
// it carries no credentials.
func probeOneStreamBase(ctx context.Context, client *http.Client, probeURL string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "Emby-In-One-Liveness/1.0")
	resp, err := client.Do(req) // CodeQL: intentional liveness probe to admin-configured stream base
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return !isStreamUnavailableStatus(resp.StatusCode)
}
