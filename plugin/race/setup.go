package race

import (
	"crypto/tls"
	"fmt"
	"time"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/pkg/parse"
	"github.com/coredns/coredns/plugin/pkg/proxy"
	pkgtls "github.com/coredns/coredns/plugin/pkg/tls"
	"github.com/coredns/coredns/plugin/pkg/transport"
)

// defaultExpire matches pkg/proxy's own, and raising it is a trap: a public
// resolver hangs up on idle DoT connections after roughly this long, so holding
// them longer does not keep them warm, it just fills the pool with connections
// that are already dead. An earlier 30s default did exactly that, and because
// race dials every upstream at once, every pool went stale at the same instant
// and the client got a SERVFAIL from all legs together.
const defaultExpire = 10 * time.Second

func init() { plugin.Register(pluginName, setup) }

func setup(c *caddy.Controller) error {
	rc, err := parseConfig(c)
	if err != nil {
		return plugin.Error(pluginName, err)
	}

	c.OnStartup(rc.start)
	c.OnShutdown(rc.stop)

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		rc.Next = next
		return rc
	})
	return nil
}

func parseConfig(c *caddy.Controller) (*Race, error) {
	rc := &Race{expire: defaultExpire}
	configured := false

	for c.Next() {
		if configured {
			return nil, c.Err("race can only be configured once per server block")
		}
		configured = true

		if !c.Args(&rc.from) {
			return nil, c.ArgErr()
		}
		zones := plugin.Host(rc.from).NormalizeExact()
		if len(zones) != 1 {
			return nil, c.Errf("cannot normalize zone %q", rc.from)
		}
		rc.from = zones[0]

		// RemainingArgs, not NextArg: NextArg returns the opening brace as an
		// argument.
		to := c.RemainingArgs()
		if len(to) < 2 {
			return nil, c.Err("race needs at least two upstreams; with one there is nothing to race and `forward` is the plugin for the job")
		}

		// The block is read before the addresses, because tls_servername has to
		// be known by the time a proxy's TLS config is built.
		for c.NextBlock() {
			switch c.Val() {
			case "tls_servername":
				args := c.RemainingArgs()
				if len(args) != 1 {
					return nil, c.ArgErr()
				}
				rc.tlsServerName = args[0]

			case "expire":
				args := c.RemainingArgs()
				if len(args) != 1 {
					return nil, c.ArgErr()
				}
				d, err := time.ParseDuration(args[0])
				if err != nil || d <= 0 {
					return nil, c.Errf("expire: %q is not a positive duration", args[0])
				}
				rc.expire = d

			default:
				return nil, c.Errf("unknown property %q", c.Val())
			}
		}

		if err := rc.addUpstreams(to); err != nil {
			return nil, c.Err(err.Error())
		}
	}

	return rc, nil
}

// addUpstreams turns Corefile addresses into proxies, following forward's
// parsing: HostPortOrFile applies the default port for the transport, so
// `tls://9.9.9.11` means `9.9.9.11:853`.
func (rc *Race) addUpstreams(to []string) error {
	hosts, err := parse.HostPortOrFile(to...)
	if err != nil {
		return err
	}

	tlsConfig, err := pkgtls.NewTLSConfigFromArgs()
	if err != nil {
		return err
	}
	tlsConfig.ServerName = rc.tlsServerName
	// Set by forward itself, not by pkg/tls or pkg/proxy. Without it every
	// reconnect pays a full handshake where it could have resumed one.
	tlsConfig.ClientSessionCache = tls.NewLRUClientSessionCache(len(hosts))
	rc.tlsConfig = tlsConfig

	for _, host := range hosts {
		trans, addr := parse.Transport(host)
		if trans != transport.DNS && trans != transport.TLS {
			return fmt.Errorf("%q is not supported as an upstream protocol, use dns:// or tls://: %s", trans, host)
		}

		p := proxy.NewProxy(pluginName, addr, trans)
		if trans == transport.TLS {
			p.SetTLSConfig(rc.tlsConfig)
		}
		p.SetExpire(rc.expire)
		p.SetMaxIdleConns(maxIdleConns)
		rc.proxies = append(rc.proxies, p)
	}

	if rc.tlsServerName == "" {
		for _, p := range rc.proxies {
			if p.GetTransport().GetTLSConfig() != nil {
				log.Warning("tls:// upstreams are configured without tls_servername, so certificates are validated against the upstream IP")
				break
			}
		}
	}
	return nil
}

// start launches each proxy's connection manager, without which no connection
// is ever pooled. Wired to caddy's OnStartup, which the reload plugin re-runs
// in place on every Corefile change.
func (rc *Race) start() error {
	for _, p := range rc.proxies {
		p.Start(hcInterval)
	}
	return nil
}

func (rc *Race) stop() error {
	for _, p := range rc.proxies {
		p.Stop()
	}
	return nil
}
