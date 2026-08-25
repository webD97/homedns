// Package blocklist provides a CoreDNS plugin that answers NXDOMAIN for domains
// named by downloaded blocklists, in the style of Pi-hole. See README.md for
// the Corefile syntax and AGENTS.md for the design decisions.
package blocklist

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/metrics"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"
	"github.com/miekg/dns"
)

const (
	pluginName = "blocklist"
	userAgent  = "homedns-blocklist/1 (+https://github.com/webd97/homedns)"
)

var log = clog.NewWithPlugin(pluginName)

type Blocklist struct {
	Next plugin.Handler

	urls         []string
	allow        []string
	bootstrap    *bootstrapResolver
	refresh      time.Duration
	readyTimeout time.Duration

	current atomic.Pointer[matcher]
	ready   atomic.Bool

	// lastGood holds the most recent successful parse per URL so a source that
	// starts failing keeps contributing. Touched only by the refresh goroutine.
	lastGood map[string][]string

	stop context.CancelFunc
	done chan struct{}
}

func (b *Blocklist) Name() string { return pluginName }

func (b *Blocklist) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}
	server := metrics.WithServer(ctx)

	if b.current.Load().blocked(normalizeQuery(state.Name())) {
		blockedCount.WithLabelValues(server).Inc()

		// Plain NXDOMAIN without an SOA, as Pi-hole and dnsmasq do.
		msg := new(dns.Msg)
		msg.SetRcode(r, dns.RcodeNameError)
		msg.Authoritative = true
		if err := w.WriteMsg(msg); err != nil {
			return dns.RcodeServerFailure, err
		}
		return dns.RcodeNameError, nil
	}

	allowedCount.WithLabelValues(server).Inc()
	return plugin.NextOrFailure(b.Name(), b.Next, ctx, w, r)
}

// Ready implements the interface CoreDNS's ready plugin looks for on each
// handler in the chain. False until a list loads or readyTimeout elapses.
func (b *Blocklist) Ready() bool { return b.ready.Load() }

// start launches the refresh loop; wired to caddy's OnStartup.
func (b *Blocklist) start() error {
	ctx, cancel := context.WithCancel(context.Background())
	b.stop = cancel
	b.done = make(chan struct{})

	go func() {
		defer close(b.done)

		b.reload(ctx)

		ticker := time.NewTicker(b.refresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				b.reload(ctx)
			}
		}
	}()

	go b.failOpenAfterTimeout(ctx)
	return nil
}

// shutdown stops the refresh loop and waits for it; wired to caddy's
// OnShutdown. Needed because the reload plugin rebuilds the handler on every
// Corefile change.
func (b *Blocklist) shutdown() error {
	if b.stop == nil {
		return nil
	}
	b.stop()
	<-b.done
	return nil
}

func (b *Blocklist) failOpenAfterTimeout(ctx context.Context) {
	timer := time.NewTimer(b.readyTimeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
	case <-timer.C:
		if !b.ready.Swap(true) {
			log.Warningf("no blocklist source loaded after %s; reporting ready and serving "+
				"queries unfiltered until one succeeds", b.readyTimeout)
			failOpen.Set(1)
		}
	}
}

// reload fetches every source and republishes the matcher. Refresh goroutine
// only.
func (b *Blocklist) reload(ctx context.Context) {
	started := time.Now()
	client := b.bootstrap.httpClient()

	var merged []string
	for _, url := range b.urls {
		if ctx.Err() != nil {
			return
		}

		names, err := fetch(ctx, client, url)
		if err != nil {
			sourceFailures.WithLabelValues(url).Inc()
			if stale, ok := b.lastGood[url]; ok {
				log.Warningf("refreshing %s: %v; keeping the %d entries from the last good fetch",
					url, err, len(stale))
				merged = append(merged, stale...)
			} else {
				log.Warningf("fetching %s: %v; no previous copy to fall back on", url, err)
			}
			continue
		}

		b.lastGood[url] = names
		sourceDomains.WithLabelValues(url).Set(float64(len(names)))
		sourceLastSuccess.WithLabelValues(url).Set(float64(time.Now().Unix()))
		merged = append(merged, names...)
	}

	reloadDuration.Observe(time.Since(started).Seconds())

	if len(merged) == 0 {
		log.Errorf("no blocklist entries loaded from %d source(s); leaving the current list in place", len(b.urls))
		return
	}

	m := &matcher{block: newNameSet(merged), allow: newNameSet(b.allow)}
	b.current.Store(m)
	domainsTotal.Set(float64(m.size()))
	failOpen.Set(0)

	verb := "refreshed"
	if !b.ready.Swap(true) {
		verb = "ready"
	}
	log.Infof("%s: %d blocked domains from %d source(s) in %s",
		verb, m.size(), len(b.urls), time.Since(started).Round(time.Millisecond))
}
