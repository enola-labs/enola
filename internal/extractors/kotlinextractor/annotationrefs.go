package kotlinextractor

import (
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// An annotation names its type the way any type reference does, and an annotation
// class used only as `@ThemePreviews` has no other use: without an edge from what it
// annotates it reads as dead. The annotation's type is recorded as a value note on
// the declaration it annotates, so resolveValueRefs keeps it as a names edge only
// where the repository declares the type, through an import or the file's own
// directory. An annotation imported from outside the repository resolves to nothing.

// kotlinDefaultAnnotations are the annotations Kotlin imports by default (kotlin,
// kotlin.annotation, kotlin.jvm). Unless a file imports the name explicitly, they are
// never looked up in the repository: binding @Deprecated to a repository type that
// happens to share the name would make it look used by every deprecation.
var kotlinDefaultAnnotations = map[string]bool{
	"Deprecated": true, "DeprecatedSinceKotlin": true, "ReplaceWith": true,
	"Suppress": true, "DslMarker": true, "PublishedApi": true, "OptIn": true,
	"RequiresOptIn": true, "SinceKotlin": true, "BuilderInference": true,
	"OverloadResolutionByLambdaReturnType": true, "UnsafeVariance": true,
	"ExtensionFunctionType": true, "ParameterName": true, "Throws": true,
	"ExperimentalStdlibApi": true, "ExperimentalUnsignedTypes": true,
	"ExperimentalMultiplatform": true, "ExperimentalSubclassOptIn": true,
	"SubclassOptInRequired": true, "ConsistentCopyVisibility": true,
	"ExposedCopyVisibility": true, "Target": true, "Retention": true,
	"Repeatable": true, "MustBeDocumented": true,
	"JvmStatic": true, "JvmName": true, "JvmField": true, "JvmOverloads": true,
	"JvmSuppressWildcards": true, "JvmWildcard": true, "JvmInline": true,
	"JvmRecord": true, "JvmDefault": true, "JvmDefaultWithCompatibility": true,
	"JvmDefaultWithoutCompatibility": true, "JvmMultifileClass": true,
	"JvmSynthetic": true, "JvmSerializableLambda": true, "Synchronized": true,
	"Volatile": true, "Transient": true, "Strictfp": true,
}

// noteAnnotation records what one annotation names against the current owner: its
// type and the types its arguments name.
func (w *astWalker) noteAnnotation(ann *sitter.Node) {
	if ut := annotationType(ann); ut != nil {
		w.noteValueRef(ut)
	}
	w.noteAnnotationArgs(ann)
}

// annotationType is the user type an annotation applies: `@A`, `@A(…)`, `@get:A`.
func annotationType(ann *sitter.Node) *sitter.Node {
	if inv := findChildByKind(ann, "constructor_invocation"); inv != nil {
		return findChildByKind(inv, "user_type")
	}
	return findChildByKind(ann, "user_type")
}

// isAnnotationType reports whether ut is the type an annotation applies, wherever
// the walk meets it: on a declaration, a parameter or a property.
func isAnnotationType(ut *sitter.Node) bool {
	p := ut.Parent()
	if p != nil && kindOf(p) == "constructor_invocation" {
		p = p.Parent()
	}
	return p != nil && kindOf(p) == "annotation"
}

// defaultAnnotation reports whether an annotation's written path names one Kotlin
// imports by default, which the file does not import itself.
func (w *astWalker) defaultAnnotation(segs []string) bool {
	if len(segs) != 1 || !kotlinDefaultAnnotations[segs[0]] {
		return false
	}
	_, imported := w.importMap[segs[0]]
	return !imported
}
