package tsextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// depAt returns the dependency fact an import at file:line produced.
func depAt(t *testing.T, ff []facts.Fact, file string, line int) facts.Fact {
	t.Helper()
	for _, f := range ff {
		if f.Kind == facts.KindDependency && f.File == file && f.Line == line {
			return f
		}
	}
	t.Fatalf("no dependency at %s:%d", file, line)
	return facts.Fact{}
}

func depTarget(f facts.Fact) string {
	if len(f.Relations) == 0 {
		return ""
	}
	return f.Relations[0].Target
}

func TestViteAliases(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"web/vite.config.ts": `import path from 'node:path'
import { fileURLToPath, URL } from 'node:url'
export default defineConfig({
  resolve: {
    alias: [
      { find: 'fs', replacement: path.resolve(__dirname, 'shim.ts') },
      { find: '@', replacement: fileURLToPath(new URL('./src', import.meta.url)) },
      { find: 'dep', replacement: '@scope/real-dep' },
      { find: /^lib\/(.*)/, replacement: ` + "`${path.resolve(import.meta.dirname, 'src/lib')}/$1`" + ` },
      { find: /^\/\//, replacement: path.join(import.meta.dirname, 'src/') },
      { find: 'magic', replacement: path.resolve(__dirname, 'x.ts'), customResolver(id) { return id } },
      { find: 'computed', replacement: someFn() },
    ],
  },
  test: { alias: { '@': '/elsewhere' } },
})`,
		"web/shim.ts":         `export const msg = 1`,
		"web/src/util.ts":     `export const u = 1`,
		"web/src/lib/deep.ts": `export const d = 1`,
		"web/src/main.ts": `import { msg } from 'fs'
import { u } from '@/util'
import d from 'dep'
import { d as dd } from 'lib/deep'
import m from 'magic'
import c from 'computed'
import r from 'react'
import { x } from './local'
import { u as u2 } from '//util'`,
		"web/src/local.ts": `export const x = 1`,
	}, false)
	cases := []struct {
		line           int
		target, source string
		via, reason    string
	}{
		{1, "web/shim.ts", "internal", aliasViaVite, ""},
		{2, "web/src/util", "internal", aliasViaVite, ""},
		{3, "@scope/real-dep", "external", aliasViaVite, ""},
		{4, "web/src/lib/deep", "internal", aliasViaVite, ""},
		{5, "magic", "external", "", aliasUnresolvedCustomResolver},
		{6, "computed", "external", "", aliasUnresolvedDynamic},
		{7, "react", "external", "", ""},
		{8, "web/src/local", "internal", "", ""},
		{9, "web/src/util", "internal", aliasViaVite, ""},
	}
	for _, c := range cases {
		f := depAt(t, ff, "web/src/main.ts", c.line)
		if got := depTarget(f); got != c.target {
			t.Errorf("line %d: target %q, want %q", c.line, got, c.target)
		}
		if f.Props["source"] != c.source {
			t.Errorf("line %d: source %v, want %s", c.line, f.Props["source"], c.source)
		}
		if got, _ := f.Props["resolved_via"].(string); got != c.via {
			t.Errorf("line %d: resolved_via %q, want %q", c.line, got, c.via)
		}
		if got, _ := f.Props["unresolved_reason"].(string); got != c.reason {
			t.Errorf("line %d: unresolved_reason %q, want %q", c.line, got, c.reason)
		}
	}
}

func TestViteAliasOverridesTSConfigPaths(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"app/tsconfig.json":  `{"compilerOptions": {"paths": {"@/*": ["./old/*"]}}}`,
		"app/vite.config.js": `export default { resolve: { alias: { '@': path.resolve(__dirname, 'src') } } }`,
		"app/src/a.ts":       `export const a = 1`,
		"app/src/b.ts":       `import { a } from '@/a'`,
	}, false)
	f := depAt(t, ff, "app/src/b.ts", 1)
	if depTarget(f) != "app/src/a" || f.Props["resolved_via"] != aliasViaVite {
		t.Errorf("vite alias should win: target %q via %v", depTarget(f), f.Props["resolved_via"])
	}
}

func TestViteAliasThroughModuleConst(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"app/vite.config.mts": `import path from 'node:path'
const root = path.resolve(__dirname, 'app/js')
export const loop = loop
export default defineConfig({
  resolve: { alias: { '@/': ` + "`${root}/`" + `, '~/': loop } },
})`,
		"app/app/js/a.ts": `export const a = 1`,
		"app/app/js/b.ts": `import { a } from '@/a'
import { z } from '~/z'`,
	}, false)
	f := depAt(t, ff, "app/app/js/b.ts", 1)
	if depTarget(f) != "app/app/js/a" || f.Props["resolved_via"] != aliasViaVite {
		t.Errorf("const-built alias: target %q via %v", depTarget(f), f.Props["resolved_via"])
	}
	if f := depAt(t, ff, "app/app/js/b.ts", 2); f.Props["unresolved_reason"] != aliasUnresolvedDynamic {
		t.Errorf("self-bound const: reason %v, want %s", f.Props["unresolved_reason"], aliasUnresolvedDynamic)
	}
}

func TestTSConfigPathsProvenance(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/tsconfig.app.json": `{"compilerOptions": {"paths": {"~lib/*": ["./src/lib/*"]}}}`,
		"pkg/src/lib/x.ts":      `export const x = 1`,
		"pkg/src/y.ts":          `import { x } from '~lib/x'`,
	}, false)
	f := depAt(t, ff, "pkg/src/y.ts", 1)
	if depTarget(f) != "pkg/src/lib/x" || f.Props["resolved_via"] != aliasViaTSConfig {
		t.Errorf("tsconfig.app.json paths: target %q via %v", depTarget(f), f.Props["resolved_via"])
	}
}

func TestNuxtAliases(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"nuxt.config.ts": `export default defineNuxtConfig({
  future: { compatibilityVersion: 4 },
  alias: { semver: resolve('./mocks/semver'), 'change-case': 'scule' },
  nitro: { alias: { 'isomorphic-ws': './mocks/ws' } },
})`,
		"app/constants/index.ts": `export const K = 1`,
		"config/emojis.ts":       `export const E = 1`,
		"shared/types/index.ts":  `export interface T {}`,
		"mocks/semver.ts":        `export const v = 1`,
		"app/composables/use.ts": `import { K } from '~/constants'
import { E } from '~~/config/emojis'
import type { T } from '#shared/types'
import { v } from 'semver'
import c from 'change-case'
import ws from 'isomorphic-ws'
import { useRoute } from '#app'`,
	}, true)
	cases := []struct {
		line           int
		target, source string
	}{
		{1, "app/constants", "internal"},
		{2, "config/emojis", "internal"},
		{3, "shared/types", "internal"},
		{4, "mocks/semver", "internal"},
		{5, "scule", "external"},
		{6, "isomorphic-ws", "external"},
		{7, "#app", "external"},
	}
	for _, c := range cases {
		f := depAt(t, ff, "app/composables/use.ts", c.line)
		if got := depTarget(f); got != c.target || f.Props["source"] != c.source {
			t.Errorf("line %d: %q (%v), want %q (%s)", c.line, got, f.Props["source"], c.target, c.source)
		}
	}
	if f := depAt(t, ff, "app/composables/use.ts", 1); f.Props["resolved_via"] != aliasViaNuxt {
		t.Errorf("~ resolved_via %v, want %s", f.Props["resolved_via"], aliasViaNuxt)
	}
}

func TestNuxt3SourceDirIsTheRoot(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"constants/index.ts": `export const K = 1`,
		"app/constants/x.ts": `export const X = 1`,
		"composables/use.ts": `import { K } from '~/constants'`,
	}, true)
	if got := depTarget(depAt(t, ff, "composables/use.ts", 1)); got != "constants" {
		t.Errorf("Nuxt 3 ~ resolved to %q, want the root's constants", got)
	}
}

func TestViteHTMLModuleScripts(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"site/vite.config.js": `export default { resolve: { alias: { fs: path.resolve(__dirname, 'shim.js') } } }`,
		"site/shim.js":        `export const msg = 1`,
		"site/src/main.ts":    `export const m = 1`,
		"site/index.html": `<!doctype html>
<script type="module" src="/src/main.ts"></script>
<script type="module" src="/src/missing.ts"></script>
<script src="/legacy.js"></script>
<script type="module">
  import { msg } from 'fs'
</script>`,
		"other/index.html": `<script type="module">import a from './a.js'</script>`,
	}, false)
	entry := depAt(t, ff, "site/index.html", 2)
	if depTarget(entry) != "site/src/main.ts" || entry.Props["html_entry"] != true {
		t.Errorf("module script src: %q %v", depTarget(entry), entry.Props)
	}
	if f := depAt(t, ff, "site/index.html", 6); depTarget(f) != "site/shim.js" || f.Props["resolved_via"] != aliasViaVite {
		t.Errorf("inline module import: %q via %v", depTarget(f), f.Props["resolved_via"])
	}
	for _, f := range ff {
		if f.Kind == facts.KindDependency && (f.File == "other/index.html" || depTarget(f) == "site/src/missing.ts" || depTarget(f) == "site/legacy.js") {
			t.Errorf("dependency that should not exist: %s", f.Name)
		}
	}
}

func TestVueImportOfAFolderModule(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/views/shell/index.ts": `import WidgetShell from './widget-shell/widget-shell.vue'
export { WidgetShell }`,
		"src/views/shell/widget-shell/widget-shell.vue": `<template><div /></template>`,
		"src/pages/home.vue": `<script setup lang="ts">
import { WidgetShell } from '../views/shell'
</script><template><WidgetShell /></template>`,
	}, false)
	home, _ := findFact(ff, "src/pages.Home")
	if !hasRelation(home, facts.RelCalls, "src/views/shell/widget-shell.WidgetShell") {
		t.Errorf("import through a barrel did not reach the component: %v", home.Relations)
	}
}

func TestBarrelExports(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"lib/index.ts":       `export { a, b as bee } from './a'` + "\n" + `export type { T } from './a'`,
		"lib/a.ts":           `export function a() {}` + "\n" + `export function b() {}` + "\n" + `export type T = {}`,
		"app/use.ts":         `import { a, bee } from '../lib'` + "\n" + `export function run() { a(); bee() }`,
		"app/local/index.ts": `export function local() {}` + "\n" + `export { local as again }`,
	}, false)
	run, _ := findFact(ff, "app.run")
	for _, want := range []string{"lib.a", "lib.b"} {
		if !hasRelation(run, facts.RelCalls, want) {
			t.Errorf("call through barrel not rewritten to %s: %v", want, run.Relations)
		}
	}
}

func TestTSConfigPathsRelativeTargetsUseBaseURL(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"dashboard/tsconfig.json": `{"compilerOptions": {"baseUrl": "./src", "paths": {"@/*": ["./*"], "@/e2e/*": ["../e2e/*"]}}}`,
		"dashboard/e2e/env.ts":    `export const URL = ''`,
		"dashboard/src/lib/a.ts":  `export const a = 1`,
		"dashboard/e2e/spec.ts":   `import { URL } from '@/e2e/env'` + "\n" + `import { a } from '@/lib/a'`,
	}, false)
	for line, want := range map[int]string{1: "dashboard/e2e/env", 2: "dashboard/src/lib/a"} {
		if got := depTarget(depAt(t, ff, "dashboard/e2e/spec.ts", line)); got != want {
			t.Errorf("line %d: %q, want %q (paths are relative to baseUrl)", line, got, want)
		}
	}
}
