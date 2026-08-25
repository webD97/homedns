package blocklist

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

const httpTimeout = 30 * time.Second

// bootstrapResolver resolves the blocklist URLs' hostnames without using the
// ambient resolver, which would be circular — this process is the resolver, and
// in-cluster DNS may forward back to it.
type bootstrapResolver struct {
	servers []string // host:port, validated at setup
	next    atomic.Uint32
}

func newBootstrapResolver(servers []string) *bootstrapResolver {
	return &bootstrapResolver{servers: servers}
}

func (b *bootstrapResolver) resolver() *net.Resolver {
	return &net.Resolver{
		// Required: the cgo resolver ignores Dial and falls back to the system
		// resolver without saying so.
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
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
			DisableKeepAlives:     true, // fetched once a day
		},
	}
}

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
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}

	names, err := parseList(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		// Almost always an error page or a moved list rather than a genuinely
		// empty blocklist; failing keeps the previous contents.
		return nil, fmt.Errorf("parsed 0 domains — wrong URL or unrecognised format?")
	}
	return names, nil
}
