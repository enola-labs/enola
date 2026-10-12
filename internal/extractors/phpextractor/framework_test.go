package phpextractor

import (
	"os"
	"path/filepath"
	"testing"
)

// writeRepo materializes a set of relative-path -> content files under a fresh temp
// dir and returns its path.
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDetectPHPFramework(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  phpFramework
	}{
		{
			name:  "wordpress via marker",
			files: map[string]string{"wp-load.php": "<?php\n", "composer.json": `{"require":{"laravel/framework":"^11"}}`},
			want:  frameworkWordPress, // WordPress wins even alongside other deps
		},
		{
			name:  "laravel via composer",
			files: map[string]string{"composer.json": `{"require":{"laravel/framework":"^11.0"}}`},
			want:  frameworkLaravel,
		},
		{
			name:  "laravel via artisan + routes",
			files: map[string]string{"artisan": "#!/usr/bin/env php\n", "routes/web.php": "<?php\n"},
			want:  frameworkLaravel,
		},
		{
			name:  "symfony via composer",
			files: map[string]string{"composer.json": `{"require":{"symfony/framework-bundle":"^7.0"}}`},
			want:  frameworkSymfony,
		},
		{
			name:  "symfony via bin/console + config",
			files: map[string]string{"bin/console": "#!/usr/bin/env php\n", "config/routes.yaml": "home:\n  path: /\n"},
			want:  frameworkSymfony,
		},
		{
			name:  "plain php",
			files: map[string]string{"composer.json": `{"require":{"monolog/monolog":"^3"}}`, "src/Foo.php": "<?php\n"},
			want:  frameworkPlain,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := writeRepo(t, tt.files)
			if got := detectPHPFramework(repo); got != tt.want {
				t.Errorf("detectPHPFramework = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIsLaravelRouteFile(t *testing.T) {
	yes := []string{
		"routes/web.php", "routes/api.php", "routes/console.php", "routes/channels.php",
		"routes/api.base.php", "routes/api/v1.php", "modules/Blog/routes/web.php",
	}
	no := []string{"app/Http/Controllers/UserController.php", "config/routes.php", "web.php", "app/routes.php"}
	for _, f := range yes {
		if !isLaravelRouteFile(f) {
			t.Errorf("isLaravelRouteFile(%q) = false, want true", f)
		}
	}
	for _, f := range no {
		if isLaravelRouteFile(f) {
			t.Errorf("isLaravelRouteFile(%q) = true, want false", f)
		}
	}
}
