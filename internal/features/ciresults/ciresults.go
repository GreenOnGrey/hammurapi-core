// Package ciresults receives test results of service pipelines (FTR.HMR.CMN-0002
// arch §9, tech §10): POST /hooks/v1/ci-results with a JUnit XML report, signed
// with CI_RESULTS_SECRET. Tests are linked to test cases by the ID in the test
// name or a property (QA-03), and the results reach the validation of the
// features whose PRs have this head.
package ciresults

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GreenOnGrey/hammurapi-core/internal/cycledata"
	"github.com/GreenOnGrey/hammurapi-core/internal/features/workflows"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/events"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/httpx"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/postgres"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/signing"
)

// Payload is the webhook body.
type Payload struct {
	Repo        string `json:"repo"`
	SHA         string `json:"sha"`
	Branch      string `json:"branch"`
	Environment string `json:"environment"` // ci | stage
	PipelineURL string `json:"pipelineUrl"`
	JUnit       string `json:"junit"` // base64 JUnit XML
}

// Test is one executed test.
type Test struct {
	Name       string
	Status     string // passed | failed | skipped
	DurationMS int
	Properties []string
}

type junitCase struct {
	Name       string    `xml:"name,attr"`
	ClassName  string    `xml:"classname,attr"`
	Time       float64   `xml:"time,attr"`
	Failure    *struct{} `xml:"failure"`
	Error      *struct{} `xml:"error"`
	Skipped    *struct{} `xml:"skipped"`
	Properties []struct {
		Name  string `xml:"name,attr"`
		Value string `xml:"value,attr"`
	} `xml:"properties>property"`
}

type junitSuite struct {
	Cases  []junitCase  `xml:"testcase"`
	Suites []junitSuite `xml:"testsuite"`
}

// ParseJUnit reads <testsuites> or <testsuite> documents.
func ParseJUnit(data []byte) ([]Test, error) {
	var root struct {
		XMLName xml.Name
		junitSuite
	}
	if err := xml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	var out []Test
	var walk func(s junitSuite)
	walk = func(s junitSuite) {
		for _, c := range s.Cases {
			t := Test{Name: strings.TrimSpace(c.ClassName + " " + c.Name), Status: "passed", DurationMS: int(c.Time * 1000)}
			switch {
			case c.Failure != nil || c.Error != nil:
				t.Status = "failed"
			case c.Skipped != nil:
				t.Status = "skipped"
			}
			for _, p := range c.Properties {
				t.Properties = append(t.Properties, p.Value)
			}
			out = append(out, t)
		}
		for _, sub := range s.Suites {
			walk(sub)
		}
	}
	walk(root.junitSuite)
	return out, nil
}

func normalize(s string) string {
	return strings.NewReplacer("-", "", "_", "", " ", "").Replace(strings.ToUpper(s))
}

// Match links tests to known test case IDs (by ID in the name or a property).
// A test case with any failed test is failed; skipped only if all are skipped.
func Match(tests []Test, known []string) []cycledata.TestResult {
	byID := map[string]*cycledata.TestResult{}
	var order []string
	for _, t := range tests {
		hay := normalize(t.Name + " " + strings.Join(t.Properties, " "))
		for _, id := range known {
			n := normalize(id)
			idx := strings.Index(hay, n)
			if idx < 0 {
				continue
			}
			// Do not match QA1 inside QA12.
			if end := idx + len(n); end < len(hay) && hay[end] >= '0' && hay[end] <= '9' {
				continue
			}
			r, ok := byID[id]
			if !ok {
				zero := 0
				r = &cycledata.TestResult{TCID: id, Status: t.Status, DurationMS: &zero}
				byID[id] = r
				order = append(order, id)
			}
			*r.DurationMS += t.DurationMS
			switch {
			case t.Status == "failed":
				r.Status = "failed"
			case t.Status == "passed" && r.Status == "skipped":
				r.Status = "passed"
			}
		}
	}
	out := make([]cycledata.TestResult, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}

// Handler serves POST /hooks/v1/ci-results.
type Handler struct {
	Pool    *pgxpool.Pool
	Secrets []string
	Events  events.Publisher
	Now     func() time.Time
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 20<<20))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	if !signing.Verify(r.Header, body, h.Secrets, now()) {
		metrics.WebhookEvents.WithLabelValues("unauthorized").Inc()
		http.Error(w, "invalid signature", http.StatusUnauthorized) // HOOK-01, HOOK-02
		return
	}
	var p Payload
	if err := json.Unmarshal(body, &p); err != nil || p.Repo == "" || p.SHA == "" {
		http.Error(w, "repo, sha and junit are required", http.StatusBadRequest)
		return
	}
	if p.Environment != "stage" {
		p.Environment = "ci"
	}
	raw, err := base64.StdEncoding.DecodeString(p.JUnit)
	if err != nil {
		raw = []byte(p.JUnit) // tolerate plain XML
	}
	tests, err := ParseJUnit(raw)
	if err != nil {
		http.Error(w, "invalid JUnit XML: "+err.Error(), http.StatusBadRequest)
		return
	}
	features, err := h.apply(r.Context(), p, tests)
	if err != nil {
		slog.ErrorContext(r.Context(), "ci results", "err", err)
		httpx.JSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]string{"code": "internal", "message": "try again later"}})
		return
	}
	for _, f := range features {
		h.Events.Publish(r.Context(), events.Event{Type: events.ValidationUpdated, Data: map[string]any{"featureId": f}})
	}
	metrics.WebhookEvents.WithLabelValues("accepted").Inc()
	w.WriteHeader(http.StatusAccepted)
}

func (h *Handler) apply(ctx context.Context, p Payload, tests []Test) ([]uuid.UUID, error) {
	var touched []uuid.UUID
	err := postgres.InTx(ctx, h.Pool, func(tx pgx.Tx) error {
		cd := cycledata.New(tx)
		prs, err := cd.PRsByHead(ctx, p.Repo, p.SHA, p.Branch)
		if err != nil {
			return err
		}
		if p.Environment == "stage" && len(prs) == 0 && p.Branch != "" {
			prs, err = cd.PRsByHead(ctx, p.Repo, "", p.Branch)
			if err != nil {
				return err
			}
		}
		seen := map[uuid.UUID]bool{}
		var known []string
		for _, pr := range prs {
			if seen[pr.FeatureID] {
				continue
			}
			seen[pr.FeatureID] = true
			touched = append(touched, pr.FeatureID)
			tcs, err := cd.TestCases(ctx, pr.FeatureID)
			if err != nil {
				return err
			}
			for _, tc := range tcs {
				known = append(known, tc.ID)
			}
		}
		results := Match(tests, known)
		if _, err := cd.InsertCIRun(ctx, p.Repo, p.SHA, p.Branch, p.Environment, p.PipelineURL, results); err != nil {
			return err
		}
		status := "success"
		for _, t := range tests {
			if t.Status == "failed" {
				status = "failure"
			}
		}
		if p.Environment == "ci" {
			for _, pr := range prs {
				if pr.HeadSHA == p.SHA {
					// FTR.HMR.DEV-0004 R17: the checks of the provider win; the report sets a PR without them
					if err := cd.SetPRCIFromReport(ctx, pr.ID, p.SHA, status); err != nil {
						return err
					}
				}
			}
		}
		for _, f := range touched {
			if _, err := workflows.SendToSubject(ctx, tx, "validation", f, "ci_results", map[string]any{"repo": p.Repo, "sha": p.SHA,
				"environment": p.Environment, "status": status}); err != nil {
				return err
			}
			var releaseID uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM releases WHERE feature_id = $1`, f).Scan(&releaseID); err == nil {
				if _, err := workflows.SendToSubject(ctx, tx, "release", releaseID, "ci_results", map[string]any{"repo": p.Repo, "sha": p.SHA, "status": status}); err != nil {
					return err
				}
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		return nil
	})
	return touched, err
}
