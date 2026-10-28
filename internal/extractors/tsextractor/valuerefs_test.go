package tsextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestValueRefs_ModuleValuesAreNamed(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/validators.ts": `export const isValid = (v: string) => v.length > 0
export function shout(s: string) { return s }`,
		"src/machines/accounts.ts": `import { isValid, shout } from "../validators"
const listQuery = gql` + "`query List { list { id } }`" + `
type Q = typeof listQuery
export const machine = factory("accounts").withConfig({
  services: {
    fetchData: async () => post({ query: listQuery.loc?.source.body }),
    shadowed: async (listQuery: string) => post({ query: listQuery }),
    keyed: async () => ({ listQuery: 1, isValid: 2 }),
  },
})
export const rules = () => ({ name: { isValid } })
const ruleSet = { email: { required: isValid, shout }, list: [listQuery], check(v: string) { return isValid(v) } }
export function run(): Q { return shout("x") }`,
	}, false)

	fetch, _ := findFact(ff, "src/machines.machine.services.fetchData")
	if !hasRelation(fetch, facts.RelNames, "src/machines.listQuery") {
		t.Errorf("fetchData does not name the query it sends: %v", fetch.Relations)
	}
	machine, _ := findFact(ff, "src/machines.machine")
	for _, m := range []string{"fetchData", "shadowed", "keyed"} {
		if !hasRelation(machine, facts.RelHasMethod, "src/machines.machine.services."+m) {
			t.Errorf("machine does not hold its member %s: %v", m, machine.Relations)
		}
	}
	rules, _ := findFact(ff, "src/machines.rules")
	if !hasRelation(rules, facts.RelNames, "src.isValid") {
		t.Errorf("rules does not name the imported validator it lists: %v", rules.Relations)
	}

	ruleSet, _ := findFact(ff, "src/machines.ruleSet")
	for _, want := range []string{"src.isValid", "src.shout", "src/machines.listQuery"} {
		if !hasRelation(ruleSet, facts.RelNames, want) {
			t.Errorf("a data object does not name %s, which it lists: %v", want, ruleSet.Relations)
		}
	}
	for name, not := range map[string][]string{
		"src/machines.machine.services.shadowed": {"src/machines.listQuery"},                // a parameter shadows it
		"src/machines.machine.services.keyed":    {"src/machines.listQuery", "src.isValid"}, // property keys
		"src/machines.run":                       {"src/machines.listQuery"},                // a type position
	} {
		f, ok := findFact(ff, name)
		if !ok {
			t.Fatalf("no symbol %s", name)
		}
		for _, target := range not {
			if hasRelation(f, facts.RelNames, target) {
				t.Errorf("%s names %s: %v", name, target, f.Relations)
			}
		}
	}
	run, _ := findFact(ff, "src/machines.run")
	if hasRelation(run, facts.RelNames, "src.shout") || !hasRelation(run, facts.RelCalls, "src.shout") {
		t.Errorf("a call stays a call: %v", run.Relations)
	}
}

// An Options API component's options object is no symbol; the values it lists are
// the file's references.
func TestValueRefs_VueOptionsObject(t *testing.T) {
	ff := extractVue(t, map[string]string{
		"src/helpers/validators.ts": `export const isPhone = (v: string) => true`,
		"src/pages/Phone.vue": `<script>
import { isPhone } from '../helpers/validators'
export default {
  validations: { phone: { isPhone } },
}
</script><template><div /></template>`,
	}, false)
	for _, f := range ff {
		if f.Kind == facts.KindFileRef && f.File == "src/pages/Phone.vue" && hasRelation(f, facts.RelNames, "src/helpers.isPhone") {
			return
		}
	}
	t.Errorf("the options object's validator is not named by the file")
}
