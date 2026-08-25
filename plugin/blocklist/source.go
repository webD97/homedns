package blocklist

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// httpTimeout caps a single source fetch. The big lists are ~6 MB, so this is
// generous; it exists to stop a hung server from stalling the refresh loop.
const httpTimeout = 30 * time.Second

// bootstrapResolver resolves the blocklist URLs' hostnames.
//
// We cannot use the ambient resolver for this. This process *is* the network's
// resolver, and inside Kubernetes /etc/resolv.conf points at the cluster DNS,
// which in this deployment may well forward back to us — so resolving
// raw.githubusercontent.com the normal way is circular and deadlocks the very
// first fetch. Dialing a known public resolver directly breaks the cycle.
type bootstrapResolver struct {
	servers []string // host:port, validated at setup time
	next    atomic.Uint32
}

func newBootstrapResolver(servers []string) *bootstrapResolver {
	return &bootstrapResolver{servers: servers}
}

// resolver builds a net.Resolver that dials our bootstrap servers.
//
// PreferGo is required, not cosmetic: with cgo resolution the Dial hook is
// ignored entirely and lookups silently fall back to the system resolver.
func (b *bootstrapResolver) resolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			// Rotate on every attempt so a dead first server doesn't stall
			// every lookup; Go's resolver retries, landing on the next one.
			n := len(b.servers)
			start := int(b.next.Add(1)-1) % n

			var err error
			for i := 0; i < n; i++ {
				srv := b.servers[(start+i)%n]
				var conn net.Conn
				conn, err = d.DialContext(ctx, network, srv)
				if err == nil {
					return conn, nil
				}
			}
			return nil, fmt.Errorf("blocklist: no bootstrap DNS server reachable: %w", err)
		},
	}
}

// httpClient returns a client whose dialer resolves through the bootstrap
// servers.
func (b *bootstrapResolver) httpClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Resolver:  b.resolver(),
	}
	return &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			// Lists are fetched once a day; pooling buys nothing and holding
			// idle connections open to a CDN for 24h buys less.
			DisableKeepAlives: true,
		},
	}
}

// fetch downloads one source and parses it into domain names.
func fetch(ctx context.Context, client *http.Client, url string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	names, err := parseList(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		// A 200 response that parses to nothing is almost always an error page
		// or a moved list, not a genuinely empty blocklist. Treating it as a
		// failure keeps the previous contents in place.
		return nil, fmt.Errorf("parsed 0 domains — wrong URL or unrecognised format?")
	}
	return names, nil
}
