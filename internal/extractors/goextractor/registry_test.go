package goextractor

import "testing"

const registrySrc = `package pkg

type Client interface {
	Authenticate(id string) (string, error)
}

type Service struct {
	clients []Client
	hooks   map[string]func() error
}

func ProvideService(cs ...Client) *Service {
	s := &Service{hooks: map[string]func() error{}}
	s.clients = append(s.clients, cs...)
	return s
}

func (s *Service) RegisterHook(name string, h func() error) {
	s.hooks[name] = h
}

// A loop over the registered clients: once per client, not per element of data.
func (s *Service) Authenticate(id string) string {
	for _, c := range s.clients {
		if who, err := c.Authenticate(id); err == nil {
			return who
		}
	}
	return ""
}

func (s *Service) runHooks() {
	for _, h := range s.hooks {
		_ = h()
	}
}

type Hub struct {
	listeners []Client
}

func (h *Hub) Join(c Client) {
	h.listeners = append(h.listeners, c)
}

// Listeners join per connection: a loop over them grows with traffic.
func (h *Hub) Broadcast(id string) {
	for _, c := range h.listeners {
		c.Authenticate(id)
	}
}

type Record struct{ ID string }

type Cache struct {
	records []Record
}

func NewCache(rs []Record) *Cache { return &Cache{records: rs} }

// Records loaded at startup are data, not handlers: a loop over them scales.
func (c *Cache) Refresh(s *Service) {
	for _, r := range c.records {
		s.Authenticate(r.ID)
	}
}
`

// A loop over a registry of handlers (an interface or function slice or map written
// only by constructors and registration hooks) is bounded by the wiring. One written
// on a request path, or one holding records, still scales.
func TestRangeOverRegistryIsBounded(t *testing.T) {
	ff := extractAll(t, map[string]string{"pkg/x.go": registrySrc})
	for name, scales := range map[string]bool{
		"pkg.Service.Authenticate": false,
		"pkg.Service.runHooks":     false,
		"pkg.Hub.Broadcast":        true,
		"pkg.Cache.Refresh":        true,
	} {
		f, ok := findFact(ff, name)
		if !ok {
			t.Fatalf("missing %s", name)
		}
		got := len(strSliceProp(f, "calls_in_scaling_loop")) > 0 || intProp(t, f, "scaling_loop_depth") > 0
		if got != scales {
			t.Errorf("%s: scales = %v, want %v (scaling_loop_depth %d, calls_in_scaling_loop %v)", name, got, scales, intProp(t, f, "scaling_loop_depth"), strSliceProp(f, "calls_in_scaling_loop"))
		}
	}
}
