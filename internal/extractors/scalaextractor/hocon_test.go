package scalaextractor

import (
	"context"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A stream job's topics are read from the configuration of its module: the block a
// topic sits in says which way it flows, a substituted value names none, a
// configuration that does not talk to Kafka is not read, and a topic named twice in
// one module is one fact.
func TestHOCONTopicsOfAStreamModule(t *testing.T) {
	root := writePlayRepo(t, map[string]string{
		"writer/src/main/scala/shop/writer/Job.scala": "package shop.writer\n\nobject Job { def main(a: Array[String]): Unit = () }\n",
		"writer/src/main/resources/application.conf": `{
  # where events come from
  sources = {
    orders = {
      bootstrap-servers = ${?KAFKA_BOOTSTRAP_SERVERS}
      topic = "shop.orders.order_placed"
    }
    prefixed = {
      topic = ${?ORDERS_TOPIC}
    }
  }
  sinks {
    documents {
      topics = ["search.documents", "search.documents_audit"]
    }
  }
}
`,
		"writer/src/main/resources/profile/local.conf": `sources { orders { topic = "shop.orders.order_placed", bootstrap-servers = "localhost:9092" } }
`,
		"forum/src/main/scala/shop/forum/Board.scala": "package shop.forum\n\nobject Board\n",
		"forum/src/main/resources/application.conf": `board { topic = "general" }
`,
	})
	files := []string{"writer/src/main/scala/shop/writer/Job.scala", "forum/src/main/scala/shop/forum/Board.scala"}
	ff, err := New().Extract(context.Background(), root, files)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range ff {
		if f.Kind == facts.KindStorage && f.PropString("storage_kind") == facts.StorageKindTopic {
			if _, dup := got[f.Name]; dup {
				t.Errorf("topic %s emitted twice", f.Name)
			}
			got[f.Name] = f.PropString(facts.PropMessagingRole)
		}
	}
	want := map[string]string{
		"shop.orders.order_placed": facts.MessagingRoleConsumer,
		"search.documents":         facts.MessagingRoleProducer,
		"search.documents_audit":   facts.MessagingRoleProducer,
	}
	for name, role := range want {
		if r, ok := got[name]; !ok || r != role {
			t.Errorf("topic %s role = %q (present %v), want %q", name, r, ok, role)
		}
	}
	if len(got) != len(want) {
		t.Errorf("topics = %v, want %v", got, want)
	}
}
