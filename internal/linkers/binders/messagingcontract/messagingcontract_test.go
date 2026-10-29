package messagingcontract

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func contract(topic, operation, id, file string) facts.Fact {
	return facts.Fact{Kind: facts.KindStorage, Name: topic, File: file, Repo: "orders", Props: map[string]any{
		"storage_kind": facts.StorageKindTopic, facts.PropSource: facts.MessagingSourceAsyncAPI,
		facts.PropMessagingOperation: operation, "operationId": id,
	}}
}

func code(topic, operation, symbol string, line int) facts.Fact {
	return facts.Fact{Kind: facts.KindStorage, Name: topic, File: "events.go", Line: line, Repo: "orders", Props: map[string]any{
		"storage_kind": facts.StorageKindTopic, facts.PropSource: facts.MessagingSourceGoKafkaCall,
		facts.PropMessagingOperation: operation, "code_symbol": symbol,
	}}
}

func TestBindUniqueOperation(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		contract("orders.created", facts.MessagingOperationPublish, "publishOrder", "asyncapi.yaml"),
		code("orders.created", facts.MessagingOperationPublish, "events.PublishOrder", 12),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	} // idempotence

	all := store.FactsRef()
	for _, f := range all {
		if isCodeOperation(f) {
			if f.Props[facts.PropMessagingContractBound] != true || f.Props[facts.PropMessagingContractOperationID] != "publishOrder" || f.Props[facts.PropMessagingContractFile] != "asyncapi.yaml" {
				t.Errorf("code binding props = %+v", f.Props)
			}
		}
		if isContractOperation(f) {
			if f.Props[facts.PropMessagingImplementationCount] != 1 {
				t.Errorf("contract props = %+v", f.Props)
			}
			if len(f.Relations) != 1 || f.Relations[0].Kind != facts.RelImplementedBy || f.Relations[0].Target != "events.PublishOrder" {
				t.Errorf("contract relations = %+v", f.Relations)
			}
		}
	}
}

func TestAmbiguousAndDirectionMismatchStayUnbound(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		contract("orders.created", facts.MessagingOperationPublish, "one", "one.yaml"),
		contract("orders.created", facts.MessagingOperationPublish, "two", "two.yaml"),
		contract("orders.created", facts.MessagingOperationSubscribe, "consume", "consume.yaml"),
		code("orders.created", facts.MessagingOperationPublish, "events.Publish", 10),
		code("orders.other", facts.MessagingOperationSubscribe, "events.ConsumeOther", 20),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	for _, f := range store.FactsRef() {
		if !isCodeOperation(f) {
			continue
		}
		if f.Props[facts.PropMessagingContractBound] == true {
			t.Errorf("ambiguous/unmatched code operation was bound: %+v", f)
		}
		want := facts.MessagingContractStatusAmbiguous
		if f.Name == "orders.other" {
			want = facts.MessagingContractStatusUndeclared
		}
		if f.PropString(facts.PropMessagingContractStatus) != want {
			t.Errorf("status = %q, want %q: %+v", f.PropString(facts.PropMessagingContractStatus), want, f)
		}
	}
}

func TestBindTypeScriptKafkaCall(t *testing.T) {
	call := code("orders.created", facts.MessagingOperationPublish, "src.publishOrder", 8)
	call.File = "events.ts"
	call.Props[facts.PropSource] = facts.MessagingSourceTSKafkaCall
	store := facts.NewStore()
	store.Add(
		contract("orders.created", facts.MessagingOperationPublish, "publishOrder", "asyncapi.yaml"),
		call,
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	for _, f := range store.FactsRef() {
		if f.PropString(facts.PropSource) == facts.MessagingSourceTSKafkaCall && f.Props[facts.PropMessagingContractBound] != true {
			t.Fatalf("TypeScript call not bound: %+v", f)
		}
	}
}

func TestProtocolCompatibility(t *testing.T) {
	tests := []struct {
		name             string
		contractProtocol string
		wantBound        bool
		wantStatus       string
	}{
		{name: "Kafka", contractProtocol: "kafka", wantBound: true, wantStatus: facts.MessagingContractStatusBound},
		{name: "Kafka secure", contractProtocol: "kafka-secure", wantBound: true, wantStatus: facts.MessagingContractStatusBound},
		{name: "unspecified", contractProtocol: "", wantBound: true, wantStatus: facts.MessagingContractStatusBound},
		{name: "MQTT", contractProtocol: "mqtt", wantStatus: facts.MessagingContractStatusProtocolMismatch},
		{name: "AMQP", contractProtocol: "amqp", wantStatus: facts.MessagingContractStatusProtocolMismatch},
		{name: "WebSocket", contractProtocol: "wss", wantStatus: facts.MessagingContractStatusProtocolMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			contractFact := contract("shared.events", facts.MessagingOperationPublish, "publishEvent", "asyncapi.yaml")
			if tt.contractProtocol != "" {
				contractFact.Props[facts.PropMessaging] = tt.contractProtocol
			}
			codeFact := code("shared.events", facts.MessagingOperationPublish, "events.Publish", 12)
			codeFact.Props[facts.PropMessaging] = "kafka"
			store := facts.NewStore()
			store.Add(contractFact, codeFact)
			if err := New().Bind(context.Background(), store); err != nil {
				t.Fatal(err)
			}
			bound := false
			status := ""
			for _, f := range store.FactsRef() {
				if isCodeOperation(f) {
					bound = f.Props[facts.PropMessagingContractBound] == true
					status = f.PropString(facts.PropMessagingContractStatus)
				}
			}
			if bound != tt.wantBound {
				t.Fatalf("bound = %v, want %v", bound, tt.wantBound)
			}
			if status != tt.wantStatus {
				t.Fatalf("status = %q, want %q", status, tt.wantStatus)
			}
		})
	}
}

func TestBoundCallWithoutSymbolStillImplementsContract(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		contract("orders.created", facts.MessagingOperationPublish, "publishOrder", "asyncapi.yaml"),
		code("orders.created", facts.MessagingOperationPublish, "", 12),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	for _, f := range store.FactsRef() {
		if !isContractOperation(f) {
			continue
		}
		if f.PropString(facts.PropMessagingImplementationStatus) != facts.MessagingImplementationImplemented || f.Props[facts.PropMessagingImplementationCount] != 1 {
			t.Fatalf("contract implementation props = %+v", f.Props)
		}
		if len(f.Relations) != 0 {
			t.Fatalf("anonymous implementation must not fabricate a symbol relation: %+v", f.Relations)
		}
	}
}

func TestProtocolDisambiguatesSameTopicOperations(t *testing.T) {
	kafkaContract := contract("shared.events", facts.MessagingOperationPublish, "publishKafka", "kafka.yaml")
	kafkaContract.Props[facts.PropMessaging] = "kafka-secure"
	mqttContract := contract("shared.events", facts.MessagingOperationPublish, "publishMQTT", "mqtt.yaml")
	mqttContract.Props[facts.PropMessaging] = "mqtt"
	codeFact := code("shared.events", facts.MessagingOperationPublish, "events.Publish", 12)
	codeFact.Props[facts.PropMessaging] = "kafka"
	store := facts.NewStore()
	store.Add(kafkaContract, mqttContract, codeFact)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	for _, f := range store.FactsRef() {
		if isCodeOperation(f) {
			if f.Props[facts.PropMessagingContractBound] != true || f.PropString(facts.PropMessagingContractOperationID) != "publishKafka" {
				t.Fatalf("Kafka call binding = %+v", f.Props)
			}
		}
		if isContractOperation(f) && f.PropString("operationId") == "publishMQTT" && f.Props[facts.PropMessagingImplementationCount] != 0 {
			t.Fatalf("MQTT contract received Kafka implementation: %+v", f)
		}
	}
}

func TestBundledDuplicateSpecDoesNotFabricateAmbiguity(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		contract("orders.created", facts.MessagingOperationPublish, "publishOrder", "asyncapi.yaml"),
		contract("orders.created", facts.MessagingOperationPublish, "publishOrder", "bundle/asyncapi.yaml"),
		code("orders.created", facts.MessagingOperationPublish, "events.PublishOrder", 12),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	for _, f := range store.FactsRef() {
		if isCodeOperation(f) {
			if f.PropString(facts.PropMessagingContractStatus) != facts.MessagingContractStatusBound {
				t.Fatalf("identical bundled declaration must still bind cleanly: %+v", f.Props)
			}
			if f.Props[facts.PropMessagingContractCandidates] != 1 {
				t.Fatalf("bundled duplicate must not count as a second candidate: %+v", f.Props)
			}
		}
		if isContractOperation(f) {
			if _, dup := f.Props[facts.PropMessagingDuplicateOf]; dup {
				t.Fatalf("identical declaration must not be flagged as a conflict: %+v", f.Props)
			}
		}
	}
}

func TestBundledDuplicateUsesOneUnimplementedCanonical(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		contract("orders.created", facts.MessagingOperationPublish, "publishOrder", "asyncapi.yaml"),
		contract("orders.created", facts.MessagingOperationPublish, "publishOrder", "bundle/asyncapi.yaml"),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	canonical, copies := 0, 0
	for _, f := range store.FactsRef() {
		if !isContractOperation(f) {
			continue
		}
		if f.PropString(facts.PropMessagingCanonicalFile) == "" {
			canonical++
		} else {
			copies++
		}
	}
	if canonical != 1 || copies != 1 {
		t.Fatalf("canonical=%d copies=%d, want 1 and 1", canonical, copies)
	}
}

func TestSameSchemaNameWithDifferentDigestConflicts(t *testing.T) {
	first := contract("orders.created", facts.MessagingOperationPublish, "publishOrder", "one.yaml")
	second := contract("orders.created", facts.MessagingOperationPublish, "publishOrder", "two.yaml")
	first.Props["message_schema"] = "schemas.yaml#/messages/Order"
	second.Props["message_schema"] = "schemas.yaml#/messages/Order"
	first.Props["message_schema_digest"] = "digest-one"
	second.Props["message_schema_digest"] = "digest-two"
	store := facts.NewStore()
	store.Add(first, second, code("orders.created", facts.MessagingOperationPublish, "events.PublishOrder", 12))
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	for _, f := range store.FactsRef() {
		if isCodeOperation(f) && f.PropString(facts.PropMessagingContractStatus) != facts.MessagingContractStatusAmbiguous {
			t.Fatalf("different schema payloads must remain ambiguous: %+v", f.Props)
		}
	}
}

func TestEquivalentCopyInheritsConflicts(t *testing.T) {
	a := contract("orders.created", facts.MessagingOperationPublish, "publishOrder", "a.yaml")
	b := contract("orders.created", facts.MessagingOperationPublish, "different", "b.yaml")
	c := contract("orders.created", facts.MessagingOperationPublish, "publishOrder", "c.yaml")
	store := facts.NewStore()
	store.Add(a, b, c)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	for _, f := range store.FactsRef() {
		if !isContractOperation(f) || f.File != "c.yaml" {
			continue
		}
		conflicts, _ := f.Props[facts.PropMessagingDuplicateOf].([]string)
		if len(conflicts) != 1 || conflicts[0] != "b.yaml" {
			t.Fatalf("copy conflicts = %v, want [b.yaml]", conflicts)
		}
	}
}

func TestConflictingContractDeclarationIsFlaggedAndStaysAmbiguous(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		contract("orders.created", facts.MessagingOperationPublish, "publishOrderV1", "old.yaml"),
		contract("orders.created", facts.MessagingOperationPublish, "publishOrderV2", "new.yaml"),
		code("orders.created", facts.MessagingOperationPublish, "events.PublishOrder", 12),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	seen := map[string][]string{}
	for _, f := range store.FactsRef() {
		if isCodeOperation(f) && f.PropString(facts.PropMessagingContractStatus) != facts.MessagingContractStatusAmbiguous {
			t.Fatalf("conflicting declarations must stay ambiguous: %+v", f.Props)
		}
		if isContractOperation(f) {
			dup, _ := f.Props[facts.PropMessagingDuplicateOf].([]string)
			seen[f.PropString("operationId")] = dup
		}
	}
	if len(seen["publishOrderV1"]) != 1 || seen["publishOrderV1"][0] != "new.yaml" {
		t.Errorf("publishOrderV1 duplicate-of = %v, want [new.yaml]", seen["publishOrderV1"])
	}
	if len(seen["publishOrderV2"]) != 1 || seen["publishOrderV2"][0] != "old.yaml" {
		t.Errorf("publishOrderV2 duplicate-of = %v, want [old.yaml]", seen["publishOrderV2"])
	}
}

// A channel parameter on either side stands for one segment: code publishing to
// "orders.{region}.created" implements the contract written for "orders.eu.created",
// and a parameterized contract is implemented by code naming one region. Two
// concrete contracts the parameter could stand for leave it ambiguous.
func TestBindChannelParameters(t *testing.T) {
	store := facts.NewStore()
	store.Add(
		contract("orders.eu.created", facts.MessagingOperationPublish, "created", "a.yaml"),
		code("orders.{region}.created", facts.MessagingOperationPublish, "events.Created", 1),
		contract("orders.{region}.paid", facts.MessagingOperationPublish, "paid", "a.yaml"),
		code("orders.eu.paid", facts.MessagingOperationPublish, "events.Paid", 2),
		contract("orders.eu.shipped", facts.MessagingOperationPublish, "shippedEU", "a.yaml"),
		contract("orders.us.shipped", facts.MessagingOperationPublish, "shippedUS", "b.yaml"),
		code("orders.{region}.shipped", facts.MessagingOperationPublish, "events.Shipped", 3),
		code("orders.{region}.created.v2", facts.MessagingOperationPublish, "events.CreatedV2", 4),
	)
	if err := New().Bind(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"events.Created":   facts.MessagingContractStatusBound,
		"events.Paid":      facts.MessagingContractStatusBound,
		"events.Shipped":   facts.MessagingContractStatusAmbiguous,
		"events.CreatedV2": facts.MessagingContractStatusUndeclared,
	}
	for _, f := range store.FactsRef() {
		if isCodeOperation(f) {
			if got := f.PropString(facts.PropMessagingContractStatus); got != want[f.PropString("code_symbol")] {
				t.Errorf("%s: status %q, want %q", f.PropString("code_symbol"), got, want[f.PropString("code_symbol")])
			}
		}
	}
}
