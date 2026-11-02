package gates

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/features"
	emocks "github.com/GreenOnGrey/hammurapi-core/internal/platform/events/mocks"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	gmocks "github.com/GreenOnGrey/hammurapi-core/internal/platform/git/mocks"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
	smocks "github.com/GreenOnGrey/hammurapi-core/internal/specdata/mocks"
	tu "github.com/GreenOnGrey/hammurapi-core/internal/testutil"
)

// fakeGen is the writer: run is the run in progress.
type fakeGen struct{ run *features.WriterRun }

func (g *fakeGen) AfterApproval(context.Context, *specdata.Feature, domain.Area, bool, []specdata.Gate, uuid.UUID) error {
	return nil
}

func (g *fakeGen) Active(context.Context, uuid.UUID) (*features.WriterRun, error) { return g.run, nil }

func (g *fakeGen) OpenQuestions(context.Context, uuid.UUID, domain.Area) ([]cycledata.Question, error) {
	return nil, nil
}

type fixture struct {
	store *smocks.MockStore
	git   *gmocks.MockProvider
	gen   *fakeGen
	svc   *Service
}

func setup(t *testing.T, feat *specdata.Feature, gates ...*specdata.Gate) *fixture {
	ctrl := gomock.NewController(t)
	f := &fixture{store: smocks.NewMockStore(ctrl), git: gmocks.NewMockProvider(ctrl)}
	tokens := gmocks.NewMockTokenSource(ctrl)
	tokens.EXPECT().Token(gomock.Any(), gomock.Any()).Return("tok", nil).AnyTimes()
	ev := emocks.NewMockPublisher(ctrl)
	ev.EXPECT().Publish(gomock.Any(), gomock.Any()).AnyTimes()
	f.gen = &fakeGen{}
	f.svc = NewService(f.store, f.git, tokens, ev, f.gen, "main")
	tu.PassThroughTx(f.store)
	f.store.EXPECT().FeatureByUniqueID(gomock.Any(), feat.UniqueID).Return(feat, nil).AnyTimes()
	var all []specdata.Gate
	for _, g := range gates {
		all = append(all, *g)
		f.store.EXPECT().ActiveGate(gomock.Any(), feat.ID, g.Area).Return(g, nil).AnyTimes()
	}
	f.store.EXPECT().ActiveGate(gomock.Any(), feat.ID, gomock.Any()).Return(nil, specdata.ErrNotFound).AnyTimes()
	f.store.EXPECT().ActiveGates(gomock.Any(), feat.ID).Return(all, nil).AnyTimes()
	return f
}

// ROLE-02 / ROLE-03: only experts of the feature's domain edit gates.
func TestSaveRequiresDomainExpert(t *testing.T) {
	feat := tu.Feature(true)
	f := setup(t, feat, tu.Gate(feat, domain.AreaDesign, domain.GateDraft))
	for _, u := range []string{"expert:PAY:product", "admin:design"} {
		_, err := f.svc.SaveDocument(context.Background(), tu.User(u), feat.UniqueID, domain.AreaDesign, SaveInput{Content: "x"})
		tu.Code(t, err, 403, "forbidden")
	}
}

// GEN-03: tech and qa are not edited by hand.
func TestSaveGeneratedGate(t *testing.T) {
	feat := tu.Feature(true)
	f := setup(t, feat, tu.Gate(feat, domain.AreaTech, domain.GateDraft))
	_, err := f.svc.SaveDocument(context.Background(), tu.User("expert:FMS:technical"), feat.UniqueID, domain.AreaTech, SaveInput{Content: "x"})
	tu.Code(t, err, 403, "gate_generated")
}

// FTR.HMR.DEV-0001 ED-04: a run of the writer owns its documents.
func TestSaveDuringWriterRun(t *testing.T) {
	feat := tu.Feature(true)
	f := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateDraft))
	f.gen.run = &features.WriterRun{Mode: "write", State: "generating", Areas: []domain.Area{domain.AreaProduct}}
	f.store.EXPECT().GetLock(gomock.Any(), feat.ID).Return(nil, nil).AnyTimes()
	_, err := f.svc.SaveDocument(context.Background(), tu.User("expert:FMS:product"), feat.UniqueID, domain.AreaProduct, SaveInput{Content: "x"})
	tu.Code(t, err, 409, "spec_run_active")
}

// LOCK-02: PUT from another user while the feature is locked → 423.
func TestSaveLockedByOther(t *testing.T) {
	feat := tu.Feature(true)
	f := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateDraft))
	f.store.EXPECT().AcquireLock(gomock.Any(), feat.ID, gomock.Any()).Return(&specdata.Lock{LockedBy: uuid.New(), LockedByName: "Anna"}, false, nil)
	_, err := f.svc.SaveDocument(context.Background(), tu.User("expert:FMS:product"), feat.UniqueID, domain.AreaProduct, SaveInput{Content: "x"})
	tu.Code(t, err, 423, "feature_locked")
}

func (f *fixture) lockAndFile(feat *specdata.Feature, content string) {
	f.store.EXPECT().AcquireLock(gomock.Any(), feat.ID, gomock.Any()).Return(&specdata.Lock{}, true, nil)
	f.git.EXPECT().GetFile(gomock.Any(), "tok", feat.Branch, "specs/FMS/CAR/FTR.FMS.CAR-0005/product/spec.md").
		Return(&git.File{Content: []byte(content)}, nil)
}

// GATE-05: a stale baseSha is refused and nothing is committed.
func TestSaveStaleBase(t *testing.T) {
	feat := tu.Feature(true)
	f := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateDraft))
	f.lockAndFile(feat, "# current\n")
	_, err := f.svc.SaveDocument(context.Background(), tu.User("expert:FMS:product"), feat.UniqueID, domain.AreaProduct,
		SaveInput{Content: "# mine\n", BaseSHA: git.BlobSHA([]byte("# old\n"))})
	tu.Code(t, err, 409, "stale_document")
}

// GATE-09: saving unchanged content creates no commit.
func TestSaveUnchangedIsNoop(t *testing.T) {
	feat := tu.Feature(true)
	f := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateDraft))
	f.lockAndFile(feat, "# same\n")
	res, err := f.svc.SaveDocument(context.Background(), tu.User("expert:FMS:product"), feat.UniqueID, domain.AreaProduct,
		SaveInput{Content: "# same\n", BaseSHA: git.BlobSHA([]byte("# same\n"))})
	if err != nil || res.Commit != nil {
		t.Fatalf("got %+v %v", res, err)
	}
}

// GATE-01 / HOOK-04: a save commits with feature/area trailers.
func TestSaveCommitsWithTrailers(t *testing.T) {
	feat := tu.Feature(true)
	f := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateInReview))
	f.lockAndFile(feat, "# old\n")
	f.git.EXPECT().Commit(gomock.Any(), "tok", feat.Branch, gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _, _, msg string, _ []git.FileChange) (string, error) {
			tr := git.ParseTrailers(msg)
			if tr.Feature != feat.UniqueID || tr.Area != "product" || tr.Agent {
				t.Fatalf("trailers %+v", tr)
			}
			return "c2", nil
		})
	res, err := f.svc.SaveDocument(context.Background(), tu.User("expert:FMS:product"), feat.UniqueID, domain.AreaProduct,
		SaveInput{Content: "# new\n"})
	if err != nil || res.Commit == nil || *res.Commit != "c2" {
		t.Fatalf("got %+v %v", res, err)
	}
}

// CG-03: specifications are read-only during code generation.
func TestSaveDuringCodegen(t *testing.T) {
	feat := tu.Feature(true)
	feat.Phase = domain.PhaseCodegen
	f := setup(t, feat, tu.Gate(feat, domain.AreaProduct, domain.GateApproved))
	_, err := f.svc.SaveDocument(context.Background(), tu.User("expert:FMS:product"), feat.UniqueID, domain.AreaProduct, SaveInput{Content: "x"})
	tu.Code(t, err, 409, "codegen_in_progress")
}
