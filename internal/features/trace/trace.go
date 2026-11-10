// Package trace projects the traceability of a feature from its gate documents
// in git (FTR.HMR.CMN-0002 R15, tech §15): requirements of the product spec, the
// services of the arch spec (FTR.HMR.SPEC-0004 R3) or, without one, of the tech
// spec, the requirements of the services of the tech spec, and the test cases of
// the qa spec. It runs after every commit to these gates and after generation.
package trace

import (
	"context"
	"errors"
	"log/slog"

	"github.com/GreenOnGrey/speclint/v2/contract"
	"github.com/google/uuid"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/markdown"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
)

// Feature identifies the feature whose documents are projected.
type Feature struct {
	ID        uuid.UUID
	Key       string
	DomainKey string
	SystemKey string
	Ref       string // branch to read from
}

// Docs are gate documents; nil means "not present / unchanged".
type Docs struct {
	Product *string
	Arch    *string
	Tech    *string
	QA      *string
}

// Read loads the product, arch, tech and qa documents from git.
func Read(ctx context.Context, provider git.Provider, token string, f Feature) (Docs, error) {
	var d Docs
	for _, area := range []domain.Area{domain.AreaProduct, domain.AreaArch, domain.AreaTech, domain.AreaQA} {
		file, err := provider.GetFile(ctx, token, f.Ref, git.SpecPath(f.DomainKey, f.SystemKey, f.Key, string(area)))
		if errors.Is(err, git.ErrNotFound) {
			continue
		}
		if err != nil {
			return d, err
		}
		s := string(file.Content)
		switch area {
		case domain.AreaProduct:
			d.Product = &s
		case domain.AreaArch:
			d.Arch = &s
		case domain.AreaTech:
			d.Tech = &s
		case domain.AreaQA:
			d.QA = &s
		}
	}
	return d, nil
}

// Result reports what was projected.
type Result struct {
	Requirements    int
	TestCases       int
	Services        []string
	UnknownServices []string
}

// Project writes the projection for the documents present in docs.
func Project(ctx context.Context, q postgres.Querier, featureID uuid.UUID, docs Docs) (Result, error) {
	cd := cycledata.New(q)
	var res Result
	if docs.Product != nil {
		reqs := markdown.ParseRequirements(*docs.Product)
		rows := make([]cycledata.Requirement, 0, len(reqs))
		for _, r := range reqs {
			rows = append(rows, cycledata.Requirement{ID: r.ID, Text: r.Text})
		}
		if err := cd.ReplaceRequirements(ctx, featureID, rows); err != nil {
			return res, err
		}
		res.Requirements = len(rows)
	}
	if err := projectServices(ctx, cd, featureID, docs, &res); err != nil {
		return res, err
	}
	if docs.QA != nil {
		tcs := markdown.ParseTestCases(*docs.QA)
		rows := make([]cycledata.TestCase, 0, len(tcs))
		for _, tc := range tcs {
			rows = append(rows, cycledata.TestCase{ID: tc.ID, Level: tc.Level, ReqIDs: tc.ReqIDs, Title: tc.Title})
		}
		if err := cd.ReplaceTestCases(ctx, featureID, rows); err != nil {
			return res, err
		}
		res.TestCases = len(rows)
	}
	return res, nil
}

// projectServices links the feature to its services: those of arch when it names them by the contract 2.0, otherwise
// those of tech (FTR.HMR.SPEC-0004 R3, tech §3.3); the requirements of a service come from tech, for the services of
// the feature only.
func projectServices(ctx context.Context, cd *cycledata.DB, featureID uuid.UUID, docs Docs, res *Result) error {
	var keys []string
	if docs.Arch != nil {
		if a := contract.Parse(*docs.Arch, contract.Arch); a.NamesServices() {
			keys = contract.FeatureServices(&a, nil)
		}
	}
	if docs.Arch == nil && docs.Tech == nil {
		return nil // without arch and tech the links are not touched; an arch that stopped naming them clears them
	}
	reqs := map[string][]string{}
	var techKeys []string
	if docs.Tech != nil {
		for _, sr := range markdown.ParseTechServices(*docs.Tech) {
			if _, ok := reqs[sr.Service]; !ok {
				techKeys = append(techKeys, sr.Service)
			}
			reqs[sr.Service] = append(reqs[sr.Service], sr.ReqIDs...)
		}
	}
	if keys == nil {
		keys = techKeys
	}
	byService := map[uuid.UUID][]string{}
	var ids []uuid.UUID
	for _, k := range keys {
		svc, err := cd.ServiceByKey(ctx, k)
		if errors.Is(err, cycledata.ErrNotFound) {
			res.UnknownServices = append(res.UnknownServices, k)
			continue
		}
		if err != nil {
			return err
		}
		if _, ok := byService[svc.ID]; ok {
			continue
		}
		ids = append(ids, svc.ID)
		byService[svc.ID] = append([]string{}, reqs[k]...)
		res.Services = append(res.Services, svc.Key)
	}
	if err := cd.SetFeatureServices(ctx, featureID, ids); err != nil {
		return err
	}
	if err := cd.ReplaceRequirementServices(ctx, featureID, byService); err != nil {
		return err
	}
	if len(res.UnknownServices) > 0 {
		slog.WarnContext(ctx, "the specification names services missing in the catalog", "feature", featureID, "services", res.UnknownServices)
	}
	return nil
}
