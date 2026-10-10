package tsextractor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func extractTSRepo(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	var rel []string
	for name, src := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		rel = append(rel, name)
	}
	slices.Sort(rel)
	ff, err := New().Extract(context.Background(), dir, rel)
	if err != nil {
		t.Fatal(err)
	}
	return ff
}

func tsSym(t *testing.T, ff []facts.Fact, suffix string) facts.Fact {
	t.Helper()
	for _, f := range ff {
		if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, suffix) {
			return f
		}
	}
	var names []string
	for _, f := range ff {
		if f.Kind == facts.KindSymbol {
			names = append(names, f.Name)
		}
	}
	t.Fatalf("no symbol ending in %s among %v", suffix, names)
	return facts.Fact{}
}

func tsBool(f facts.Fact, key string) bool {
	b, _ := f.PropAny(key).(bool)
	return b
}

// An Angular service holds its client in a constructor parameter property. The
// call is on a field named for nothing; its declared type is what says it.
func TestAngularHttpClientCallIsDirectIO(t *testing.T) {
	ff := extractTSRepo(t, map[string]string{
		"package.json": `{"dependencies":{"@angular/core":"17.0.0"}}`,
		"src/app/alarm.service.ts": `import { HttpClient } from '@angular/common/http';
import { Injectable } from '@angular/core';

@Injectable()
export class AlarmService {
  constructor(private http: HttpClient, private names: Map<string, string>) {}

  ack(id: string) {
    return this.http.post('/api/alarm/' + id + '/ack', null);
  }

  label(id: string) {
    return this.names.get(id);
  }
}
`,
		"src/app/alarm.component.ts": `import { AlarmService } from './alarm.service';

export class AlarmComponent {
  constructor(private alarms: AlarmService) {}

  ackAll(ids: string[]) {
    for (const id of ids) {
      this.alarms.ack(id);
    }
  }

  labels(ids: string[]) {
    return ids.map(id => this.alarms.label(id));
  }
}
`,
	})
	ack := tsSym(t, ff, "AlarmService.ack")
	if !tsBool(ack, "io_direct") {
		t.Errorf("AlarmService.ack is not io_direct; relations=%v", ack.Relations)
	}
	if calls, _ := ack.PropAny("io_calls").([]string); !slices.Contains(calls, "@angular/common/http.HttpClient.post") {
		t.Errorf("AlarmService.ack io_calls = %v, want the HttpClient.post", calls)
	}
	if !tsBool(tsSym(t, ff, "AlarmComponent.ackAll"), "performs_io") {
		t.Errorf("AlarmComponent.ackAll is not flagged: component, service, HttpClient")
	}
	// A Map's get is not the client's, and the method that calls it is not I/O.
	if tsBool(tsSym(t, ff, "AlarmService.label"), "performs_io") || tsBool(tsSym(t, ff, "AlarmComponent.labels"), "performs_io") {
		t.Errorf("a Map lookup was read as an HTTP call")
	}
}

// A client that wraps fetch for retries hands it over and never calls it. And a
// facade over a singleton calls a method on what a function of the file returns.
func TestFetchHandedToAWrapperAndCallOnADeclaredResult(t *testing.T) {
	ff := extractTSRepo(t, map[string]string{
		"src/connection/callApi.ts": `import fetchRetry from 'fetch-retry';

export default async function callApi(url: string, options: object) {
  const fetchWithRetry = fetchRetry(fetch, options);
  return fetchWithRetry(url, {});
}

export function fullUrl(base: string, path: string) {
  return base + path;
}
`,
		"src/connection/ClientClass.ts": `import callApi from './callApi';

export default class ClientClass {
  delete(url: string) {
    return this.request(url);
  }

  request(url: string) {
    return callApi(url, {});
  }

  describe() {
    return 'client';
  }
}
`,
		"src/connection/Client.ts": `import ClientClass from './ClientClass';

let singleton: ClientClass | undefined;

function getInstance(): ClientClass {
  return singleton!;
}

function pending(): Promise<ClientClass> {
  return Promise.resolve(singleton!);
}

export const Client = {
  delete: (url: string) => getInstance().delete(url),
  describe: () => getInstance().describe(),
  later: () => pending().then(c => c),
};
`,
	})
	if !tsBool(tsSym(t, ff, "connection.callApi"), "io_direct") {
		t.Errorf("callApi hands fetch to a retry wrapper and is not io_direct")
	}
	if tsBool(tsSym(t, ff, "connection.fullUrl"), "performs_io") {
		t.Errorf("fullUrl concatenates two strings and is flagged")
	}
	del := tsSym(t, ff, "Client.delete")
	if !slices.ContainsFunc(del.Relations, func(r facts.Relation) bool {
		return r.Kind == facts.RelCalls && strings.HasSuffix(r.Target, "connection.ClientClass.delete")
	}) {
		t.Errorf("Client.delete relations = %v, want a call to ClientClass.delete through getInstance()", del.Relations)
	}
	if !tsBool(del, "performs_io") {
		t.Errorf("Client.delete is not flagged: facade, class, request, callApi, fetch")
	}
	if tsBool(tsSym(t, ff, "Client.describe"), "performs_io") {
		t.Errorf("Client.describe is flagged: it returns a literal")
	}
	// A generic return type names the wrapper, and nothing is resolved through it.
	later := tsSym(t, ff, "Client.later")
	if slices.ContainsFunc(later.Relations, func(r facts.Relation) bool {
		return r.Kind == facts.RelCalls && strings.HasSuffix(r.Target, ".then")
	}) {
		t.Errorf("Client.later resolved a call through Promise<…>: %v", later.Relations)
	}
}
