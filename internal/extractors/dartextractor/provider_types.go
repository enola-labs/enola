package dartextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// A provider read hands back the provider's value, its notifier, or both under
// different names. These are the shapes whose declaration names the result.
var (
	dartProviderValueArgument    = map[string]int{"Provider": 0, "StateProvider": 0, "ViewProvider": 0, "ChangeNotifierProvider": 0, "NotifierProvider": 1, "StateNotifierProvider": 1, "ReduxProvider": 1}
	dartProviderNotifierArgument = map[string]int{"NotifierProvider": 0, "StateNotifierProvider": 0, "ReduxProvider": 0, "ChangeNotifierProvider": 0}
	dartProviderReceivers        = map[string]bool{"Ref": true, "WidgetRef": true, "ProviderContainer": true}
)

// dartProviderStep splits `read(xProvider)` into its method and provider argument.
func dartProviderStep(step string) (method, provider string, ok bool) {
	open := strings.IndexByte(step, '(')
	if open <= 0 || !strings.HasSuffix(step, ")") || open == len(step)-2 {
		return "", "", false
	}
	method = step[:open]
	if method != "read" && method != "watch" && method != "notifier" {
		return "", "", false
	}
	return method, step[open+1 : len(step)-1], true
}

// providerResult types `ref.read(xProvider)`, `ref.watch(xProvider.notifier)` and
// `ref.notifier(xProvider)` from the provider's declared type arguments.
func (r *dartMemberResolver) providerResult(receiver dartValueType, method, provider string, from *facts.Fact, locals map[string]dartLocalValue, seen map[string]bool) dartValueType {
	// A receiver whose type is not known still counts: the provider it reads is declared
	// here with a provider type, which is the evidence. A receiver known to be
	// something else does not.
	if receiver.Name != "" && (!dartProviderReceivers[dartBaseType(receiver.Name)] || receiver.Owner != nil) {
		return dartValueType{}
	}
	notifier := method == "notifier"
	if name, ok := strings.CutSuffix(provider, ".notifier"); ok {
		provider, notifier = name, true
	}
	decl := r.expressionType(provider, from, locals, seen)
	base := dartBaseType(decl.Name)
	choose := dartProviderValueArgument
	if notifier {
		choose = dartProviderNotifierArgument
	}
	index, ok := choose[base]
	if !ok {
		return dartValueType{}
	}
	args := dartTypeArguments(decl.Name)
	if index >= len(args) || args[index] == "" || args[index] == "dynamic" || r.typeHasParameter(args[index], from) {
		return dartValueType{}
	}
	return dartValueType{Name: args[index], File: decl.File}
}

// enumConstant types `Enum.value` as the enum itself, so its members resolve.
func (r *dartMemberResolver) enumConstant(owner *facts.Fact, name string, receiver dartValueType) dartValueType {
	if owner == nil || !receiver.Namespace || owner.PropString("dart_construct") != "enum" {
		return dartValueType{}
	}
	if r.enumConstants[owner.Name+"."+name] {
		return dartValueType{Name: owner.Name, File: owner.File, Owner: owner}
	}
	return dartValueType{}
}
