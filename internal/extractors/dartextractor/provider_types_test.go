package dartextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestDartProviderReceiversAreTypedByTheirDeclaration(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/state.dart": `class Server { void restart() {} void stop() {} }
 class ServerState {}
 class Transfers { int getStatuses() => 0; }
 class Manager { void sync() {} }
 class Other { void restart() {} }
 final serverProvider = NotifierProvider<Server, ServerState?>((ref) => Server());
 final transfersProvider = ChangeNotifierProvider((ref) => Transfers());
 final managerProvider = Provider<Manager>((ref) { return Manager(); });
 `,
		"lib/use.dart": `import 'state.dart';
 void notifier(Ref ref) { ref.notifier(serverProvider).restart(); }
 void value(Ref ref) { ref.read(transfersProvider).getStatuses(); }
 void plain(WidgetRef ref) { ref.watch(managerProvider).sync(); }
 void viaNotifier(WidgetRef ref) { ref.read(serverProvider.notifier).stop(); }
 void state(Ref ref) { ref.read(serverProvider).restart(); }
 void unknownRef(Foo ref) { ref.read(managerProvider).sync(); }
 void untyped(Ref ref, other) { ref.read(other).sync(); }
 void unknownType(dynamic ref) { ref.read(managerProvider).sync(); }
 void notAProvider(Foo f) { f.read(managerProvider).sync(); }
 `})
	for name, want := range map[string]string{
		"lib.notifier":    "lib.Server.restart",
		"lib.value":       "lib.Transfers.getStatuses",
		"lib.plain":       "lib.Manager.sync",
		"lib.viaNotifier": "lib.Server.stop",
		"lib.unknownType": "lib.Manager.sync",
	} {
		if f := findFact(all, facts.KindSymbol, name); !hasReviewCall(f, want) {
			t.Errorf("%s: want a call to %s: %+v", name, want, f.Relations)
		}
	}
	for _, name := range []string{"lib.state", "lib.unknownRef", "lib.untyped", "lib.notAProvider"} {
		if f := findFact(all, facts.KindSymbol, name); hasReviewCall(f, "lib.Server.restart") || hasReviewCall(f, "lib.Manager.sync") {
			t.Errorf("%s resolved without declared evidence: %+v", name, f.Relations)
		}
	}
}

func TestDartEnumConstantReceiverResolvesItsMembers(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/dialect.dart": `enum Dialect { sqlite, postgres; String escape(String s) => s; }
 class Other { String escape(String s) => s; }
 String use(String name) { return Dialect.sqlite.escape(name); }
 `})
	if f := findFact(all, facts.KindSymbol, "lib.use"); !hasReviewCall(f, "lib.Dialect.escape") {
		t.Errorf("enum constant receiver did not resolve: %+v", f.Relations)
	}
}

func TestDartProviderReadBoundToALocalKeepsItsProvider(t *testing.T) {
	all := dartReviewFacts(t, map[string]string{
		"lib/state.dart": `class History { void addAlbums() {} }
 class Player { void play() {} }
 final historyProvider = Provider((ref) => History(ref));
 final playerProvider = NotifierProvider<Player, int>(() => Player());
 `,
		"lib/use.dart": `import 'state.dart';
 void actions(WidgetRef ref) {
   final history = ref.watch(historyProvider);
   final player = ref.watch(playerProvider.notifier);
   history.addAlbums();
   player.play();
 }
 `})
	f := findFact(all, facts.KindSymbol, "lib.actions")
	for _, want := range []string{"lib.History.addAlbums", "lib.Player.play"} {
		if !hasReviewCall(f, want) {
			t.Errorf("want a call to %s: %+v", want, f.Relations)
		}
	}
}
