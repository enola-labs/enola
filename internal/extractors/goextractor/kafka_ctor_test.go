package goextractor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// kafkaCtorTopics parses files as one package and returns the constructor-pass
// topic facts by name.
func kafkaCtorTopics(t *testing.T, files map[string]string) map[string]facts.Fact {
	t.Helper()
	fset := token.NewFileSet()
	pp := &parsedPkg{fileMap: map[string]*ast.File{}}
	for _, name := range []string{"internal/consumers/handler.go", "internal/consumers/wiring.go", "internal/consumers/topics.go"} {
		src, ok := files[name]
		if !ok {
			continue
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		pp.relFiles = append(pp.relFiles, name)
		pp.parsedFiles = append(pp.parsedFiles, f)
		pp.fileMap[name] = f
	}
	byName := map[string]facts.Fact{}
	for _, f := range extractKafkaConstructorFacts(fset, pp, "internal/consumers") {
		byName[f.Name] = f
	}
	return byName
}

const consumerCtor = `package consumers

func NewOrderPlacedSubscription(logger Logger, repo Repo, topic string, group string, instances uint8) events.Subscription {
	return events.Subscription{
		Message: events.MessageSubscription{
			Topic:         topic,
			ConsumerGroup: group,
			Instances:     instances,
		},
		Handler: newHandler(logger, repo),
	}
}
`

func TestExtractKafkaConstructorFacts(t *testing.T) {
	tests := []struct {
		name      string
		files     map[string]string
		wantTopic string
		wantRole  string
		wantSym   string
		wantNone  bool
	}{
		{
			name: "literal at call site in another file",
			files: map[string]string{
				"internal/consumers/handler.go": consumerCtor,
				"internal/consumers/wiring.go": `package consumers

func productionSubscriptions(d Deps) []events.Subscription {
	return []events.Subscription{
		NewOrderPlacedSubscription(d.Logger(), d.Orders(), "platform.events.shop.orders.order_placed", groupName, 1),
	}
}
`,
			},
			wantTopic: "platform.events.shop.orders.order_placed",
			wantRole:  facts.MessagingRoleConsumer,
			wantSym:   "internal/consumers.productionSubscriptions",
		},
		{
			name: "const declared in a third file",
			files: map[string]string{
				"internal/consumers/handler.go": consumerCtor,
				"internal/consumers/topics.go": `package consumers

const orderPlacedTopic = "shop.orders.order_placed"
`,
				"internal/consumers/wiring.go": `package consumers

func subscriptions(d Deps) []events.Subscription {
	return []events.Subscription{NewOrderPlacedSubscription(d.Logger(), d.Orders(), orderPlacedTopic, groupName, 1)}
}
`,
			},
			wantTopic: "shop.orders.order_placed",
			wantRole:  facts.MessagingRoleConsumer,
			wantSym:   "internal/consumers.subscriptions",
		},
		{
			name: "producer config by type name",
			files: map[string]string{
				"internal/consumers/handler.go": `package consumers

func NewInvoicePublisher(topic string) *bus.Publisher {
	return &bus.Publisher{Topic: topic}
}
`,
				"internal/consumers/wiring.go": `package consumers

func wire() { _ = NewInvoicePublisher("shop.billing.invoice_issued") }
`,
			},
			wantTopic: "shop.billing.invoice_issued",
			wantRole:  facts.MessagingRoleProducer,
			wantSym:   "internal/consumers.wire",
		},
		{
			name: "Topic field without group or producer type is not Kafka",
			files: map[string]string{
				"internal/consumers/handler.go": `package consumers

func NewNotice(topic string) Notice { return Notice{Topic: topic, Body: "x"} }
`,
				"internal/consumers/wiring.go": `package consumers

func wire() { _ = NewNotice("shop.orders.order_placed") }
`,
			},
			wantNone: true,
		},
		{
			name: "non-literal argument is not resolved",
			files: map[string]string{
				"internal/consumers/handler.go": consumerCtor,
				"internal/consumers/wiring.go": `package consumers

func wire(cfg Config) { _ = NewOrderPlacedSubscription(nil, nil, cfg.Topic(), "g", 1) }
`,
			},
			wantNone: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := kafkaCtorTopics(t, tc.files)
			if tc.wantNone {
				if len(got) != 0 {
					t.Fatalf("want no topic facts, got %v", keys(got))
				}
				return
			}
			f, ok := got[tc.wantTopic]
			if !ok {
				t.Fatalf("missing topic %q; got %v", tc.wantTopic, keys(got))
			}
			if f.Props[facts.PropMessagingRole] != tc.wantRole {
				t.Errorf("role = %v, want %s", f.Props[facts.PropMessagingRole], tc.wantRole)
			}
			if f.Props["code_symbol"] != tc.wantSym {
				t.Errorf("code_symbol = %v, want %s", f.Props["code_symbol"], tc.wantSym)
			}
			if f.Props[facts.PropSource] != facts.MessagingSourceGoKafkaCall {
				t.Errorf("source = %v", f.Props[facts.PropSource])
			}
			if f.File != "internal/consumers/wiring.go" {
				t.Errorf("file = %s, want the call site's file", f.File)
			}
		})
	}
}
