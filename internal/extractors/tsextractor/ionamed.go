package tsextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// propIONamedCalls lists, on a function, the resolved calls that are I/O by their
// method name alone (tsIOMemberMethods). It carries them from the body walk to
// settleIONamedCalls, which decides them and removes it.
const propIONamedCalls = "io_named_calls"

// tsORMPackages are the packages whose findOne, findMany and the rest are a
// database round trip. An entry ending in "/" is an npm scope.
var tsORMPackages = []string{
	"@prisma/client", "prisma", "typeorm", "@mikro-orm/", "drizzle-orm",
	"sequelize", "sequelize-typescript", "mongoose", "mongodb", "knex",
	"objection", "kysely", "@nestjs/typeorm", "@nestjs/mongoose", "@nestjs/sequelize",
}

// settleIONamedCalls marks io_direct the functions whose body makes a call that is
// I/O by its method name and resolved to a method of an ORM package
// (typeorm.Repository.findOne) or of this repository. A call resolved into any other
// package is not I/O: svg.js's Element.findOne is a DOM query.
//
// A method this repository declares keeps the name as evidence, and is not left to
// the closure to decide from its body: the body usually reaches I/O through a helper
// the closure does not know (`static deleteMany(ids) { return ajax(...) }`).
func settleIONamedCalls(all []facts.Fact, modules map[string]bool) {
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol || f.Props == nil {
			continue
		}
		calls, _ := f.Props[propIONamedCalls].([]string)
		delete(f.Props, propIONamedCalls)
		for _, target := range calls {
			if tsIsORMTarget(target) || tsInRepoTarget(target, modules) {
				f.SetProp("io_direct", true)
				break
			}
		}
	}
}

func tsIsORMTarget(target string) bool {
	for _, p := range tsORMPackages {
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(target, p) {
				return true
			}
		} else if strings.HasPrefix(target, p+".") || strings.HasPrefix(target, p+"/") {
			return true
		}
	}
	return false
}

// tsInRepoTarget reports whether a call target is named under one of this
// repository's modules. A module path may itself hold dots, so every prefix
// ending before a dot is tried.
func tsInRepoTarget(target string, modules map[string]bool) bool {
	for i := strings.IndexByte(target, '.'); i > 0; {
		if modules[target[:i]] {
			return true
		}
		next := strings.IndexByte(target[i+1:], '.')
		if next < 0 {
			break
		}
		i += 1 + next
	}
	return false
}
