package tsextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

const meController = `import { Controller, Get, Patch } from '@nestjs/common';

@Controller('me')
export class MeController {
  @Get()
  me() {}
  @Patch('addresses/:id')
  update() {}
}
`

func nestRoutes(t *testing.T, mains ...string) []string {
	t.Helper()
	files := map[string]string{
		"package.json":         `{"name": "api", "dependencies": {"@nestjs/common": "^10.0.0"}}`,
		"src/me.controller.ts": meController,
	}
	for i, m := range mains {
		files["src/main"+string(rune('a'+i))+".ts"] = m
	}
	var out []string
	for _, f := range extractRepoAllFiles(t, files) {
		if f.Kind == facts.KindRoute && f.PropString(facts.PropRole) == facts.RoleServer {
			out = append(out, f.Name)
		}
	}
	slices.Sort(out)
	return out
}

func TestNestGlobalPrefix_LiteralIsApplied(t *testing.T) {
	got := nestRoutes(t, "app.setGlobalPrefix('v1');\n", "app.setGlobalPrefix(\"/v1/\");\n")
	if want := []string{"/v1/me", "/v1/me/addresses/:id"}; !slices.Equal(got, want) {
		t.Errorf("routes = %v, want %v", got, want)
	}
}

func TestNestGlobalPrefix_NotAppliedUnlessCertain(t *testing.T) {
	want := []string{"/me", "/me/addresses/:id"}
	for name, mains := range map[string][]string{
		"none":       nil,
		"variable":   {"app.setGlobalPrefix(process.env.PREFIX);\n"},
		"disagree":   {"app.setGlobalPrefix('v1');\n", "app.setGlobalPrefix('v2');\n"},
		"options":    {"app.setGlobalPrefix('v1', { exclude: ['health'] });\n"},
		"template":   {"app.setGlobalPrefix(`${base}`);\n"},
		"one of two": {"app.setGlobalPrefix('v1');\n", "app.setGlobalPrefix(prefix);\n"},
	} {
		if got := nestRoutes(t, mains...); !slices.Equal(got, want) {
			t.Errorf("%s: routes = %v, want %v", name, got, want)
		}
	}
}
