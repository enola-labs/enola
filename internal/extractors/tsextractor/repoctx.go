package tsextractor

import (
	"github.com/enola-labs/enola/internal/extractors/tsutil"
)

type tsFrameworks struct {
	isNextJS, isVue, isNuxt, isSvelteKit, isEmber, isReactNav, isAngular bool
	orms                                                                 ormFlags
}

type repoCtx struct {
	tsFrameworks
	graphqlServer      graphqlServerContext
	knownFiles         map[string]bool
	files              *tsFileIndex
	nuxtAutoComponents map[string]string
	grpcStubs          *grpcStubIndex
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
}

func (c *extractCtx) callScope(kinds *tsutil.KindTable, className string, fieldTypes map[string]string) callScope {
	return callScope{
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
