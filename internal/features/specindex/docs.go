package specindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"path"
	"strings"

	"github.com/GreenOnGrey/speclint/v2/contract"
	"github.com/jackc/pgx/v5"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// maxVectorChars bounds the text given to to_tsvector: a tsvector is limited
// to 1 MB, and long documents are found by their beginning anyway.
const maxVectorChars = 400_000

// syncDocuments brings the index of documents and files to the tree: only
// blobs with a new SHA are read (IDX-17, NAV-12); paths of folders that are
// not indexable (unknown, deleted) leave the index.
func (s *Service) syncDocuments(ctx context.Context, token, commit string, dirs map[string]*folder) error {
	have, err := shaMap(ctx, s.pool, `SELECT path, blob_sha FROM spec_documents`)
	if err != nil {
		return err
	}
	haveFiles, err := shaMap(ctx, s.pool, `SELECT path, blob_sha FROM spec_files`)
	if err != nil {
		return err
	}
	wantDocs, wantFiles := map[string]bool{}, map[string]bool{}
	for _, f := range dirs {
		for area, e := range f.docs {
			wantDocs[e.Path] = true
			if have[e.Path] == e.SHA {
				continue
			}
			content, err := s.git.Blob(ctx, token, e.SHA)
			if err != nil {
				return fmt.Errorf("read %s: %w", e.Path, err)
			}
			loc := Loc{Domain: f.domain, System: f.system, Folder: f.folder, Area: area, Rest: "spec.md"}
			if err := postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
				return s.writeDocument(ctx, tx, loc, e.SHA, commit, string(content))
			}); err != nil {
				return err
			}
		}
		for _, fe := range f.files {
			wantFiles[fe.entry.Path] = true
			if haveFiles[fe.entry.Path] == fe.entry.SHA {
				continue
			}
			size := fe.entry.Size
			if size < 0 { // GitLab: no sizes in the tree
				b, err := s.git.Blob(ctx, token, fe.entry.SHA)
				if err != nil {
					return fmt.Errorf("read %s: %w", fe.entry.Path, err)
				}
				size = int64(len(b))
			}
			if err := writeFile(ctx, s.pool, fe.loc, fe.entry.Path, fe.entry.SHA, size); err != nil {
				return err
			}
		}
	}
	var goneDocs, goneFiles []string
	for p := range have {
		if !wantDocs[p] {
			goneDocs = append(goneDocs, p)
		}
	}
	for p := range haveFiles {
		if !wantFiles[p] {
			goneFiles = append(goneFiles, p)
		}
	}
	if len(goneDocs) > 0 {
		if _, err := s.pool.Exec(ctx, `DELETE FROM spec_documents WHERE path = ANY($1)`, goneDocs); err != nil {
			return err
		}
	}
	if len(goneFiles) > 0 {
		if _, err := s.pool.Exec(ctx, `DELETE FROM spec_files WHERE path = ANY($1)`, goneFiles); err != nil {
			return err
		}
	}
	return nil
}

func shaMap(ctx context.Context, q postgres.Querier, sql string) (map[string]string, error) {
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, sha string
		if err := rows.Scan(&p, &sha); err != nil {
			return nil, err
		}
		out[p] = sha
	}
	return out, rows.Err()
}

// writeDocument stores a spec.md with its title, contents, search vector,
// requirements and links (product only) and references — recomputed together.
func (s *Service) writeDocument(ctx context.Context, tx pgx.Tx, loc Loc, blobSHA, commit, md string) error {
	title := Title(md)
	toc := TOC(md)
	search := SearchText(md)
	if int64(len(md)) > s.cfg.MaxFileBytes {
		// Too large: found by the title and the headings only.
		parts := []string{title}
		for _, h := range toc {
			parts = append(parts, h.Text)
		}
		search = strings.Join(parts, "\n")
	}
	vec := search
	if len(vec) > maxVectorChars {
		cut := maxVectorChars
		for cut > 0 && !utf8Start(vec[cut]) { // cut before a rune, not inside it
			cut--
		}
		vec = vec[:cut]
	}
	tocJSON, _ := json.Marshal(toc)
	p := loc.Dir() + loc.Area + "/spec.md"
	if _, err := tx.Exec(ctx, `INSERT INTO spec_documents (path, feature_key, domain, system, area, blob_sha, commit_sha, title, toc,
			markdown, search_text, search_vector, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9, $10, $11,
			setweight(to_tsvector('russian', $8), 'A') || setweight(to_tsvector('russian', $12), 'B') ||
			setweight(to_tsvector('english', $12), 'C'), now())
		ON CONFLICT (path) DO UPDATE SET blob_sha = EXCLUDED.blob_sha, commit_sha = EXCLUDED.commit_sha, title = EXCLUDED.title,
			toc = EXCLUDED.toc, markdown = EXCLUDED.markdown, search_text = EXCLUDED.search_text,
			search_vector = EXCLUDED.search_vector, updated_at = now()`,
		p, loc.Folder, loc.Domain, loc.System, loc.Area, blobSHA, commit, title, tocJSON, md, search, vec); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM spec_requirements WHERE path = $1`, p); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM spec_references WHERE source_path = $1`, p); err != nil {
		return err
	}
	if loc.Area == "product" || loc.Area == "arch" {
		if err := writeItems(ctx, tx, loc.Folder, p, loc.Area, md); err != nil {
			return err
		}
		if err := writeLinks(ctx, tx, loc.Folder, p, loc.Area, md); err != nil {
			return err
		}
	}
	for _, r := range References(md, loc.Folder) {
		if _, err := tx.Exec(ctx, `INSERT INTO spec_references (source_path, source_key, area, section, target_key, target_req, snippet)
			VALUES ($1, $2, $3, NULLIF($4, ''), $5, NULLIF($6, ''), $7)`, p, loc.Folder, loc.Area, r.Section, r.Target, r.Req, r.Snippet); err != nil {
			return err
		}
	}
	return nil
}

func writeFile(ctx context.Context, q postgres.Querier, loc Loc, p, sha string, size int64) error {
	_, err := q.Exec(ctx, `INSERT INTO spec_files (path, feature_key, area, name, blob_sha, size_bytes, mime_type, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (path) DO UPDATE SET blob_sha = EXCLUDED.blob_sha, size_bytes = EXCLUDED.size_bytes,
			mime_type = EXCLUDED.mime_type, updated_at = now()`,
		p, loc.Folder, loc.Area, loc.Rest, sha, size, MimeOf(p))
	return err
}

// MimeOf is the media type by extension; unknown types are downloads.
func MimeOf(p string) string {
	// the contract files of tech (FTR.HMR.SPEC-0004 tech §2.2)
	if _, format, ok := contract.ContractFile(path.Base(p)); ok {
		switch format {
		case contract.FormatOpenAPIJSON:
			return MimeOpenAPIJSON
		case contract.FormatOpenAPIYAML:
			return MimeOpenAPIYAML
		case contract.FormatProto:
			return MimeProto
		}
	}
	ext := strings.ToLower(path.Ext(p))
	switch ext {
	case ".md":
		return "text/markdown"
	case ".svg":
		return "image/svg+xml"
	case ".html", ".htm":
		return "text/html"
	}
	if t := mime.TypeByExtension(ext); t != "" {
		if i := strings.IndexByte(t, ';'); i > 0 {
			t = t[:i]
		}
		return t
	}
	return "application/octet-stream"
}

// The types of the contract files: the viewer shows them as API (FTR.HMR.SPEC-0004 R7).
const (
	MimeOpenAPIJSON = "application/vnd.oai.openapi+json"
	MimeOpenAPIYAML = "application/vnd.oai.openapi"
	MimeProto       = "text/x-protobuf"
)

// IsContract tells whether a type is of a contract file.
func IsContract(mt string) bool {
	return mt == MimeOpenAPIJSON || mt == MimeOpenAPIYAML || mt == MimeProto
}

// writeItems stores the requirements R<n> of product or the decisions A<n> of
// arch (FTR.HMR.DEV-0003 R10); the old rows of the document are already gone.
func writeItems(ctx context.Context, q postgres.Querier, key, path, area, md string) error {
	items, kind := Requirements(md), "requirement"
	if area == "arch" {
		items, kind = Decisions(md), "decision"
	}
	for _, r := range items {
		crit, _ := json.Marshal(r.Criteria)
		if _, err := q.Exec(ctx, `INSERT INTO spec_requirements (feature_key, req_id, path, section, text, criteria, kind)
			VALUES ($1, $2, $3, NULLIF($4, ''), $5, $6, $7)
			ON CONFLICT (feature_key, req_id) DO UPDATE SET path = EXCLUDED.path, section = EXCLUDED.section,
				text = EXCLUDED.text, criteria = EXCLUDED.criteria, kind = EXCLUDED.kind`, key, r.ID, path, r.Section, r.Text, crit, kind); err != nil {
			return err
		}
	}
	return nil
}

// writeLinks recomputes the links of one layer of a feature from its product or
// arch document (FTR.HMR.DEV-0002 R13, FTR.HMR.DEV-0003 R15): the old rows of
// the layer go, the new ones come in the same transaction as the document.
func writeLinks(ctx context.Context, q postgres.Querier, key, path, area, md string) error {
	layer := LayerProduct
	if area == "arch" {
		layer = LayerArch
	}
	if _, err := q.Exec(ctx, `DELETE FROM spec_links WHERE source_key = $1 AND layer = $2`, key, layer); err != nil {
		return err
	}
	parent, _ := Parent(md) // a broken front matter is reported by the check, here it is no parent
	for _, l := range Links(md, area, key, parent) {
		if _, err := q.Exec(ctx, `INSERT INTO spec_links (source_key, target_key, kind, requirements, source_path, layer)
			VALUES ($1, $2, $3, $4, $5, $6)`, key, l.Target, l.Kind, l.Requirements, path, l.Layer); err != nil {
			return err
		}
	}
	return nil
}

// errScanBusy: a check of the repository holds the lock; the backfill waits for the next tick.
var errScanBusy = errors.New("a check of the specification repository is running")

// ReindexContract rewrites the requirements, decisions and links of every
// product and arch document of the index when the index was written by another
// version of the document contract (FTR.HMR.DEV-0003 tech §4.2): the documents
// stay, what is read from them is read again. It runs under the lock of the
// checks and locks the documents it reads, so a check or a push does not
// interleave with it; running it again changes nothing.
func (s *Service) ReindexContract(ctx context.Context) (int, error) {
	n := 0
	err := postgres.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		var locked bool
		var version *string
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(`+lockKey+`), (SELECT value FROM spec_index_state WHERE key = 'contract')`).Scan(&locked, &version); err != nil {
			return err
		}
		if !locked {
			return errScanBusy
		}
		if version != nil && *version == contract.Version {
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT feature_key, path, area, markdown FROM spec_documents WHERE area IN ('product', 'arch') ORDER BY path FOR UPDATE`)
		if err != nil {
			return err
		}
		type doc struct{ key, path, area, md string }
		var docs []doc
		for rows.Next() {
			var d doc
			if err := rows.Scan(&d.key, &d.path, &d.area, &d.md); err != nil {
				rows.Close()
				return err
			}
			docs = append(docs, d)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, d := range docs {
			if _, err := tx.Exec(ctx, `DELETE FROM spec_requirements WHERE path = $1`, d.path); err != nil {
				return err
			}
			if err := writeItems(ctx, tx, d.key, d.path, d.area, d.md); err != nil {
				return err
			}
			if err := writeLinks(ctx, tx, d.key, d.path, d.area, d.md); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO spec_index_state (key, value) VALUES ('contract', $1)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, contract.Version); err != nil {
			return err
		}
		n = len(docs)
		return nil
	})
	return n, err
}
