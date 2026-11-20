package asyncapiextractor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func writeSpec(t *testing.T, repo, rel, content string) {
	t.Helper()
	path := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
func extract(t *testing.T, repo string) []facts.Fact {
	t.Helper()
	got, err := New().Extract(context.Background(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
func find(got []facts.Fact, name, role string) *facts.Fact {
	for i := range got {
		if got[i].Name == name && got[i].PropString("messaging_role") == role {
			return &got[i]
		}
	}
	return nil
}

func TestNameAndDetect(t *testing.T) {
	if New().Name() != "asyncapi" {
		t.Fatalf("Name = %q", New().Name())
	}
	repo := t.TempDir()
	writeSpec(t, repo, "events.yaml", "name: ordinary config\n")
	ok, err := New().Detect(repo)
	if err != nil || ok {
		t.Fatalf("ordinary YAML detected: ok=%v err=%v", ok, err)
	}
	writeSpec(t, repo, "contracts/events.yaml", asyncAPI2)
	ok, err = New().Detect(repo)
	if err != nil || !ok {
		t.Fatalf("AsyncAPI not detected: ok=%v err=%v", ok, err)
	}
}

func TestDetectRequiresRootKey(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "nested.yaml", "metadata:\n  asyncapi: 3.0.0\n")
	ok, err := New().Detect(repo)
	if err != nil || ok {
		t.Fatalf("nested key detected: ok=%v err=%v", ok, err)
	}
}

const asyncAPI2 = `asyncapi: 2.6.0
info: {title: Orders, version: 1.0.0}
defaultContentType: application/json
servers:
  production: {url: kafka:9092, protocol: kafka}
channels:
  svc-orders.order_created:
    servers: [production]
    publish:
      operationId: emitOrderCreated
      summary: Emit an order
      tags: [{name: orders}]
      message: {$ref: '#/components/messages/OrderCreated'}
    subscribe:
      operationId: receiveOrderCreated
      message:
        name: OrderCreated
        contentType: application/avro
`

func TestExtractV2(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "contracts/events.yaml", asyncAPI2)
	got := extract(t, repo)
	if len(got) != 2 {
		t.Fatalf("got %d facts: %+v", len(got), got)
	}
	producer := find(got, "svc-orders.order_created", facts.MessagingRoleProducer)
	if producer == nil {
		t.Fatal("missing producer")
	}
	if producer.Props["messaging"] != "kafka" || producer.Props["operationId"] != "emitOrderCreated" || producer.Props["message"] != "OrderCreated" {
		t.Errorf("producer props = %+v", producer.Props)
	}
	if producer.Props["content_type"] != "application/json" {
		t.Errorf("default content type = %v", producer.Props["content_type"])
	}
	consumer := find(got, "svc-orders.order_created", facts.MessagingRoleConsumer)
	if consumer == nil || consumer.Props["content_type"] != "application/avro" {
		t.Errorf("consumer = %+v", consumer)
	}
}

func TestUnquotedTwoPartVersionDetectsAndExtracts(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "asyncapi.yaml", "asyncapi: 2.0\nchannels:\n  orders.created:\n    publish: {message: {name: Order}}\n")
	ok, err := New().Detect(repo)
	if err != nil || !ok {
		t.Fatalf("Detect = %v, %v", ok, err)
	}
	if got := extract(t, repo); len(got) != 1 {
		t.Fatalf("Extract got %d facts: %+v", len(got), got)
	}
}

func TestUnquotedV3TwoPartVersionUsesV3Operations(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "asyncapi.yaml", `asyncapi: 3.0
channels:
  orders:
    address: orders.created
operations:
  publishOrder:
    action: send
    channel: {$ref: '#/channels/orders'}
    messages: []
  receiveOrder:
    action: receive
    channel: {$ref: '#/channels/orders'}
    messages: []
`)
	ok, err := New().Detect(repo)
	if err != nil || !ok {
		t.Fatalf("Detect = %v, %v", ok, err)
	}
	got := extract(t, repo)
	if len(got) != 2 {
		t.Fatalf("Extract got %d facts, want v3 send and receive: %+v", len(got), got)
	}
	for _, f := range got {
		if f.PropString("asyncapi_version") != "3.0" {
			t.Errorf("asyncapi_version = %q, want 3.0", f.PropString("asyncapi_version"))
		}
	}
}

func TestAsyncAPIMajor(t *testing.T) {
	for version, want := range map[string]int{
		"2": 2, "2.0": 2, "2.6.0": 2,
		"3": 3, "3.0": 3, "3.1.0": 3,
	} {
		if got := asyncAPIMajor(version); got != want {
			t.Errorf("asyncAPIMajor(%q) = %d, want %d", version, got, want)
		}
	}
}

func TestExtractV3JSONAndChannelRef(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "asyncapi.json", `{
  "asyncapi":"3.0.0",
  "servers":{"broker":{"host":"localhost","protocol":"mqtt"}},
  "channels":{"orderEvents":{"address":"orders/{orderId}","servers":[{"$ref":"#/servers/broker"}]}},
  "operations":{"consumeOrders":{"action":"receive","channel":{"$ref":"#/channels/orderEvents"},"messages":[{"$ref":"#/components/messages/Order"}]}}
}`)
	got := extract(t, repo)
	if len(got) != 1 {
		t.Fatalf("got %d facts: %+v", len(got), got)
	}
	f := got[0]
	if f.Name != "orders/{orderId}" || f.Props["messaging_role"] != facts.MessagingRoleConsumer || f.Props["operationId"] != "consumeOrders" || f.Props["messaging"] != "mqtt" || f.Props["message"] != "Order" {
		t.Errorf("fact = %+v", f)
	}
}

func TestExtractV3ExternalChannelResolvesServersInItsOwnDocument(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "asyncapi.yaml", `asyncapi: 3.0.0
info: {title: Orders, version: 1.0.0}
servers:
  broker: {host: mqtt.example, protocol: mqtt}
channels:
  orders: {$ref: './channels.yaml#/channels/orders'}
operations:
  publishOrder:
    action: send
    channel: {$ref: '#/channels/orders'}
`)
	writeSpec(t, repo, "channels.yaml", `servers:
  broker: {host: kafka.example:9092, protocol: kafka}
channels:
  orders:
    address: orders.created
    servers: [{$ref: '#/servers/broker'}]
`)

	got := extract(t, repo)
	topic := find(got, "orders.created", facts.MessagingRoleProducer)
	if topic == nil {
		t.Fatal("missing externally referenced channel operation")
	}
	if topic.Props["messaging"] != "kafka" {
		t.Fatalf("messaging = %v, want protocol from external channel document", topic.Props["messaging"])
	}
}

func TestMalformedSkippedAndContextCancellation(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "bad.yaml", "asyncapi: 2.6.0\nchannels: [broken")
	if got := extract(t, repo); len(got) != 0 {
		t.Fatalf("malformed spec emitted %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New().Extract(ctx, repo, nil); err != context.Canceled {
		t.Fatalf("cancel error = %v", err)
	}
}

func TestSkipsTestdata(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "testdata/fixture/asyncapi.yaml", asyncAPI2)
	ok, err := New().Detect(repo)
	if err != nil || ok {
		t.Fatalf("testdata detected: ok=%v err=%v", ok, err)
	}
}

func TestExternalRefsAndMessageSchemaIdentity(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "asyncapi.yaml", `asyncapi: 3.1.0
info: {title: Orders, version: 1.0.0}
servers:
  broker: {$ref: './parts/server.yaml#/broker'}
channels:
  orderCreated: {$ref: './parts/channel.yaml#/orderCreated'}
operations:
  publishOrder:
    action: send
    channel: {$ref: '#/channels/orderCreated'}
    messages: [{$ref: './parts/messages.yaml#/OrderCreated'}]
`)
	writeSpec(t, repo, "parts/server.yaml", `broker:
  host: kafka.example:9092
  protocol: kafka-secure
`)
	writeSpec(t, repo, "parts/channel.yaml", `orderCreated:
  address: svc-orders.order_created
  servers: [{$ref: '#/servers/broker'}]
`)
	writeSpec(t, repo, "parts/messages.yaml", `OrderCreated:
  name: OrderCreated
  contentType: application/json
  schemaFormat: application/schema+json;version=draft-07
  payload: {$ref: './schemas.yaml#/OrderPayload'}
`)
	writeSpec(t, repo, "parts/schemas.yaml", `OrderPayload:
  type: object
  required: [id]
  properties:
    id: {type: string, format: uuid}
    note: {type: string, description: Optional note}
    lines:
      type: array
      items: {$ref: '#/Line'}
Line:
  type: object
`)

	got := extract(t, repo)
	topic := find(got, "svc-orders.order_created", facts.MessagingRoleProducer)
	if topic == nil {
		t.Fatal("missing externally referenced channel operation")
	}
	if topic.Props["messaging"] != "kafka-secure" {
		t.Errorf("messaging = %v", topic.Props["messaging"])
	}
	schemaName := "parts/schemas.yaml#/messages/OrderCreated"
	if topic.Props["message_schema"] != schemaName {
		t.Errorf("message_schema = %v, want %s", topic.Props["message_schema"], schemaName)
	}
	if topic.PropString("message_schema_digest") == "" {
		t.Error("message_schema_digest must identify the resolved payload content")
	}
	if topic.Props["schema_format"] != "application/schema+json;version=draft-07" {
		t.Errorf("schema_format = %v", topic.Props["schema_format"])
	}
	for _, f := range got {
		if f.Kind == facts.KindSymbol {
			t.Fatalf("schema details must not create symbol noise: %+v", f)
		}
	}
}

func TestExternalRefCycleAndMissingFileAreSafe(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "asyncapi.yaml", `asyncapi: 2.6.0
info: {title: Cycles, version: 1.0.0}
channels:
  events:
    publish:
      message: {$ref: './messages.yaml#/A'}
  missing:
    subscribe:
      message: {$ref: './does-not-exist.yaml#/Missing'}
`)
	writeSpec(t, repo, "messages.yaml", `A: {$ref: '#/B'}
B: {$ref: '#/A'}
`)
	got := extract(t, repo)
	if find(got, "events", facts.MessagingRoleProducer) == nil || find(got, "missing", facts.MessagingRoleConsumer) == nil {
		t.Fatalf("unresolved message refs must not delete channel operations: %+v", got)
	}
	for _, f := range got {
		if f.Kind == facts.KindSymbol {
			t.Fatalf("cycle or missing ref emitted a schema symbol: %+v", f)
		}
	}
}

func TestExternalRefCannotEscapeRepository(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo")
	writeSpec(t, parent, "outside.yaml", `Secret:
  type: object
  properties: {token: {type: string}}
`)
	writeSpec(t, repo, "asyncapi.yaml", `asyncapi: 2.6.0
info: {title: Safe, version: 1.0.0}
channels:
  events:
    publish:
      message:
        name: MustStayInside
        payload: {$ref: '../outside.yaml#/Secret'}
`)
	got := extract(t, repo)
	if find(got, "events", facts.MessagingRoleProducer) == nil {
		t.Fatal("unsafe payload ref must not delete the channel operation")
	}
	for _, f := range got {
		if f.Kind == facts.KindSymbol {
			t.Fatalf("out-of-repository ref emitted schema data: %+v", f)
		}
	}
}

// A channel may $ref into a linked worktree nested in the repository: inside the root
// and present, but another checkout's file. It is not followed, so the operation keeps
// the channel's own name rather than the worktree's address; an in-repo ref still
// resolves.
func TestExternalRefIntoNestedWorktreeIsNotFollowed(t *testing.T) {
	repo := t.TempDir()
	writeSpec(t, repo, "asyncapi/asyncapi.yaml", `asyncapi: 3.0.0
info: {title: Events, version: 1.0.0}
channels:
  leaked:
    $ref: '../.claude/worktrees/feat/asyncapi/channels.yaml#/leaked'
  normal:
    $ref: './channels.yaml#/normal'
operations:
  sendLeaked:
    action: send
    channel: {$ref: '#/channels/leaked'}
  sendNormal:
    action: send
    channel: {$ref: '#/channels/normal'}
`)
	writeSpec(t, repo, "asyncapi/channels.yaml", "normal:\n  address: main.normal.topic\n")
	writeSpec(t, repo, ".git/worktrees/feat/commondir", "../..\n")
	writeSpec(t, repo, ".claude/worktrees/feat/.git", "gitdir: ../../../.git/worktrees/feat\n")
	writeSpec(t, repo, ".claude/worktrees/feat/asyncapi/channels.yaml", "leaked:\n  address: worktree.leaked.topic\n")

	got := extract(t, repo)
	if f := find(got, "worktree.leaked.topic", facts.MessagingRoleProducer); f != nil {
		t.Errorf("a channel address was read from a nested worktree: %+v", f)
	}
	if find(got, "leaked", facts.MessagingRoleProducer) == nil {
		t.Errorf("the operation on an unfollowed channel must keep the channel's name: %+v", got)
	}
	if find(got, "main.normal.topic", facts.MessagingRoleProducer) == nil {
		t.Errorf("the in-repo channel ref must still resolve: %+v", got)
	}
}
