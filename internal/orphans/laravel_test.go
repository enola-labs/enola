package orphans

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// laravelStore builds PHP symbols the way the PHP extractor emits them: a parent
// class, an implemented interface and a used trait are each an implements edge.
type laravelStore struct{ facts []facts.Fact }

func (s *laravelStore) class(name, file string, rels ...facts.Relation) {
	s.facts = append(s.facts, facts.Fact{Kind: facts.KindSymbol, Name: name, File: file,
		Props:     map[string]any{"symbol_kind": facts.SymbolClass, "language": "php", "exported": true},
		Relations: append([]facts.Relation{{Kind: facts.RelDeclares, Target: dirOf(file)}}, rels...)})
}

func (s *laravelStore) method(name, file string, rels ...facts.Relation) {
	s.facts = append(s.facts, facts.Fact{Kind: facts.KindSymbol, Name: name, File: file,
		Props:     map[string]any{"symbol_kind": facts.SymbolMethod, "language": "php", "exported": true},
		Relations: append([]facts.Relation{{Kind: facts.RelDeclares, Target: dirOf(file)}}, rels...)})
}

func (s *laravelStore) reported() map[string]bool {
	st := facts.NewStore()
	st.Add(s.facts...)
	st.BuildGraph()
	out := map[string]bool{}
	for _, o := range Detect(st) {
		out[o.Name] = true
	}
	return out
}

func dirOf(file string) string {
	for i := len(file) - 1; i >= 0; i-- {
		if file[i] == '/' {
			return file[:i]
		}
	}
	return "."
}

func implements(target string) facts.Relation {
	return facts.Relation{Kind: facts.RelImplements, Target: target}
}

func calls(target string) facts.Relation { return facts.Relation{Kind: facts.RelCalls, Target: target} }

func names(target string) facts.Relation { return facts.Relation{Kind: facts.RelNames, Target: target} }

func wantReported(t *testing.T, reported map[string]bool, want map[string]bool) {
	t.Helper()
	for name, dead := range want {
		if reported[name] != dead {
			t.Errorf("%s reported = %v, want %v", name, reported[name], dead)
		}
	}
}

// TestLaravelMigrationsAreRunByTheMigrator: a migration, named or anonymous, is run by
// its file; a class extending Migration outside a migrations directory, or a plain
// class in one, is not.
func TestLaravelMigrationsAreRunByTheMigrator(t *testing.T) {
	var s laravelStore
	const named = "database/migrations/2018_07_15_011916_add_2fa_to_users_table.php"
	s.class("Add2faToUsersTable", named, implements(laravelMigration))
	s.method("Add2faToUsersTable::up", named)
	s.method("Add2faToUsersTable::down", named)
	const anon = "class@anonymous:database/migrations/2023_12_04_041631_create_push_subscriptions_table"
	s.class(anon, "database/migrations/2023_12_04_041631_create_push_subscriptions_table.php", implements(laravelMigration))
	s.method(anon+"::up", "database/migrations/2023_12_04_041631_create_push_subscriptions_table.php")
	s.class(`App\Support\StrayMigration`, "app/Support/StrayMigration.php", implements(laravelMigration))
	s.class(`Database\Migrations\Helper`, "database/migrations/Helper.php")
	s.method(`Database\Migrations\Helper::up`, "database/migrations/Helper.php")
	wantReported(t, s.reported(), map[string]bool{
		"Add2faToUsersTable": false, "Add2faToUsersTable::up": false, "Add2faToUsersTable::down": false,
		anon: false, anon + "::up": false,
		`App\Support\StrayMigration`: true,
		`Database\Migrations\Helper`: true, `Database\Migrations\Helper::up`: true,
	})
}

// TestLaravelCommandsAreLoadedFromTheirDirectory: the console kernel loads every
// command under app/Console/Commands, through an application base class too.
func TestLaravelCommandsAreLoadedFromTheirDirectory(t *testing.T) {
	var s laravelStore
	s.class(`App\Console\Commands\BaseCommand`, "app/Console/Commands/BaseCommand.php", implements(laravelCommand))
	s.class(`App\Console\Commands\Internal\PruneOldNotifications`, "app/Console/Commands/Internal/PruneOldNotifications.php",
		implements(`App\Console\Commands\BaseCommand`))
	s.method(`App\Console\Commands\Internal\PruneOldNotifications::handle`, "app/Console/Commands/Internal/PruneOldNotifications.php")
	s.class(`App\Console\Commands\Unfinished`, "app/Console/Commands/Unfinished.php", implements(laravelCommand))
	s.facts[len(s.facts)-1].Props["abstract"] = true
	s.class(`App\Tools\Exporter`, "app/Tools/Exporter.php", implements(laravelCommand))
	s.class(`App\Console\Commands\Formatter`, "app/Console/Commands/Formatter.php")
	s.class(`Database\Seeders\DatabaseSeeder`, "database/seeders/DatabaseSeeder.php", implements(laravelSeeder))
	s.method(`Database\Seeders\DatabaseSeeder::run`, "database/seeders/DatabaseSeeder.php")
	s.class(`Database\Seeders\DemoSeeder`, "database/seeders/DemoSeeder.php", implements(laravelSeeder))
	wantReported(t, s.reported(), map[string]bool{
		`App\Console\Commands\Internal\PruneOldNotifications`:         false,
		`App\Console\Commands\Internal\PruneOldNotifications::handle`: false,
		`App\Console\Commands\Unfinished`:                             true, // abstract: the kernel does not load it
		`App\Tools\Exporter`:                                          true, // a command, but outside the loaded directory
		`App\Console\Commands\Formatter`:                              true, // in the directory, but not a command
		`Database\Seeders\DatabaseSeeder`:                             false,
		`Database\Seeders\DatabaseSeeder::run`:                        false,
		`Database\Seeders\DemoSeeder`:                                 true,
	})
}

// TestLaravelHooksAreKeyedOnTheFrameworkType: the framework calls casts() on a model
// and envelope() on a mailable, and nothing else's; a class the hook sits on is still
// judged by its own references.
func TestLaravelHooksAreKeyedOnTheFrameworkType(t *testing.T) {
	var s laravelStore
	s.class(`App\Models\Status`, "app/Models/Status.php", implements(laravelModel))
	s.method(`App\Models\Status::casts`, "app/Models/Status.php")
	s.method(`App\Models\Status::unusedHelper`, "app/Models/Status.php")
	s.class(`App\HasSnowflakePrimary`, "app/HasSnowflakePrimary.php")
	s.method(`App\HasSnowflakePrimary::bootHasSnowflakePrimary`, "app/HasSnowflakePrimary.php")
	s.method(`App\HasSnowflakePrimary::bootOther`, "app/HasSnowflakePrimary.php")
	s.facts[0].Relations = append(s.facts[0].Relations, implements(`App\HasSnowflakePrimary`))
	s.class(`App\Mail\AdminInviteEmail`, "app/Mail/AdminInviteEmail.php", implements(`Illuminate\Mail\Mailable`))
	s.method(`App\Mail\AdminInviteEmail::envelope`, "app/Mail/AdminInviteEmail.php")
	s.class(`App\Services\Report`, "app/Services/Report.php")
	s.method(`App\Services\Report::casts`, "app/Services/Report.php")
	s.method(`App\Services\Report::envelope`, "app/Services/Report.php")
	s.class(`App\Notifications\Welcome`, "app/Notifications/Welcome.php", implements(laravelNotice))
	s.method(`App\Notifications\Welcome::toMail`, "app/Notifications/Welcome.php")
	s.method(`App\Notifications\Welcome::tomorrow`, "app/Notifications/Welcome.php")
	s.class(`App\Services\Captcha\CaptchaManager`, "app/Services/Captcha/CaptchaManager.php", implements(laravelManager))
	s.method(`App\Services\Captcha\CaptchaManager::createTurnstileDriver`, "app/Services/Captcha/CaptchaManager.php")
	s.class(`App\Http\Controllers\Auth\LoginController`, "app/Http/Controllers/Auth/LoginController.php",
		implements(`Illuminate\Foundation\Auth\AuthenticatesUsers`))
	s.method(`App\Http\Controllers\Auth\LoginController::throttleKey`, "app/Http/Controllers/Auth/LoginController.php")
	s.class(`App\Http\Controllers\Caller`, "app/Http/Controllers/Caller.php")
	s.method(`App\Http\Controllers\Caller::send`, "app/Http/Controllers/Caller.php",
		calls(`App\Models\Status::query`), names(`App\Notifications\Welcome`), names(`App\Services\Captcha\CaptchaManager`),
		names(`App\Http\Controllers\Auth\LoginController`), names(`App\Services\Report`))
	wantReported(t, s.reported(), map[string]bool{
		`App\Models\Status::casts`:                                   false,
		`App\Models\Status::unusedHelper`:                            true,
		`App\HasSnowflakePrimary::bootHasSnowflakePrimary`:           false, // the model boots the trait it uses
		`App\HasSnowflakePrimary::bootOther`:                         true,
		`App\Mail\AdminInviteEmail`:                                  true, // nothing sends it, so its hook does not keep it
		`App\Mail\AdminInviteEmail::envelope`:                        false,
		`App\Services\Report::casts`:                                 true,
		`App\Services\Report::envelope`:                              true,
		`App\Notifications\Welcome::toMail`:                          false,
		`App\Notifications\Welcome::tomorrow`:                        true,
		`App\Services\Captcha\CaptchaManager::createTurnstileDriver`: false,
		`App\Http\Controllers\Auth\LoginController::throttleKey`:     false,
	})
}

// TestEloquentObserversAreCalledByTheirModel: an observer's event methods run when it
// is registered, by an observe() call on a model or by the model's attribute; an
// event-named method on an unregistered class does not.
func TestEloquentObserversAreCalledByTheirModel(t *testing.T) {
	var s laravelStore
	s.class(`App\Models\Avatar`, "app/Models/Avatar.php", implements(laravelModel))
	s.class(`App\Models\Theme`, "app/Models/Theme.php", implements(laravelModel), names(`App\Observers\ThemeObserver`))
	s.class(`App\Providers\AppServiceProvider`, "app/Providers/AppServiceProvider.php", implements(serviceProvider))
	s.method(`App\Providers\AppServiceProvider::boot`, "app/Providers/AppServiceProvider.php",
		calls(`App\Models\Avatar::observe`), names(`App\Observers\AvatarObserver`))
	for _, o := range []string{`App\Observers\AvatarObserver`, `App\Observers\ThemeObserver`} {
		s.class(o, "app/Observers/x.php")
		s.method(o+"::created", "app/Observers/x.php")
		s.method(o+"::helper", "app/Observers/x.php")
	}
	s.class(`App\Services\Feed`, "app/Services/Feed.php")
	s.method(`App\Services\Feed::created`, "app/Services/Feed.php")
	wantReported(t, s.reported(), map[string]bool{
		`App\Providers\AppServiceProvider::boot`: false,
		`App\Observers\AvatarObserver::created`:  false,
		`App\Observers\AvatarObserver::helper`:   true,
		`App\Observers\ThemeObserver::created`:   false,
		`App\Services\Feed::created`:             true,
	})
}

// TestEloquentScopeIsUsedByItsAlias: scopeModerated is called as moderated().
func TestEloquentScopeIsUsedByItsAlias(t *testing.T) {
	var s laravelStore
	s.class(`App\Models\Instance`, "app/Models/Instance.php", implements(laravelModel))
	s.method(`App\Models\Instance::scopeModerated`, "app/Models/Instance.php")
	s.method(`App\Models\Instance::scopeForgotten`, "app/Models/Instance.php")
	s.class(`App\Services\Plain`, "app/Services/Plain.php")
	s.method(`App\Services\Plain::scopeLocal`, "app/Services/Plain.php")
	s.class(`App\Http\Controllers\DomainBlocksController`, "app/Http/Controllers/DomainBlocksController.php")
	s.method(`App\Http\Controllers\DomainBlocksController::index`, "app/Http/Controllers/DomainBlocksController.php",
		calls(`App\Models\Instance::moderated`), calls("local"), names(`App\Services\Plain`))
	wantReported(t, s.reported(), map[string]bool{
		`App\Models\Instance::scopeModerated`: false,
		`App\Models\Instance::scopeForgotten`: true,
		`App\Services\Plain::scopeLocal`:      true, // not a model, so not a scope
	})
}

// TestFractalIncludesFollowTheIncludeLists: Fractal calls includeX only for an entry
// in the transformer's include lists.
func TestFractalIncludesFollowTheIncludeLists(t *testing.T) {
	var s laravelStore
	const tr = `App\Transformer\Api\ResultsTransformer`
	s.class(tr, "app/Transformer/Api/ResultsTransformer.php", implements(fractalTransform))
	s.facts[len(s.facts)-1].Props[propFractalIncl] = []any{"accounts", "ordered_items"}
	for _, m := range []string{"transform", "includeAccounts", "includeOrderedItems", "includeTags"} {
		s.method(tr+"::"+m, "app/Transformer/Api/ResultsTransformer.php")
	}
	wantReported(t, s.reported(), map[string]bool{
		tr + "::transform":           false,
		tr + "::includeAccounts":     false,
		tr + "::includeOrderedItems": false,
		tr + "::includeTags":         true, // no list names it
	})
}
