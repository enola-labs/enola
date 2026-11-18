package pythonextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A helper module beside the notebooks is called from a code cell; the notebook
// names it, magics and markdown cells are skipped, and a call to a library is
// dropped.
func TestNotebookReferencesTheHelpersItCalls(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"analysis/helpers.py": "def load_orders(path):\n    return path\n\ndef unused():\n    return 1\n",
		"analysis/report.ipynb": `{
 "cells": [
  {"cell_type": "markdown", "source": ["# Report\n", "load_orders is described here"]},
  {"cell_type": "code", "source": ["%matplotlib inline\n", "!pip install pandas\n", "import pandas as pd\n", "from helpers import load_orders\n"]},
  {"cell_type": "code", "source": "orders = load_orders('x.csv')\ndf = pd.DataFrame(orders)\n"}
 ],
 "metadata": {}, "nbformat": 4, "nbformat_minor": 5
}`,
		"analysis/.ipynb_checkpoints/report-checkpoint.ipynb": `{"cells": [{"cell_type": "code", "source": "from helpers import unused\nunused()\n"}]}`,
	})
	var refs []string
	for _, f := range ff {
		if f.Kind == facts.KindFileRef && f.PropBool("notebook") {
			if f.File != "analysis/report.ipynb" {
				t.Errorf("a reference from %s", f.File)
			}
			for _, r := range f.Relations {
				refs = append(refs, r.Target)
			}
		}
	}
	found := false
	for _, r := range refs {
		found = found || r == "analysis/helpers.load_orders"
		if r == "analysis/helpers.unused" {
			t.Errorf("the checkpoint copy was read: %v", refs)
		}
	}
	if !found {
		t.Errorf("notebook references = %v, want analysis/helpers.load_orders", refs)
	}
}

// A notebook's queries, in a string or a %%bigquery cell, name the tables it
// reads; a Python import and a cell without a query name none.
func TestNotebookReadsTheTablesItsQueriesName(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"analysis/report.ipynb": `{
 "cells": [
  {"cell_type": "code", "source": ["from os.path import join\n", "from google.cloud import bigquery\n"]},
  {"cell_type": "code", "source": ["q = f\"\"\"\n", "SELECT o.id FROM ` + "`{PROJECT}.marts.fct_orders`" + ` o\n", "LEFT JOIN staging.stg_refunds r ON r.id = o.id\n", "\"\"\"\n"]},
  {"cell_type": "code", "source": ["%%bigquery df\n", "select * from ` + "`shop-data.dimensions.dim_users`" + `\n"]},
  {"cell_type": "code", "source": "q = 'SELECT * FROM ` + "`shop-data.marts`.`mrt_returns`" + `'\n"},
  {"cell_type": "code", "source": "rows = load_from(data.frame)\n"}
 ],
 "metadata": {}, "nbformat": 4, "nbformat_minor": 5
}`,
	})
	var got []string
	for _, f := range ff {
		if f.Kind != facts.KindFileRef || f.PropString("reads_tables") == "" {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelDependsOn {
				got = append(got, r.Target)
			}
		}
	}
	want := []string{"dim_users", "fct_orders", "mrt_returns", "stg_refunds"}
	if !slices.Equal(got, want) {
		t.Errorf("tables read = %v, want %v", got, want)
	}
}
