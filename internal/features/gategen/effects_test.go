package gategen

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/mock/gomock"

	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	gmocks "github.com/GreenOnGrey/hammurapi-core/internal/platform/git/mocks"
)

// RQ-11: two gates of a run are files of one commit; a checked or skipped gate changes nothing.
func TestDocumentChanges(t *testing.T) {
	f := writerFeature("arch", true)
	f.Branch = "b"
	areas := []domain.Area{domain.AreaProduct, domain.AreaDesign, domain.AreaArch}
	written, changes := documentChanges(f, areas, map[domain.Area]Submission{
		domain.AreaProduct: {Content: "# p"}, domain.AreaDesign: {Content: "# d"}, domain.AreaArch: {Unchanged: true},
	}, map[domain.Area][]submittedFile{domain.AreaDesign: {{Path: "mockups.html", Content: "<html>"}}})
	if len(written) != 2 || len(changes) != 3 || !strings.HasSuffix(changes[2].Path, "/design/mockups.html") {
		t.Fatalf("%v %+v", written, changes)
	}
	msg := commitMessage(f, written, []domain.Area{domain.AreaArch}, "anna")
	tr := git.ParseTrailers(msg)
	if !tr.Generated || !tr.Agent || tr.Area != "" || !strings.Contains(msg, "write product, design; skip arch") {
		t.Fatalf("%q %+v", msg, tr)
	}
}

// ED-06: a branch that moved is tried once more, a document that changed is not overwritten.
func TestCommitConflict(t *testing.T) {
	f := writerFeature("product", true)
	f.Branch = "b"
	ctx := context.Background()
	areas := []domain.Area{domain.AreaProduct}
	base := map[domain.Area]string{domain.AreaProduct: "# base"}

	ctrl := gomock.NewController(t)
	p := gmocks.NewMockProvider(ctrl)
	e := &Effects{Git: p}
	gomock.InOrder(
		p.EXPECT().Commit(ctx, "tok", "b", "m", gomock.Any()).Return("", git.ErrConflict),
		p.EXPECT().GetFile(ctx, "tok", "b", gomock.Any()).Return(&git.File{Content: []byte("# base")}, nil),
		p.EXPECT().Commit(ctx, "tok", "b", "m", gomock.Any()).Return("c2", nil),
	)
	if sha, err := e.commit(ctx, "tok", f, areas, base, "m", nil); err != nil || sha != "c2" {
		t.Fatalf("%q %v", sha, err)
	}

	p.EXPECT().Commit(ctx, "tok", "b", "m", gomock.Any()).Return("", git.ErrConflict)
	p.EXPECT().GetFile(ctx, "tok", "b", gomock.Any()).Return(&git.File{Content: []byte("# edited")}, nil)
	_, err := e.commit(ctx, "tok", f, areas, base, "m", nil)
	if err == nil || errors.Is(err, git.ErrConflict) || !strings.Contains(err.Error(), "changed during the run") {
		t.Fatalf("%v", err)
	}
}
