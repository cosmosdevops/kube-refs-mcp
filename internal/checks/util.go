package checks

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// The manifests are read as plain maps, not typed objects, so a field with
// the wrong type in one place does not stop the checks elsewhere. These
// helpers return the zero value for anything missing or of another type.

func getMap(m map[string]any, path ...string) map[string]any {
	for _, p := range path {
		next, ok := m[p].(map[string]any)
		if !ok {
			return nil
		}
		m = next
	}
	return m
}

func getSlice(m map[string]any, key string) []any {
	s, _ := m[key].([]any)
	return s
}

func getString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func getBool(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

// asMap returns v as a map, or nil.
func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// stringMap keeps the string values of a map, as label maps have.
func stringMap(m map[string]any) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// portValue reads a port that may be a number or a name. It returns the
// text form and whether it is a number.
func portValue(v any) (text string, number bool, ok bool) {
	switch p := v.(type) {
	case string:
		if p == "" {
			return "", false, false
		}
		return p, false, true
	case float64:
		if p != float64(int64(p)) {
			return "", false, false
		}
		return strconv.FormatInt(int64(p), 10), true, true
	case int64:
		return strconv.FormatInt(p, 10), true, true
	case int:
		return strconv.Itoa(p), true, true
	}
	return "", false, false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// labelText renders labels as "a=1,b=2", in key order.
func labelText(l map[string]string) string {
	if len(l) == 0 {
		return "(no labels)"
	}
	parts := make([]string, 0, len(l))
	for _, k := range sortedKeys(l) {
		parts = append(parts, k+"="+l[k])
	}
	return strings.Join(parts, ",")
}

// limit shortens a list for display.
func limit(items []string, max int) []string {
	if len(items) <= max {
		return items
	}
	return append(append([]string(nil), items[:max]...), fmt.Sprintf("... and %d more", len(items)-max))
}

// distance is the edit distance between two strings. Swapping two
// neighbouring letters counts as one edit, because that is the most common
// typing slip: "wbe" is one edit from "web", not two.
func distance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	var before []int
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				cur[j] = min(cur[j], before[j-2]+1)
			}
		}
		before, prev = prev, cur
	}
	return prev[len(rb)]
}

// slack is how many edits still count as a likely slip for a name of this
// length: one for a short name, more for a long one. A short name gets
// little slack on purpose. Two edits turn "web" into "db", which is a
// different name, not a typo.
func slack(want string) int {
	return max(1, len([]rune(want))/3)
}

// maxCompare bounds the names that are compared for a hint. Comparing two
// names takes time in proportion to both lengths multiplied together, and
// no valid Kubernetes name is longer than this.
const maxCompare = 320

// clip shortens text for display.
func clip(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "") + "..."
}

// nearest returns the candidate closest to want, if one is close enough to
// be a likely slip.
func nearest(want string, candidates []string) (string, bool) {
	if len(want) > maxCompare {
		return "", false
	}
	allowed := slack(want)
	best, bestDist := "", -1
	for _, c := range candidates {
		// A name whose length is further off than the slack allows cannot
		// be close enough, so it is not worth comparing.
		if len(c) > maxCompare || len(c)-len(want) > 4*allowed || len(want)-len(c) > 4*allowed {
			continue
		}
		d := distance(want, c)
		if bestDist < 0 || d < bestDist || (d == bestDist && c < best) {
			best, bestDist = c, d
		}
	}
	if bestDist < 0 || bestDist > slack(want) {
		return "", false
	}
	return best, true
}

// article returns "a" or "an" for the word that follows.
func article(word string) string {
	if word != "" && strings.ContainsRune("AEIOUaeiou", rune(word[0])) {
		return "an"
	}
	return "a"
}

func joinList(items []string) string { return strings.Join(items, ", ") }
