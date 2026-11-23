package orphans

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// discoveredPolicies are the policies Laravel's gate finds for a model by name,
// with no registration: for `App\Models\Song` it tries `App\Models\Policies\SongPolicy`
// and then `App\Policies\SongPolicy`, a Policies namespace under each enclosing
// namespace of the model. The model must be one, so a class merely named like a
// policy is still judged by its references.
func (x *laravelIndex) discoveredPolicies() map[string]bool {
	out := map[string]bool{}
	models := map[string][]string{} // short name -> model namespaces
	for name := range x.types {
		if x.is(name, laravelModel) {
			ns, short := splitClass(name)
			models[short] = append(models[short], ns)
		}
	}
	for name, t := range x.types {
		if t.abstract {
			continue
		}
		ns, short := splitClass(name)
		stem, ok := strings.CutSuffix(short, "Policy")
		if !ok || stem == "" {
			continue
		}
		base, ok := strings.CutSuffix(ns, `\Policies`)
		if !ok {
			continue
		}
		for _, modelNS := range models[stem] {
			if modelNS == base || strings.HasPrefix(modelNS, base+`\`) {
				out[name] = true
				break
			}
		}
	}
	return out
}

// flarumCommandHandlers are the handlers Flarum's command bus resolves by name: a
// dispatched `Foo\Command\DoThing` runs `Foo\Command\DoThingHandler`. A handler is
// rooted only in code built on Flarum, when its handle() takes the command and the
// command is built somewhere, which is how it is dispatched.
func (x *laravelIndex) flarumCommandHandlers(symbols, fileRefs []facts.Fact) map[string]bool {
	if !x.buildsOnFlarum() {
		return nil
	}
	built := map[string]bool{}
	handles := map[string][]string{} // handler class -> classes its handle() names
	for _, list := range [][]facts.Fact{symbols, fileRefs} {
		for _, f := range list {
			if f.PropString(propLanguage) != "php" {
				continue
			}
			owner, method, isMember := strings.Cut(f.Name, "::")
			for _, r := range f.Relations {
				switch {
				case r.Kind == facts.RelInstantiates:
					built[r.Target] = true
				case r.Kind == facts.RelNames && isMember && method == "handle":
					handles[owner] = append(handles[owner], r.Target)
				}
			}
		}
	}
	out := map[string]bool{}
	for name, t := range x.types {
		command, ok := strings.CutSuffix(name, "Handler")
		if !ok || t.abstract || x.types[command] == nil || !built[command] {
			continue
		}
		for _, named := range handles[name] {
			if named == command {
				out[name] = true
				break
			}
		}
	}
	return out
}

// buildsOnFlarum reports whether the application is Flarum or an extension of it:
// it declares a type in the Flarum namespace or builds one on a Flarum type.
func (x *laravelIndex) buildsOnFlarum() bool {
	for name, t := range x.types {
		if strings.HasPrefix(name, `Flarum\`) {
			return true
		}
		for _, p := range t.parents {
			if strings.HasPrefix(p, `Flarum\`) {
				return true
			}
		}
	}
	return false
}

// splitClass splits a PHP class name into its namespace and short name.
func splitClass(name string) (string, string) {
	i := strings.LastIndex(name, `\`)
	if i < 0 {
		return "", name
	}
	return name[:i], name[i+1:]
}
