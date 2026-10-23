package tsextractor

import (
	"github.com/enola-labs/enola/internal/extractors/tsutil"
)

type tsFrameworks struct {
	isNextJS, isVue, isNuxt, isSvelteKit, isEmber, isReactNav, isAngular bool
	// isReact is a React dependency in package.json, Next.js or not.
	isReact bool
	// appDirs are the packages that are applications whose every use this extractor
	// reads, repo-relative, "" for the repository root: Next.js apps, and React apps
	// with an entry file. The reachability pass gives a verdict only inside them; a
	// library's code is called by whoever consumes it. See reach.go.
	appDirs []string
	orms    ormFlags
	// nextDirs are the packages of a workspace that are Next.js apps, repo-relative,
	// where the repository's own root is not one (see nextJSPackageDirs).
	nextDirs []string
}

type repoCtx struct {
	tsFrameworks
	graphqlServer      graphqlServerContext
	knownFiles         map[string]bool
	files              *tsFileIndex
	nuxtAutoComponents map[string]string
	grpcStubs          *grpcStubIndex
	requestHelpers     *requestHelperIndex // exported request helpers, by file; see importedwrapper.go
}

type callScope struct {
	kinds          *tsutil.KindTable
	src            []byte
	dir, className string
	importMap      map[string]string
	fieldTypes     map[string]string
	memberRoots    map[string]string
	returnTypes    map[string]string
	ioBindings     map[string]bool
	drizzle        bool
	typeNames      *typeNameCache
}

func (c *extractCtx) callScope(kinds *tsutil.KindTable, className string, fieldTypes map[string]string) callScope {
	if c.typeNames == nil {
		c.typeNames = &typeNameCache{}
	}
	return callScope{
		typeNames:   c.typeNames,
		kinds:       kinds,
		src:         c.src,
		dir:         c.dir,
		className:   className,
		importMap:   c.importMap,
		fieldTypes:  fieldTypes,
		memberRoots: c.memberRoots,
		returnTypes: c.returnTypes,
		ioBindings:  c.ioBindings,
		drizzle:     c.orms.drizzle,
	}
}
