package pythonextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A method read through self or cls without a call (`return self.test_func`, a
// callback handed on) is a use of the method: it names the class's own method, and
// leaves a method its class inherits from another file to the inherited-calls binder
// (member_names). A call, a write, and a method of another class are not.
func TestPyMethodReadAsAValueIsNamed(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"app/__init__.py": "",
		"app/mixins.py": `
from app.base import RemoteBase


def register(callback=None):
    return callback


class UserPassesTestMixin(RemoteBase):
    def test_func(self):
        raise NotImplementedError

    def get_test_func(self):
        return self.test_func

    def on_done(self):
        return 1

    def handler(self):
        return 2

    def wire(self):
        h = self.handler
        register(callback=self.on_done)
        self.data = 1
        self.on_done()
        return h, self.remote_hook, self.data

    @classmethod
    def make(cls):
        return cls.handler

    def wire_self(self):
        return self.wire_self


class Other:
    def test_func(self):
        return 3
`})
	get := pySym(t, ff, "mixins.UserPassesTestMixin.get_test_func")
	if !get.HasRelation(facts.RelNames, "app/mixins.UserPassesTestMixin.test_func") {
		t.Errorf("get_test_func does not name the method it returns: %v", get.Relations)
	}
	if get.HasRelation(facts.RelNames, "app/mixins.Other.test_func") {
		t.Errorf("get_test_func names another class's method: %v", get.Relations)
	}
	wire := pySym(t, ff, "mixins.UserPassesTestMixin.wire")
	for _, want := range []string{"app/mixins.UserPassesTestMixin.handler", "app/mixins.UserPassesTestMixin.on_done"} {
		if !wire.HasRelation(facts.RelNames, want) {
			t.Errorf("wire does not name %s: %v", want, wire.Relations)
		}
	}
	for _, r := range wire.Relations {
		if r.Kind == facts.RelNames && (r.Target == "app/mixins.UserPassesTestMixin.data" || r.Target == "app/mixins.UserPassesTestMixin.wire") {
			t.Errorf("wire names %s", r.Target)
		}
	}
	if got := wire.PropStrings(facts.PropMemberNames); !slices.Equal(got, []string{"data", "remote_hook"}) {
		t.Errorf("member_names = %v, want the reads its file cannot place, the write left out", got)
	}
	if make := pySym(t, ff, "mixins.UserPassesTestMixin.make"); !make.HasRelation(facts.RelNames, "app/mixins.UserPassesTestMixin.handler") {
		t.Errorf("make does not name the method read through cls: %v", make.Relations)
	}
	if self := pySym(t, ff, "mixins.UserPassesTestMixin.wire_self"); self.HasRelation(facts.RelNames, "app/mixins.UserPassesTestMixin.wire_self") {
		t.Errorf("a method names itself: %v", self.Relations)
	}
}
