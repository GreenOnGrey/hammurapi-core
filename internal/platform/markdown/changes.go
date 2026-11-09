package markdown

import "github.com/GreenOnGrey/speclint/contract"

// FeatureKey is the pattern of a feature key: FTR.<DOMAIN>.<SYSTEM>-NNNN. The
// sections, notes and links of documents are read by the document contract
// (github.com/GreenOnGrey/speclint/contract, FTR.HMR.DEV-0003 R17).
const FeatureKey = `FTR\.[A-Z][A-Z0-9]{1,9}\.[A-Z][A-Z0-9]{1,9}-\d{4}`

// UnnumberedDecisions lists the decisions of an arch document without a number
// A<n> in the table of decisions (FTR.HMR.DEV-0003 R16, R21).
func UnnumberedDecisions(doc string) []string {
	var out []string
	for _, v := range contract.Parse(doc, contract.Arch).Violations {
		if v.Element == contract.ViolDecisionsUnnumb {
			out = append(out, v.Message)
		}
	}
	return out
}
