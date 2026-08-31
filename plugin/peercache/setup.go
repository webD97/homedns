package peercache

import (
	"context"
	"os"
	"strconv"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"
	"github.com/miekg/dns"
	"k8s.io/apimachinery/pkg/labels"
)

// defaultPort is both the local listener port and the port siblings are dialled
// on, because every replica runs the same Corefile.
const defaultPort = 8053

func init() { plugin.Register(pluginName, setup) }

func setup(c *caddy.Controller) error {
	p, err := parseConfig(c)
	if err != nil {
		return plugin.Error(pluginName, err)
	}

	c.OnStartup(p.start)
	c.OnShutdown(p.shutdown)

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		p.Next = next
		return p
	})
	return nil
}

func parseConfig(c *caddy.Controller) (*PeerCache, error) {
	p := &PeerCache{
		port:   defaultPort,
		store:  newStore(storeCapacity),
		client: &dns.Client{Net: "udp", Timeout: probeTimeout},
	}
	configured := false

	for c.Next() {
		if configured {
			return nil, c.Err("peercache can only be configured once per server block")
		}
		configured = true

		// RemainingArgs, not NextArg: NextArg returns the opening brace as an
		// argument.
		if args := c.RemainingArgs(); len(args) > 0 {
			return nil, c.Err("peercache takes no arguments; configure it with `selector` inside the block")
		}

		for c.NextBlock() {
			switch c.Val() {
			case "selector":
				args := c.RemainingArgs()
				if len(args) != 1 {
					return nil, c.ArgErr()
				}
				if _, err := labels.Parse(args[0]); err != nil {
					return nil, c.Errf("selector: %q is not a label selector: %v", args[0], err)
				}
				p.selector = args[0]

			case "port":
				args := c.RemainingArgs()
				if len(args) != 1 {
					return nil, c.ArgErr()
				}
				n, err := strconv.Atoi(args[0])
				if err != nil || n < 1 || n > 65535 {
					return nil, c.Errf("port: %q is not a port number", args[0])
				}
				p.port = n

			default:
				return nil, c.Errf("unknown property %q", c.Val())
			}
		}
	}

	if p.selector == "" {
		return nil, c.Err("peercache needs a `selector` matching the replicas of this deployment")
	}

	return p, nil
}

// start brings up the probe listener and pod discovery; wired to caddy's
// OnStartup. Neither is fatal: without POD_IP the plugin degrades to a
// pass-through that still records answers for later.
func (p *PeerCache) start() error {
	ctx, cancel := context.WithCancel(context.Background())
	p.stop = cancel

	self := os.Getenv(podIPEnv)
	if self == "" {
		log.Warningf("%s is not set, so this replica cannot identify itself or bind a probe "+
			"listener; peer racing is disabled and every query takes the upstream leg alone", podIPEnv)
		return nil
	}

	p.wg.Add(2)
	go p.listen(ctx, self)
	go p.discover(ctx, self)
	return nil
}

// shutdown stops both goroutines and waits for them; wired to caddy's
// OnShutdown. Needed because the reload plugin rebuilds the handler on every
// Corefile change.
func (p *PeerCache) shutdown() error {
	if p.stop == nil {
		return nil
	}
	p.stop()
	p.wg.Wait()
	return nil
}
