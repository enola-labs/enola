package check

import (
	"encoding/json"
	"testing"
)

// A SARIF document is uploaded and attributed, not read locally. GitHub code scanning
// keys alerts on the driver name, so a wrapper binary that declared itself "enola"
// merged its alerts into another tool's and stamped them with enola's internal/version —
// which no wrapper stamps, so "dev".
func TestSARIF_DriverNamesTheBinaryThatGraded(t *testing.T) {
	out, err := formatVerdict().SARIF(Tool{Name: "acme-arch", Version: "v1.4.2"})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Runs []struct {
			Tool struct {
				Driver struct {
					Name           string `json:"name"`
					Version        string `json:"version"`
					InformationURI string `json:"informationUri"`
				} `json:"driver"`
			} `json:"tool"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	d := doc.Runs[0].Tool.Driver
	if d.Name != "acme-arch" {
		t.Errorf("driver.name = %q, want the binary that produced the verdict", d.Name)
	}
	if d.Version != "1.4.2" {
		t.Errorf("driver.version = %q, want the caller's version with the v stripped", d.Version)
	}
	if d.InformationURI == "" {
		t.Error("driver.informationUri must always be set; it is where the rules are documented")
	}
}

// The zero Tool is enola, which is what keeps every caller that never asked for a
// different identity byte-identical.
func TestSARIF_ZeroToolIsEnola(t *testing.T) {
	a, err := formatVerdict().SARIF(Tool{})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Runs []struct {
			Tool struct {
				Driver struct{ Name string } `json:"driver"`
			} `json:"tool"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(a, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Runs[0].Tool.Driver.Name != "enola" {
		t.Errorf("zero Tool rendered %q, want enola", doc.Runs[0].Tool.Driver.Name)
	}
}
