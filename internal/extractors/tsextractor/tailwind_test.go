package tsextractor

import (
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// tokenUsers returns, per token, the files whose component or file_ref names it.
func tokenUsers(ff []facts.Fact) (map[string]bool, map[string][]string) {
	tokens := map[string]bool{}
	byNode := map[string]string{}
	for _, f := range ff {
		if t, _ := f.Props[propToken].(string); t != "" {
			tokens[t] = true
			byNode[f.Name] = t
		}
	}
	users := map[string][]string{}
	for _, f := range ff {
		for _, r := range f.Relations {
			if t := byNode[r.Target]; t != "" && r.Kind == facts.RelNames {
				users[t] = append(users[t], f.File)
			}
		}
	}
	for t := range users {
		sort.Strings(users[t])
	}
	return tokens, users
}

// A preset whose theme is spread in from another module, and an app that uses it.
func TestTailwind_PresetTokensAndTheComponentsUsingThem(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"preset/src/theme.ts": `export const theme = {
  extend: {
    colors: {
      app: {
        bg: { accent: { DEFAULT: "var(--bg-accent)", hover: { DEFAULT: "var(--bg-accent-hover)" } } },
        badge: { warm: { border: { DEFAULT: "var(--badge-warm-border)" } } },
      },
    },
  },
}`,
		"preset/src/plugin.ts": `import plugin from "tailwindcss/plugin"
import { theme } from "./theme"
export default plugin(() => {}, { theme: { extend: { ...theme.extend, borderRadius: { card: "12px" } } } })`,
		"app/package.json":       `{"devDependencies": {"tailwindcss": "3"}}`,
		"app/tailwind.config.js": `module.exports = { presets: [require("../preset")], theme: { extend: { colors: { paper: "hsl(var(--paper))" } } } }`,
		"app/src/Dropzone.tsx": `export function Dropzone({ over }) {
  return <div className={cn("rounded-card border", over && "bg-app-bg-accent", "hover:bg-app-bg-accent-hover/50")} />
}`,
		"app/src/Badge.tsx": `const variants = cva("", { variants: { tone: { warm: "border-app-badge-warm-border" } } })
export function Badge() { return <span className={clsx({ "text-paper": true }, "bg-red-500")} /> }`,
		"app/src/i18n.ts":       `export const keys = { label: "the paper key" }`,
		"tools/codegen/emit.ts": `export const sample = "bg-app-bg-accent"`,
	}, false)
	tokens, users := tokenUsers(ff)
	for _, want := range []string{"app-bg-accent", "app-bg-accent-hover", "app-badge-warm-border", "card", "paper"} {
		if !tokens[want] {
			t.Errorf("token %s not read", want)
		}
	}
	for token, want := range map[string][]string{
		"app-bg-accent":         {"app/src/Dropzone.tsx"},
		"app-bg-accent-hover":   {"app/src/Dropzone.tsx"},
		"card":                  {"app/src/Dropzone.tsx"},
		"app-badge-warm-border": {"app/src/Badge.tsx"},
		"paper":                 {"app/src/Badge.tsx"},
	} {
		if len(users[token]) != 1 || users[token][0] != want[0] {
			t.Errorf("%s used by %v, want %v", token, users[token], want)
		}
	}
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && f.Props[propToken] != nil && f.Props["symbol_kind"] != facts.SymbolConstant {
			t.Errorf("token %s is not a constant symbol", f.Name)
		}
	}
}

func TestTailwind_V4ThemeAndNoTailwind(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"web/package.json":    `{"dependencies": {"tailwindcss": "4"}}`,
		"web/src/app.css":     "@import \"tailwindcss\";\n@theme {\n  --color-brand: #f00;\n  --radius-pill: 999px;\n}\n",
		"web/src/Button.tsx":  `export const Button = () => <button className="bg-brand rounded-pill" />`,
		"other/package.json":  `{"dependencies": {"unocss": "1"}}`,
		"other/src/Panel.tsx": `export const Panel = () => <div className="bg-brand" />`,
	}, false)
	_, users := tokenUsers(ff)
	if len(users["brand"]) != 1 || users["brand"][0] != "web/src/Button.tsx" || len(users["pill"]) != 1 {
		t.Errorf("v4 @theme tokens: %v", users)
	}
	empty := extractAll(t, map[string]string{
		"src/App.tsx": `export const App = () => <div className="bg-primary p-4" />`,
	}, false)
	if tokens, _ := tokenUsers(empty); len(tokens) != 0 {
		t.Errorf("tokens read in a repository with no Tailwind theme: %v", tokens)
	}
}

// A configured prefix is part of every class, and a CSS custom property in a string
// is no class.
func TestTailwind_PrefixAndCustomProperties(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"ui/package.json":       `{"devDependencies": {"tailwindcss": "3"}}`,
		"ui/tailwind.config.js": `module.exports = { prefix: "tw-", theme: { extend: { colors: { primary: { 600: "#06c" } }, fontFamily: { sans: ["Inter"] } } } }`,
		"ui/src/Button.tsx":     `export const Button = () => <button className="tw-bg-primary-600 hover:tw-text-primary-600" style={{ fontFamily: "var(--font-sans)" }} />`,
		"ui/src/Font.tsx":       `export const Font = () => <div style={{ ["--font-sans" as string]: "Inter" }} />`,
	}, false)
	_, users := tokenUsers(ff)
	if len(users["primary-600"]) != 1 {
		t.Errorf("prefixed classes not matched: %v", users)
	}
	if len(users["sans"]) != 0 {
		t.Errorf("a CSS custom property was read as a class: %v", users["sans"])
	}
}

// An Angular template's classes belong to the component that names it.
func TestTailwind_AngularTemplateBelongsToItsComponent(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"package.json":                `{"dependencies": {"@angular/core": "17", "tailwindcss": "3"}}`,
		"tailwind.config.js":          `module.exports = { prefix: "tw-", theme: { extend: { colors: { primary: { 600: "#06c" } } } } }`,
		"src/app/card.component.ts":   "import { Component } from '@angular/core'\n@Component({ selector: 'app-card', templateUrl: './card.component.html' })\nexport class CardComponent {}",
		"src/app/card.component.html": `<div class="tw-rounded tw-bg-primary-600"></div>`,
	}, false)
	_, users := tokenUsers(ff)
	if len(users["primary-600"]) != 1 || users["primary-600"][0] != "src/app/card.component.ts" {
		t.Errorf("template classes not on the component: %v", users)
	}
}

// A class belongs to the declaration it is written in: each component its own, and a
// variants constant the constant, so impact goes on to whoever uses it.
func TestTailwind_ClassBelongsToItsDeclaration(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"package.json":       `{"dependencies": {"tailwindcss": "3"}}`,
		"tailwind.config.js": `module.exports = { theme: { extend: { colors: { paper: "#fff", ink: "#000", accent: "#06c" } } } }`,
		"src/cards.tsx": `import { cva } from "class-variance-authority"
const cardVariants = cva("rounded", { variants: { tone: { accent: "bg-accent" } } })
export function Card() { return <div className={cardVariants({ tone: "accent" })} /> }
export function Sheet() { return <div className="bg-paper" /> }
export function Label() { return <span className="text-ink" /> }`,
	}, false)
	for holder, token := range map[string]string{
		"src.cardVariants": "tailwind/colors.accent",
		"src.Sheet":        "tailwind/colors.paper",
		"src.Label":        "tailwind/colors.ink",
	} {
		f, ok := findFact(ff, holder)
		if !ok {
			t.Fatalf("no symbol %s", holder)
		}
		if !hasRelation(f, facts.RelNames, token) {
			t.Errorf("%s does not name %s: %v", holder, token, f.Relations)
		}
	}
	card, _ := findFact(ff, "src.Card")
	if !hasRelation(card, facts.RelCalls, "src.cardVariants") {
		t.Errorf("the component using the variants does not reach them: %v", card.Relations)
	}
}

// A stylesheet uses the tokens its @apply lists name; a quoted string or a comment
// in CSS names none.
func TestTailwind_ApplyInStylesheets(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"app/package.json":       `{"devDependencies": {"tailwindcss": "3"}}`,
		"app/tailwind.config.js": `module.exports = { theme: { extend: { colors: { paper: "hsl(var(--paper))", ink: "#000", mist: "#eee" } } } }`,
		"app/src/main.css": `@tailwind base;
/* @apply bg-mist; */
.card { @apply bg-paper hover:text-ink/50 !important; font-family: "bg-mist"; }`,
		"app/src/x.scss": `.a { @apply text-ink; }`,
	}, false)
	_, users := tokenUsers(ff)
	for token, want := range map[string][]string{
		"paper": {"app/src/main.css"},
		"ink":   {"app/src/main.css", "app/src/x.scss"},
	} {
		if strings.Join(users[token], ",") != strings.Join(want, ",") {
			t.Errorf("%s used by %v, want %v", token, users[token], want)
		}
	}
	if len(users["mist"]) != 0 {
		t.Errorf("mist is named only in a comment and a string: %v", users["mist"])
	}
}

// Tailwind's own package declares the default palette; those tokens are no project's.
func TestTailwind_FrameworkPackageDeclaresNoTokens(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"packages/tailwindcss/package.json": `{"name": "tailwindcss"}`,
		"packages/tailwindcss/theme.css":    `@theme { --color-red-500: oklch(0.6 0.2 25); }`,
		"packages/site/package.json":        `{"name": "site", "devDependencies": {"tailwindcss": "4"}}`,
		"packages/site/app.css":             `@theme { --color-brand: #f00; }`,
		"packages/site/Page.tsx":            `export const Page = () => <div className="bg-brand bg-red-500" />`,
	}, false)
	tokens, _ := tokenUsers(ff)
	if !tokens["brand"] || tokens["red-500"] {
		t.Errorf("tokens %v: want the site's brand, not the framework's red-500", tokens)
	}
}

// nodeUsers returns, per token node, the files whose component or file_ref names it.
func nodeUsers(ff []facts.Fact) map[string][]string {
	nodes := map[string]bool{}
	for _, f := range ff {
		if f.Props[propToken] != nil {
			nodes[f.Name] = true
		}
	}
	users := map[string][]string{}
	for _, f := range ff {
		for _, r := range f.Relations {
			if r.Kind == facts.RelNames && nodes[r.Target] {
				users[r.Target] = append(users[r.Target], f.File)
			}
		}
	}
	for n := range users {
		sort.Strings(users[n])
	}
	return users
}

// Two apps declaring the same name declare two tokens. A docs app sees the base
// config it imports by package name before the preset that config lists; a
// stylesheet sees the theme it @imports, and a package its @source names sees that
// stylesheet's theme. A themed project naming a token it does not reach is using
// Tailwind's own class; a package no theme reaches names every declaration.
func TestTailwind_TokenNamesAreScopedPerProject(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"preset/package.json": `{"name": "@acme/preset", "dependencies": {"tailwindcss": "3"}}`,
		"preset/src/index.ts": `import type { Config } from "tailwindcss"
export default { theme: { extend: { colors: { brand: "#00f", ink: "#111" } } } } satisfies Config`,
		"docs/base/package.json":       `{"name": "docs-tailwind", "devDependencies": {"tailwindcss": "3"}}`,
		"docs/base/tailwind.config.js": `module.exports = { presets: [require("@acme/preset")], theme: { extend: { colors: { brand: "#0f0" } } } }`,
		"docs/site/package.json":       `{"name": "site", "devDependencies": {"tailwindcss": "3"}}`,
		"docs/site/tailwind.config.js": `import base from "docs-tailwind"
module.exports = { ...base, theme: { ...base.theme, backgroundImage: { hero: "url(/a.svg)" } } }`,
		"docs/site/Page.tsx":       `export const Page = () => <div className="bg-brand text-ink bg-hero" />`,
		"admin/package.json":       `{"name": "admin", "devDependencies": {"tailwindcss": "3"}}`,
		"admin/tailwind.config.js": `module.exports = { presets: [require("@acme/preset")], content: ["./src/**/*.tsx"] }`,
		"admin/src/Panel.tsx":      `export const Panel = () => <div className="bg-brand bg-hero" />`,
		"config/package.json":      `{"name": "config"}`,
		"config/theme.css":         `@theme { --color-paper: #fff; }`,
		"web/package.json":         `{"name": "web", "devDependencies": {"tailwindcss": "4"}}`,
		"web/styles/globals.css": `@import 'config/theme.css';
@source '../../ui/src/**/*.tsx';
@theme { --color-brand: #f00; }`,
		"web/app/Home.tsx":  `export const Home = () => <div className="bg-brand bg-paper text-ink" />`,
		"ui/package.json":   `{"name": "ui", "devDependencies": {"tailwindcss": "3"}}`,
		"ui/src/Button.tsx": `export const Button = () => <button className="bg-brand bg-paper" />`,
		"lib/package.json":  `{"name": "lib", "devDependencies": {"tailwindcss": "3"}}`,
		"lib/src/Card.tsx":  `export const Card = () => <div className="bg-brand" />`,
	}, false)
	users := nodeUsers(ff)
	for node, want := range map[string][]string{
		"tailwind/preset/src/colors.brand": {"admin/src/Panel.tsx", "lib/src/Card.tsx"},
		"tailwind/docs/base/colors.brand":  {"docs/site/Page.tsx", "lib/src/Card.tsx"},
		"tailwind/web/styles/colors.brand": {"lib/src/Card.tsx", "ui/src/Button.tsx", "web/app/Home.tsx"},
		"tailwind/colors.ink":              {"docs/site/Page.tsx"},
		"tailwind/colors.paper":            {"ui/src/Button.tsx", "web/app/Home.tsx"},
		"tailwind/backgroundImage.hero":    {"docs/site/Page.tsx"},
	} {
		if strings.Join(users[node], ",") != strings.Join(want, ",") {
			t.Errorf("%s used by %v, want %v", node, users[node], want)
		}
	}
	if users["tailwind/colors.brand"] != nil {
		t.Errorf("a name three themes declare kept the shared node")
	}
}

// A v4 variable belongs to the longest namespace it starts with.
func TestTailwind_V4CompoundNamespaces(t *testing.T) {
	cases := map[string][2]string{
		"color-brand":       {"colors", "brand"},
		"text-lg":           {"fontSize", "lg"},
		"text-color-muted":  {"textColor", "muted"},
		"font-weight-bold":  {"fontWeight", "bold"},
		"border-color-soft": {"borderColor", "soft"},
		"text-shadow-sm":    {"", ""},
		"breakpoint-md":     {"", ""},
		"text":              {"", ""},
	}
	for in, want := range cases {
		if s, n := v4Token(in); s != want[0] || n != want[1] {
			t.Errorf("v4Token(%q) = %q, %q, want %q, %q", in, s, n, want[0], want[1])
		}
	}
	ff := extractAll(t, map[string]string{
		"web/package.json": `{"dependencies": {"tailwindcss": "4"}}`,
		"web/src/app.css":  "@theme {\n  --color-muted: #999;\n  --text-color-muted: #777;\n}\n",
		"web/src/Hint.tsx": `export const Hint = () => <p className="text-muted" />`,
	}, false)
	users := nodeUsers(ff)
	if strings.Join(users["tailwind/textColor.muted"], ",") != "web/src/Hint.tsx" || users["tailwind/colors.muted"] != nil {
		t.Errorf("text-muted users %v", users)
	}
}

// Two themes a stylesheet @imports that both declare a name: the later import wins.
// Projects imported in an order no one stylesheet fixes keep both declarations.
func TestTailwind_LaterImportWins(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"tokens/package.json": `{"name": "tokens"}`,
		"tokens/theme.css":    `@theme { --radius-sm: 2px; --color-ink: #111; }`,
		"kit/package.json":    `{"name": "@acme/kit"}`,
		"kit/src/styles.css":  `@theme { --radius-sm: 4px; --color-ink: #222; }`,
		"web/package.json":    `{"name": "web", "devDependencies": {"tailwindcss": "4"}}`,
		"web/styles.css": `@import "tailwindcss";
@import "../tokens/theme.css";
@import "@acme/kit/src/styles.css";`,
		"web/Home.tsx":       `export const Home = () => <div className="rounded-sm" />`,
		"embed/package.json": `{"name": "embed", "devDependencies": {"tailwindcss": "4"}}`,
		"embed/styles.css": `@import "@acme/kit/src/styles.css";
@import "../tokens/theme.css";`,
		"embed/Frame.tsx":    `export const Frame = () => <div className="rounded-sm" />`,
		"split/package.json": `{"name": "split", "devDependencies": {"tailwindcss": "4"}}`,
		"split/a.css":        `@import "../tokens/theme.css";`,
		"split/b.css":        `@import "../kit/src/styles.css";`,
		"split/Pane.tsx":     `export const Pane = () => <div className="text-ink" />`,
	}, false)
	users := nodeUsers(ff)
	for node, want := range map[string][]string{
		"tailwind/kit/src/borderRadius.sm": {"web/Home.tsx"},
		"tailwind/tokens/borderRadius.sm":  {"embed/Frame.tsx"},
		"tailwind/kit/src/colors.ink":      {"split/Pane.tsx"},
		"tailwind/tokens/colors.ink":       {"split/Pane.tsx"},
	} {
		if strings.Join(users[node], ",") != strings.Join(want, ",") {
			t.Errorf("%s used by %v, want %v", node, users[node], want)
		}
	}
}
