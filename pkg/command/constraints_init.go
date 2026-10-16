package command

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/intent"
	"github.com/enola-labs/enola/pkg/check"
)

// roleDirectories is where a Rails-shaped repository keeps each role a
// shipped recipe declares. init binds a role only when its directory exists,
// so the written declaration claims nothing about code the tree does not
// have; a role with no conventional directory is left for the author.
var roleDirectories = map[string][]string{
	"controllers":        {"app/controllers"},
	"jobs":               {"app/jobs", "app/workers"},
	"models":             {"app/models"},
	"mailers":            {"app/mailers"},
	"policies":           {"app/policies"},
	"serializers":        {"app/serializers"},
	"view-components":    {"app/components"},
	"helpers":            {"app/helpers"},
	"services":           {"app/services"},
	"forms":              {"app/forms"},
	"decorators":         {"app/decorators"},
	"presenters":         {"app/presenters"},
	"presentation":       {"app/controllers", "app/views", "app/components"},
	"application":        {"app/services", "app/use_cases", "app/jobs"},
	"domain":             {"app/domain", "app/models"},
	"infrastructure":     {"app/adapters", "app/infrastructure", "lib"},
	"core":               {"lib", "app/domain"},
	"ports":              {"app/ports"},
	"adapters":           {"app/adapters", "app/integrations", "app/infrastructure"},
	"frameworks":         {"app/controllers", "app/jobs", "app/mailers"},
	"interface-adapters": {"app/adapters", "app/presenters", "app/serializers"},
	"use-cases":          {"app/use_cases", "app/services"},
	"entities":           {"app/entities", "app/domain", "app/models"},
	"commands":           {"app/commands"},
	"queries":            {"app/queries"},
	"read-models":        {"app/read_models"},
	"events":             {"app/events"},
	"publishers":         {"app/publishers"},
	"handlers":           {"app/subscribers", "app/handlers", "app/listeners"},
	"module-public":      {"app/public", "packs"},
	"module-internal":    {"app/internal"},
	"other-modules":      {"packs"},
	"code":               {"app", "lib"},
}

const initFileName = "recipes.yaml"

// ConstraintsInit is `enola constraints init [repo_path]`: a first
// declaration in one command. It binds every shipped recipe whose roles
// resolve to directories the repository has and writes one use_recipe per
// recipe to enola/constraints/recipes.yaml, refusing to overwrite, and prints
// what it bound and what it left for the author.
func (r *Runner) ConstraintsInit(args []string) {
	fs := flag.NewFlagSet("constraints init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	only := fs.String("recipe", "", "bind only this shipped recipe (default: every recipe with at least one bound role)")
	dry := fs.Bool("dry-run", false, "print the declaration instead of writing it")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: "+r.name()+" constraints init [--recipe NAME] [--dry-run] [repo_path]\n\n"+
			"Writes a first declaration under enola/constraints/ binding every shipped recipe\n"+
			"whose roles resolve to directories the repository has. Nothing is guessed: a role\n"+
			"with no directory is left unbound and named in the output.\n")
		fs.PrintDefaults()
		r.writeHelpFooter(fs.Output(), args, "constraints init")
	}
	if err := fs.Parse(args); err != nil {
		os.Exit(check.StatusUsageError.ExitCode())
	}
	repoPath := "."
	if fs.NArg() > 0 {
		repoPath = fs.Arg(0)
	}
	repoPath, err := filepath.Abs(repoPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(check.StatusUsageError.ExitCode())
	}
	body, report, err := initDeclaration(repoPath, *only)
	if err != nil {
		fmt.Fprintln(os.Stderr, "constraints init:", err)
		os.Exit(check.StatusUsageError.ExitCode())
	}
	fmt.Print(report)
	if body == "" {
		fmt.Println("nothing to bind: no shipped recipe's roles resolve to a directory here")
		return
	}
	if *dry {
		fmt.Println()
		fmt.Print(body)
		return
	}
	target := filepath.Join(repoPath, intent.ConstraintsDirName, initFileName)
	if _, err := os.Stat(target); err == nil {
		fmt.Fprintf(os.Stderr, "constraints init: %s exists; this command never overwrites a declaration\n", target)
		os.Exit(check.StatusUsageError.ExitCode())
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.WriteFile(target, []byte(body), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("\nwrote %s; run `%s constraints lint %s` to see what each part resolves to\n", target, r.name(), repoPath)
}

// initDeclaration builds the YAML and a human report. Exposed for the test
// and for the dry run; the writer is ConstraintsInit.
func initDeclaration(repoPath, only string) (string, string, error) {
	recipes, problems := intent.BuiltinRecipes()
	if len(problems) > 0 {
		return "", "", fmt.Errorf("shipped recipes do not load: %s", strings.Join(problems, "; "))
	}
	sort.Slice(recipes, func(i, j int) bool { return recipes[i].Name < recipes[j].Name })
	var body, report strings.Builder
	body.WriteString("use_recipe:\n")
	bound := 0
	for _, rec := range recipes {
		if only != "" && rec.Name != only {
			continue
		}
		var lines []string
		var unbound []string
		var missing []string
		required := map[string]bool{}
		for _, role := range intent.RequiredRoles(rec) {
			required[role] = true
		}
		for _, role := range rec.Roles {
			dir := existingDirectory(repoPath, roleDirectories[role.Name])
			if dir == "" {
				if required[role.Name] {
					missing = append(missing, role.Name)
				} else {
					unbound = append(unbound, role.Name)
				}
				continue
			}
			lines = append(lines, fmt.Sprintf("      %s: { match: [%q] }", role.Name, dir+"/**"))
		}
		// A recipe is bound only when every role its rules need has a
		// directory; binding half a recipe would make the written file refuse
		// to load, and guessing a directory would claim code the tree lacks.
		if len(missing) > 0 {
			fmt.Fprintf(&report, "%-20s not bound: no directory for %s\n", rec.Name, strings.Join(missing, ", "))
			continue
		}
		if len(lines) == 0 {
			fmt.Fprintf(&report, "%-20s no role resolves to a directory here; not bound\n", rec.Name)
			continue
		}
		bound++
		fmt.Fprintf(&body, "  - recipe: %s\n    as: %s\n    bind:\n%s\n", rec.Name, instanceName(rec.Name), strings.Join(lines, "\n"))
		fmt.Fprintf(&report, "%-20s bound %d role(s)", rec.Name, len(lines))
		if len(unbound) > 0 {
			fmt.Fprintf(&report, "; optional, left for the author: %s", strings.Join(unbound, ", "))
		}
		report.WriteString("\n")
	}
	if bound == 0 {
		return "", report.String(), nil
	}
	return body.String(), report.String(), nil
}

func existingDirectory(repoPath string, candidates []string) string {
	for _, c := range candidates {
		if info, err := os.Stat(filepath.Join(repoPath, c)); err == nil && info.IsDir() {
			return c
		}
	}
	return ""
}

// instanceName is the recipe's name as a lowercase token, which is what an
// instantiation's rules are prefixed with.
func instanceName(recipe string) string {
	return strings.ReplaceAll(strings.ToLower(recipe), "_", "-")
}
