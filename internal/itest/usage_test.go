//go:build integration

package itest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/usage"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/agent"
)

// FTR.HMR.CMN-0007 HN-11: the history of usage stays for reports after the
// agent of Hammurapi is gone.
func TestUsageReport(t *testing.T) {
	ctx := context.Background()
	s := &usage.Service{Pool: pool}
	admin := &domain.Principal{GlobalAdmin: true}
	for i, sc := range []agent.Scenario{agent.ScenarioIssueAnalysis, agent.ScenarioIssueAnalysis, agent.ScenarioCodegen} {
		model := "nabu:hammurapi-issue-analysis"
		if sc == agent.ScenarioCodegen {
			model = "nabu"
		}
		if err := s.Add(ctx, usage.Record{Scenario: sc, Model: model,
			Usage: agent.Usage{TokensIn: int64(1000 * (i + 1)), TokensOut: 100, CostUSD: 0.01 * float64(i+1)}}); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := s.Report(ctx, admin, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), []string{"model"})
	if err != nil {
		t.Fatal(err)
	}
	var in int64
	for _, r := range rep.Rows {
		in += r.TokensIn
	}
	if rep.Totals.Runs < 3 || in != rep.Totals.TokensIn || len(rep.Rows) < 2 {
		t.Fatalf("%+v", rep)
	}
	if _, err := s.Report(ctx, &domain.Principal{}, time.Now().Add(-time.Hour), time.Now(), nil); !isCode(err, "forbidden") {
		t.Fatalf("not a global admin: %v", err)
	}
	if _, err := s.Report(ctx, admin, time.Now(), time.Now().Add(-time.Hour), nil); !isCode(err, "invalid_period") {
		t.Fatalf("period: %v", err)
	}
}

func isCode(err error, code string) bool {
	var e *apperr.Error
	return errors.As(err, &e) && e.Code == code
}
