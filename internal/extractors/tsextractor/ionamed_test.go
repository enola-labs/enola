package tsextractor

import (
	"testing"
)

// findOne is TypeORM's query and svg.js's DOM lookup. Once the call resolves, the
// package it resolved into decides, not the name.
func TestIONamedCall_ResolvedTargetDecides(t *testing.T) {
	ff := extractTSRepo(t, map[string]string{
		"src/flow.ts": `import { Element } from '@svgdotjs/svg.js';

export class FlowAnimation {
  constructor(private element: Element) {}

  finish() {
    this.element.findOne('path')?.remove();
  }
}
`,
		"src/user.service.ts": `import { Repository } from 'typeorm';
import { User } from './user';

export class UserService {
  constructor(private users: Repository<User>) {}

  load(id: number) {
    return this.users.findOne({ where: { id } });
  }
}
`,
		"src/user.ts": `export class User { id: number; }
`,
	})
	finish := tsSym(t, ff, "FlowAnimation.finish")
	if tsBool(finish, "io_direct") || tsBool(finish, "performs_io") {
		t.Errorf("svg.js Element.findOne was read as a query; relations=%v", finish.Relations)
	}
	if !tsBool(tsSym(t, ff, "UserService.load"), "io_direct") {
		t.Errorf("typeorm Repository.findOne is not io_direct")
	}
	for _, f := range ff {
		if _, ok := f.Props[propIONamedCalls]; ok {
			t.Errorf("%s still carries %s", f.Name, propIONamedCalls)
		}
	}
}

// A call resolved into the repository keeps the name as evidence: a findOne a class
// inherits from typeorm's Repository has no fact, and a declared deleteMany reaches
// its request through a helper the closure does not know.
func TestIONamedCall_InheritedAndDeclaredMethods(t *testing.T) {
	ff := extractTSRepo(t, map[string]string{
		"src/user.repository.ts": `import { Repository } from 'typeorm';

export class UserRepository extends Repository<object> {}
`,
		"src/models/post.ts": `import { ajax } from '../lib/ajax';

export class Post {
  static deleteMany(ids: number[]) {
    return ajax('/posts/destroy_many', { type: 'DELETE', data: { ids } });
  }
}
`,
		"src/lib/ajax.ts": `export function ajax(url: string, options: object) {
  return window.jQuery.ajax(url, options);
}
`,
		"src/lookup.ts": `import { UserRepository } from './user.repository';
import { Post } from './models/post';

export class Lookup {
  constructor(private users: UserRepository) {}

  user(id: number) {
    return this.users.findOne({ where: { id } });
  }

  deletePosts(ids: number[]) {
    return Post.deleteMany(ids);
  }
}
`,
	})
	for _, name := range []string{"Lookup.user", "Lookup.deletePosts"} {
		if f := tsSym(t, ff, name); !tsBool(f, "io_direct") {
			t.Errorf("%s is not io_direct; relations=%v", name, f.Relations)
		}
	}
}
