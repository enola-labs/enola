package tsextractor

import (
	"sort"
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
