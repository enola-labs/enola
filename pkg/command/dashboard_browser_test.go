package command

import (
	"reflect"
	"testing"
)

func TestBrowserCommandPreservesDashboardURL(t *testing.T) {
	const url = "http://127.0.0.1:54279"
	tests := []struct {
		goos string
		name string
		args []string
	}{
		{goos: "darwin", name: "open", args: []string{"-u", url}},
		{goos: "windows", name: "rundll32", args: []string{"url.dll,FileProtocolHandler", url}},
		{goos: "linux", name: "xdg-open", args: []string{url}},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			name, args := browserCommand(tt.goos, url)
			if name != tt.name || !reflect.DeepEqual(args, tt.args) {
				t.Fatalf("browserCommand(%q, %q) = %q, %q; want %q, %q", tt.goos, url, name, args, tt.name, tt.args)
			}
		})
	}
}
