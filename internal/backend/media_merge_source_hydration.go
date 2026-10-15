package backend

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// detailNeedsSourceHydration compares this particular upstream item's current
// source list to the only reliable local evidence: source-qualified members
// already associated with the same item. A single returned source cannot prove
// there are no other versions, even if MediaSourceCount is also one.
func (a *App) detailNeedsSourceHydration(id, server, original string, data map[string]any) bool {
	kind, _ := data["Type"].(string)
	if kind != "Movie" && kind != "Episode" {
		return false
	}
	raw := asItems(map[string]any{"Items": data["MediaSources"]})
	if len(raw) <= 1 {
		return true
	}
	if count := playbackTicksFromBody(data, "MediaSourceCount"); count.valid() &&
		count.Ticks > int64(len(raw)) {
		return true
	}
	group := a.IDStore.ResolveMergeGroup(id)
	if group == nil {
		return false
	}
	observed := make(map[string]bool, len(raw))
	for _, source := range raw {
		if key, _ := source["Id"].(string); key != "" {
			observed[key] = true
		}
	}
	for _, member := range group.Members {
		ref := member.Ref
		if ref.ServerID == server && ref.ItemID == original &&
			ref.MediaSourceID != "" && !observed[ref.MediaSourceID] {
			return true
		}
	}
	return false
}

// Only safe descriptive metadata may pass from a speculative PlaybackInfo
// discovery into item details. DirectStreamUrl, TranscodingUrl, Path,
// PlaySessionId, token-bearing nested URLs, etc. belong exclusively to the
// authenticated PlaybackInfo/stream route with per-request rewriting.
func mergeSourceDisplayMetadata(source map[string]any) map[string]any {
	keep := map[string]any{}
	for _, name := range []string{
		"Id", "Name", "Container", "Protocol", "RunTimeTicks", "Size", "Bitrate",
		"VideoType", "VideoCodec", "AudioCodec", "Width", "Height",
		"SupportsDirectPlay", "SupportsDirectStream", "SupportsTranscoding",
		"IsRemote", "IsInfiniteStream", "DefaultAudioStreamIndex",
		"DefaultSubtitleStreamIndex",
	} {
		if v, exists := source[name]; exists && v != nil {
			keep[name] = v
		}
	}
	if streams := asItems(map[string]any{"Items": source["MediaStreams"]}); len(streams) > 0 {
		safe := make([]any, 0, len(streams))
		for _, stream := range streams {
			item := map[string]any{}
			for _, key := range []string{
				"Index", "Type", "Codec", "Language", "DisplayTitle", "Title",
				"IsDefault", "IsForced", "IsExternal", "Channels", "BitRate",
				"Width", "Height", "SampleRate", "AverageFrameRate",
			} {
				if v, ok := stream[key]; ok && v != nil {
					item[key] = v
				}
			}
			if len(item) > 0 {
				safe = append(safe, item)
			}
		}
		if len(safe) > 0 {
			keep["MediaStreams"] = safe
		}
	}
	return keep
}

type detailProbeKey struct {
	Client         *UpstreamClient
	UpstreamItemID string
	UpstreamUserID string
	ProxyUserID    string
}

type detailProbeEntry struct {
	Sources []map[string]any // only whitelisted display metadata
	Expires time.Time
}

// In-memory, user-scoped, bounded short-lived source snapshots. No playback
// sessions, direct stream URLs, tokens, or paths may enter this cache.
type detailProbeCache struct {
	mu       sync.Mutex
	entries  map[detailProbeKey]detailProbeEntry
	inFlight map[detailProbeKey]chan struct{}
}

func (c *detailProbeCache) lookupOrFetch(
	ctx context.Context, key detailProbeKey, fetch func() []map[string]any,
) []map[string]any {
	for {
		if ctx.Err() != nil {
			return nil
		}
		c.mu.Lock()
		now := time.Now()
		if saved, ok := c.entries[key]; ok && now.Before(saved.Expires) {
			out := make([]map[string]any, 0, len(saved.Sources))
			for _, source := range saved.Sources {
				out = append(out, deepCloneMap(source))
			}
			c.mu.Unlock()
			return out
		}
		if pending := c.inFlight[key]; pending != nil {
			c.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return nil
			}
		}
		if c.inFlight == nil {
			c.inFlight = make(map[detailProbeKey]chan struct{})
		}
		ch := make(chan struct{})
		c.inFlight[key] = ch
		c.mu.Unlock()

		found := fetch()
		ttl := 30 * time.Second
		if len(found) == 0 {
			ttl = 5 * time.Second
		}
		safe := make([]map[string]any, 0, len(found))
		for _, source := range found {
			safe = append(safe, deepCloneMap(source))
		}

		c.mu.Lock()
		if c.entries == nil {
			c.entries = make(map[detailProbeKey]detailProbeEntry)
		}
		for k, entry := range c.entries {
			if !time.Now().Before(entry.Expires) {
				delete(c.entries, k)
			}
		}
		c.entries[key] = detailProbeEntry{Sources: safe, Expires: time.Now().Add(ttl)}
		// Opportunistic cap avoids unbounded growth during heavy client use.
		for len(c.entries) > 512 {
			for k := range c.entries {
				delete(c.entries, k)
				break
			}
		}
		delete(c.inFlight, key)
		close(ch)
		c.mu.Unlock()
		return found
	}
}

func (a *App) requestSafeDetailVersions(
	ctx context.Context, reqCtx *RequestContext, instance seriesInstance,
) []map[string]any {
	userID := instance.Client.clientUserID()
	proxyID := ""
	if reqCtx != nil && reqCtx.ProxyUser != nil {
		proxyID = reqCtx.ProxyUser.UserID
	}
	key := detailProbeKey{
		Client: instance.Client, UpstreamItemID: instance.OriginalID,
		UpstreamUserID: userID, ProxyUserID: proxyID,
	}
	return a.sourceProbeCache.lookupOrFetch(ctx, key, func() []map[string]any {
		query := url.Values{"UserId": {userID}}
		body := map[string]any{"UserId": userID, "IsPlayback": false}
		payload, err := instance.Client.RequestJSON(
			ctx, reqCtx, a.Identity, http.MethodPost,
			"/Items/"+instance.OriginalID+"/PlaybackInfo", query, body,
		)
		if err != nil || ctx.Err() != nil || !a.isServerAllowed(reqCtx, instance.ServerID) {
			return nil
		}
		result, ok := payload.(map[string]any)
		if !ok {
			return nil
		}
		sources := asItems(map[string]any{"Items": result["MediaSources"]})
		safe := make([]map[string]any, 0, len(sources))
		for _, source := range sources {
			id, _ := source["Id"].(string)
			if strings.TrimSpace(id) == "" {
				continue
			}
			if owner, _ := source["ItemId"].(string); owner != "" && owner != instance.OriginalID {
				continue
			}
			safe = append(safe, mergeSourceDisplayMetadata(source))
		}
		return safe
	})
}

// At most one supplemental request per *user-opened detail item*, never per
// list row, scan, search result or arbitrary historical group member.
// The upstream may not implement PlaybackInfo or may still return one version:
// either way the caller keeps the original detail response without fabricating
// playable sources merely from old ID-store members.
func (a *App) hydrateSingleItemSources(
	ctx context.Context, reqCtx *RequestContext, id string,
	instance seriesInstance, data map[string]any,
) {
	if instance.Client == nil ||
		!a.isServerAllowed(reqCtx, instance.ServerID) ||
		!a.detailNeedsSourceHydration(id, instance.ServerID, instance.OriginalID, data) ||
		ctx.Err() != nil {
		return
	}
	userID := instance.Client.clientUserID()
	if userID == "" {
		return
	}
	supplemental := a.requestSafeDetailVersions(ctx, reqCtx, instance)
	if len(supplemental) == 0 {
		return
	}
	existing := asItems(map[string]any{"Items": data["MediaSources"]})
	combined := make([]any, 0, len(existing)+len(supplemental))
	index := map[string]int{}
	for _, source := range existing {
		copy := deepCloneMap(source)
		key, _ := copy["Id"].(string)
		if strings.TrimSpace(key) != "" {
			index[key] = len(combined)
		}
		combined = append(combined, copy)
	}
	for _, source := range supplemental {
		key, _ := source["Id"].(string)
		if strings.TrimSpace(key) == "" {
			continue
		}
		// Supplemental metadata was validated and sanitized before caching.
		safe := source
		if pos, present := index[key]; present {
			destination, ok := combined[pos].(map[string]any)
			if !ok {
				continue
			}
			for field, value := range safe {
				if original, exists := destination[field]; !exists || original == nil || original == "" {
					destination[field] = value
				}
			}
		} else {
			index[key] = len(combined)
			combined = append(combined, safe)
		}
	}
	if len(combined) > len(existing) {
		data["MediaSources"] = combined
	}
}
