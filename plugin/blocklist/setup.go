package blocklist

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"time"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
)

const (
	defaultRefresh      = 24 * time.Hour
	defaultReadyTimeout = 60 * time.Second
)

var defaultBootstrapDNS = []string{"1.1.1.1:53", "9.9.9.9:53"}

func init() { plugin.Register(pluginName, setup) }

func setup(c *caddy.Controller) error {
	b, err := parseConfig(c)
	if err != nil {
		return plugin.Error(pluginName, err)
	}

	c.OnStartup(b.start)
	c.OnShutdown(b.shutdown)

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		b.Next = next
		return b
	})
	return nil
}

func parseConfig(c *caddy.Controller) (*Blocklist, error) {
	b := &Blocklist{
		refresh:      defaultRefresh,
		readyTimeout: defaultReadyTimeout,
		lastGood:     map[string][]string{},
	}
	var bootstrap []string
	configured := false

	for c.Next() {
		if configured {
			return nil, c.Err("blocklist can only be configured once per server block")
		}
		configured = true

		// RemainingArgs, not NextArg: NextArg returns the opening brace as an
		// argument.
		if args := c.RemainingArgs(); len(args) > 0 {
			return nil, c.Err("blocklist takes no arguments; configure sources with `url` inside the block")
		}

		for c.NextBlock() {
			switch c.Val() {
			case "url":
				args := c.RemainingArgs()
				if len(args) == 0 {
					return nil, c.ArgErr()
				}
				for _, raw := range args {
					if err := validateURL(raw); err != nil {
						return nil, c.Err(err.Error())
					}
					b.urls = append(b.urls, raw)
				}

			case "allow":
				args := c.RemainingArgs()
				if len(args) == 0 {
					return nil, c.ArgErr()
				}
				for _, raw := range args {
					name, ok := normalizeEntry(raw)
					if !ok {
						return nil, c.Errf("allow: %q is not a domain name", raw)
					}
					b.allow = append(b.allow, name)
				}

			case "bootstrap_dns":
				args := c.RemainingArgs()
				if len(args) == 0 {
					return nil, c.ArgErr()
				}
				for _, raw := range args {
					addr, err := normalizeDNSAddr(raw)
					if err != nil {
						return nil, c.Err(err.Error())
					}
					bootstrap = append(bootstrap, addr)
				}

			case "refresh":
				d, err := durationArg(c)
				if err != nil {
					return nil, err
				}
				if d < time.Minute {
					return nil, c.Errf("refresh: %s is too short, use at least 1m", d)
				}
				b.refresh = d

			case "ready_timeout":
				d, err := durationArg(c)
				if err != nil {
					return nil, err
				}
				b.readyTimeout = d

			default:
				return nil, c.Errf("unknown property %q", c.Val())
			}
		}
	}

	if len(b.urls) == 0 {
		return nil, c.Err("blocklist needs at least one `url`")
	}
	if len(bootstrap) == 0 {
		bootstrap = defaultBootstrapDNS
	}
	b.bootstrap = newBootstrapResolver(bootstrap)

	return b, nil
}

func durationArg(c *caddy.Controller) (time.Duration, error) {
	prop := c.Val()
	args := c.RemainingArgs()
	if len(args) != 1 {
		return 0, c.ArgErr()
	}
	d, err := time.ParseDuration(args[0])
	if err != nil {
		return 0, c.Errf("%s: %q is not a duration (try 24h, 30m, 90s)", prop, args[0])
	}
	if d <= 0 {
		return 0, c.Errf("%s must be positive, got %s", prop, d)
	}
	return d, nil
}

func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("url: %q is not a valid URL: %v", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url: %q must use http or https", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("url: %q has no host", raw)
	}
	return nil
}

// normalizeDNSAddr accepts a bare IP or ip:port and returns a dialable
// host:port. Hostnames are rejected: resolving one is what the bootstrap
// resolver exists to avoid.
func normalizeDNSAddr(raw string) (string, error) {
	if addr, err := netip.ParseAddr(raw); err == nil {
		return net.JoinHostPort(addr.String(), "53"), nil
	}

	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		return "", fmt.Errorf("bootstrap_dns: %q must be an IP or ip:port", raw)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return "", fmt.Errorf("bootstrap_dns: %q must be an IP address, not a hostname", raw)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return "", fmt.Errorf("bootstrap_dns: %q has an invalid port", raw)
	}
	return net.JoinHostPort(addr.String(), port), nil
}
