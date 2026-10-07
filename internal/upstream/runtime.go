package upstream

import (
	"net/http"
	"time"
)

// RuntimeOptions are immutable for the lifetime of one client snapshot.
type RuntimeOptions struct {
	Timeout, HeaderTimeout, IdleTimeout                     time.Duration
	SanitizeFingerprints, PromptCacheKey, RepairToolHistory bool
}

// WithRuntime prepares independent HTTP clients and a transport. Existing streams
// retain their old client/transport; caches and immutable realm profiles are shared.
func (c *Client) WithRuntime(o RuntimeOptions) *Client {
	next := *c
	transport := c.HTTP.Transport
	if tr, ok := transport.(*http.Transport); ok {
		copy := tr.Clone()
		copy.ResponseHeaderTimeout = o.HeaderTimeout
		transport = copy
	}
	short := *c.HTTP
	short.Timeout, short.Transport = o.Timeout, transport
	chat := *c.chatHTTP()
	chat.Timeout, chat.Transport = 0, transport
	next.HTTP, next.ChatHTTP = &short, &chat
	next.HeaderTimeout, next.IdleTimeout = o.HeaderTimeout, o.IdleTimeout
	next.SanitizeFingerprints = o.SanitizeFingerprints
	next.PromptCacheKey = o.PromptCacheKey
	next.RepairToolHistory = o.RepairToolHistory
	return &next
}

// CloseIdleConnections never interrupts active response bodies. Transport's
// close-idle mode also retires active connections when they become idle.
func (c *Client) CloseIdleConnections() {
	if c != nil && c.HTTP != nil {
		c.HTTP.CloseIdleConnections()
	}
	if c != nil && c.ChatHTTP != nil {
		c.ChatHTTP.CloseIdleConnections()
	}
}
