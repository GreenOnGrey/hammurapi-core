package specindex

import (
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/markdown"
)

// Kinds of links between features (FTR.HMR.DEV-0002 R13).
const (
	LinkExtends = "extends" // «Опирается на»
	LinkChanges = "changes" // the section «Изменения в других решениях», and the parent of a fix
)

// Link is a link of a feature to another one, from its product specification.
type Link struct {
	Target       string
	Kind         string
	Requirements []string
}

var (
	featureKeyRe = regexp.MustCompile(markdown.FeatureKey)
	// extendsRe is the header line «**Опирается на:**» (also "Builds on").
	extendsRe = regexp.MustCompile(`^\*\*(?i:Опирается на|Builds on):\*\*(.*)$`)
)

// Links extracts the links of a feature from its product specification
// (FTR.HMR.DEV-0002 arch §3.6): "extends" from the header line «Опирается на»,
// "changes" from the rows of the section «Изменения в других решениях» with
// their requirements merged per target, and "changes" to the parent of a fix
// when the section does not name it. Links to the feature itself are dropped;
// keys outside the index are kept (R17). Sorted by kind and target.
func Links(product, self, parent string) []Link {
	_, body := splitFrontMatter(product)
	var out []Link
	seen := map[string]bool{}
	add := func(l Link) {
		if l.Target == self || seen[l.Kind+" "+l.Target] {
			return
		}
		seen[l.Kind+" "+l.Target] = true
		out = append(out, l)
	}
	for _, l := range lines(body) {
		t := strings.TrimSpace(l.text)
		if l.code {
			continue
		}
		if t == "---" || headingRe.MatchString(t) && !strings.HasPrefix(t, "# ") {
			break // the header ends at the rule or the first section
		}
		if m := extendsRe.FindStringSubmatch(t); m != nil {
			for _, k := range featureKeyRe.FindAllString(m[1], -1) {
				add(Link{Target: k, Kind: LinkExtends, Requirements: []string{}})
			}
		}
	}
	changes := map[string]int{}
	for _, c := range markdown.ParseChanges(product) {
		if c.Feature == self {
			continue
		}
		i, ok := changes[c.Feature]
		if !ok {
			i = len(out)
			changes[c.Feature] = i
			add(Link{Target: c.Feature, Kind: LinkChanges, Requirements: []string{}})
		}
		if c.Req != "" && !slices.Contains(out[i].Requirements, c.Req) {
			out[i].Requirements = append(out[i].Requirements, c.Req)
		}
	}
	if featureKeyRe.FindString(parent) == parent && parent != "" { // an old-form parent is not a key
		add(Link{Target: parent, Kind: LinkChanges, Requirements: []string{}})
	}
	for _, l := range out {
		sortReqs(l.Requirements)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == LinkExtends
		}
		return out[i].Target < out[j].Target
	})
	return out
}

// sortReqs orders requirement IDs by number, then by letter: R2, R13, R26, R26a.
func sortReqs(ids []string) {
	num := func(s string) (int, string) {
		n, i := 0, 1
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			n = n*10 + int(s[i]-'0')
			i++
		}
		return n, s[min(i, len(s)):]
	}
	sort.SliceStable(ids, func(i, j int) bool {
		a, as := num(ids[i])
		b, bs := num(ids[j])
		if a != b {
			return a < b
		}
		return as < bs
	})
}
