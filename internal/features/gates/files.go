package gates

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/auth"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/features"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/specindex"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// ─── files of a gate in the branch of the feature (FTR.HMR.DEV-0003 R1–R5) ───

// GateFile is a file of the folder of a gate beside its spec.md.
type GateFile struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	MimeType    string `json:"mimeType"`
	BlobSHA     string `json:"blobSha"`
	Previewable bool   `json:"previewable"`
	// ViewURL opens the file as a page of its own in an opaque origin, with the
	// version of the blob for the cache; a path of the API, without the host.
	ViewURL string `json:"viewUrl,omitempty"`
}

// GateFiles are the files of a gate and the head of the branch they were read at.
type GateFiles struct {
	Ref   string     `json:"ref"`
	Files []GateFile `json:"files"`
}

func (s *Service) gateFiles(ctx context.Context, p *domain.Principal, uniqueID string, area domain.Area) (*GateFiles, error) {
	f, token, ref, err := s.gateRef(ctx, p, uniqueID, area)
	if err != nil {
		return nil, err
	}
	dir := path.Dir(specPath(f, area))
	entries, err := s.git.Tree(ctx, token, ref, dir)
	if err == nil && len(entries) == 0 {
		_, err = s.git.BranchHead(ctx, token, ref) // GitLab answers an empty tree for a missing branch
	}
	if errors.Is(err, git.ErrNotFound) {
		return nil, apperr.NotFound("feature_branch_not_found", "the branch of the feature is not found")
	}
	if err != nil {
		return nil, auth.MapGitError(err)
	}
	out := &GateFiles{Ref: ref, Files: []GateFile{}}
	for _, e := range entries {
		name := strings.TrimPrefix(e.Path, dir+"/")
		if name == "spec.md" || strings.Contains(name, "/") {
			continue // the document itself, and files of nested folders
		}
		size := e.Size
		if size < 0 { // GitLab gives no size in a tree: the folder of a gate has a few files
			file, err := s.git.GetFile(ctx, token, ref, e.Path)
			if err != nil {
				return nil, auth.MapGitError(err)
			}
			size = int64(len(file.Content))
		}
		out.Files = append(out.Files, s.gateFile(f, area, name, e.SHA, size))
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Name < out.Files[j].Name })
	return out, nil
}

// gateRef loads the feature with its active gate of the area, the token of the
// user and the ref the documents of the feature are read at.
func (s *Service) gateRef(ctx context.Context, p *domain.Principal, uniqueID string, area domain.Area) (*specdata.Feature, string, string, error) {
	f, _, err := s.loadGate(ctx, uniqueID, area)
	if err != nil {
		return nil, "", "", err
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, "", "", err
	}
	return f, token, s.readRef(f), nil
}

func (s *Service) gateFile(f *specdata.Feature, area domain.Area, name, sha string, size int64) GateFile {
	return s.folderFile("/api/v1/features/"+url.PathEscape(f.UniqueID)+"/gates/"+url.PathEscape(string(area))+"/files/raw", name, sha, size)
}

// folderFile is a file of a folder of the feature with the address of its raw
// endpoint; a size below zero is unknown (FTR.HMR.DISC-0001 tech §3.6) and the
// limit is checked when the file is served.
func (s *Service) folderFile(raw, name, sha string, size int64) GateFile {
	mt := specindex.MimeOf(name)
	gf := GateFile{Name: name, Size: size, MimeType: mt, BlobSHA: sha, Previewable: specindex.Previewable(mt) && size <= s.PreviewMaxBytes}
	if gf.Previewable && !specindex.IsContract(mt) { // a contract is read as text by the viewer of the API
		gf.ViewURL = raw + "?" + url.Values{"name": {name}, "mode": {"view"}, "v": {sha}}.Encode()
	}
	return gf
}

// Files lists the files of the folder of a gate in the branch of the feature
// (R1), or in the default branch after the release, as the document.
func (s *Service) Files(ctx context.Context, p *domain.Principal, uniqueID string, area domain.Area) (*GateFiles, error) {
	return s.gateFiles(ctx, p, uniqueID, area)
}

// File reads a file of the folder of a gate by its name: a name has no path, so
// a file outside the folder is never read (R5).
func (s *Service) File(ctx context.Context, p *domain.Principal, uniqueID string, area domain.Area, name string) (*GateFile, []byte, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." || name == "spec.md" {
		return nil, nil, apperr.BadRequest("invalid_name", "name is a file of the folder of the gate")
	}
	f, token, ref, err := s.gateRef(ctx, p, uniqueID, area)
	if err != nil {
		return nil, nil, err
	}
	file, err := s.git.GetFile(ctx, token, ref, path.Dir(specPath(f, area))+"/"+name)
	if errors.Is(err, git.ErrNotFound) {
		return nil, nil, apperr.NotFound("spec_file_not_found", "no such file in the folder of the gate")
	}
	if err != nil {
		return nil, nil, auth.MapGitError(err)
	}
	gf := s.gateFile(f, area, name, file.BlobSHA, int64(len(file.Content)))
	return &gf, file.Content, nil
}

// files is GET /features/{uniqueId}/gates/{area}/files.
func (h *Handlers) files(w http.ResponseWriter, r *http.Request) error {
	p, uid, area, err := params(r)
	if err != nil {
		return err
	}
	out, err := h.svc.Files(r.Context(), p, uid, area)
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

// fileRaw is GET /features/{uniqueId}/gates/{area}/files/raw?name=&mode=view|download (R5).
func (h *Handlers) fileRaw(w http.ResponseWriter, r *http.Request) error {
	p, uid, area, err := params(r)
	if err != nil {
		return err
	}
	gf, data, err := h.svc.File(r.Context(), p, uid, area, r.URL.Query().Get("name"))
	if err != nil {
		return err
	}
	// cached only with the version of the blob in the address: a branch changes under the same name
	return specindex.ServeFile(w, gf.Name, gf.MimeType, data, r.URL.Query().Get("mode"), h.svc.PreviewMaxBytes, r.URL.Query().Get("v") == gf.BlobSHA)
}

// ─── links of a feature in work (FTR.HMR.DEV-0003 R6, R7, R9) ───

// DraftLinkRef is a feature a document of the feature in work names.
type DraftLinkRef struct {
	Key     string   `json:"key"`
	Title   *string  `json:"title"`   // null outside the index
	Indexed bool     `json:"indexed"` // the feature has documents in the default branch (R17 of FTR.HMR.DEV-0002)
	Items   []string `json:"items"`
}

// DraftLayer are the outgoing links of one document of the feature in work.
type DraftLayer struct {
	Layer   string         `json:"layer"` // product | arch
	State   string         `json:"state"` // draft | approved | skipped
	Extends []DraftLinkRef `json:"extends"`
	Changes []DraftLinkRef `json:"changes"`
}

// DraftLinks reads the links of a feature from the product and arch documents
// of its branch (R6): not stored in the index, which keeps only the default
// branch. A layer is there once its document is; a skipped gate has none.
func (s *Service) DraftLinks(ctx context.Context, p *domain.Principal, uniqueID string) ([]DraftLayer, error) {
	f, err := features.Load(ctx, s.store, uniqueID)
	if err != nil {
		return nil, err
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	gates, err := s.store.ActiveGates(ctx, f.ID)
	if err != nil {
		return nil, err
	}
	byArea := map[domain.Area]specdata.Gate{}
	for _, g := range gates {
		byArea[g.Area] = g
	}
	out := []DraftLayer{}
	for _, area := range []domain.Area{domain.AreaProduct, domain.AreaArch} {
		layer := DraftLayer{Layer: string(area), State: "draft", Extends: []DraftLinkRef{}, Changes: []DraftLinkRef{}}
		if containsArea(f.SkippedAreas, string(area)) {
			layer.State = "skipped"
			out = append(out, layer)
			continue
		}
		file, err := s.git.GetFile(ctx, token, s.readRef(f), specPath(f, area))
		if errors.Is(err, git.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, auth.MapGitError(err)
		}
		if g, ok := byArea[area]; ok && g.Status == domain.GateApproved {
			layer.State = "approved"
		}
		md := string(file.Content)
		parent, _ := specindex.Parent(md)
		for _, l := range specindex.Links(md, string(area), f.UniqueID, parent) {
			ref := DraftLinkRef{Key: l.Target, Items: l.Requirements}
			if l.Kind == specindex.LinkExtends {
				layer.Extends = append(layer.Extends, ref)
			} else {
				layer.Changes = append(layer.Changes, ref)
			}
		}
		if err := s.titles(ctx, layer.Extends); err != nil {
			return nil, err
		}
		if err := s.titles(ctx, layer.Changes); err != nil {
			return nil, err
		}
		out = append(out, layer)
	}
	return out, nil
}

// titles fills the title and the presence in the index of linked features in one query.
func (s *Service) titles(ctx context.Context, refs []DraftLinkRef) error {
	if len(refs) == 0 {
		return nil
	}
	keys := make([]string, len(refs))
	for i, r := range refs {
		keys[i] = r.Key
	}
	rows, err := s.store.Q().Query(ctx, `SELECT k, EXISTS (SELECT 1 FROM spec_documents WHERE feature_key = k),
		(SELECT title FROM features WHERE unique_id = k) FROM unnest($1::text[]) AS k`, keys)
	if err != nil {
		return err
	}
	defer rows.Close()
	type info struct {
		indexed bool
		title   *string
	}
	got := map[string]info{}
	for rows.Next() {
		var k string
		var i info
		if err := rows.Scan(&k, &i.indexed, &i.title); err != nil {
			return err
		}
		got[k] = i
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range refs {
		in := got[refs[i].Key]
		refs[i].Indexed = in.indexed
		if in.indexed {
			refs[i].Title = in.title
		}
	}
	return nil
}

func containsArea(xs []string, a string) bool {
	for _, x := range xs {
		if x == a {
			return true
		}
	}
	return false
}

// links is GET /features/{uniqueId}/spec/links.
func (h *Handlers) links(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return err
	}
	out, err := h.svc.DraftLinks(r.Context(), p, chi.URLParam(r, "uniqueId"))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"layers": out})
	return nil
}
