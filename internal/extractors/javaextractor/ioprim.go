package javaextractor

import "strings"

// This file says what a call on a receiver of a LIBRARY type is. io.go seeds
// I/O from what this repository declares: a Spring Data repository, a Feign
// client, a Room DAO. A service that holds a JdbcTemplate, a RestTemplate or an
// EntityManager makes its round trips through a type the repository does not
// declare, and none of those were seeded, so the method that ran the statement
// carried no I/O and neither did anything that called it.
//
// It is the Java counterpart of goextractor/ioprim.go and pythonextractor/
// ioprim.go and follows their rule: the list is by MEMBER. `JdbcTemplate.update`
// is a statement and `JdbcTemplate.getDataSource` returns a field;
// `EntityManager.find` is a query and `EntityManager.createQuery` builds one
// that `Query.getResultList` runs.
//
// A call is matched by the declared type of its receiver (receivers.go), so only
// where the source states that type. A call on a call's result is not found
// here, and the name lists in the analyzer still read it.

// javaIOMembers maps a fully-qualified type to its methods that perform I/O. `*`
// is every method of the type but those in javaIONonMembers.
var javaIOMembers = map[string][]string{
	"org.springframework.jdbc.core.JdbcTemplate": {
		"execute", "query", "queryForObject", "queryForList", "queryForMap", "queryForRowSet",
		"queryForStream", "update", "batchUpdate", "call",
	},
	"org.springframework.jdbc.core.namedparam.NamedParameterJdbcTemplate": {
		"execute", "query", "queryForObject", "queryForList", "queryForMap", "queryForRowSet",
		"queryForStream", "update", "batchUpdate",
	},
	"org.springframework.jdbc.core.simple.JdbcClient": {"*"},
	// A transaction is a BEGIN and a COMMIT whatever its callback does.
	"org.springframework.transaction.support.TransactionTemplate": {"execute", "executeWithoutResult"},
	"org.springframework.web.client.RestTemplate": {
		"exchange", "execute", "getForObject", "getForEntity", "postForObject", "postForEntity",
		"postForLocation", "put", "delete", "patchForObject", "headForHeaders", "optionsForAllow",
	},
	"org.springframework.kafka.core.KafkaTemplate":                     {"send", "sendDefault", "flush", "executeInTransaction"},
	"org.springframework.data.repository.CrudRepository":               {"*"},
	"org.springframework.data.repository.PagingAndSortingRepository":   {"*"},
	"org.springframework.data.repository.ListCrudRepository":           {"*"},
	"org.springframework.data.jpa.repository.JpaRepository":            {"*"},
	"org.springframework.data.jpa.repository.JpaSpecificationExecutor": {"*"},
	"org.springframework.data.redis.connection.RedisConnection":        {"*"},
	"org.springframework.web.socket.WebSocketSession":                  {"sendMessage", "close"},
	"jakarta.persistence.EntityManager":                                javaEntityManagerIO,
	"javax.persistence.EntityManager":                                  javaEntityManagerIO,
	"jakarta.persistence.Query":                                        javaQueryIO,
	"jakarta.persistence.TypedQuery":                                   javaQueryIO,
	"javax.persistence.Query":                                          javaQueryIO,
	"javax.persistence.TypedQuery":                                     javaQueryIO,
	"java.sql.Connection":                                              {"prepareStatement", "prepareCall", "commit", "rollback", "getMetaData"},
	"java.sql.Statement":                                               javaStatementIO,
	"java.sql.PreparedStatement":                                       javaStatementIO,
	"java.sql.CallableStatement":                                       javaStatementIO,
	"java.sql.DriverManager":                                           {"getConnection"},
	"javax.sql.DataSource":                                             {"getConnection"},
	"java.net.http.HttpClient":                                         {"send", "sendAsync"},
	"org.apache.http.client.HttpClient":                                {"execute"},
	"org.apache.http.impl.client.CloseableHttpClient":                  {"execute"},
	"okhttp3.Call": {"execute", "enqueue"},
	"org.apache.kafka.clients.producer.Producer":       javaKafkaProducerIO,
	"org.apache.kafka.clients.producer.KafkaProducer":  javaKafkaProducerIO,
	"org.apache.kafka.clients.consumer.Consumer":       javaKafkaConsumerIO,
	"org.apache.kafka.clients.consumer.KafkaConsumer":  javaKafkaConsumerIO,
	"org.apache.kafka.clients.admin.Admin":             {"*"},
	"org.apache.kafka.clients.admin.AdminClient":       {"*"},
	"com.datastax.oss.driver.api.core.CqlSession":      {"execute", "executeAsync", "prepare", "prepareAsync"},
	"com.datastax.oss.driver.api.core.session.Session": {"execute", "executeAsync", "prepare", "prepareAsync"},
	"org.eclipse.paho.client.mqttv3.MqttClient":        javaMqttIO,
	"org.eclipse.paho.client.mqttv3.MqttAsyncClient":   javaMqttIO,
	"org.eclipse.paho.mqttv5.client.MqttClient":        javaMqttIO,
	"org.eclipse.paho.mqttv5.client.MqttAsyncClient":   javaMqttIO,
	"io.netty.channel.Channel":                         {"writeAndFlush", "flush"},
	"io.netty.channel.ChannelHandlerContext":           {"writeAndFlush", "flush"},
	"io.netty.channel.ChannelOutboundInvoker":          {"writeAndFlush", "flush"},
	"java.nio.file.Files": {
		"readAllBytes", "readAllLines", "readString", "lines", "write", "writeString", "exists",
		"notExists", "isDirectory", "isRegularFile", "isReadable", "isWritable", "size", "delete",
		"deleteIfExists", "copy", "move", "list", "walk", "walkFileTree", "find", "createDirectory",
		"createDirectories", "createFile", "createTempFile", "createTempDirectory", "newInputStream",
		"newOutputStream", "newBufferedReader", "newBufferedWriter", "newByteChannel",
		"getLastModifiedTime", "setLastModifiedTime", "probeContentType", "readAttributes",
	},
	"java.io.File": {
		"exists", "length", "delete", "mkdir", "mkdirs", "list", "listFiles", "isDirectory", "isFile",
		"createNewFile", "renameTo", "lastModified", "canRead", "canWrite", "createTempFile",
	},
}

var (
	javaEntityManagerIO = []string{"persist", "merge", "remove", "find", "flush", "refresh", "lock"}
	javaQueryIO         = []string{"getResultList", "getSingleResult", "getResultStream", "executeUpdate"}
	javaStatementIO     = []string{
		"execute", "executeQuery", "executeUpdate", "executeBatch", "executeLargeUpdate", "executeLargeBatch",
	}
	javaKafkaProducerIO = []string{"send", "flush", "commitTransaction", "initTransactions", "partitionsFor"}
	javaKafkaConsumerIO = []string{
		"poll", "commitSync", "commitAsync", "endOffsets", "beginningOffsets", "offsetsForTimes",
		"committed", "listTopics", "partitionsFor",
	}
	javaMqttIO = []string{"connect", "connectWithResult", "publish", "subscribe", "unsubscribe", "disconnect"}
)

// javaIONonMembers are the methods a `*` entry does not cover: what configures,
// closes or describes the client and sends nothing. Nor does it cover a bean
// setter, or a `…Commands` accessor, which hands out the object the command is
// then called on (`connection.stringCommands().get(key)`).
var javaIONonMembers = map[string]bool{
	"close": true, "isClosed": true, "isOpen": true, "toString": true, "hashCode": true, "equals": true,
	"getNativeConnection": true, "isPipelined": true, "isQueueing": true,
}

// javaPureTypes are library types that work on memory and nothing else, and
// javaPurePackages the packages every type of which does. A method of one is
// not I/O whatever it is called: `schema.getAllOf()` reads a field of an
// OpenAPI model, and `nodes.findValue(…)` walks a JSON tree already parsed.
//
// What is left out matters as much. `java.util.function.*` runs whatever it was
// handed, an executor runs a task, a `Properties` or a `Scanner` may read a
// stream: none of them says anything either way.
var javaPureTypes = map[string]bool{
	"java.util.List": true, "java.util.ArrayList": true, "java.util.LinkedList": true,
	"java.util.Map": true, "java.util.HashMap": true, "java.util.LinkedHashMap": true,
	"java.util.TreeMap": true, "java.util.Map.Entry": true, "java.util.Set": true,
	"java.util.HashSet": true, "java.util.LinkedHashSet": true, "java.util.TreeSet": true,
	"java.util.Collection": true, "java.util.Collections": true, "java.util.Arrays": true,
	"java.util.Optional": true, "java.util.Objects": true, "java.util.UUID": true,
	"java.util.Deque": true, "java.util.ArrayDeque": true, "java.util.StringJoiner": true,
	"java.util.Comparator": true, "java.util.BitSet": true, "java.util.EnumMap": true,
	"java.util.EnumSet": true, "java.util.Base64": true,
	"java.util.concurrent.ConcurrentHashMap": true, "java.util.concurrent.ConcurrentMap": true,
	"java.util.concurrent.CopyOnWriteArrayList":       true,
	"com.fasterxml.jackson.databind.JsonNode":         true,
	"com.google.gson.JsonObject":                      true,
	"com.google.gson.JsonElement":                     true,
	"com.google.gson.JsonArray":                       true,
	"com.google.gson.JsonPrimitive":                   true,
	"org.apache.commons.lang3.StringUtils":            true,
	"org.apache.commons.collections4.CollectionUtils": true,
}

var javaPurePackages = []string{
	"java.util.stream.", "java.util.concurrent.atomic.", "java.time.",
	"com.fasterxml.jackson.databind.node.", "com.google.common.collect.", "io.swagger.v3.oas.models.",
}

// What javaLibraryCall says of a call.
const (
	javaCallUnknown = iota // not a library type this file describes, or a member it is silent on
	javaCallIO             // performs I/O
	javaCallPure           // a method of a type that works on memory only
)

// javaLibraryCall classes a call by the declared type of its receiver, which the
// repository does not declare.
func javaLibraryCall(typ, method string) int {
	if members, ok := javaIOMembers[typ]; ok {
		for _, m := range members {
			if m == method || (m == "*" && !javaIONonMembers[method] && !javaAccessor(method) && !strings.HasSuffix(method, "Commands")) {
				return javaCallIO
			}
		}
		return javaCallUnknown
	}
	if javaPureTypes[typ] {
		return javaCallPure
	}
	for _, p := range javaPurePackages {
		if strings.HasPrefix(typ, p) {
			return javaCallPure
		}
	}
	return javaCallUnknown
}

// javaAccessor reports whether a method name is a bean setter: configuration of
// the client, which a `*` entry does not mean.
func javaAccessor(method string) bool {
	return len(method) > 3 && strings.HasPrefix(method, "set") && method[3] >= 'A' && method[3] <= 'Z'
}
