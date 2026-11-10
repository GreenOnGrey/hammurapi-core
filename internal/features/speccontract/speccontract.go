// Package speccontract checks the services, the C4 diagram and the contract files of a feature when its gate arch
// or tech is submitted (FTR.HMR.SPEC-0004 R1–R4, R6, tech §3.1): the checks of the document contract shared with
// speclint, the catalog of services of Hammurapi, and the contract files parsed by Hammurapi itself.
package speccontract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/GreenOnGrey/speclint/v2/contract"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.yaml.in/yaml/v3"

	"github.com/GreenOnGrey/hammurapi-core/internal/apperr"
	"github.com/GreenOnGrey/hammurapi-core/internal/domain"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/git"
	"github.com/GreenOnGrey/hammurapi-core/internal/platform/metrics"
	"github.com/GreenOnGrey/hammurapi-core/internal/specdata"
)

// Violation is a broken check in the answer 422 spec_contract.
type Violation struct {
	Check   string `json:"check"`
	Subject string `json:"subject,omitempty"` // the key of a service, or the file; the web words the violation by it
	Message string `json:"message"`
	Line    int    `json:"line,omitempty"`
	File    string `json:"file,omitempty"` // a contract file, relative to the folder of the feature: tech/booking.proto
	Suggest string `json:"suggest,omitempty"`
}

// Checker reads the documents and the files of a feature in its branch.
type Checker struct {
	Git      git.Provider
	Pool     *pgxpool.Pool
	MaxBytes int64 // the limit of a contract file: the limit of the preview of files
}

// ServiceKeys are the keys of the catalog that are not deleted (tech §3.2).
func ServiceKeys(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	rows, err := pool.Query(ctx, `SELECT key FROM services WHERE NOT deleted_in_catalog ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Check returns 422 spec_contract with every violation of the gate, or nil. Other gates have none.
func (c *Checker) Check(ctx context.Context, token string, f *specdata.Feature, area domain.Area) error {
	if area != domain.AreaArch && area != domain.AreaTech {
		return nil
	}
	read := func(a domain.Area) (*contract.Document, error) {
		file, err := c.Git.GetFile(ctx, token, f.Branch, git.SpecPath(f.DomainKey, f.SystemKey, f.UniqueID, string(a)))
		if errors.Is(err, git.ErrNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		d := contract.Parse(string(file.Content), contract.Area(a))
		return &d, nil
	}
	arch, err := read(domain.AreaArch)
	if err != nil {
		return err
	}
	var out []Violation
	add := func(file string, fs []contract.Finding) {
		for _, x := range fs {
			out = append(out, Violation{Check: x.Check, Subject: x.Subject, Message: x.Message, Line: x.Line, File: file, Suggest: x.Suggest})
		}
	}
	if area == domain.AreaArch {
		if arch == nil {
			return nil
		}
		add("", contract.CheckArch(*arch))
		if arch.NamesServices() {
			keys, err := ServiceKeys(ctx, c.Pool)
			if err != nil {
				return err
			}
			add("", contract.CheckCatalog(*arch, keys))
		}
	} else {
		tech, err := read(domain.AreaTech)
		if err != nil || tech == nil {
			return err
		}
		add("", contract.CheckTech(*tech, arch))
		files, err := c.contractFiles(ctx, token, f, contract.FeatureServices(arch, tech))
		if err != nil {
			return err
		}
		out = append(out, files...)
	}
	if len(out) == 0 {
		return nil
	}
	for _, v := range out {
		metrics.SpecContractRejections.WithLabelValues(string(area), v.Check).Inc()
	}
	return apperr.Unprocessable("spec_contract", violationsMessage(out)).With("violations", out)
}

func violationsMessage(vs []Violation) string {
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		parts = append(parts, v.Check+": "+v.Message)
	}
	return "the document breaks the contract: " + strings.Join(parts, "; ")
}

// contractFile is a contract file next to tech in the branch.
type contractFile struct {
	name string // booking.openapi.yaml
	path string // the path in the repository
	size int64  // -1 when the provider gives none
}

// list lists the contract files next to tech in the branch; no folder is no files.
func list(ctx context.Context, g git.Provider, token string, f *specdata.Feature) ([]contractFile, error) {
	dir := path.Dir(git.SpecPath(f.DomainKey, f.SystemKey, f.UniqueID, string(domain.AreaTech)))
	entries, err := g.Tree(ctx, token, f.Branch, dir)
	if errors.Is(err, git.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []contractFile
	for _, e := range entries {
		name := strings.TrimPrefix(e.Path, dir+"/")
		if _, _, ok := contract.ContractFile(name); ok && !strings.Contains(name, "/") {
			out = append(out, contractFile{name: name, path: e.Path, size: e.Size})
		}
	}
	// the violations come in the order of the names, whatever order the provider lists the tree in
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// CheckFile checks a contract file of tech as the submission does (tech §2.2): its service is one of services, it is
// of its format by the module and, for OpenAPI in YAML, parsed by Hammurapi. rel is tech/<name>.
func CheckFile(rel string, content []byte, services []string) []Violation {
	_, format, _ := contract.ContractFile(path.Base(rel))
	fs := contract.CheckContractFile(rel, content, services)
	out := make([]Violation, 0, len(fs))
	for _, x := range fs {
		out = append(out, Violation{Check: x.Check, Subject: x.Subject, Message: x.Message, Line: x.Line, File: rel})
	}
	if !hasCheck(fs, contract.CheckContractFileInvalid) && format == contract.FormatOpenAPIYAML {
		if line, err := checkOpenAPIYAML(content); err != nil {
			out = append(out, Violation{Check: contract.CheckContractFileInvalid, Message: err.Error(), Line: line, File: rel})
		}
	}
	return out
}

// contractFiles checks the contract files next to tech (tech §2.2).
func (c *Checker) contractFiles(ctx context.Context, token string, f *specdata.Feature, services []string) ([]Violation, error) {
	files, err := list(ctx, c.Git, token, f)
	if err != nil {
		return nil, err
	}
	var out []Violation
	tooLarge := func(rel string) Violation {
		return Violation{Check: contract.CheckContractFileInvalid, File: rel, Line: 1, Message: fmt.Sprintf("the file is larger than %d bytes", c.MaxBytes)}
	}
	for _, cf := range files {
		rel := "tech/" + cf.name
		if c.MaxBytes > 0 && cf.size > c.MaxBytes {
			out = append(out, tooLarge(rel))
			continue
		}
		file, err := c.Git.GetFile(ctx, token, f.Branch, cf.path)
		if err != nil {
			return nil, err
		}
		if c.MaxBytes > 0 && int64(len(file.Content)) > c.MaxBytes { // GitLab gives no size in a tree
			out = append(out, tooLarge(rel))
			continue
		}
		out = append(out, CheckFile(rel, file.Content, services)...)
	}
	return out, nil
}

func hasCheck(fs []contract.Finding, check string) bool {
	for _, f := range fs {
		if f.Check == check {
			return true
		}
	}
	return false
}

// checkOpenAPIYAML parses the YAML: openapi is 3.x and paths is a mapping (tech §2.2, FILE-03).
func checkOpenAPIYAML(b []byte) (int, error) {
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&doc); err != nil {
		var te *yaml.TypeError
		if errors.As(err, &te) {
			return 1, err
		}
		return yamlLine(err.Error()), err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return 1, errors.New("the document is not a mapping")
	}
	root := doc.Content[0]
	var version string
	paths := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		switch k.Value {
		case "openapi":
			version = v.Value
		case "paths":
			if v.Kind != yaml.MappingNode {
				return v.Line, errors.New("paths is not a mapping")
			}
			paths = true
		}
	}
	if !strings.HasPrefix(version, "3.") {
		return 1, errors.New("openapi is not 3.x")
	}
	if !paths {
		return 1, errors.New("no paths")
	}
	return 0, nil
}

// yamlLine reads the line of an error of the YAML parser: "yaml: line 4: …".
func yamlLine(msg string) int {
	var n int
	if i := strings.Index(msg, "line "); i >= 0 {
		if _, err := fmt.Sscanf(msg[i:], "line %d", &n); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

// Contracts reads the contract files next to tech in the branch: name → content; service "" reads those of every
// service (the writer), a key — those of one service (a code task, FTR.HMR.SPEC-0004 R8). A file larger than
// maxBytes is left out: the submission refuses it anyway.
func Contracts(ctx context.Context, g git.Provider, token string, f *specdata.Feature, service string, maxBytes int64) (map[string]string, error) {
	files, err := list(ctx, g, token, f)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, cf := range files {
		if svc, _, _ := contract.ContractFile(cf.name); service != "" && svc != service || maxBytes > 0 && cf.size > maxBytes {
			continue
		}
		file, err := g.GetFile(ctx, token, f.Branch, cf.path)
		if err != nil {
			return nil, err
		}
		if maxBytes <= 0 || int64(len(file.Content)) <= maxBytes {
			out[cf.name] = string(file.Content)
		}
	}
	return out, nil
}

// WriteContracts writes the contracts after the documents of a prompt, sorted by name.
func WriteContracts(b *strings.Builder, contracts map[string]string, heading string) {
	names := make([]string, 0, len(contracts))
	for n := range contracts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(b, heading, n)
		b.WriteString(contracts[n])
		b.WriteString("\n")
	}
}
