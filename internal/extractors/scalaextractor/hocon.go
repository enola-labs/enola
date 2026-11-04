package scalaextractor

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linescan"
)

// Kafka topics named in HOCON configuration.
//
// A JVM stream job (Flink, Kafka Streams, Akka) rarely writes its topics in code. It
// reads them from application.conf through Typesafe Config or pureconfig:
//
//	sources {
//	  item-events {
//	    bootstrap-servers = ${?KAFKA_BOOTSTRAP_SERVERS}
//	    topic = "shop.items.item_created"
//	  }
//	}
//
// The code holds a KafkaSource and a config case class, and no topic name at all, so
// the job looked isolated. The configuration of a module that runs Kafka is read for
// `topic`/`topics` keys; the block a key sits in says which way it flows when its name
// does (sources, input, consumer against sinks, output, producer).

// hoconTopics emits one topic fact per topic a module's configuration names: the
// .conf files under src/main/resources of each module the Scala files belong to.
func hoconTopics(repoPath string, scalaFiles []string) []facts.Fact {
	roots := map[string]string{} // module root -> its first source directory, as declared
	for _, f := range scalaFiles {
		i := strings.Index(f, "/src/main/")
		if i < 0 && strings.HasPrefix(f, "src/main/") {
			i = 0
		}
		if i < 0 {
			continue
		}
		root, dir := f[:i], factpath.Dir(f)
		if prev, ok := roots[root]; !ok || dir < prev {
			roots[root] = dir
		}
	}
	var out []facts.Fact
	for _, root := range sortedRoots(roots) {
		res := filepath.Join(repoPath, filepath.FromSlash(root), "src", "main", "resources")
		seen := map[string]bool{}
		_ = filepath.WalkDir(res, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".conf") {
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil || !kafkaConfig(src) {
				return nil
			}
			rel, err := filepath.Rel(repoPath, p)
			if err != nil {
				return nil
			}
			relFile := factpath.Slash(rel)
			for _, t := range scanHOCONTopics(src) {
				if seen[t.name] {
					continue
				}
				seen[t.name] = true
				props := map[string]any{
					"language":          "hocon",
					"storage_kind":      facts.StorageKindTopic,
					facts.PropMessaging: "kafka",
					facts.PropSource:    "hocon-config",
					"declared_as":       t.path,
				}
				if t.role != "" {
					props[facts.PropMessagingRole] = t.role
				}
				out = append(out, facts.Fact{
					Kind: facts.KindStorage, Name: t.name, File: relFile, Line: t.line, Props: props,
					Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: roots[root]}},
				})
			}
			return nil
		})
	}
	return out
}

// kafkaConfig reports a configuration file that talks to Kafka: one that names its
// bootstrap servers or Kafka itself. A `topic` key in any other file is a domain word.
func kafkaConfig(src []byte) bool {
	lower := bytes.ToLower(src)
	return bytes.Contains(lower, []byte("bootstrap")) || bytes.Contains(lower, []byte("kafka"))
}

type hoconTopic struct {
	name, path, role string
	line             int
}

// scanHOCONTopics reads the `topic = "…"` and `topics = ["…", …]` values of a HOCON
// file with the block keys around each. A substituted value (${?VAR}) names no topic.
// It is a scanner, not a parser: one key per line, braces opening and closing blocks.
func scanHOCONTopics(src []byte) []hoconTopic {
	var out []hoconTopic
	var stack []string
	sc := linescan.New(bytes.NewReader(src))
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(stripHOCONComment(sc.Text()))
		if text == "" {
			continue
		}
		for strings.HasPrefix(text, "}") {
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			text = strings.TrimSpace(strings.TrimPrefix(text, "}"))
		}
		if text == "" {
			continue
		}
		key, value, hasSep := cutHOCONKey(text)
		if strings.HasSuffix(text, "{") {
			if !hasSep {
				key = strings.TrimSpace(strings.TrimSuffix(text, "{"))
			}
			stack = append(stack, strings.Trim(key, `"`))
			continue
		}
		if !hasSep {
			continue
		}
		k := strings.ToLower(strings.Trim(key, `"`))
		if k != "topic" && k != "topics" && k != "topic-name" && k != "topic_name" {
			continue
		}
		for _, name := range hoconStrings(value) {
			if !plausibleTopicName(name) {
				continue
			}
			out = append(out, hoconTopic{name: name, path: strings.Join(append(append([]string{}, stack...), key), "."), role: hoconRole(stack), line: line})
		}
	}
	return out
}

func stripHOCONComment(s string) string {
	inQuote := false
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '"':
			inQuote = !inQuote
		case !inQuote && s[i] == '#':
			return s[:i]
		case !inQuote && s[i] == '/' && i+1 < len(s) && s[i+1] == '/':
			return s[:i]
		}
	}
	return s
}

// cutHOCONKey splits `key = value`, `key: value` and `key = {`.
func cutHOCONKey(text string) (key, value string, ok bool) {
	i := strings.IndexAny(text, "=:")
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(text[:i]), strings.TrimSpace(text[i+1:]), true
}

// hoconStrings returns the quoted strings of a value: one, or a list of them.
func hoconStrings(value string) []string {
	if strings.Contains(value, "${") {
		return nil
	}
	var out []string
	for {
		i := strings.IndexByte(value, '"')
		if i < 0 {
			break
		}
		j := strings.IndexByte(value[i+1:], '"')
		if j < 0 {
			break
		}
		out = append(out, value[i+1:i+1+j])
		value = value[i+j+2:]
	}
	return out
}

// hoconRole reads which way a topic flows from the blocks around it.
func hoconRole(stack []string) string {
	for i := len(stack) - 1; i >= 0; i-- {
		k := strings.ToLower(stack[i])
		switch {
		case strings.Contains(k, "source") || strings.Contains(k, "input") || strings.Contains(k, "consumer"):
			return facts.MessagingRoleConsumer
		case strings.Contains(k, "sink") || strings.Contains(k, "output") || strings.Contains(k, "producer"):
			return facts.MessagingRoleProducer
		}
	}
	return ""
}

func sortedRoots(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
