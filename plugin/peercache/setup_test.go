package peercache

import (
	"testing"

	"github.com/coredns/caddy"
)

func TestParseConfigDefaults(t *testing.T) {
	c := caddy.NewTestController("dns", `peercache {
		selector app.kubernetes.io/name=homedns
	}`)

	p, err := parseConfig(c)
	if err != nil {
		t.Fatal(err)
	}

	if p.selector != "app.kubernetes.io/name=homedns" {
		t.Errorf("selector = %q", p.selector)
	}
	if p.port != defaultPort {
		t.Errorf("port = %d, want %d", p.port, defaultPort)
	}
	if p.store == nil {
		t.Error("store not initialised")
	}
}

func TestParseConfigFull(t *testing.T) {
	c := caddy.NewTestController("dns", `peercache {
		selector app.kubernetes.io/name=homedns,app.kubernetes.io/instance=homedns
		port 9053
	}`)

	p, err := parseConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	if p.port != 9053 {
		t.Errorf("port = %d", p.port)
	}
}

func TestParseConfigErrors(t *testing.T) {
	for name, input := range map[string]string{
		"no selector":       `peercache {}`,
		"inline argument":   "peercache foo {\n selector a=b\n}",
		"twice":             "peercache {\n selector a=b\n}\npeercache {\n selector a=b\n}",
		"unknown property":  "peercache {\n selector a=b\n nope 1\n}",
		"selector no value": "peercache {\n selector\n}",
		"selector two args": "peercache {\n selector a=b c=d\n}",
		"selector invalid":  "peercache {\n selector =\n}",
		"port no value":     "peercache {\n selector a=b\n port\n}",
		"port not a number": "peercache {\n selector a=b\n port http\n}",
		"port zero":         "peercache {\n selector a=b\n port 0\n}",
		"port too large":    "peercache {\n selector a=b\n port 70000\n}",
	} {
		if _, err := parseConfig(caddy.NewTestController("dns", input)); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

func TestSetupRegistersPlugin(t *testing.T) {
	c := caddy.NewTestController("dns", `peercache {
		selector app.kubernetes.io/name=homedns
	}`)
	if err := setup(c); err != nil {
		t.Fatal(err)
	}
}

// Without POD_IP there is nothing to bind or to exclude, so the plugin must
// come up as a pass-through rather than refusing to start.
func TestStartWithoutPodIP(t *testing.T) {
	t.Setenv(podIPEnv, "")

	p := &PeerCache{store: newStore(storeCapacity)}
	if err := p.start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := p.shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}
