package codegen

// The working copy of a code task (FTR.HMR.CMN-0007 R8, tech §7.3): the files
// of a ref through the API of the git provider, packed for Nabu.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
)

type tooLarge struct{ files int }

func (e *tooLarge) Error() string {
	return fmt.Sprintf("the working copy has more than %d files (%d at least)", MaxCopyFiles, e.files)
}

// upload streams the working copy of ref (plus the files of extra it lacks)
// to Nabu: a tar.gz without the top directory and without .git.
func (c *Code) upload(ctx context.Context, p git.Provider, token, ref, extra, id string) error {
	pr, pw := io.Pipe()
	go func() {
		gz := gzip.NewWriter(pw)
		tw := tar.NewWriter(gz)
		seen := map[string]bool{}
		err := c.copyRef(ctx, p, token, ref, tw, seen)
		if err == nil && extra != "" {
			err = c.copyRef(ctx, p, token, extra, tw, seen)
		}
		if err == nil {
			err = tw.Close()
		}
		if err == nil {
			err = gz.Close()
		}
		pw.CloseWithError(err)
	}()
	_, err := c.Nabu.UploadFiles(ctx, id, pr)
	_ = pr.CloseWithError(err)
	return err
}

// copyRef writes the files of ref not yet seen: the archive of the provider
// when it has one, else file by file.
func (c *Code) copyRef(ctx context.Context, p git.Provider, token, ref string, tw *tar.Writer, seen map[string]bool) error {
	if a, ok := p.(git.Archiver); ok {
		rc, err := a.Archive(ctx, token, ref)
		if err == nil {
			defer rc.Close()
			return copyArchive(rc, tw, seen)
		}
		slog.WarnContext(ctx, "archive of the provider, reading file by file", "ref", ref, "err", err)
	}
	files, err := p.ListFiles(ctx, token, ref, "")
	if err != nil {
		return err
	}
	for _, f := range files {
		if seen[f] || strings.HasPrefix(f, ".git/") {
			continue
		}
		file, err := p.GetFile(ctx, token, ref, f)
		if errors.Is(err, git.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		if err := put(tw, seen, &tar.Header{Name: f, Mode: 0o644, Size: int64(len(file.Content)), Typeflag: tar.TypeReg}, bytes.NewReader(file.Content)); err != nil {
			return err
		}
	}
	return nil
}

func put(tw *tar.Writer, seen map[string]bool, h *tar.Header, body io.Reader) error {
	if h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeSymlink {
		if len(seen) >= MaxCopyFiles {
			return &tooLarge{files: len(seen) + 1}
		}
		seen[h.Name] = true
	}
	h.Uname, h.Gname, h.Uid, h.Gid = "", "", 0, 0
	if err := tw.WriteHeader(h); err != nil {
		return err
	}
	if body != nil && h.Typeflag == tar.TypeReg {
		_, err := io.Copy(tw, body)
		return err
	}
	return nil
}

// copyArchive strips the top directory of a provider archive (repo-sha/…).
func copyArchive(r io.Reader, tw *tar.Writer, seen map[string]bool) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := h.Name
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[i+1:]
		} else {
			continue // the top directory itself or a global header
		}
		name = strings.TrimSuffix(name, "/")
		if name == "" || name == ".git" || strings.HasPrefix(name, ".git/") || seen[name] {
			continue
		}
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeSymlink, tar.TypeDir:
		default:
			continue
		}
		out := &tar.Header{Name: name, Mode: h.Mode & 0o755, Size: h.Size, Typeflag: h.Typeflag, Linkname: h.Linkname, ModTime: h.ModTime}
		if h.Typeflag == tar.TypeDir {
			out.Name, out.Size = name+"/", 0
		}
		if err := put(tw, seen, out, tr); err != nil {
			return err
		}
	}
}
