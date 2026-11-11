package features

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	emocks "github.com/GreenOnGrey/hammurapi-core/internal/platform/events/mocks"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	gmocks "github.com/GreenOnGrey/hammurapi-core/internal/platform/git/mocks"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
	smocks "github.com/GreenOnGrey/hammurapi-core/internal/specdata/mocks"
	tu "github.com/GreenOnGrey/hammurapi-core/internal/testutil"
)

type fixture struct {
	store *smocks.MockStore
	git   *gmocks.MockProvider
	svc   *Service
}

func setup(t *testing.T) *fixture {
	ctrl := gomock.NewController(t)
	f := &fixture{store: smocks.NewMockStore(ctrl), git: gmocks.NewMockProvider(ctrl)}
	tokens := gmocks.NewMockTokenSource(ctrl)
	tokens.EXPECT().Token(gomock.Any(), gomock.Any()).Return("tok", nil).AnyTimes()
	ev := emocks.NewMockPublisher(ctrl)
	ev.EXPECT().Publish(gomock.Any(), gomock.Any()).AnyTimes()
	f.svc = NewService(f.store, f.git, tokens, ev, "main")
	return f
}

// DSC-09: the product gate gets the success metric; a failing commit deletes the branch.
func TestCreateFromIssueGitFailure(t *testing.T) {
	sys := &specdata.System{ID: uuid.New(), DomainKey: "FMS", Key: "CAR", ApprovalRequired: true}
	f := setup(t)
	f.store.EXPECT().NextNumber(gomock.Any(), sys.ID).Return(8, nil)
	f.git.EXPECT().BranchHead(gomock.Any(), "tok", "main").Return("base", nil)
	f.git.EXPECT().GetFile(gomock.Any(), "tok", "main", "rules/product/template.md").
		Return(&git.File{Content: []byte("# Product spec: <feature title>\n\n## Success metrics\n\nHow.\n")}, nil)
	f.git.EXPECT().CreateBranch(gomock.Any(), "tok", "feature/FTR.FMS.CAR-0008", "base").Return(nil)
	f.git.EXPECT().Commit(gomock.Any(), "tok", "feature/FTR.FMS.CAR-0008", gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _, msg string, ch []git.FileChange) (string, error) {
			if ch[0].Path != "specs/FMS/CAR/FTR.FMS.CAR-0008/product/spec.md" || !strings.Contains(string(ch[0].Content), "| bookings | `SELECT 1` | +5% | 14d |") {
				t.Fatalf("unexpected product doc %s %q", ch[0].Path, ch[0].Content)
			}
			if len(ch) != 2 || ch[1].Path != "specs/FMS/CAR/FTR.FMS.CAR-0008/discovery/discovery.md" { // DF-01: in the folder of the materials (FTR.HMR.DISC-0001 R30)
				t.Fatalf("discovery.md missing: %+v", ch)
			}
			if tr := git.ParseTrailers(msg); tr.Feature != "FTR.FMS.CAR-0008" || tr.Area != "product" {
				t.Fatalf("trailers %+v", tr)
			}
			return "", errors.New("rate limited")
		})
	f.git.EXPECT().DeleteBranch(gomock.Any(), "tok", "feature/FTR.FMS.CAR-0008").Return(nil)
	_, err := f.svc.CreateFromIssue(context.Background(), f.store, tu.User("expert:FMS:product"), FromIssue{
		System: sys, Title: "Weekend booking", Discovery: "# Discovery\n",
		Measure: &cycledata.Measure{Source: "bookings", Query: "SELECT 1", Target: "+5%", Window: "14d"},
	})
	if err == nil {
		t.Fatal("expected failure")
	}
}

func (f *fixture) withFeature(feat *specdata.Feature, gates ...*specdata.Gate) {
	f.store.EXPECT().FeatureByUniqueID(gomock.Any(), feat.UniqueID).Return(feat, nil).AnyTimes()
	var all []specdata.Gate
	for _, g := range gates {
		all = append(all, *g)
	}
	f.store.EXPECT().ActiveGates(gomock.Any(), feat.ID).Return(all, nil).AnyTimes()
}

// DEL-08
func TestDeleteConfirmMismatch(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(true)
	f.withFeature(feat)
	err := f.svc.Delete(context.Background(), tu.User("expert:FMS:product"), feat.UniqueID, "FTR.FMS.CAR-0006")
	tu.Code(t, err, 422, "confirm_mismatch")
}

// ROLE-03: an expert of another domain cannot delete.
func TestDeleteRequiresDomainExpert(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(true)
	f.withFeature(feat)
	err := f.svc.Delete(context.Background(), tu.User("expert:PAY:technical"), feat.UniqueID, feat.UniqueID)
	tu.Code(t, err, 403, "forbidden")
}

// A feature cannot be deleted once its release exists.
func TestDeleteAfterRelease(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(true)
	feat.Phase = domain.PhaseInRelease
	f.withFeature(feat)
	err := f.svc.Delete(context.Background(), tu.User("global"), feat.UniqueID, feat.UniqueID)
	tu.Code(t, err, 409, "release_exists")
}

// DEL-11
func TestDeleteLockedByOther(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(true)
	f.withFeature(feat)
	f.store.EXPECT().GetLock(gomock.Any(), feat.ID).Return(&specdata.Lock{LockedBy: uuid.New(), LockedByName: "Anna"}, nil)
	err := f.svc.Delete(context.Background(), tu.User("expert:FMS:product"), feat.UniqueID, feat.UniqueID)
	tu.Code(t, err, 423, "feature_locked")
}

// DEL-12: a deleted feature answers 410 with author and time.
func TestLoadDeleted(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(true)
	feat.Phase = domain.PhaseDeleted
	name := "Eugene"
	feat.DeletedByName = &name
	f.store.EXPECT().FeatureByUniqueID(gomock.Any(), feat.UniqueID).Return(feat, nil)
	_, err := Load(context.Background(), f.store, feat.UniqueID)
	tu.Code(t, err, 410, "feature_deleted")
}

// LOCK-02
func TestLockTakenByOther(t *testing.T) {
	f := setup(t)
	feat := tu.Feature(true)
	f.withFeature(feat)
	f.store.EXPECT().AcquireLock(gomock.Any(), feat.ID, gomock.Any()).Return(&specdata.Lock{LockedByName: "Anna"}, false, nil)
	_, err := f.svc.Lock(context.Background(), tu.User("expert:FMS:technical"), feat.UniqueID)
	tu.Code(t, err, 409, "feature_locked")
}

// CG-03: specifications are read-only during code generation; RB-08: nothing after rollback.
func TestLockOutsideSpecPhase(t *testing.T) {
	for phase, code := range map[domain.FeaturePhase]string{domain.PhaseCodegen: "codegen_in_progress", domain.PhaseRolledBack: "feature_read_only"} {
		f := setup(t)
		feat := tu.Feature(true)
		feat.Phase = phase
		f.withFeature(feat)
		_, err := f.svc.Lock(context.Background(), tu.User("expert:FMS:technical"), feat.UniqueID)
		tu.Code(t, err, 409, code)
	}
}

// GEN-08 / GEN-09 / ROLE-02: permissions by expert kind; tech and qa are not editable.
func TestPermissions(t *testing.T) {
	feat := tu.Feature(true)
	gates := []specdata.Gate{
		*tu.Gate(feat, domain.AreaProduct, domain.GateInReview),
		*tu.Gate(feat, domain.AreaArch, domain.GateInReview),
		*tu.Gate(feat, domain.AreaTech, domain.GateDraft),
	}
	p := PermissionsOf(tu.User("expert:FMS:product"), feat, gates)
	if len(p.Approve) != 1 || p.Approve[0] != domain.AreaProduct {
		t.Fatalf("product expert approves %v", p.Approve)
	}
	for _, a := range p.Edit {
		if a.Generated() {
			t.Fatalf("generated gate editable: %v", p.Edit)
		}
	}
	p = PermissionsOf(tu.User("expert:FMS:technical"), feat, gates)
	if len(p.Approve) != 1 || p.Approve[0] != domain.AreaArch {
		t.Fatalf("technical expert approves %v", p.Approve)
	}
	p = PermissionsOf(tu.User("admin:product"), feat, gates)
	if len(p.Edit) != 0 || p.Delete {
		t.Fatalf("non-expert: %+v", p)
	}
	// CG-01 / CG-02: codegen needs approvals only when the domain requires them.
	if p := PermissionsOf(tu.User("expert:FMS:product"), feat, gates); p.Codegen {
		t.Fatal("codegen allowed without approvals and without qa")
	}
	feat.ApprovalRequired = false
	gates = append(gates, *tu.Gate(feat, domain.AreaQA, domain.GateDraft))
	if p := PermissionsOf(tu.User("expert:FMS:product"), feat, gates); !p.Codegen || len(p.Submit) != 0 {
		t.Fatalf("no-approval domain: %+v", p)
	}
}

func TestHumanGatesApproved(t *testing.T) {
	feat := tu.Feature(true)
	gates := []specdata.Gate{*tu.Gate(feat, domain.AreaProduct, domain.GateApproved), *tu.Gate(feat, domain.AreaTech, domain.GateDraft)}
	if !HumanGatesApproved(gates) {
		t.Fatal("generated gates must not block generation")
	}
	gates = append(gates, *tu.Gate(feat, domain.AreaDesign, domain.GateInReview))
	if HumanGatesApproved(gates) {
		t.Fatal("design is in review")
	}
}
