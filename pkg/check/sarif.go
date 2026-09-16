package check

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/version"
)

// The SARIF 2.1.0 shapes this writer emits. Only the properties a reader
// needs are present; field order is fixed by the struct so two runs over one
// verdict are byte-identical.
type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	Version        string      `json:"version"`
	InformationURI string      `json:"informationUri"`
	Rules          []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID               string       `json:"id"`
	ShortDescription sarifMessage `json:"shortDescription"`
}

type sarifMessage struct {
	Text string `json:"text"`
}

type sarifResult struct {
	RuleID              string             `json:"ruleId"`
	RuleIndex           int                `json:"ruleIndex"`
	Level               string             `json:"level"`
	Message             sarifMessage       `json:"message"`
	Locations           []sarifLocation    `json:"locations,omitempty"`
	PartialFingerprints map[string]string  `json:"partialFingerprints"`
	Suppressions        []sarifSuppression `json:"suppressions,omitempty"`
	Properties          sarifProperties    `json:"properties"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysical `json:"physicalLocation"`
}

type sarifPhysical struct {
	ArtifactLocation sarifArtifact `json:"artifactLocation"`
	Region           *sarifRegion  `json:"region,omitempty"`
}

type sarifArtifact struct {
	URI string `json:"uri"`
}

type sarifRegion struct {
	StartLine   int `json:"startLine"`
	StartColumn int `json:"startColumn,omitempty"`
	EndLine     int `json:"endLine,omitempty"`
	EndColumn   int `json:"endColumn,omitempty"`
}

type sarifSuppression struct {
	Kind          string `json:"kind"`
	Justification string `json:"justification"`
}

type sarifProperties struct {
	Bucket          string  `json:"bucket"`
	Confidence      float64 `json:"confidence"`
	Policy          string  `json:"policy"`
	Source          string  `json:"source"`
	SuggestedAction string  `json:"suggestedAction,omitempty"`
}

const (
	sarifSchema = "https://json.schemastore.org/sarif-2.1.0.json"
	sarifVer    = "2.1.0"
	// fingerprintKey names the identity scheme, so a reader that stores
	// fingerprints can tell this one from a later scheme that replaces it.
	fingerprintKey = "enola/v1"
)

// Tool identifies the binary that produced a verdict, for the SARIF driver block
// a consumer attributes its findings to.
//
// It is a parameter rather than the constant it used to be because a wrapper
// binary produces verdicts too, and a SARIF document is uploaded rather than
// read locally: GitHub code scanning keys alerts on the driver name, so an
// a wrapper's `check --format sarif` that declared itself "enola" would merge its
// alerts into another tool's and stamp them with a version it was never built at —
// enola's internal/version, which no wrapper stamps, so "dev".
//
// The zero value is enola, which is what keeps every existing caller correct.
type Tool struct {
	Name           string
	Version        string
	InformationURI string
}

// resolve fills the enola defaults for whatever the caller left empty.
func (t Tool) resolve() Tool {
	if t.Name == "" {
		t.Name = "enola"
	}
	if t.Version == "" {
		t.Version = version.Version
	}
	if t.InformationURI == "" {
		// The engine's project page, deliberately, even for a wrapper: it is
		// where the rules a reader has just been shown are documented.
		t.InformationURI = "https://github.com/enola-labs/enola"
	}
	t.Version = strings.TrimPrefix(t.Version, "v")
	return t
}

// SARIF renders the verdict as one SARIF 2.1.0 run: a rule per distinct rule
// id with the team's reason as its short description, a result per finding in
// every bucket, the region from the evidence the explainer measured, the
// finding's identity as a partial fingerprint, and the bucket, confidence and
// policy outcome as properties. Resolved findings carry no region, because the
// position they had is on the baseline side and the tree may no longer have
// that line. Suppressed and exempted findings carry the excuse that kept them
// out of the gate, as a SARIF suppression.
func (v Verdict) SARIF(tool Tool) ([]byte, error) {
	driver := tool.resolve()
	places := v.placements()
	reasons := map[string]string{}
	for _, p := range places {
		id := ruleOf(p.finding)
		if _, seen := reasons[id]; !seen {
			reasons[id] = reasonOf(p.finding)
		}
	}
	ids := make([]string, 0, len(reasons))
	for id := range reasons {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	index := map[string]int{}
	rules := make([]sarifRule, 0, len(ids))
	for i, id := range ids {
		index[id] = i
		rules = append(rules, sarifRule{ID: id, ShortDescription: sarifMessage{Text: reasons[id]}})
	}

	results := make([]sarifResult, 0, len(places))
	for _, p := range places {
		id := ruleOf(p.finding)
		r := sarifResult{
			RuleID:              id,
			RuleIndex:           index[id],
			Level:               p.bucket.level,
			Message:             sarifMessage{Text: oneLine(p.finding.Title)},
			PartialFingerprints: map[string]string{fingerprintKey: p.identity},
			Properties: sarifProperties{
				Bucket:          p.bucket.name,
				Confidence:      p.finding.Confidence,
				Policy:          v.policyOf(p.finding),
				Source:          p.finding.Source,
				SuggestedAction: actionOf(p.finding),
			},
		}
		if p.located && p.bucket.name != "resolved" {
			region := &sarifRegion{StartLine: p.evidence.Line, StartColumn: p.evidence.Column, EndLine: p.evidence.EndLine, EndColumn: p.evidence.EndColumn}
			r.Locations = []sarifLocation{{PhysicalLocation: sarifPhysical{
				ArtifactLocation: sarifArtifact{URI: hostPath(p.evidence.File)},
				Region:           region,
			}}}
		}
		if excuse := v.excuseOf(p.bucket.name, p.finding); excuse != "" {
			r.Suppressions = []sarifSuppression{{Kind: "external", Justification: excuse}}
		}
		results = append(results, r)
	}

	doc := sarifLog{
		Schema:  sarifSchema,
		Version: sarifVer,
		Runs: []sarifRun{{
			Tool: sarifTool{Driver: sarifDriver{
				Name:           driver.Name,
				Version:        driver.Version,
				InformationURI: driver.InformationURI,
				Rules:          rules,
			}},
			Results: results,
		}},
	}
	return json.MarshalIndent(doc, "", "  ")
}
