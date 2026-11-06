package dartextractor

import (
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// This file classifies Flutter's architectural roles. The route, HTTP-client and
// storage surfaces live in navroutes.go, httpclient.go and storage.go; all of them are
// reached through extractFrameworkSurface below.

// extractFrameworkSurface runs the passes that need their own walk over the tree,
// after declarations have been emitted.
//
// Every one of them is gated on this file's imports first. That gate is what makes it
// safe to match on names as short as `go`, `get` and `watch`: a file that has not
// imported go_router cannot be calling go_router's `go`, because Dart has no ambient
// namespace and no unqualified access to a package it did not import.
func (w *walker) extractFrameworkSurface(root *sitter.Node) {
	w.extractNavigationRoutes(root)
	w.extractHTTPClients(root)
	w.extractStorage(root)
}

// flutterBases maps a Flutter/state-management base type to the architectural role it
// gives its subclass.
//
// These are read off the supertype the class already declares, so no extra parsing is
// needed and there is nothing to guess: a type either says `extends StatelessWidget` or
// it does not.
var flutterBases = map[string]string{
	"StatelessWidget":          "widget",
	"StatefulWidget":           "widget",
	"InheritedWidget":          "widget",
	"ImplicitlyAnimatedWidget": "widget",
	"PreferredSizeWidget":      "widget",
	"State":                    "widget_state",
	"ConsumerWidget":           "widget",       // riverpod
	"ConsumerStatefulWidget":   "widget",       // riverpod
	"ConsumerState":            "widget_state", // riverpod
	"HookWidget":               "widget",       // flutter_hooks
	"HookConsumerWidget":       "widget",
	"StatelessElement":         "element",
	"RenderObjectWidget":       "widget",
	"ChangeNotifier":           "view_model",
	"ValueNotifier":            "view_model",
	"Notifier":                 "view_model", // riverpod 2
	"AsyncNotifier":            "view_model",
	"StateNotifier":            "view_model", // riverpod / state_notifier
	"Cubit":                    "view_model", // bloc
	"Bloc":                     "view_model",
	"GetxController":           "view_model", // get
	"WidgetsBindingObserver":   "observer",
	"NavigatorObserver":        "observer",
	"RouteObserver":            "observer",
	"StatefulWidgetBuilder":    "widget",
}

// flutterRole returns the architectural role a type's supertypes imply, or "".
//
// A class conforming to several of these is classified by the FIRST match in declaration
// order, which follows Dart's own ordering rule: `extends` precedes `with` and
// `implements`, so the base class wins over a mixin, which is the right precedence — a
// `StatefulWidget with WidgetsBindingObserver` is a widget that observes, not an
// observer that happens to be a widget.
func flutterRole(supers []string) string {
	for _, s := range supers {
		if role, ok := flutterBases[s]; ok {
			return role
		}
	}
	return ""
}

// isFlutterFile reports whether this file imports Flutter at all. It gates the widget
// classification, so a pure-Dart package declaring a class named `State` (the Dart SDK
// has several) is not labelled a Flutter widget.
func (w *walker) isFlutterFile() bool {
	return w.importsAny("package:flutter/")
}

// importsAny reports whether the file imports a URI with any of the given prefixes.
func (w *walker) importsAny(prefixes ...string) bool {
	for _, uri := range w.importURIs {
		for _, p := range prefixes {
			if len(uri) >= len(p) && uri[:len(p)] == p {
				return true
			}
		}
	}
	return false
}
