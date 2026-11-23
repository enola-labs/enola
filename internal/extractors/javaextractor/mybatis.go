package javaextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// MyBatis mappers. A mapper is an interface whose abstract methods are
// statements MyBatis runs: every call to one is a round trip, as a Spring Data
// repository's is. Two things register one, and neither is in the interface's
// method names:
//
//   - MyBatis's own @Mapper on the interface. MapStruct has an annotation of the
//     same simple name that generates a bean converter and runs nothing, so the
//     annotation counts only when the file imports it from MyBatis.
//   - A @MapperScan on any class of the repository, naming the packages whose
//     interfaces Spring registers as mappers, recursively. That is a fact of
//     another file, so the walker records the packages and the abstract
//     methods, and markMyBatisMappers decides once every file is walked.
//
// The walker's records are scratch props, removed here before the facts leave
// the extractor.

const (
	// propMapperScan carries, on the class that declares a @MapperScan, the
	// packages it names.
	propMapperScan = "mybatis_mapper_scan"
	// propMyBatisMapper marks an interface annotated with MyBatis's @Mapper.
	propMyBatisMapper = "mybatis_mapper"
	// propAbstractDecl marks an interface method with no body: the only kind a
	// mapper turns into a statement.
	propAbstractDecl = "abstract_decl"
)

// myBatisMapperScanTypes are the @MapperScan annotations that register mapper
// interfaces: MyBatis-Spring's and the tk.mybatis fork's.
var myBatisMapperScanTypes = []string{
	"org.mybatis.spring.annotation.MapperScan",
	"tk.mybatis.spring.annotation.MapperScan",
}

// importedAs reports whether the annotation's simple name resolves, through the
// file's imports, to one of the given fully-qualified types.
func (w *astWalker) importedAs(simple string, fqns ...string) bool {
	for _, fqn := range fqns {
		if w.importMap[simple] == fqn {
			return true
		}
		dot := strings.LastIndexByte(fqn, '.')
		if fqn[dot+1:] != simple {
			continue
		}
		if _, named := w.importMap[simple]; named {
			continue // a single-type import shadows every on-demand one
		}
		for _, wc := range w.wildcards {
			if wc == fqn[:dot] {
				return true
			}
		}
	}
	return false
}

// recordMyBatis notes on a type's fact what markMyBatisMappers needs from this
// file: whether it is an interface MyBatis's @Mapper marks, and the packages a
// @MapperScan on it names.
func (w *astWalker) recordMyBatis(f *facts.Fact, kind string, annotations []javaAnnotation) {
	if kind == facts.SymbolInterface && hasAnnotation(annotations, "Mapper") &&
		w.importedAs("Mapper", "org.apache.ibatis.annotations.Mapper") {
		f.SetProp(propMyBatisMapper, true)
	}
	scan := findAnnotation(annotations, "MapperScan")
	if scan == nil || !w.importedAs("MapperScan", myBatisMapperScanTypes...) {
		return
	}
	var pkgs []string
	for _, v := range append(append([]string{}, scan.positional...), scan.named["value"], scan.named["basePackages"]) {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			// A placeholder or an Ant pattern names no package this reading can
			// match; it is left out rather than guessed.
			if p == "" || strings.ContainsAny(p, "${}*?") {
				continue
			}
			pkgs = append(pkgs, p)
		}
	}
	if len(pkgs) > 0 {
		f.SetProp(propMapperScan, pkgs)
	}
}

// markMyBatisMappers makes every mapper interface an io_type and its abstract
// methods io_direct, then drops the walker's scratch props. It runs before the
// typed calls are resolved (they read io_type for a method inherited from a
// library base) and before I/O is propagated to callers.
func markMyBatisMappers(all []facts.Fact) {
	var scanned []string
	for i := range all {
		if pkgs, ok := all[i].PropAny(propMapperScan).([]string); ok {
			scanned = append(scanned, pkgs...)
			all[i].DelProp(propMapperScan)
		}
	}
	inScan := func(fqn string) bool {
		for _, p := range scanned {
			if strings.HasPrefix(fqn, p+".") {
				return true
			}
		}
		return false
	}
	mappers := make(map[string]bool)
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		marked, _ := f.PropAny(propMyBatisMapper).(bool)
		f.DelProp(propMyBatisMapper)
		if k, _ := f.PropAny("symbol_kind").(string); k != facts.SymbolInterface {
			continue
		}
		fqn, _ := f.PropAny("fqn").(string)
		if marked || (fqn != "" && inScan(fqn)) {
			mappers[f.Name] = true
			f.SetProp(propIOType, true)
		}
	}
	for i := range all {
		f := &all[i]
		abstract, _ := f.PropAny(propAbstractDecl).(bool)
		if !abstract {
			continue
		}
		f.DelProp(propAbstractDecl)
		if dot := strings.LastIndexByte(f.Name, '.'); dot > 0 && mappers[f.Name[:dot]] {
			f.SetProp("io_direct", true)
			f.SetProp("performs_io", true)
		}
	}
}
