package blocklist

import (
	"slices"
	"testing"
	"time"

	"github.com/coredns/caddy"
)

func TestParseConfigDefaults(t *testing.T) {
	c := caddy.NewTestController("dns", `blocklist {
		url https://lists.example.com/hosts
	}`)

	b, err := parseConfig(c)
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(b.urls, []string{"https://lists.example.com/hosts"}) {
		t.Errorf("urls = %v", b.urls)
	}
	if b.refresh != defaultRefresh {
		t.Errorf("refresh = %s, want %s", b.refresh, defaultRefresh)
	}
	if b.readyTimeout != defaultReadyTimeout {
		t.Errorf("readyTimeout = %s, want %s", b.readyTimeout, defaultReadyTimeout)
	}
	if !slices.Equal(b.bootstrap.servers, defaultBootstrapDNS) {
		t.Errorf("bootstrap = %v, want %v", b.bootstrap.servers, defaultBootstrapDNS)
	}
}

func TestParseConfigFull(t *testing.T) {
	c := caddy.NewTestController("dns", `blocklist {
		url https://lists.example.com/hosts https://lists.example.com/domains
		url https://third.example.com/list
		bootstrap_dns 9.9.9.9 1.1.1.1:5353 2606:4700:4700::1111
		allow good.example.com *.also-good.example.net
		refresh 6h
		ready_timeout 30s
	}`)

	b, err := parseConfig(c)
	if err != nil {
		t.Fatal(err)
	}

	if len(b.urls) != 3 {
		t.Errorf("urls = %v, want 3", b.urls)
	}
	if b.refresh != 6*time.Hour {
		t.Errorf("refresh = %s", b.refresh)
	}
	if b.readyTimeout != 30*time.Second {
		t.Errorf("readyTimeout = %s", b.readyTimeout)
	}

	// A bare IP gains :53; an explicit port is kept; IPv6 is bracketed.
	want := []string{"9.9.9.9:53", "1.1.1.1:5353", "[2606:4700:4700::1111]:53"}
	if !slices.Equal(b.bootstrap.servers, want) {
		t.Errorf("bootstrap = %v, want %v", b.bootstrap.servers, want)
	}

	// allow entries are normalised the same way list entries are.
	if !slices.Equal(b.allow, []string{"good.example.com", "also-good.example.net"}) {
		t.Errorf("allow = %v", b.allow)
	}
}

func TestParseConfigErrors(t *testing.T) {
	for name, input := range map[string]string{
		"no url":            `blocklist { refresh 1h }`,
		"empty block":       `blocklist { }`,
		"url without value": `blocklist { url }`,
		"bad scheme":        `blocklist { url ftp://lists.example.com/hosts }`,
		"no host":           `blocklist { url https:/// }`,
		"unknown property": `blocklist { url https://a.example.com/l
			sinkhole 0.0.0.0 }`,
		"inline argument": `blocklist https://a.example.com/l`,
		"bad duration": `blocklist { url https://a.example.com/l
			refresh sometimes }`,
		"refresh too short": `blocklist { url https://a.example.com/l
			refresh 1s }`,
		"negative duration": `blocklist { url https://a.example.com/l
			ready_timeout -5s }`,
		// A hostname here would need resolving, which is exactly what the
		// bootstrap resolver exists to avoid.
		"bootstrap hostname": `blocklist { url https://a.example.com/l
			bootstrap_dns dns.example.com }`,
		"bootstrap bad port": `blocklist { url https://a.example.com/l
			bootstrap_dns 1.1.1.1:99999 }`,
		"allow not a domain": `blocklist { url https://a.example.com/l
			allow localhost }`,
		"configured twice": `blocklist { url https://a.example.com/l }
			blocklist { url https://b.example.com/l }`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseConfig(caddy.NewTestController("dns", input)); err == nil {
				t.Error("expected an error, got none")
			}
		})
	}
}

func TestSetupRegistersPlugin(t *testing.T) {
	c := caddy.NewTestController("dns", `blocklist {
		url https://lists.example.com/hosts
	}`)
	if err := setup(c); err != nil {
		t.Fatal(err)
	}
}
