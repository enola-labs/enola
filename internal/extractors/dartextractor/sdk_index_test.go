package dartextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestDartBundledSDKSignatures(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/repo.dart": ` class Repo {void load() {}}
 void first(List<Repo> xs) {xs.first.load();}
 void mapped(List<Repo> xs) {xs.map((x) => x).toList();}
 Future<void> awaited(Future<Repo> f) async {final r = await f; r.load();}
 void keyed(Map<String, Repo> m) {m.values.first.load();}
 void text(String s) {s.trim().toLowerCase();}
 class List2 {void add(int x) {}}
 void shadow(List2 l) {l.add(1);}
 `})
	for name, want := range map[string]string{
		"lib.first":   "sdk/lib/core.Iterable.first",
		"lib.awaited": "lib.Repo.load",
		"lib.mapped":  "sdk/lib/core.Iterable.map",
		"lib.text":    "sdk/lib/core.String.toLowerCase",
		"lib.shadow":  "lib.List2.add",
	} {
		if f := findFact(all, facts.KindSymbol, name); !hasReviewCall(f, want) {
			t.Errorf("%s: want call to %s: %+v", name, want, f)
		}
	}
	for _, f := range all {
		if f.File == "sdk/lib/core/core.dart" {
			t.Fatalf("bundled SDK fact leaked into the output: %s", f.Name)
		}
	}
}

func TestDartBundledFlutterSignatures(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/page.dart": `import 'package:flutter/material.dart';
 class Page extends StatefulWidget {const Page({super.key}); State<Page> createState() => PageState();}
 class PageState extends State<Page> {
   void tap() {setState(() {});}
   void leave(BuildContext context) {Navigator.of(context).pop();}
   void theme(BuildContext context) {Theme.of(context).copyWith();}
   void dep() {didChangeDependencies();}
 }
 `,
		"lib/mine.dart": `class Navigator { static void pushIt() {} }`,
		"lib/own.dart": `import 'package:flutter/material.dart';
 import 'mine.dart';
 void own() {Navigator.pushIt();}
 `})
	f := findFact(all, facts.KindSymbol, "lib.PageState.tap")
	if !hasReviewCall(f, "packages/flutter/lib/src/widgets.State.setState") {
		t.Errorf("setState did not resolve: %+v", f)
	}
	if f := findFact(all, facts.KindSymbol, "lib.PageState.leave"); !hasReviewCall(f, "packages/flutter/lib/src/widgets.Navigator.of") {
		t.Errorf("Navigator.of did not resolve: %+v", f)
	}
	if f := findFact(all, facts.KindSymbol, "lib.own"); !hasReviewCall(f, "lib.Navigator.pushIt") {
		t.Errorf("a repository type must win over the bundled one: %+v", f)
	}
	for _, f := range all {
		if f.File == "packages/flutter/lib/widgets.dart" {
			t.Fatalf("bundled Flutter fact leaked into the output: %s", f.Name)
		}
	}
}

func TestDartExtensionOnBundledStringStaysApplicable(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/ext.dart": `extension Hex on String { String combine(String o) => o; }`,
		"lib/use.dart": `import 'package:flutter/material.dart';
 import 'ext.dart';
 class W { final TextEditingController c = TextEditingController(); String? validate(String? v, String o) { if (v == null || v.isEmpty) { return null; } c.text.combine(o); return v.combine(o); } }
 String? validate2(String? v, String o) { if (v == null || v.isEmpty) { return null; } final r = int.tryParse(v.combine(o)); return null; }
 `})
	if f := findFact(all, facts.KindSymbol, "lib.validate2"); !hasReviewCall(f, "lib.Hex.combine") {
		t.Errorf("extension method lost: %+v", f.Relations)
	}
}
