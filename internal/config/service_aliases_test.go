package config

import "testing"

// The signal and the unmatched-routes binder each build their matcher from the linking
// vocabulary, so that is where aliases must arrive, or the two would resolve a service
// name differently.
func TestLinkingVocab_CarriesServiceAliases(t *testing.T) {
	cfg := Default()
	v, err := cfg.LinkingVocab()
	if err != nil {
		t.Fatal(err)
	}
	if len(v.ServiceAliases) != 0 {
		t.Errorf("aliases present with none declared: %v", v.ServiceAliases)
	}

	cfg.ServiceAliases = map[string]string{"resource-api": "gateway"}
	if v, err = cfg.LinkingVocab(); err != nil {
		t.Fatal(err)
	}
	if v.ServiceAliases["resource-api"] != "gateway" {
		t.Errorf("declared alias did not reach the vocabulary: %v", v.ServiceAliases)
	}
}
