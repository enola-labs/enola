// Package factpath holds the path operations for REPO-RELATIVE FACT PATHS:
// the `src/lib/site-blocks.ts` that lands in Fact.File, the `src/lib` that
// names a module, the import target an extractor resolves. Every one of them
// is forward-slash on every host, and this package is what keeps that true.
//
// The distinction it draws is not stylistic. `path/filepath` is the HOST
// filesystem's dialect, and on Windows its output carries backslashes:
// filepath.Dir("src/lib/x.ts") returns `src\lib` there, because Clean ends in
// FromSlash. Feed that to a module name and every consumer downstream — layer
// classification, module resolution, the declaration dialect, the ignore
// globs, the cross-repo linker — silently stops matching, because all of them
// split on "/". That is exactly how a declared layer order came to classify
// 0 modules on Windows while reporting itself valid (issue #242): nothing
// errored, the paths simply stopped being the same strings.
//
// filepath's INPUT is not the problem — on Windows it accepts "/" and "\"
// alike, which is why filepath.Base and filepath.Ext are safe on either
// dialect and are not restated here. It is the output that must be pinned, so
// this package covers exactly the operations that BUILD a path: Dir, Clean,
// Join, Split, and Match (whose separator semantics are host-dependent too).
//
// Use `path/filepath` when constructing a path to hand to the operating
// system — os.ReadFile(filepath.Join(repoPath, relFile)) is correct and stays.
// Use this package for anything that becomes, or is compared against, a fact.
// internal/facts' contract test enforces the split.
package factpath

import (
	"path"
	"path/filepath"
	"strings"
)

// Every function here normalises its INPUT before operating on it, which makes
// them total: whatever dialect a caller has, the answer is in fact-path form.
// That matters because the slash-only operations are not merely wrong on a
// backslash path, they are wrong QUIETLY — path.Dir(`src\lib\x.ts`) finds no
// separator and answers ".", collapsing a module to the repo root rather than
// erroring. A caller that has not yet been normalised should get the right
// answer, not a subtler bug than the one this package replaced.
//
// The normalisation is free on Unix: filepath.ToSlash returns its argument
// unchanged wherever the separator already is "/".

// Dir is path.Dir over a fact path: its slash-separated directory.
func Dir(p string) string { return path.Dir(Slash(p)) }

// Base is path.Base. Provided for symmetry with Dir at call sites that read
// better paired; filepath.Base is equally correct on both dialects.
func Base(p string) string { return path.Base(Slash(p)) }

// Ext is path.Ext. As with Base, filepath.Ext is equally safe — this exists so
// a call site that has already reached for this package need not mix the two.
func Ext(p string) string { return path.Ext(Slash(p)) }

// Clean is path.Clean: lexical cleaning that resolves "." and ".." without
// ever emitting a host separator.
func Clean(p string) string { return path.Clean(Slash(p)) }

// Join is path.Join: joins fact-path elements with "/" and cleans the result.
func Join(elem ...string) string {
	slashed := make([]string, len(elem))
	for i, e := range elem {
		slashed[i] = Slash(e)
	}
	return path.Join(slashed...)
}

// Match is path.Match: glob matching where "/" is the separator on every host.
// filepath.Match would treat "\" as the separator on Windows, so `src/*` would
// match across a directory boundary there and nowhere else.
//
// The pattern is normalised alongside the name, so an ignore or test glob a
// user wrote with backslashes still selects what they meant.
func Match(pattern, name string) (bool, error) { return path.Match(Slash(pattern), Slash(name)) }

// Slash converts a HOST path to fact-path form. This is the boundary call: use
// it immediately on anything filepath.Rel or filepath.Walk hands back, so the
// host dialect never travels further than the line that produced it.
//
// It is host-conditional, and deliberately so: on Unix it is the identity, which
// is what keeps a file legitimately NAMED `weird\name.ts` — legal on every Unix
// filesystem — from being rewritten into a directory it does not live in. Use
// Declared for text a person wrote, where the same reasoning inverts.
func Slash(p string) string { return filepath.ToSlash(p) }

// Declared converts an AUTHORED path — one written by a person in a declaration,
// config or glob — to fact-path form, on every host.
//
// The difference from Slash is the whole point. A declaration is portable source
// text: the same enola-intent.yaml is read on the Windows laptop that wrote
// `src\lib\**` and on the Linux runner that gates the pull request, and it has to
// select the same code in both. Slash cannot do that job, because it is a no-op
// wherever the separator already is "/" — it would normalise that declaration on
// Windows and reject it in CI, which is worse than either answer alone.
//
// The unconditional rewrite is safe here for the reason it is unsafe for a real
// path: a backslash in a declared path can only mean a directory separator. No
// author writes one to name a file that has a backslash in it, and the dialects
// this feeds — layer paths, component match globs — do not accept such a name
// anyway.
func Declared(p string) string { return strings.ReplaceAll(p, `\`, "/") }
