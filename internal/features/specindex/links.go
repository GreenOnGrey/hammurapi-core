package specindex

import (
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/GreenOnGrey/speclint/v2/contract"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/markdown"
)

// Kinds of links between features (FTR.HMR.DEV-0002 R13).
const (
	LinkExtends = "extends" // «Опирается на»
	LinkChanges = "changes" // the section «Изменения в других решениях», and the parent of a fix
)

// Layers of links (FTR.HMR.DEV-0003 R15): the document a link comes from.
const (
	LayerProduct = "product"
	LayerArch    = "arch"
)

// Link is a link of a feature to another one, from its product or arch document.
type Link struct {
	Target       string
	Kind         string
	Layer        string
	Requirements []string // R<n> of product, A<n> of arch
}

var featureKeyRe = regexp.MustCompile(markdown.FeatureKey)

// Links extracts the links of a feature from a document of the area «product»
// or «arch» by the document contract (FTR.HMR.DEV-0002 arch §3.6,
// FTR.HMR.DEV-0003 R11, R12): "extends" from the header line «Опирается на»
// with the items named there, "changes" from the rows of the section
// «Изменения в других решениях» with their items merged per target, and for
// product "changes" to the parent of a fix when the section does not name it.
// Links to the feature itself are dropped; keys outside the index are kept
// (R17). Sorted by kind and target.
func Links(md, area, self, parent string) []Link {
	layer := LayerProduct
	if area == string(contract.Arch) {
		layer = LayerArch
	}
	d := contract.Parse(md, contract.Area(area))
	var out []Link
	at := map[string]int{}
	add := func(target, kind string, items []string) {
		if target == self {
			return
		}
		i, ok := at[kind+" "+target]
		if !ok {
			i = len(out)
			at[kind+" "+target] = i
			out = append(out, Link{Target: target, Kind: kind, Layer: layer, Requirements: []string{}})
		}
		for _, it := range items {
			if it != "" && !slices.Contains(out[i].Requirements, it) {
				out[i].Requirements = append(out[i].Requirements, it)
			}
		}
	}
	for _, r := range d.DependsOn {
		add(r.Key, LinkExtends, r.Items)
	}
	for _, c := range d.Changes {
		add(c.Key, LinkChanges, []string{c.Item})
	}
	if layer == LayerProduct && parent != "" && featureKeyRe.FindString(parent) == parent { // an old-form parent is not a key
		add(parent, LinkChanges, nil)
	}
	for _, l := range out {
		contract.SortItems(l.Requirements)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind == LinkExtends
		}
		return out[i].Target < out[j].Target
	})
	return out
}

// Decisions are the decisions A<n> of an arch document (FTR.HMR.DEV-0003 R10)
// as requirements of the index: the text of the row, no criteria.
func Decisions(md string) []Requirement {
	d := contract.Parse(md, contract.Arch)
	_, body := splitFrontMatter(md)
	heads := toc(lines(body))
	off := strings.Count(strings.ReplaceAll(md, "\r\n", "\n"), "\n") - strings.Count(body, "\n") // lines of the front matter
	out := make([]Requirement, 0, len(d.Items))
	for _, it := range d.Items {
		sec := ""
		for _, h := range heads {
			if h.line < it.Line-1-off { // Line counts from 1 over the whole document, h.line from 0 over the body
				sec = h.Text
			}
		}
		out = append(out, Requirement{ID: it.ID, Text: plain(it.Text), Criteria: []string{}, Section: sec})
	}
	return out
}
