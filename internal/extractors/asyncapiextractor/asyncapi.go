// Package asyncapiextractor reads AsyncAPI contracts as messaging topic facts.
package asyncapiextractor

import (
	"context"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/workspace"
	"gopkg.in/yaml.v3"
)

// Extractor extracts channels and operations from AsyncAPI 2.x and 3.x specs.
// It walks the repository itself because YAML and JSON are excluded from the
// engine's normal source-file walk.
type Extractor struct{}

func New() *Extractor             { return &Extractor{} }
func (e *Extractor) Name() string { return "asyncapi" }

func (e *Extractor) Detect(repoPath string) (bool, error) {
	found := false
	err := filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return err
		}
		if d.IsDir() {
			if skipDir(d.Name()) || (path != repoPath && workspace.IsLinkedWorktree(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if isCandidate(path) && hasAsyncAPIRoot(path) {
			found = true
		}
		return nil
	})
	return found, err
}

func (e *Extractor) Extract(ctx context.Context, repoPath string, _ []string) ([]facts.Fact, error) {
	var result []facts.Fact
	err := filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if d.IsDir() {
			if skipDir(d.Name()) || (path != repoPath && workspace.IsLinkedWorktree(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isCandidate(path) || !hasAsyncAPIContent(path) {
			return nil
		}
		rawRel, relErr := filepath.Rel(repoPath, path)
		if relErr != nil {
			return nil
		}
		rel := factpath.Slash(rawRel)
		ff, parseErr := parseFile(path, rel, repoPath)
		if parseErr != nil {
			log.Printf("[asyncapi-extractor] skipping %s: %v", rel, parseErr)
			return nil
		}
		result = append(result, ff...)
		return nil
	})
	return result, err
}

func parseFile(absPath, relFile, repoPath string) ([]facts.Fact, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing yaml/json: %w", err)
	}
	version := stringValue(doc["asyncapi"])
	if version == "" {
		return nil, fmt.Errorf("not an AsyncAPI spec (missing asyncapi field)")
	}

	resolver := newRefResolver(repoPath, absPath, doc)
	channels := mapValue(doc["channels"])
	servers := serverProtocols(mapValue(doc["servers"]), absPath, resolver)
	defaultContentType := stringValue(doc["defaultContentType"])
	var operations []operation
	if asyncAPIMajor(version) == 3 {
		operations = operationsV3(doc, absPath, resolver)
	} else {
		operations = operationsV2(channels, absPath, resolver)
	}

	specDir := factpath.Dir(relFile)
	seen := map[string]bool{}
	result := make([]facts.Fact, 0, len(operations))
	for _, op := range operations {
		if op.channel == "" || op.role == "" {
			continue
		}
		key := op.channel + "\x00" + op.role + "\x00" + op.id
		if seen[key] {
			continue
		}
		seen[key] = true
		props := map[string]any{
			"storage_kind": facts.StorageKindTopic, facts.PropMessagingRole: op.role,
			"asyncapi_action": op.action, "asyncapi_version": version,
			facts.PropMessagingOperation: operationForRole(op.role),
			facts.PropSource:             facts.MessagingSourceAsyncAPI, "language": "asyncapi", "spec_file": relFile,
		}
		protocol := op.protocol
		if protocol == "" {
			protocol = protocolFor(op.serverNames, servers)
		}
		if protocol != "" {
			props[facts.PropMessaging] = protocol
		}
		optional(props, "operationId", op.id)
		optional(props, "summary", op.summary)
		optional(props, "description", op.description)
		if len(op.tags) > 0 {
			props["tags"] = op.tags
		}
		optional(props, "message", op.message)
		optional(props, "message_schema", op.messageInfo.schemaName)
		optional(props, "message_schema_digest", op.messageInfo.schemaDigest)
		optional(props, "schema_format", op.messageInfo.schemaFormat)
		contentType := op.contentType
		if contentType == "" {
			contentType = defaultContentType
		}
		optional(props, "content_type", contentType)
		result = append(result, facts.Fact{
			Kind: facts.KindStorage, Name: op.channel, File: relFile, Props: props,
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: specDir}},
		})
	}
	return result, nil
}

func operationForRole(role string) string {
	if role == facts.MessagingRoleProducer {
		return facts.MessagingOperationPublish
	}
	if role == facts.MessagingRoleConsumer {
		return facts.MessagingOperationSubscribe
	}
	return ""
}

type operation struct {
	channel, role, action, id, summary, description, message, contentType, protocol string
	tags, serverNames                                                               []string
	messageInfo                                                                     messageInfo
}

func operationsV2(channels map[string]any, baseFile string, resolver *refResolver) []operation {
	var out []operation
	for channelName, raw := range channels {
		channel := resolver.resolve(raw, baseFile)
		if len(channel.value) == 0 {
			continue
		}
		serverNames := stringList(channel.value["servers"])
		for _, action := range []string{"publish", "subscribe"} {
			opResolved := resolver.resolve(channel.value[action], channel.absFile)
			opMap := opResolved.value
			if len(opMap) == 0 {
				continue
			}
			role := facts.MessagingRoleProducer
			if action == "subscribe" {
				role = facts.MessagingRoleConsumer
			}
			out = append(out, makeOperation(channelName, role, action, opMap, serverNames, opResolved.absFile, resolver))
		}
	}
	return out
}

func operationsV3(doc map[string]any, baseFile string, resolver *refResolver) []operation {
	var out []operation
	for operationID, raw := range mapValue(doc["operations"]) {
		opResolved := resolver.resolve(raw, baseFile)
		opMap := opResolved.value
		action := strings.ToLower(stringValue(opMap["action"]))
		role := ""
		switch action {
		case "send":
			role = facts.MessagingRoleProducer
		case "receive":
			role = facts.MessagingRoleConsumer
		}
		channelRaw := opMap["channel"]
		channelRef := mapValue(channelRaw)
		channelKey := localRefName(stringValue(channelRef["$ref"]), "channels")
		channel := resolver.resolve(channelRaw, opResolved.absFile)
		channelName := stringValue(channel.value["address"])
		if channelName == "" {
			channelName = channelKey
		}
		op := makeOperation(channelName, role, action, opMap, refNames(channel.value["servers"], "servers"), opResolved.absFile, resolver)
		op.protocol = referencedServerProtocol(channel.value["servers"], channel.absFile, resolver)
		if op.id == "" {
			op.id = operationID
		}
		out = append(out, op)
	}
	return out
}

// referencedServerProtocol resolves AsyncAPI 3 Server Reference Objects from
// the document that contains the channel. This matters for external channel
// files: a local #/servers/... reference belongs to that file, not the root
// AsyncAPI document. If several referenced servers use different protocols,
// leave the protocol unspecified rather than choosing one arbitrarily.
func referencedServerProtocol(raw any, baseFile string, resolver *refResolver) string {
	protocols := map[string]bool{}
	for _, item := range sliceValue(raw) {
		server := resolver.resolve(item, baseFile)
		if protocol := strings.ToLower(stringValue(server.value["protocol"])); protocol != "" {
			protocols[protocol] = true
		}
	}
	if len(protocols) != 1 {
		return ""
	}
	for protocol := range protocols {
		return protocol
	}
	return ""
}

func makeOperation(channel, role, action string, raw map[string]any, servers []string, baseFile string, resolver *refResolver) operation {
	op := operation{channel: channel, role: role, action: action, id: stringValue(raw["operationId"]),
		summary: stringValue(raw["summary"]), description: stringValue(raw["description"]),
		tags: tagNames(raw["tags"]), serverNames: servers}
	var messageRaw = raw["message"]
	if messageRaw == nil {
		if messages := sliceValue(raw["messages"]); len(messages) > 0 {
			messageRaw = messages[0]
		}
	}
	op.messageInfo = messageInfoFor(messageRaw, baseFile, resolver)
	op.message, op.contentType = op.messageInfo.name, op.messageInfo.contentType
	return op
}

func serverProtocols(raw map[string]any, baseFile string, resolver *refResolver) map[string]string {
	out := make(map[string]string, len(raw))
	for name, value := range raw {
		server := resolver.resolve(value, baseFile)
		out[name] = strings.ToLower(stringValue(server.value["protocol"]))
	}
	return out
}

func protocolFor(names []string, protocols map[string]string) string {
	for _, name := range names {
		if protocols[name] != "" {
			return protocols[name]
		}
	}
	unique := map[string]bool{}
	for _, protocol := range protocols {
		if protocol != "" {
			unique[protocol] = true
		}
	}
	if len(unique) == 1 {
		for protocol := range unique {
			return protocol
		}
	}
	return ""
}

func tagNames(raw any) []string {
	var out []string
	for _, item := range sliceValue(raw) {
		if name := stringValue(mapValue(item)["name"]); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func refNames(raw any, section string) []string {
	var out []string
	for _, item := range sliceValue(raw) {
		if name := localRefName(stringValue(mapValue(item)["$ref"]), section); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func localRefName(ref, section string) string {
	prefix := "#/" + section + "/"
	if !strings.HasPrefix(ref, prefix) {
		return ""
	}
	return strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(ref, prefix), "~1", "/"), "~0", "~")
}
func mapValue(v any) map[string]any { m, _ := v.(map[string]any); return m }
func sliceValue(v any) []any        { s, _ := v.([]any); return s }
func stringValue(v any) string {
	switch value := v.(type) {
	case string:
		return value
	case float64:
		formatted := strconv.FormatFloat(value, 'f', -1, 64)
		// YAML decodes an unquoted semantic version such as 3.0 as the number 3.
		// Restore a minor component so version dispatch still recognizes it as 3.x.
		if !strings.Contains(formatted, ".") {
			formatted += ".0"
		}
		return formatted
	case int:
		return strconv.Itoa(value)
	default:
		return ""
	}
}

func asyncAPIMajor(version string) int {
	major, _, _ := strings.Cut(strings.TrimSpace(version), ".")
	n, _ := strconv.Atoi(major)
	return n
}
func stringList(v any) []string {
	var out []string
	for _, item := range sliceValue(v) {
		if s := stringValue(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}
func optional(props map[string]any, key, value string) {
	if value != "" {
		props[key] = value
	}
}

func isCandidate(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml", ".json":
		return true
	default:
		return false
	}
}

func hasAsyncAPIContent(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	content := string(buf[:n])
	return strings.Contains(content, "asyncapi:") || strings.Contains(content, `"asyncapi"`)
}

func hasAsyncAPIRoot(path string) bool {
	if !hasAsyncAPIContent(path) {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var probe struct {
		AsyncAPI string `yaml:"asyncapi"`
	}
	return yaml.Unmarshal(data, &probe) == nil && probe.AsyncAPI != ""
}

func skipDir(name string) bool {
	switch name {
	case "vendor", "node_modules", ".git", ".enola", "backstage", "tmp", "log", "build", ".build", ".gradle", "testdata":
		return true
	default:
		return false
	}
}
