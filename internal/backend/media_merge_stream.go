package backend

import (
	"net/url"
)

// qualifiedManifestLine builds one rewritten, proxy-relative manifest line while
// keeping the segment on the version selected for its manifest, even when the
// upstream omits MediaSourceId from relative segment references. It is the
// single-pass replacement for the former second pass over the whole manifest:
// RewriteM3U8ForItemWithSource calls it directly with the already parsed path and
// query parameters, so no line is split or parsed twice.
func qualifiedManifestLine(path string, params url.Values, source string) string {
	delete(params, "mediaSourceId")
	params.Set("MediaSourceId", source)
	u := url.URL{Path: path, RawQuery: params.Encode()}
	return u.String()
}
