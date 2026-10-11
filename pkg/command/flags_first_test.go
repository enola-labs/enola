package command

import (
	"flag"
	"io"
	"reflect"
	"testing"
)

func TestFlagsFirst(t *testing.T) {
	newSet := func() (*flag.FlagSet, *int, *string, *bool) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		return fs, fs.Int("keep-last", 0, ""), fs.String("keep-since", "", ""), fs.Bool("apply", false, "")
	}
	cases := []struct {
		name      string
		args      []string
		keepLast  int
		keepSince string
		apply     bool
		rest      []string
	}{
		{name: "flags first, as the flag package wants", args: []string{"--keep-last", "5", "--apply", "store", "."}, keepLast: 5, apply: true, rest: []string{"store", "."}},
		{name: "store first, as the documentation shows", args: []string{"store", "--keep-last", "5", "--apply"}, keepLast: 5, apply: true, rest: []string{"store"}},
		{name: "flags between and after the positionals", args: []string{"store", "--keep-last", "5", ".", "--apply"}, keepLast: 5, apply: true, rest: []string{"store", "."}},
		{name: "a value written with an equals sign", args: []string{"store", "--keep-since=90d", "."}, keepSince: "90d", rest: []string{"store", "."}},
		{name: "a boolean flag does not swallow the next argument", args: []string{"--apply", "store"}, apply: true, rest: []string{"store"}},
		{name: "single dash spelling", args: []string{"store", "-keep-last", "2"}, keepLast: 2, rest: []string{"store"}},
		{name: "after a bare double dash everything is positional", args: []string{"--apply", "--", "--keep-last", "store"}, apply: true, rest: []string{"--keep-last", "store"}},
		{name: "a lone dash is a positional", args: []string{"-", "--apply"}, apply: true, rest: []string{"-"}},
		{name: "no arguments", args: nil},
		{name: "positionals only", args: []string{"store", "."}, rest: []string{"store", "."}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs, keepLast, keepSince, apply := newSet()
			if err := fs.Parse(flagsFirst(fs, tc.args)); err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if *keepLast != tc.keepLast || *keepSince != tc.keepSince || *apply != tc.apply {
				t.Errorf("flags = keep-last %d, keep-since %q, apply %v; want %d, %q, %v",
					*keepLast, *keepSince, *apply, tc.keepLast, tc.keepSince, tc.apply)
			}
			if got := fs.Args(); !reflect.DeepEqual(got, tc.rest) && (len(got) != 0 || len(tc.rest) != 0) {
				t.Errorf("positionals = %v, want %v", got, tc.rest)
			}
		})
	}

	// An unknown flag is still an error, and it is Parse that reports it.
	fs, _, _, _ := newSet()
	if err := fs.Parse(flagsFirst(fs, []string{"store", "--no-such-flag"})); err == nil {
		t.Error("an unknown flag after a positional must still be refused")
	}
	// A flag missing its value is still an error.
	fs, _, _, _ = newSet()
	if err := fs.Parse(flagsFirst(fs, []string{"store", "--keep-last"})); err == nil {
		t.Error("a flag with no value must still be refused")
	}
}
