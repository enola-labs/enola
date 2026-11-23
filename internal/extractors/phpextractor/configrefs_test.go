package phpextractor

import (
	"context"
	"sort"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// fileRefNames returns the classes the file reference of rel names, sorted.
func fileRefNames(ff []facts.Fact, rel string) []string {
	var out []string
	for _, f := range ff {
		if f.Kind != facts.KindFileRef || f.File != rel {
			continue
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelNames {
				out = append(out, r.Target)
			}
		}
	}
	sort.Strings(out)
	return out
}

// TestConfigFilesNameTheClassesTheyRegister: a PHPStan .neon file registers its
// extensions by class name, and `artisan db:seed --class=` in a composer script or
// a shell script runs a seeder by name, under Database\Seeders when the name has no
// namespace. A class named inside a quoted pattern (an ignored error message), a
// class the repository does not declare, and a --class= on a line that seeds
// nothing name nothing.
func TestConfigFilesNameTheClassesTheyRegister(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "composer.json", `{"scripts": {
  "refresh-test-database": ["@php artisan db:seed --class=DummyContentSeeder --database=mysql_testing"],
  "other": ["@php artisan make:thing --class=GhostSeeder"]
}}`)
	writeFile(t, dir, "dev/seed.sh", "#!/bin/sh\nphp artisan db:seed --force --class='Database\\Seeders\\LargeSeeder'\n")
	writeFile(t, dir, "phpstan/extension.neon", `includes:
    - baseline.neon
services:
	-
		class: Acme\PHPStan\ModelRelationsExtension
		tags:
			- phpstan.broker.methodsClassReflectionExtension
	- Acme\PHPStan\BareService
	-
		class: Acme\PHPStan\Resolver
		arguments:
			- @Acme\PHPStan\FilesProvider
	- Vendor\Missing\Thing
parameters:
	ignoreErrors:
		- '#Call to an undefined method Acme\\PHPStan\\Unused::run\(\)#'
`)
	for rel, src := range map[string]string{
		"database/seeders/DummyContentSeeder.php": "<?php\nnamespace Database\\Seeders;\nclass DummyContentSeeder extends \\Illuminate\\Database\\Seeder {}\n",
		"database/seeders/LargeSeeder.php":        "<?php\nnamespace Database\\Seeders;\nclass LargeSeeder extends \\Illuminate\\Database\\Seeder {}\n",
		"database/seeders/GhostSeeder.php":        "<?php\nnamespace Database\\Seeders;\nclass GhostSeeder extends \\Illuminate\\Database\\Seeder {}\n",
		"phpstan/src/ModelRelationsExtension.php": "<?php\nnamespace Acme\\PHPStan;\nclass ModelRelationsExtension {}\n",
		"phpstan/src/BareService.php":             "<?php\nnamespace Acme\\PHPStan;\nclass BareService {}\n",
		"phpstan/src/Resolver.php":                "<?php\nnamespace Acme\\PHPStan;\nclass Resolver {}\n",
		"phpstan/src/FilesProvider.php":           "<?php\nnamespace Acme\\PHPStan;\nclass FilesProvider {}\n",
		"phpstan/src/Unused.php":                  "<?php\nnamespace Acme\\PHPStan;\nclass Unused {}\n",
	} {
		writeFile(t, dir, rel, src)
	}
	files := []string{"composer.json", "dev/seed.sh", "phpstan/extension.neon",
		"database/seeders/DummyContentSeeder.php", "database/seeders/LargeSeeder.php", "database/seeders/GhostSeeder.php",
		"phpstan/src/ModelRelationsExtension.php", "phpstan/src/BareService.php", "phpstan/src/Resolver.php",
		"phpstan/src/FilesProvider.php", "phpstan/src/Unused.php"}
	ff, err := (&PHPExtractor{}).Extract(context.Background(), dir, files)
	if err != nil {
		t.Fatal(err)
	}
	wantTargets(t, "composer.json names", fileRefNames(ff, "composer.json"), `Database\Seeders\DummyContentSeeder`)
	wantTargets(t, "seed.sh names", fileRefNames(ff, "dev/seed.sh"), `Database\Seeders\LargeSeeder`)
	wantTargets(t, "extension.neon names", fileRefNames(ff, "phpstan/extension.neon"),
		`Acme\PHPStan\BareService`, `Acme\PHPStan\FilesProvider`, `Acme\PHPStan\ModelRelationsExtension`, `Acme\PHPStan\Resolver`)
}
