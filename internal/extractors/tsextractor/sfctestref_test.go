package tsextractor

import (
	"context"
	"testing"
)

// TestExtractTestRefs_ComponentUnderATestDirectory pins that a .vue or .svelte file
// under test/ or tests/ is read for test refs through its script blocks, as a
// script file there is, and declares nothing.
func TestExtractTestRefs_ComponentUnderATestDirectory(t *testing.T) {
	dir := setupTSProject(t, map[string]string{
		"src/api.ts": `export function load() {}`,
		"test/fixtures/App.vue": `<template><div /></template>
<script setup lang="ts">
import { load } from '../../src/api'
load()
</script>`,
		"tests/fmt/Widget.svelte": `<script>
import { load } from '../../src/api'
load()
</script>
<p>hi</p>`,
	}, false)
	for _, file := range []string{"test/fixtures/App.vue", "tests/fmt/Widget.svelte"} {
		ff, err := New().ExtractTestRefs(context.Background(), dir, []string{file}, []string{"src/api.ts"})
		if err != nil {
			t.Fatal(err)
		}
		if got := testRefTargets(t, ff, file); !got["src.load"] {
			t.Errorf("%s: missing target src.load; got %v", file, got)
		}
	}
}
