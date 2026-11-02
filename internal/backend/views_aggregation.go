package backend

import (
	"context"
	"net/http"
	"time"
)

// After the first non-empty Views response, give other fast upstreams a brief
// window. An empty success must not prematurely suppress a slower library.
// This is independent of search/metadata merging: libraries stay per-upstream.
const userViewsGracePeriod = 150 * time.Millisecond

type upstreamViewsResponse struct {
	index int
	items []map[string]any
	err   error
}

// collectUserViews returns successful raw upstream Views in configured order.
// Missing results remain nil, and successful empty results still count as a
// success. The separate success count prevents an upstream outage from being
// mistaken for a genuinely empty library.
//
// Requests are tied to the incoming HTTP request and bounded by globalTimeout.
// Once a response is ready, outstanding requests are canceled; unlike generic
// media-item aggregation, there is no background mapping or source fallback.
func (a *App) collectUserViews(
	request *http.Request,
	reqCtx *RequestContext,
	clients []*UpstreamClient,
	hidden map[string]map[string]struct{},
	globalTimeout time.Duration,
) ([][]map[string]any, int) {
	groups := make([][]map[string]any, len(clients))
	if len(clients) == 0 {
		return groups, 0
	}
	if globalTimeout <= 0 {
		globalTimeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(request.Context(), globalTimeout)
	defer cancel()

	results := make(chan upstreamViewsResponse, len(clients))
	for i, c := range clients {
		go func(index int, client *UpstreamClient) {
			query := cloneValues(request.URL.Query())
			query.Set("UserId", client.clientUserID())
			payload, err := client.RequestJSON(ctx, reqCtx, a.Identity, http.MethodGet,
				"/Users/"+client.clientUserID()+"/Views", query, nil)
			// A buffered channel permits cancellation without requiring the
			// handler to wait for a late upstream response.
			results <- upstreamViewsResponse{index: index, items: asItems(payload), err: err}
		}(i, c)
	}

	received, successes := 0, 0
	var graceTimer *time.Timer
	var graceDone <-chan time.Time
	accept := func(result upstreamViewsResponse) {
		received++
		if result.err != nil {
			return
		}
		successes++
		groups[result.index] = result.items
		if graceTimer == nil && received < len(clients) {
			// A fast-but-hidden library is not useful to this user. Do
			// not let it suppress an accessible library on a slower source.
			for _, item := range result.items {
				if !isHiddenLibraryItem(hidden, clients[result.index].ID, item) {
					graceTimer = time.NewTimer(userViewsGracePeriod)
					graceDone = graceTimer.C
					break
				}
			}
		}
	}

collect:
	for received < len(clients) {
		select {
		case result := <-results:
			accept(result)
		case <-graceDone:
			break collect
		case <-ctx.Done():
			break collect
		}
	}
	if graceTimer != nil {
		defer graceTimer.Stop()
	}
	// Drain completed results at the deadline, without delaying the response
	// for an upstream that has not actually finished.
	for received < len(clients) {
		select {
		case result := <-results:
			accept(result)
		default:
			return groups, successes
		}
	}
	return groups, successes
}
