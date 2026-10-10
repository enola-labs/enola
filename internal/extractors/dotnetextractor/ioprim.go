package dotnetextractor

import "strings"

// This file says what a call on a receiver of a LIBRARY type is. The name lists
// in helpers.go read a call by its method name on any receiver, which finds
// `client.GetAsync(…)` and cannot tell `query.ToList()` on an EF queryable from
// the same call on a list. Here the receiver's declared type is known
// (receivers.go) and the repository does not declare it, so the call is the
// library's member.
//
// It is the C# counterpart of goextractor/ioprim.go and follows its rule: the
// list is by MEMBER. `IQueryable.ToListAsync` runs the query and
// `IQueryable.Where` adds a clause to one nobody has run.
//
// A type is named as the source writes it, by its simple name: the walker has no
// assembly to ask. A repository that declares a type of the same name resolves
// the call to its own declaration first, and this file is not consulted.

// csIOMembers maps a library type's simple name to its methods that perform I/O.
var csIOMembers = map[string][]string{
	"HttpClient": {
		"GetAsync", "PostAsync", "PutAsync", "DeleteAsync", "PatchAsync", "SendAsync", "Send",
		"GetStringAsync", "GetStreamAsync", "GetByteArrayAsync", "GetFromJsonAsync", "PostAsJsonAsync",
		"PutAsJsonAsync", "PatchAsJsonAsync", "DeleteFromJsonAsync",
	},
	// The calls that run a query. Everything else on a queryable composes one.
	"IQueryable":            csQueryRuns,
	"IOrderedQueryable":     csQueryRuns,
	"DbSet":                 append([]string{"Find", "FindAsync"}, csQueryRuns...),
	"DbContext":             {"SaveChanges", "SaveChangesAsync", "Find", "FindAsync"},
	"DbCommand":             csCommandIO,
	"SqlCommand":            csCommandIO,
	"SqliteCommand":         csCommandIO,
	"NpgsqlCommand":         csCommandIO,
	"MySqlCommand":          csCommandIO,
	"IDbCommand":            csCommandIO,
	"DbConnection":          csConnectionIO,
	"SqlConnection":         csConnectionIO,
	"SqliteConnection":      csConnectionIO,
	"NpgsqlConnection":      csConnectionIO,
	"MySqlConnection":       csConnectionIO,
	"IDbConnection":         csConnectionIO,
	"DbTransaction":         {"Commit", "CommitAsync", "Rollback", "RollbackAsync"},
	"IDbContextTransaction": {"Commit", "CommitAsync", "Rollback", "RollbackAsync"},
	"FileStream": {
		"Read", "ReadAsync", "ReadExactly", "ReadExactlyAsync", "ReadAtLeast", "ReadAtLeastAsync", "Write",
		"WriteAsync", "Flush", "FlushAsync", "CopyTo", "CopyToAsync",
	},
	"NetworkStream": {"Read", "ReadAsync", "Write", "WriteAsync", "Flush", "FlushAsync", "CopyTo", "CopyToAsync"},
	"Process":       {"Start", "WaitForExit", "WaitForExitAsync", "Kill"},
	"TcpClient":     {"Connect", "ConnectAsync"},
	"UdpClient":     {"Send", "SendAsync", "Receive", "ReceiveAsync"},
	"Socket": {
		"Connect", "ConnectAsync", "Send", "SendAsync", "SendTo", "SendToAsync", "Receive", "ReceiveAsync",
		"ReceiveFrom", "ReceiveFromAsync", "Accept", "AcceptAsync",
	},
	"WebSocket":       {"SendAsync", "ReceiveAsync", "CloseAsync", "CloseOutputAsync"},
	"ClientWebSocket": {"ConnectAsync", "SendAsync", "ReceiveAsync", "CloseAsync", "CloseOutputAsync"},
	"DirectoryInfo": {
		"EnumerateFiles", "EnumerateDirectories", "EnumerateFileSystemInfos", "GetFiles", "GetDirectories",
		"GetFileSystemInfos", "Create", "Delete", "MoveTo", "Refresh",
	},
	"FileInfo": {
		"Open", "OpenRead", "OpenWrite", "OpenText", "Create", "CreateText", "AppendText", "Delete",
		"CopyTo", "MoveTo", "Replace", "Refresh",
	},
	"SmtpClient": {"Send", "SendAsync", "SendMailAsync", "Connect", "ConnectAsync"},
}

var (
	csQueryRuns = []string{
		"ToList", "ToListAsync", "ToArray", "ToArrayAsync", "ToDictionary", "ToDictionaryAsync", "ToHashSet",
		"ToHashSetAsync", "First", "FirstAsync", "FirstOrDefault", "FirstOrDefaultAsync", "Single",
		"SingleAsync", "SingleOrDefault", "SingleOrDefaultAsync", "Last", "LastAsync", "LastOrDefault",
		"LastOrDefaultAsync", "Any", "AnyAsync", "All", "AllAsync", "Count", "CountAsync", "LongCount",
		"LongCountAsync", "Min", "MinAsync", "Max", "MaxAsync", "Sum", "SumAsync", "Average", "AverageAsync",
		"Contains", "ContainsAsync", "Load", "LoadAsync", "ForEachAsync", "ExecuteDelete",
		"ExecuteDeleteAsync", "ExecuteUpdate", "ExecuteUpdateAsync",
	}
	csCommandIO = []string{
		"ExecuteReader", "ExecuteReaderAsync", "ExecuteNonQuery", "ExecuteNonQueryAsync", "ExecuteScalar",
		"ExecuteScalarAsync", "Prepare", "PrepareAsync",
	}
	csConnectionIO = []string{
		"Open", "OpenAsync", "BeginTransaction", "BeginTransactionAsync", "Execute", "ExecuteAsync", "Query",
		"QueryAsync", "QueryFirst", "QueryFirstAsync", "QueryFirstOrDefault", "QueryFirstOrDefaultAsync",
		"QuerySingle", "QuerySingleAsync", "QuerySingleOrDefault", "QuerySingleOrDefaultAsync",
		"ExecuteScalar", "ExecuteScalarAsync",
	}
)

// csPureTypes are library types that work on memory and nothing else. A method
// of one is not I/O whatever it is called: `candidates.Find(…)` on a List
// searches it, and `_logger.LogError(…)` reports on the program and is not its
// work.
//
// A Stream, a TextReader and an XmlWriter are absent on purpose. Each reads or
// writes whatever it wraps, a file or a buffer, and says nothing either way.
var csPureTypes = map[string]bool{
	"List": true, "IList": true, "IReadOnlyList": true, "ICollection": true, "IReadOnlyCollection": true,
	"IEnumerable": true, "IOrderedEnumerable": true, "Dictionary": true, "IDictionary": true,
	"IReadOnlyDictionary": true, "ConcurrentDictionary": true, "HashSet": true, "ISet": true,
	"SortedSet": true, "SortedDictionary": true, "Queue": true, "Stack": true, "LinkedList": true,
	"ConcurrentBag": true, "ConcurrentQueue": true, "ImmutableArray": true, "ImmutableList": true,
	"ImmutableDictionary": true, "ImmutableHashSet": true, "Array": true, "Span": true,
	"ReadOnlySpan": true, "Memory": true, "ReadOnlyMemory": true, "StringBuilder": true, "String": true,
	"Guid": true, "DateTime": true, "DateTimeOffset": true, "TimeSpan": true, "Version": true,
	"Regex": true, "Match": true, "Type": true, "Expression": true, "MemoryStream": true,
	"ILogger": true, "ILoggerFactory": true, "CancellationToken": true, "CancellationTokenSource": true,
	"Utf8JsonWriter": true, "Utf8JsonReader": true, "JsonElement": true, "JsonDocument": true,
}

// What csLibraryCall says of a call.
const (
	csCallUnknown = iota // not a library type this file describes, or a member it is silent on
	csCallIO             // performs I/O
	csCallPure           // a method of a type that works on memory only
)

// csLibraryCall classes a call by the declared type of its receiver, as the
// source names it, when the repository does not declare that type.
func csLibraryCall(typ, method string) int {
	typ = csSimpleTypeName(typ)
	if members, ok := csIOMembers[typ]; ok {
		for _, m := range members {
			if m == method {
				return csCallIO
			}
		}
		return csCallUnknown
	}
	if csPureTypes[typ] {
		return csCallPure
	}
	// An SDK's client: `TMDbClient.GetMovieAsync`, `BlobClient.UploadAsync`,
	// `AmazonS3Client.PutObjectAsync`. The library is not known, and the pair of
	// conventions is: a type named for being a client, and a method named for
	// returning a task. Its synchronous members are configuration as often as
	// requests and are left alone.
	if strings.HasSuffix(typ, "Client") && strings.HasSuffix(method, "Async") {
		return csCallIO
	}
	return csCallUnknown
}

// csSimpleTypeName strips a namespace qualifier, generic arguments and a
// nullable mark from a type as written: `System.Collections.Generic.List<Item>?`
// is a List.
func csSimpleTypeName(typ string) string {
	typ = strings.TrimSpace(typ)
	if i := strings.IndexByte(typ, '<'); i >= 0 {
		typ = typ[:i]
	}
	typ = strings.TrimSuffix(typ, "?")
	if i := strings.LastIndexByte(typ, '.'); i >= 0 {
		typ = typ[i+1:]
	}
	return typ
}
