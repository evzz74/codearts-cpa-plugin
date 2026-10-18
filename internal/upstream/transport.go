package upstream

import "net/http"

// SetTransport installs a request-scoped HTTP boundary for both short requests
// and long-lived SSE streams. A nil transport leaves the standalone clients as-is.
func (c *Client) SetTransport(transport http.RoundTripper) {
	if transport == nil {
		return
	}
	c.http.Transport = transport
	c.streamHTTP.Transport = transport
}
