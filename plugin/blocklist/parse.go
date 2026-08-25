package blocklist

import (
	"bufio"
	"io"
	"net/netip"
	"strings"
)

// localNames are the pseudo-hosts that /etc/hosts-format blocklists map to
// loopback and multicast addresses in their preamble. StevenBlack/hosts ships
// all of these; taking field 1 of every IP-led line without filtering them
// would block localhost.
var localNames = map[string]struct{}{
	"localhost":             {},
	"localhost.localdomain": {},
	"local":                 {},
	"broadcasthost":         {},
	"ip6-localhost":         {},
	"ip6-loopback":          {},
	"ip6-localnet":          {},
	"ip6-mcastprefix":       {},
	"ip6-allnodes":          {},
	"ip6-allrouters":        {},
	"ip6-allhosts":          {},
}

// parseList reads a blocklist and returns the domains it names.
//
// Both formats in the wild are handled without the caller declaring which is
// which, because they are unambiguous per line:
//
//	0.0.0.0 ads.example.com # comment    -> /etc/hosts format
//	*.ads.example.com                    -> bare domain list (oisd wildcard)
//	ads.example.com                      -> bare domain list
//
// Every returned name is normalised to lowercase with no trailing dot, no
// leading "*." and no leading ".". Names that are not blockable domains are
// dropped: bare IPs, single-label names, and the localNames set above.
//
// Callers get names only — the mapped IP in hosts format is discarded, since we
// always answer NXDOMAIN rather than redirecting to a sinkhole address.
func parseList(r io.Reader) ([]string, error) {
	var out []string

	sc := bufio.NewScanner(r)
	// Blocklist lines are short, but a malformed file shouldn't kill the parse
	// with "token too long" — 1 MiB is far beyond any legitimate line.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		fields := strings.Fields(line)
		var candidates []string
		switch {
		case len(fields) >= 2 && isIP(fields[0]):
			// hosts format: "IP name [name...]".
			candidates = fields[1:]
		case len(fields) == 1:
			candidates = fields
		default:
			// Neither shape: a hosts line with an unparseable first field, or
			// some other syntax (Adblock "||domain^", dnsmasq, ...). Skipping
			// beats guessing.
			continue
		}

		for _, c := range candidates {
			if name, ok := normalizeEntry(c); ok {
				out = append(out, name)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// normalizeEntry canonicalises one raw list entry and reports whether it is a
// blockable domain.
func normalizeEntry(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ".")

	// oisd's "domainswild" prefixes every one of its ~269k entries with "*.".
	// Leaving it on loads entries that can never match a query name, which
	// looks healthy in the domain count while blocking nothing at all.
	s = strings.TrimPrefix(s, "*.")
	s = strings.TrimPrefix(s, ".")

	if s == "" {
		return "", false
	}
	// Guards against hosts lines like "0.0.0.0 0.0.0.0" and against a
	// single-label name such as "local" or "broadcasthost" reaching the set.
	if isIP(s) || !strings.Contains(s, ".") {
		return "", false
	}
	if _, isLocal := localNames[s]; isLocal {
		return "", false
	}
	if !isDomainName(s) {
		return "", false
	}
	return s, true
}

// isDomainName reports whether s is something that can appear as a query name.
//
// This is what keeps other list syntaxes from being mistaken for bare domains.
// An Adblock rule such as "||ads.example.com^" is a single dot-containing field
// and would otherwise be stored verbatim — an entry that can never match any
// query, so the domain count looks healthy while nothing is blocked. Same class
// of bug as leaving oisd's "*." prefix on.
//
// Non-ASCII is rejected rather than converted: query names arrive punycoded, and
// every list we support already publishes punycode ("xn--...").
func isDomainName(s string) bool {
	if len(s) > 253 {
		return false
	}
	labelLen := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '.':
			if labelLen == 0 {
				return false // empty label: "a..b"
			}
			labelLen = 0
			continue
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
			// Underscore is not strictly legal in a hostname but appears in
			// real entries (and in _dmarc-style names), so it is allowed.
		default:
			return false
		}
		if labelLen++; labelLen > 63 {
			return false
		}
	}
	return labelLen > 0 // rejects a trailing empty label
}

// isIP reports whether s is an IP literal. The zone suffix on link-local
// addresses ("fe80::1%lo0", which appears in StevenBlack's preamble) is
// stripped first, since netip rejects it in ParseAddr.
func isIP(s string) bool {
	if i := strings.IndexByte(s, '%'); i >= 0 {
		s = s[:i]
	}
	_, err := netip.ParseAddr(s)
	return err == nil
}
