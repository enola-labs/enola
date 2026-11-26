package swiftextractor

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// Classes the runtime instantiates by name from a resource, not from source.
//
//   - An Info.plist names the scene delegate, a WatchKit extension delegate, a
//     notification service or an app's principal class:
//     `<string>$(PRODUCT_MODULE_NAME).SceneDelegate</string>`, or a bare class
//     name under a key ending in ClassName or PrincipalClass.
//   - A storyboard or nib names the view controller, view or cell it loads:
//     `customClass="HostingController"`.
//
// Nothing in Swift source names such a class, so it read as unused or test-only.
// A class a resource names is framework_registered: the runtime enters it. Only a
// top-level class of the repository matches, by its simple name; when several
// modules declare one, the ones nearest the resource in the tree do.
//
// The resources are no Swift extractor's own files, so they feed every
// extractor's cache key through the shared hash (engine.computeExtractorKeys).

var (
	plistModuleClass = regexp.MustCompile(`<string>\s*\$\((?:PRODUCT_MODULE_NAME|TARGET_NAME)\)\.([A-Za-z_][A-Za-z0-9_]*)\s*</string>`)
	plistClassKey    = regexp.MustCompile(`<key>[A-Za-z]*(?:ClassName|PrincipalClass)</key>\s*<string>\s*(?:[A-Za-z_][A-Za-z0-9_]*\.)?([A-Za-z_][A-Za-z0-9_]*)\s*</string>`)
	ibCustomClass    = regexp.MustCompile(`\bcustomClass="([A-Za-z_][A-Za-z0-9_]*)"`)
)

// isResourceNamingClasses reports a file that can name a class for the runtime.
func isResourceNamingClasses(relFile string) bool {
	switch strings.ToLower(factpath.Ext(relFile)) {
	case ".plist", ".storyboard", ".xib":
		return true
	}
	return false
}

// resourceClassNames returns, per simple class name, the resources that name it.
func resourceClassNames(repoPath string, files []string) map[string][]string {
	out := map[string][]string{}
	for _, rel := range files {
		if !isResourceNamingClasses(rel) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		var res []*regexp.Regexp
		if strings.EqualFold(factpath.Ext(rel), ".plist") {
			res = []*regexp.Regexp{plistModuleClass, plistClassKey}
		} else {
			res = []*regexp.Regexp{ibCustomClass}
		}
		for _, re := range res {
			for _, m := range re.FindAllSubmatch(src, -1) {
				name := string(m[1])
				if !containsString(out[name], rel) {
					out[name] = append(out[name], rel)
				}
			}
		}
	}
	return out
}

// markResourceClasses sets framework_registered on the top-level classes the
// repository's resources name.
func markResourceClasses(allFacts []facts.Fact, named map[string][]string) {
	if len(named) == 0 {
		return
	}
	cands := map[string][]int{} // simple name -> top-level class facts
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		if sk, _ := f.PropAny("symbol_kind").(string); sk != facts.SymbolClass {
			continue
		}
		owner := ownerOf(f.Name)
		if !f.HasRelation(facts.RelDeclares, owner) {
			continue // nested: the runtime name of a nested class is mangled
		}
		short := lastDotComponent(f.Name)
		if len(named[short]) > 0 {
			cands[short] = append(cands[short], i)
		}
	}
	names := make([]string, 0, len(cands))
	for n := range cands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		idx := cands[n]
		for _, res := range named[n] {
			best, nearest := -1, []int(nil)
			for _, i := range idx {
				d := commonDirDepth(res, allFacts[i].File)
				switch {
				case d > best:
					best, nearest = d, []int{i}
				case d == best:
					nearest = append(nearest, i)
				}
			}
			for _, i := range nearest {
				allFacts[i].SetProp("framework_registered", true)
			}
		}
	}
}

// commonDirDepth is the number of leading directories two repository paths share.
func commonDirDepth(a, b string) int {
	da := strings.Split(factpath.Dir(a), "/")
	db := strings.Split(factpath.Dir(b), "/")
	n := 0
	for n < len(da) && n < len(db) && da[n] == db[n] && da[n] != "." {
		n++
	}
	return n
}
