package phpextractor

import (
	"github.com/enola-labs/enola/internal/extractors/ioclosure"
	"testing"
)

func TestPhpIODirect_GlobalPrimitive(t *testing.T) {
	cases := map[string]string{
		"file_get_contents": `$data = file_get_contents($path);`,
		"curl_exec":         `$r = curl_exec($ch);`,
		"wp_remote_get":     `$resp = wp_remote_get($url);`,
		"mysqli_query":      `$res = mysqli_query($link, $sql);`,
		"namespaced_call":   `$data = \file_get_contents($path);`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			src := "<?php\nfunction f() {\n" + body + "\n}\n"
			result := extractFileAST([]byte(src), "x.php")
			f := symbolsByName(result)["f"]
			if f.Props["io_direct"] != true {
				t.Errorf("io_direct = %v, want true (%s)", f.Props["io_direct"], name)
			}
		})
	}
}

func TestPhpIODirect_WpdbMethod(t *testing.T) {
	src := `<?php
function load() {
    global $wpdb;
    return $wpdb->get_results("SELECT * FROM t");
}
`
	result := extractFileAST([]byte(src), "x.php")
	f := symbolsByName(result)["load"]
	if f.Props["io_direct"] != true {
		t.Errorf("io_direct = %v, want true ($wpdb->get_results is a DB round-trip)", f.Props["io_direct"])
	}
}

func TestPhpIODirect_AmbiguousVerbExcluded(t *testing.T) {
	// A bare ->get() is an in-memory accessor in countless WordPress classes; it must
	// not be treated as I/O (the curated set deliberately excludes ambiguous verbs).
	src := `<?php
function compute() {
    $cache = new Cache();
    return $cache->get('key');
}
`
	result := extractFileAST([]byte(src), "x.php")
	f := symbolsByName(result)["compute"]
	if f.Props["io_direct"] == true {
		t.Errorf("io_direct = true, want unset (->get is an in-memory accessor)")
	}
}

func TestPhpComputePerformsIO_Transitive(t *testing.T) {
	// handler -> load_row -> $wpdb->get_results. io_direct is set on load_row by the
	// walker; performs_io is the transitive fixpoint (package-level), so run it here.
	src := `<?php
function handler() {
    return load_row(1);
}
function load_row($id) {
    global $wpdb;
    return $wpdb->get_results("SELECT 1");
}
`
	result := extractFileAST([]byte(src), "x.php")
	ioclosure.Propagate(result)

	lr := symbolsByName(result)["load_row"]
	if lr.Props["io_direct"] != true {
		t.Fatalf("precondition: load_row io_direct = %v, want true", lr.Props["io_direct"])
	}
	h := symbolsByName(result)["handler"]
	if h.Props["performs_io"] != true {
		t.Errorf("handler performs_io = %v, want true (transitive through load_row)", h.Props["performs_io"])
	}
}
