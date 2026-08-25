package blocklist

import (
	"bufio"
	"io"
	"net/netip"
	"strings"
)

// localNames are the pseudo-hosts an /etc/hosts-format list maps in its
// preamble. Without this, field 1 of every IP-led line blocks localhost.
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

// parseList reads a blocklist and returns the domains it names, handling both
// /etc/hosts and bare-domain formats. See AGENTS.md for the parsing rules and
// why each rejection exists.
func parseList(r io.Reader) ([]string, error) {
	var out []string

	sc := bufio.NewScanner(r)
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
			candidates = fields[1:]
		case len(fields) == 1:
			candidates = fields
		default:
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

// normalizeEntry canonicalises one raw entry and reports whether it is a
// blockable domain.
func normalizeEntry(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.TrimSuffix(s, ".")
	// oisd prefixes every entry with "*."; leaving it on loads entries that can
	// never match a query name.
	s = strings.TrimPrefix(s, "*.")
	s = strings.TrimPrefix(s, ".")

	if s == "" || isIP(s) || !strings.Contains(s, ".") || !isDomainName(s) {
		return "", false
	}
	if _, isLocal := localNames[s]; isLocal {
		return "", false
	}
	return s, true
}

// isDomainName reports whether s can appear as a query name. This is what stops
// other list syntaxes (Adblock's "||domain^", for one) from being stored as
// domains that never match.
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
				return false
			}
			labelLen = 0
			continue
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
		if labelLen++; labelLen > 63 {
			return false
		}
	}
	return labelLen > 0
}

// isIP reports whether s is an IP literal, tolerating the zone suffix on
// link-local addresses ("fe80::1%lo0") that netip itself rejects.
func isIP(s string) bool {
	if i := strings.IndexByte(s, '%'); i >= 0 {
		s = s[:i]
	}
	_, err := netip.ParseAddr(s)
	return err == nil
}
