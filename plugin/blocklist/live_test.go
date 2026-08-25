package blocklist

import (
	"context"
	"io"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Each list declares its own entry count in a header comment, which is the
// oracle this test checks the parser against.
var (
	stevenBlackCount = regexp.MustCompile(`(?m)^#\s*Number of unique domains:\s*([\d,]+)`)
	oisdCount        = regexp.MustCompile(`(?m)^#\s*Entries:\s*([\d,]+)`)
)

// TestLiveLists checks the parser against the live lists. Opt-in because it
// needs the network; a scheduled workflow runs it.
func TestLiveLists(t *testing.T) {
	if os.Getenv("HOMEDNS_LIVE_LISTS") == "" {
		t.Skip("set HOMEDNS_LIVE_LISTS=1 to check the parser against the live blocklists")
	}

	for _, tc := range []struct {
		name     string
		url      string
		declared *regexp.Regexp
	}{
		{
			name:     "stevenblack",
			url:      "https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts",
			declared: stevenBlackCount,
		},
		{
			name:     "oisd-big",
			url:      "https://big.oisd.nl/domainswild",
			declared: oisdCount,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := download(t, tc.url)

			want := declaredCount(t, tc.declared, body)
			names, err := parseList(strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}

			unique := make(map[string]struct{}, len(names))
			for _, n := range names {
				unique[n] = struct{}{}
			}

			if len(unique) != want {
				t.Errorf("parsed %d unique domains but %s advertises %d — the list format changed",
					len(unique), tc.url, want)
			}

			// A matching count alone is not enough: unmatchable entries still
			// count.
			for n := range unique {
				if strings.ContainsAny(n, "*/|^ \t") || strings.HasPrefix(n, ".") || strings.HasSuffix(n, ".") {
					t.Fatalf("entry %q is not a usable query name", n)
				}
			}

			set := newNameSet(names)
			probe := "doubleclick.net"
			if !set.covers(probe) && !set.covers("ads."+probe) {
				t.Logf("note: %s does not cover %s", tc.name, probe)
			}
			t.Logf("%s: %d unique, %d after subtree pruning", tc.name, len(unique), len(set))
		})
	}
}

func download(t *testing.T, url string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("cannot reach %s: %v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: unexpected status %s", url, resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func declaredCount(t *testing.T, re *regexp.Regexp, body string) int {
	t.Helper()
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no declared entry count in the list header; the format changed")
	}
	n, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
	if err != nil {
		t.Fatal(err)
	}
	return n
}
