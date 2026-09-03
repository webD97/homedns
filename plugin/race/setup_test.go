package race

import (
	"testing"
	"time"

	"github.com/coredns/caddy"
)

func TestParseConfigDefaults(t *testing.T) {
	c := caddy.NewTestController("dns", `race . 9.9.9.11 149.112.112.11`)

	rc, err := parseConfig(c)
	if err != nil {
		t.Fatal(err)
	}

	if rc.from != "." {
		t.Errorf("from = %q, want the root zone", rc.from)
	}
	if len(rc.proxies) != 2 {
		t.Fatalf("proxies = %d, want 2", len(rc.proxies))
	}
	if rc.expire != defaultExpire {
		t.Errorf("expire = %s, want %s", rc.expire, defaultExpire)
	}
	// HostPortOrFile applies the transport's default port.
	if got := rc.proxies[0].Addr(); got != "9.9.9.11:53" {
		t.Errorf("addr = %q, want 9.9.9.11:53", got)
	}
}

func TestParseConfigFull(t *testing.T) {
	c := caddy.NewTestController("dns", `race . tls://9.9.9.11 tls://149.112.112.11 {
		tls_servername dns.quad9.net
		expire 45s
	}`)

	rc, err := parseConfig(c)
	if err != nil {
		t.Fatal(err)
	}

	if rc.expire != 45*time.Second {
		t.Errorf("expire = %s", rc.expire)
	}
	if got := rc.proxies[0].Addr(); got != "9.9.9.11:853" {
		t.Errorf("addr = %q, want the DoT default port", got)
	}
	if rc.tlsConfig.ServerName != "dns.quad9.net" {
		t.Errorf("ServerName = %q", rc.tlsConfig.ServerName)
	}
	for i, p := range rc.proxies {
		if p.GetTransport().GetTLSConfig() == nil {
			t.Errorf("proxy %d has no TLS config for a tls:// upstream", i)
		}
	}
}

// Session resumption is set up by forward, not by pkg/tls or pkg/proxy. Losing
// it turns every reconnect into a full handshake, silently.
func TestParseConfigEnablesSessionResumption(t *testing.T) {
	c := caddy.NewTestController("dns", `race . tls://9.9.9.11 tls://149.112.112.11 {
		tls_servername dns.quad9.net
	}`)

	rc, err := parseConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	if rc.tlsConfig.ClientSessionCache == nil {
		t.Error("no ClientSessionCache: every reconnect would pay a full TLS handshake")
	}
}

func TestParseConfigErrors(t *testing.T) {
	for name, input := range map[string]string{
		"no arguments":          `race`,
		"zone but no upstreams": `race .`,
		"single upstream":       `race . 9.9.9.11`,
		"bad address":           `race . not-an-ip also-not-an-ip`,
		"unsupported transport": `race . https://9.9.9.11 https://149.112.112.11`,
		"twice":                 "race . 9.9.9.11 149.112.112.11\nrace . 1.1.1.1 1.0.0.1",
		"unknown property":      "race . 9.9.9.11 149.112.112.11 {\n nope 1\n}",
		"servername no value":   "race . 9.9.9.11 149.112.112.11 {\n tls_servername\n}",
		"servername two args":   "race . 9.9.9.11 149.112.112.11 {\n tls_servername a b\n}",
		"expire no value":       "race . 9.9.9.11 149.112.112.11 {\n expire\n}",
		"expire not a duration": "race . 9.9.9.11 149.112.112.11 {\n expire soon\n}",
		"expire zero":           "race . 9.9.9.11 149.112.112.11 {\n expire 0s\n}",
		"expire negative":       "race . 9.9.9.11 149.112.112.11 {\n expire -5s\n}",
	} {
		if _, err := parseConfig(caddy.NewTestController("dns", input)); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

func TestSetupRegistersPlugin(t *testing.T) {
	c := caddy.NewTestController("dns", `race . 9.9.9.11 149.112.112.11`)
	if err := setup(c); err != nil {
		t.Fatal(err)
	}
}

func TestStartStop(t *testing.T) {
	rc, err := parseConfig(caddy.NewTestController("dns", `race . 9.9.9.11 149.112.112.11`))
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := rc.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}
