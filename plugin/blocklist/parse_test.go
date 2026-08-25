package blocklist

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestParseListHostsFormat(t *testing.T) {
	in := `
# a comment
0.0.0.0 ads.example.com
0.0.0.0 tracker.example.net # inline comment
0.0.0.0	tabbed.example.org
127.0.0.1 legacy-sinkhole.example.com
0.0.0.0 multi-a.example.com multi-b.example.com
`
	got := mustParse(t, in)
	want := []string{
		"ads.example.com",
		"tracker.example.net",
		"tabbed.example.org",
		"legacy-sinkhole.example.com",
		"multi-a.example.com",
		"multi-b.example.com",
	}
	assertSameSet(t, got, want)
}

func TestParseListDomainFormat(t *testing.T) {
	in := `
# oisd-style header
*.ads.example.com
plain.example.net
.leading-dot.example.org
TRAILING.Dot.Example.com.
`
	got := mustParse(t, in)
	want := []string{
		"ads.example.com",
		"plain.example.net",
		"leading-dot.example.org",
		"trailing.dot.example.com",
	}
	assertSameSet(t, got, want)
}

// Everything here would be wrong to block. The /etc/hosts preamble of a real
// list maps all of them, so a parser that just takes field 1 of every IP-led
// line blocks localhost.
func TestParseListRejectsNonDomains(t *testing.T) {
	in := `
127.0.0.1 localhost
127.0.0.1 localhost.localdomain
127.0.0.1 local
255.255.255.255 broadcasthost
::1 localhost
::1 ip6-localhost
::1 ip6-loopback
fe80::1%lo0 localhost
ff00::0 ip6-localnet
ff00::0 ip6-mcastprefix
ff02::1 ip6-allnodes
ff02::2 ip6-allrouters
ff02::3 ip6-allhosts
0.0.0.0 0.0.0.0
0.0.0.0 ::1
singlelabel
# comment only

||adblock-syntax.example.com^
`
	if got := mustParse(t, in); len(got) != 0 {
		t.Errorf("expected nothing blockable, got %v", got)
	}
}

// A domain whose leading labels look like an IP must survive: only field 0 of a
// hosts line is the address. Both of these are real StevenBlack entries.
func TestParseListKeepsIPLookingNames(t *testing.T) {
	got := mustParse(t, "0.0.0.0 0.0.0.0.hpyrdr.com\n0.0.0.0 0.0.0.0.creative.hpyrdr.com\n")
	assertSameSet(t, got, []string{"0.0.0.0.hpyrdr.com", "0.0.0.0.creative.hpyrdr.com"})
}

func TestParseListHandlesCRLF(t *testing.T) {
	got := mustParse(t, "0.0.0.0 a.example.com\r\n*.b.example.com\r\n")
	assertSameSet(t, got, []string{"a.example.com", "b.example.com"})
}

// Golden counts over checked-in excerpts of the two lists the README
// recommends. These fixtures carry every shape found in the real files;
// TestLiveLists covers the full downloads.
func TestParseListFixtures(t *testing.T) {
	for _, tc := range []struct {
		file  string
		count int
		// A name that must be present, spelled as it appears after
		// normalisation rather than as it appears in the file.
		sample string
	}{
		// 16 real entries; the 12-name /etc/hosts preamble and the literal
		// "0.0.0.0 0.0.0.0" line are all rejected.
		{"testdata/stevenblack-excerpt.txt", 16, "docs.pipenv.org"},
		// Every oisd entry carries a "*." prefix. If the prefix survives
		// parsing, the count still looks right but nothing can ever match, so
		// the sample check is the part that matters here.
		{"testdata/oisd-excerpt.txt", 12, "0-02.net"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			f, err := os.Open(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()

			got, err := parseList(f)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.count {
				t.Errorf("parsed %d entries, want %d\ngot: %v", len(got), tc.count, got)
			}
			if !slices.Contains(got, tc.sample) {
				t.Errorf("expected %q among the parsed entries; got %v", tc.sample, got)
			}
			for _, n := range got {
				if strings.HasPrefix(n, "*") || strings.HasPrefix(n, ".") || strings.HasSuffix(n, ".") {
					t.Errorf("entry %q was not normalised", n)
				}
			}
		})
	}
}

func mustParse(t *testing.T, in string) []string {
	t.Helper()
	got, err := parseList(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func assertSameSet(t *testing.T, got, want []string) {
	t.Helper()
	g, w := slices.Clone(got), slices.Clone(want)
	slices.Sort(g)
	slices.Sort(w)
	if !slices.Equal(g, w) {
		t.Errorf("got  %v\nwant %v", g, w)
	}
}
