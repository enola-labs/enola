package layers

import "testing"

func layerOf(t *testing.T, defs []layerDef, module string) (string, int) {
	t.Helper()
	i, ok := classifyModule(module, false, patternDef{layers: defs})
	if !ok {
		return "", -1
	}
	return defs[i].Name, defs[i].Level
}

// An api directory is a client or a contract unless the router serves it.
func TestAPILayer_IsAContractNotTheOuterLayer(t *testing.T) {
	if name, level := layerOf(t, nextjsLayers, "client/libs/ads/ads-data/src/api"); name != "api" || level != 1 {
		t.Errorf("a library's api client = %s/%d, want api at the services tier", name, level)
	}
	if name, _ := layerOf(t, nextjsLayers, "pages/api/orders"); name != "pages" {
		t.Errorf("pages/api = %s, want pages", name)
	}
	if name, _ := layerOf(t, hexagonalLayers, "features/address/api/entity"); name == "handler" {
		t.Error("an api module is no handler in the hexagonal taxonomy")
	}
}

func TestConformance_Contradicts(t *testing.T) {
	for _, c := range []struct {
		conf conformance
		want bool
	}{
		{conformance{Inward: 3, Against: 30}, true},
		{conformance{Inward: 30, Against: 3}, false},
		{conformance{Inward: 2, Against: 5}, false}, // too few imports to judge
		{conformance{}, false},
	} {
		if got := c.conf.contradicts(); got != c.want {
			t.Errorf("%+v.contradicts() = %v, want %v", c.conf, got, c.want)
		}
	}
}
