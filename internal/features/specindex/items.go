package specindex

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/GreenOnGrey/speclint/contract"
	"github.com/go-chi/chi/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// ─── the text of a requirement or a decision (FTR.HMR.DEV-0003 R8) ───

// ItemText is the Markdown of a requirement R<n>, a decision A<n> with its
// notes, or the header of a document ("header").
type ItemText struct {
	Key      string `json:"key"`
	Item     string `json:"item"`
	Kind     string `json:"kind"` // requirement | decision | header
	Markdown string `json:"markdown"`
	URL      string `json:"url"` // the route of the navigator
}

var (
	// a requirement as the index reads it: **R1.** or **R1. Title.** — not a bold reference to another feature
	itemReqRe = regexp.MustCompile(`^\*\*(` + contract.ReqID + `)(?:\.\*\*|\.\s)`)
	itemDecRe = regexp.MustCompile(`^\|\s*(` + contract.DecisionID + `)\s*\|`)
	archNote  = regexp.MustCompile(`^(?:[-*+]\s+)?\*(` + contract.DecisionID + `) — `)
)

// ItemMarkdown cuts an item out of a document: a requirement up to the next
// requirement or heading, with its notes and criteria; a decision as the table
// header, its row and its notes under the table; the header up to the rule or
// the first section. "" when there is no such item.
func ItemMarkdown(md, item string) string {
	_, body := splitFrontMatter(md)
	ls := lines(body)
	var out []string
	switch {
	case item == "header":
		for _, l := range ls {
			t := strings.TrimSpace(l.text)
			if !l.code && (t == "---" || headingRe.MatchString(t) && !strings.HasPrefix(t, "# ")) {
				break
			}
			out = append(out, l.text)
		}
	case contract.ItemKind(item) == contract.KindRequirement:
		in := false
		for _, l := range ls {
			t := strings.TrimSpace(l.text)
			if !l.code {
				if m := itemReqRe.FindStringSubmatch(t); m != nil {
					if in {
						break
					}
					in = m[1] == item
				} else if in && headingRe.MatchString(t) {
					break
				}
			}
			if in {
				out = append(out, l.text)
			}
		}
	case contract.ItemKind(item) == contract.KindDecision:
		var head []string
		for i, l := range ls {
			t := strings.TrimSpace(l.text)
			if l.code {
				continue
			}
			if strings.HasPrefix(t, "|") && (i == 0 || !strings.HasPrefix(strings.TrimSpace(ls[i-1].text), "|")) {
				head = []string{l.text} // the first line of a table
				continue
			}
			if len(head) == 1 && strings.HasPrefix(t, "|") && strings.Trim(t, "|:- ") == "" {
				head = append(head, l.text)
				continue
			}
			if m := itemDecRe.FindStringSubmatch(t); m != nil && m[1] == item && len(out) == 0 {
				out = append(append(out, head...), l.text)
				continue
			}
			if m := archNote.FindStringSubmatch(t); m != nil && m[1] == item && len(out) > 0 {
				if !strings.HasPrefix(strings.TrimSpace(out[len(out)-1]), "-") && !strings.HasPrefix(strings.TrimSpace(out[len(out)-1]), "*") {
					out = append(out, "")
				}
				out = append(out, l.text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// Item reads an item of a feature of the index (R8).
func (s *Service) Item(ctx context.Context, key, item string) (*ItemText, error) {
	kind, area := "header", "product"
	switch contract.ItemKind(item) {
	case contract.KindRequirement:
		kind = contract.KindRequirement
	case contract.KindDecision:
		kind, area = contract.KindDecision, "arch"
	default:
		if item != "header" {
			return nil, apperr.BadRequest("invalid_item", "item is R<n>, A<n> or header")
		}
	}
	var md string
	err := s.pool.QueryRow(ctx, `SELECT markdown FROM spec_documents WHERE feature_key = $1 AND area = $2`, key, area).Scan(&md)
	if postgres.IsNoRows(err) {
		return nil, apperr.NotFound("spec_item_not_found", "no such item in the specification index")
	}
	if err != nil {
		return nil, err
	}
	text := ItemMarkdown(md, item)
	if text == "" {
		return nil, apperr.NotFound("spec_item_not_found", "no such item in the specification index")
	}
	u := "/spec/" + url.PathEscape(key) + "/" + area
	if kind != "header" {
		u += "#" + strings.ToLower(item)
	}
	return &ItemText{Key: key, Item: item, Kind: kind, Markdown: text, URL: u}, nil
}

// itemRoute is GET /spec/items/{key}/{item}: any signed-in user, cached by the version of the index.
func (s *Service) itemRoute(r chi.Router) {
	r.Get("/items/{key}/{item}", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		item := chi.URLParam(r, "item")
		if item != "header" && item != "" {
			item = strings.ToUpper(item[:1]) + item[1:] // r1 of an anchor is R1
		}
		if item != "header" && contract.ItemKind(item) == "" {
			return apperr.BadRequest("invalid_item", "item is R<n>, A<n> or header")
		}
		tag, err := s.IndexTag(r.Context())
		if err != nil {
			return err
		}
		etag := `"` + tag + `"`
		w.Header().Set("Cache-Control", "private, no-cache")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return nil
		}
		it, err := s.Item(r.Context(), strings.ToUpper(chi.URLParam(r, "key")), item)
		if err != nil {
			return err
		}
		w.Header().Set("ETag", etag)
		httpx.JSON(w, http.StatusOK, it)
		return nil
	}))
}
