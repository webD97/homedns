package blocklist

import (
	"sort"
	"strings"
)

// matcher is an immutable snapshot of the loaded lists. Refreshes build a new
// one and publish it atomically, so lookups never take a lock.
type matcher struct {
	block nameSet
	allow nameSet

	// perSource is the post-normalisation entry count each source contributed,
	// before cross-source dedup. Reported as a metric so a list that silently
	// starts returning an error page is visible.
	perSource map[string]int
}

// blocked reports whether qname should be answered NXDOMAIN. qname must already
// be normalised by normalizeQuery.
func (m *matcher) blocked(qname string) bool {
	if m == nil || qname == "" {
		return false
	}
	// allow always wins, so a single entry can carve a name back out from under
	// a blocked parent.
	if m.allow.covers(qname) {
		return false
	}
	return m.block.covers(qname)
}

// size is the number of blocked subtrees, i.e. what the domain-count metric
// reports.
func (m *matcher) size() int {
	if m == nil {
		return 0
	}
	return len(m.block)
}

// normalizeQuery converts a wire-format qname into the form nameSet stores.
func normalizeQuery(qname string) string {
	return strings.TrimSuffix(strings.ToLower(qname), ".")
}

// nameSet is a sorted, deduplicated set of domain subtrees.
//
// A sorted slice rather than a map: the combined StevenBlack + oisd lists are
// ~287k entries, which costs roughly 10 MB here against ~30 MB as a
// map[string]struct{}, and it is cheaper to build and to swap wholesale.
// Lookups are a handful of binary searches, since real lists top out at nine
// labels deep and average three.
type nameSet []string

// newNameSet sorts, deduplicates, and prunes names covered by an ancestor that
// is itself in the set.
//
// The pruning is not just a size win: because every entry denotes a whole
// subtree, "ads.example.com" is already answered by "example.com" being listed.
// Dropping it keeps every lookup path shorter. On the real lists this removes
// 12% of the combined set and 45% of StevenBlack on its own.
func newNameSet(names []string) nameSet {
	if len(names) == 0 {
		return nil
	}

	sorted := make([]string, len(names))
	copy(sorted, names)
	sort.Strings(sorted)

	// Compacted in place. Tracking the previous value in a local rather than
	// reading sorted[i-1] back matters: that slot may already have been
	// overwritten by an earlier append.
	deduped := nameSet(sorted[:0])
	last := ""
	for i, n := range sorted {
		if i == 0 || n != last {
			deduped = append(deduped, n)
			last = n
		}
	}

	// Read from deduped, write into a fresh slice — pruning in place would
	// corrupt the very set hasAncestor is searching.
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

// hasAncestor is covers minus the name itself.
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
