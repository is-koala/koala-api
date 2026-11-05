package httpx

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
)

// HeaderForwardedFor is the de facto proxy header that carries the client
// address chain (ADR-0021). The RFC 7239 Forwarded header is not read: the
// proxy in front of the API (Traefik) rewrites X-Forwarded-For but passes
// Forwarded through, so a client could forge it.
const HeaderForwardedFor = "X-Forwarded-For"

// maxForwardedHops bounds how many X-Forwarded-For entries are examined.
const maxForwardedHops = 16

type clientIPKey struct{}

// ClientIPFrom returns the client address resolved by the router, or the
// zero Addr when there is none.
func ClientIPFrom(ctx context.Context) netip.Addr {
	a, _ := ctx.Value(clientIPKey{}).(netip.Addr)
	return a
}

// ClientIPResolver resolves the client address of a request behind
// trusted reverse proxies.
type ClientIPResolver struct {
	trusted []netip.Prefix
}

// NewClientIPResolver returns a resolver that believes X-Forwarded-For
// only from the given proxies. With none, the TCP peer is the client.
func NewClientIPResolver(trusted []netip.Prefix) *ClientIPResolver {
	return &ClientIPResolver{trusted: append([]netip.Prefix(nil), trusted...)}
}

// Resolve returns the client address of r:
//
//   - The TCP peer, unless it is a trusted proxy.
//   - Otherwise X-Forwarded-For is read from the right: each trusted proxy
//     appended the address it received the request from. The first entry
//     that is not a trusted proxy is the client. Entries left of it were
//     written by the client and are never believed.
//   - When every entry is a trusted proxy, the leftmost one is the client
//     (an internal caller).
//   - A malformed entry stops the walk; the last trusted hop is used, so a
//     garbled header can only make the client look like the proxy, never
//     like an address of its choosing.
//
// It returns the zero Addr only when RemoteAddr itself is unparsable.
func (c *ClientIPResolver) Resolve(r *http.Request) netip.Addr {
	peer := parseHostAddr(r.RemoteAddr)
	if !peer.IsValid() || !c.isTrusted(peer) {
		return peer
	}
	entries := forwardedEntries(r.Header)
	client := peer
	for i, hops := len(entries)-1, 0; i >= 0 && hops < maxForwardedHops; i, hops = i-1, hops+1 {
		a := parseHostAddr(entries[i])
		if !a.IsValid() {
			return client
		}
		client = a
		if !c.isTrusted(a) {
			return a
		}
	}
	return client
}

func (c *ClientIPResolver) isTrusted(a netip.Addr) bool {
	for _, p := range c.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// middleware stores the resolved client address in the request context.
func (c *ClientIPResolver) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), clientIPKey{}, c.Resolve(r))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// forwardedEntries joins every X-Forwarded-For line in order, as RFC 9110
// §5.3 combines repeated fields.
func forwardedEntries(h http.Header) []string {
	var out []string
	for _, line := range h.Values(HeaderForwardedFor) {
		for e := range strings.SplitSeq(line, ",") {
			out = append(out, strings.TrimSpace(e))
		}
	}
	return out
}

// parseHostAddr parses "ip", "ip:port", "[ipv6]" or "[ipv6]:port" and
// unmaps IPv4-in-IPv6. Zones are refused.
func parseHostAddr(s string) netip.Addr {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		s = ap.Addr().String()
	} else if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" {
		return netip.Addr{}
	}
	return a.Unmap()
}
