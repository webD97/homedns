package blocklist

import (
	"sort"
	"strings"
)

// matcher is an immutable snapshot of the loaded lists. Refreshes build a new
// one and swap it in, so lookups never take a lock.
type matcher struct {
	block nameSet
	allow nameSet
}

// blocked reports whether qname should be answered NXDOMAIN. qname must be
// normalised by normalizeQuery first.
func (m *matcher) blocked(qname string) bool {
	if m == nil || qname == "" {
		return false
	}
	if m.allow.covers(qname) {
		return false
	}
	return m.block.covers(qname)
}

func (m *matcher) size() int {
	if m == nil {
		return 0
	}
	return len(m.block)
}

func normalizeQuery(qname string) string {
	return strings.TrimSuffix(strings.ToLower(qname), ".")
}

// nameSet is a sorted, deduplicated set of domain subtrees.
type nameSet []string

// newNameSet sorts, deduplicates, and drops names already covered by an
// ancestor in the set.
func newNameSet(names []string) nameSet {
	if len(names) == 0 {
		return nil
	}

	sorted := make([]string, len(names))
	copy(sorted, names)
	sort.Strings(sorted)

	// Tracking the previous value in a local rather than reading sorted[i-1]:
	// that slot may already have been overwritten by an earlier append.
	deduped := nameSet(sorted[:0])
	last := ""
	for i, n := range sorted {
		if i == 0 || n != last {
			deduped = append(deduped, n)
			last = n
		}
	}

	// Written into a fresh slice; pruning in place would corrupt the set
	// hasAncestor is searching.
	out := make([]string, 0, len(deduped))
	for _, n := range deduped {
		if !deduped.hasAncestor(n) {
			out = append(out, n)
		}
	}
	return out
}

// covers reports whether name, or any of its ancestors, is in the set.
func (s nameSet) covers(name string) bool {
	for name != "" {
		if s.has(name) {
			return true
		}
		i := strings.IndexByte(name, '.')
		if i < 0 {
			return false
		}
		name = name[i+1:]
	}
	return false
}

func (s nameSet) hasAncestor(name string) bool {
	i := strings.IndexByte(name, '.')
	if i < 0 {
		return false
	}
	return s.covers(name[i+1:])
}

func (s nameSet) has(name string) bool {
	i := sort.SearchStrings(s, name)
	return i < len(s) && s[i] == name
}
