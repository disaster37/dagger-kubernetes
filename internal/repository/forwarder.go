package repository

import (
	"net/http"

	"github.com/disaster37/go-raft/httpext"
)

// DisableForwarderRedirects configures fwd to pass redirect responses through
// unchanged instead of following them. A reverse proxy must never follow
// redirects: the Set-Cookie headers on a 302 (e.g. the OAuth callback, which
// sets the session cookies before redirecting the browser to the SPA) would
// otherwise be dropped, and the browser would receive the redirect target's
// body instead of the redirect itself.
func DisableForwarderRedirects(fwd *httpext.Forwarder) {
	if fwd == nil {
		return
	}
	fwd.Client().CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
}
