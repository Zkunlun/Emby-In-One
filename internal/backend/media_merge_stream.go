package backend

import (
	"net/url"
	"strings"
)

// Keep each rewritten segment on the version selected for its manifest, even
// when the upstream omits MediaSourceId from relative segment references.
func qualifyMergeManifestSources(content, item, source string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		u, err := url.Parse(trimmed)
		if err != nil || u.IsAbs() {
			continue
		}
		parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
		if len(parts) < 3 || (parts[0] != "Videos" && parts[0] != "Audio") || parts[1] != item {
			continue
		}
		q := u.Query()
		delete(q, "mediaSourceId")
		q.Set("MediaSourceId", source)
		u.RawQuery = q.Encode()
		lines[i] = u.String()
	}
	return strings.Join(lines, "\n")
}
