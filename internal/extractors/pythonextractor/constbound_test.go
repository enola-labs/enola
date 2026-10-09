package pythonextractor

import "testing"

func TestPyConstantLoopsAddNoScalingDepth(t *testing.T) {
	cases := map[string]string{
		"all-caps constant":  "    for x in xs:\n        for p in RESERVED_PREFIXES:\n            use(x, p)",
		"view of a constant": "    for x in xs:\n        for name, d in RESOURCE_MAP.items():\n            use(x, name, d)",
		"module constant":    "    for x in xs:\n        for p in settings.RESERVED_PREFIXES:\n            use(x, p)",
		"enum class":         "    for x in xs:\n        for state in DagRunState:\n            use(x, state)",
		"constant range":     "    for x in xs:\n        for i in range(MAX_RETRIES):\n            use(x, i)",
		"sorted constant":    "    for x in xs:\n        for p in sorted(RESERVED_PREFIXES):\n            use(x, p)",
		"local literal":      "    attrs = (\"a\", \"b\", \"c\")\n    for x in xs:\n        for a in attrs:\n            use(x, a)",
	}
	for name, body := range cases {
		loop, scaling := pyScaling(t, body)
		if loop != 2 {
			t.Errorf("%s: loop_depth = %d, want 2", name, loop)
		}
		if scaling != 1 {
			t.Errorf("%s: scaling_loop_depth = %d, want 1", name, scaling)
		}
	}
}

func TestPyLoopsThatOnlyLookConstantStillScale(t *testing.T) {
	cases := map[string]string{
		"lower-case name":         "    for x in xs:\n        for p in prefixes:\n            use(x, p)",
		"range over a length":     "    for x in xs:\n        for i in range(len(ys)):\n            use(x, i)",
		"local that is appended":  "    seen = []\n    for x in xs:\n        seen.append(x)\n        for s in seen:\n            use(x, s)",
		"local assigned twice":    "    acc = [1]\n    acc = load()\n    for x in xs:\n        for a in acc:\n            use(x, a)",
		"method call on constant": "    for x in xs:\n        for p in REGISTRY.lookup(x):\n            use(x, p)",
		"single capital":          "    for x in xs:\n        for i in range(N):\n            use(x, i)",
	}
	for name, body := range cases {
		if _, scaling := pyScaling(t, body); scaling != 2 {
			t.Errorf("%s: scaling_loop_depth = %d, want 2", name, scaling)
		}
	}
}
