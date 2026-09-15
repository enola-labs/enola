package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "enola.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad_ClientsAndServiceAliases(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
clients:
  - name: sdk-http
    language: typescript
    receiver_types: [IHttpRequestService]
    methods:
      - name: sendRequest
        service_arg: 0
        path_arg: 1
        options_arg: 2
        default_verb: get
service_aliases:
  resource-api: gateway
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Clients) != 1 || len(cfg.Clients[0].Methods) != 1 {
		t.Fatalf("clients not parsed: %+v", cfg.Clients)
	}
	m := cfg.Clients[0].Methods[0]
	if m.ServiceArg == nil || *m.ServiceArg != 0 {
		t.Errorf("service_arg 0 must survive as a set position, got %v", m.ServiceArg)
	}
	if m.PathArg == nil || *m.PathArg != 1 || m.OptionsArg == nil || *m.OptionsArg != 2 {
		t.Errorf("argument positions not parsed: path=%v options=%v", m.PathArg, m.OptionsArg)
	}
	if m.VerbOption != "method" || m.DefaultVerb != "GET" {
		t.Errorf("load did not normalize: verb_option=%q default_verb=%q", m.VerbOption, m.DefaultVerb)
	}
	if cfg.ServiceAliases["resource-api"] != "gateway" {
		t.Errorf("service_aliases not parsed: %v", cfg.ServiceAliases)
	}
}

func TestLoad_InvalidClientsFailNamingTheFile(t *testing.T) {
	p := writeConfig(t, `
clients:
  - name: sdk-http
    language: typescript
    receiver_types: [IHttpRequestService]
    methods:
      - name: sendRequest
        default_verb: FETCH
service_aliases:
  resource-api: ""
`)
	_, err := Load(p)
	if err == nil {
		t.Fatal("invalid clients accepted")
	}
	for _, want := range []string{p, "path_arg is required", `default_verb "FETCH"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error omits %q:\n%v", want, err)
		}
	}
}

// A client for a language with no reader must not stop the rest of the config loading.
func TestLoad_ClientForLanguageWithoutReaderLoads(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
clients:
  - name: py-http
    language: python
    receiver_types: [HttpClient]
    methods:
      - name: send
        path_arg: 0
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Clients) != 1 || cfg.Clients[0].Language != "python" {
		t.Errorf("clients not parsed: %+v", cfg.Clients)
	}
}

func TestLoad_InvalidServiceAliasFails(t *testing.T) {
	_, err := Load(writeConfig(t, "service_aliases:\n  resource-api: \"\"\n"))
	if err == nil || !strings.Contains(err.Error(), "empty repository label") {
		t.Fatalf("empty alias target accepted: %v", err)
	}
}
