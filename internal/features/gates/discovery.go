package gates

import (
	"context"
	"errors"
	"net/http"
	"net/url"
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

// ─── materials of the research: discovery/ of the feature (FTR.HMR.DISC-0001 R30) ───

// DiscoveryDocument is the Discovery document of a feature.
type DiscoveryDocument struct {
	Path     string `json:"path"`
	Markdown string `json:"markdown"`
	BlobSHA  string `json:"blobSha"`
}

// DiscoveryFolder is GET /features/{uniqueId}/discovery: the document and the
// materials beside it, read at the ref the documents of the feature are read at.
type DiscoveryFolder struct {
	Ref      string             `json:"ref"`
	Document *DiscoveryDocument `json:"document"`
	Files    []GateFile         `json:"files"`
}

// discoveryRef loads the feature, the token of the user and the ref: discovery
// is not a gate, so no gate is loaded (FTR.HMR.DISC-0001 tech decision 45).
func (s *Service) discoveryRef(ctx context.Context, p *domain.Principal, uniqueID string) (*specdata.Feature, string, string, error) {
	f, err := features.Load(ctx, s.store, uniqueID)
	if err != nil {
		return nil, "", "", err
	}
	ref := s.readRef(f)
	if ref == "" {
		return nil, "", "", apperr.NotFound("feature_branch_not_found", "the branch of the feature is not found")
	}
	token, err := s.tokens.Token(ctx, p.UserID)
	if err != nil {
		return nil, "", "", err
	}
	return f, token, ref, nil
}

// Discovery reads the folder discovery/ of a feature: discovery.md, otherwise
// the legacy discovery.md in the root of the folder, and the other files of
// discovery/ without nested folders, by name (tech §3.6).
func (s *Service) Discovery(ctx context.Context, p *domain.Principal, uniqueID string) (*DiscoveryFolder, error) {
	f, token, ref, err := s.discoveryRef(ctx, p, uniqueID)
	if err != nil {
		return nil, err
	}
	dir := git.DiscoveryDir(f.DomainKey, f.SystemKey, f.UniqueID)
	entries, err := s.git.Tree(ctx, token, ref, dir)
	if err == nil && len(entries) == 0 {
		_, err = s.git.BranchHead(ctx, token, ref) // an empty tree: no folder, or GitLab on a missing branch
	}
	if errors.Is(err, git.ErrNotFound) {
		return nil, apperr.NotFound("feature_branch_not_found", "the branch of the feature is not found")
	}
	if err != nil {
		return nil, auth.MapGitError(err)
	}
	out := &DiscoveryFolder{Ref: ref, Files: []GateFile{}}
	docPath := git.LegacyDiscoveryPath(f.DomainKey, f.SystemKey, f.UniqueID)
	for _, e := range entries {
		name := strings.TrimPrefix(e.Path, dir+"/")
		if strings.Contains(name, "/") {
			continue // files of nested folders
		}
		if name == git.DiscoveryDoc {
			docPath = e.Path
			continue
		}
		// GitLab gives no size in a tree: the size stays unknown (-1) rather than every material — a deck, a video —
		// being downloaded to measure it; the limit of viewing is checked when the file is served
		out.Files = append(out.Files, s.discoveryFile(f, name, e.SHA, e.Size))
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Name < out.Files[j].Name })
	doc, err := s.git.GetFile(ctx, token, ref, docPath)
	switch {
	case errors.Is(err, git.ErrNotFound):
	case err != nil:
		return nil, auth.MapGitError(err)
	default:
		out.Document = &DiscoveryDocument{Path: docPath, Markdown: string(doc.Content), BlobSHA: doc.BlobSHA}
	}
	return out, nil
}

// DiscoveryFile reads a file of discovery/ by its name; discovery.md falls back
// to the legacy path. A name has no path, so nothing outside the folder is read.
func (s *Service) DiscoveryFile(ctx context.Context, p *domain.Principal, uniqueID, name string) (*GateFile, []byte, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return nil, nil, apperr.BadRequest("invalid_name", "name is a file of the folder discovery")
	}
	f, token, ref, err := s.discoveryRef(ctx, p, uniqueID)
	if err != nil {
		return nil, nil, err
	}
	file, err := s.git.GetFile(ctx, token, ref, git.DiscoveryDir(f.DomainKey, f.SystemKey, f.UniqueID)+"/"+name)
	if errors.Is(err, git.ErrNotFound) && name == git.DiscoveryDoc {
		file, err = s.git.GetFile(ctx, token, ref, git.LegacyDiscoveryPath(f.DomainKey, f.SystemKey, f.UniqueID))
	}
	if errors.Is(err, git.ErrNotFound) {
		return nil, nil, apperr.NotFound("spec_file_not_found", "no such file in the folder discovery")
	}
	if err != nil {
		return nil, nil, auth.MapGitError(err)
	}
	gf := s.discoveryFile(f, name, file.BlobSHA, int64(len(file.Content)))
	return &gf, file.Content, nil
}

func (s *Service) discoveryFile(f *specdata.Feature, name, sha string, size int64) GateFile {
	return s.folderFile("/api/v1/features/"+url.PathEscape(f.UniqueID)+"/discovery/files/raw", name, sha, size)
}

// discovery is GET /features/{uniqueId}/discovery.
func (h *Handlers) discovery(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return err
	}
	out, err := h.svc.Discovery(r.Context(), p, chi.URLParam(r, "uniqueId"))
	if err != nil {
		return err
	}
	httpx.JSON(w, http.StatusOK, out)
	return nil
}

// discoveryRaw is GET /features/{uniqueId}/discovery/files/raw?name=&mode=view|download.
func (h *Handlers) discoveryRaw(w http.ResponseWriter, r *http.Request) error {
	p, err := httpx.MustPrincipal(r)
	if err != nil {
		return err
	}
	gf, data, err := h.svc.DiscoveryFile(r.Context(), p, chi.URLParam(r, "uniqueId"), r.URL.Query().Get("name"))
	if err != nil {
		return err
	}
	return specindex.ServeFile(w, gf.Name, gf.MimeType, data, r.URL.Query().Get("mode"), h.svc.PreviewMaxBytes, r.URL.Query().Get("v") == gf.BlobSHA)
}
