package dotnetextractor

import (
	"slices"
	"testing"
)

func TestCSharpLibraryCall(t *testing.T) {
	tests := []struct {
		typ, method string
		want        int
	}{
		{"HttpClient", "GetAsync", csCallIO},
		{"HttpClient", "CancelPendingRequests", csCallUnknown},
		// The call that runs a query, and the one that composes it.
		{"IQueryable<Item>", "ToListAsync", csCallIO},
		{"IQueryable<Item>", "Where", csCallUnknown},
		{"DbCommand", "ExecuteReaderAsync", csCallIO},
		{"SqliteConnection", "CreateCommand", csCallUnknown},
		{"FileStream", "WriteAsync", csCallIO},
		// A stream is whatever it wraps.
		{"Stream", "WriteAsync", csCallUnknown},
		{"MemoryStream", "Write", csCallPure},
		{"System.Collections.Generic.List<Item>?", "Find", csCallPure},
		{"ILogger<Worker>", "LogError", csCallPure},
		// An SDK nobody listed: a client type and a task-returning method.
		{"TMDbClient", "GetMovieAsync", csCallIO},
		{"TMDbClient", "GetImageUrl", csCallUnknown},
		{"Unlisted", "SaveAsync", csCallUnknown},
	}
	for _, tt := range tests {
		if got := csLibraryCall(tt.typ, tt.method); got != tt.want {
			t.Errorf("csLibraryCall(%s, %s) = %d, want %d", tt.typ, tt.method, got, tt.want)
		}
	}
}

func csStrings(v any) []string {
	s, _ := v.([]string)
	return s
}

// A request made through a field that holds a third-party client is I/O, and
// what calls the method that makes it reaches I/O. A search of a list is not.
func TestCSharpCallOnALibraryReceiverIsClassedByMember(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Lib.Tmdb/TmdbManager.cs": `namespace Lib.Tmdb;

using System.Collections.Generic;
using System.Threading.Tasks;
using TMDbLib.Client;

public class TmdbManager
{
    private readonly TMDbClient _client;

    public TmdbManager(TMDbClient client)
    {
        _client = client;
    }

    public async Task<Movie> GetMovie(int id)
    {
        return await _client.GetMovieAsync(id).ConfigureAwait(false);
    }

    public string GetPosterUrl(string path)
    {
        return "https://image.example/" + path;
    }

    public async Task<List<Movie>> GetAll(List<int> ids)
    {
        var movies = new List<Movie>();
        foreach (var id in ids)
        {
            movies.Add(await GetMovie(id).ConfigureAwait(false));
        }

        return movies;
    }

    public int CountKnown(List<int> ids, List<int> known)
    {
        var n = 0;
        foreach (var id in ids)
        {
            if (known.Find(k => k == id) != 0)
            {
                n++;
            }
        }

        return n;
    }
}
`,
	})

	get := csSym(t, ff, "TmdbManager.GetMovie")
	if got := csStrings(get.PropAny("io_calls")); !slices.Contains(got, "_client.GetMovieAsync") {
		t.Errorf("GetMovie io_calls = %v, want _client.GetMovieAsync", got)
	}
	if !csPerformsIO(csSym(t, ff, "TmdbManager.GetAll")) {
		t.Error("GetAll reaches the request through GetMovie and must be performs_io")
	}
	if csPerformsIO(csSym(t, ff, "TmdbManager.GetPosterUrl")) {
		t.Error("GetPosterUrl formats a string and must not be performs_io")
	}
	count := csSym(t, ff, "TmdbManager.CountKnown")
	if got := csStrings(count.PropAny("pure_calls")); !slices.Contains(got, "known.Find") {
		t.Errorf("CountKnown pure_calls = %v, want known.Find; calls_in_loop = %v", got, count.PropAny("calls_in_loop"))
	}
}

// `base.M()` reaches the declaration the override extends, and its I/O with it.
func TestCSharpBaseCallReachesTheBaseDeclaration(t *testing.T) {
	ff := extractRepo(t, map[string]string{
		"Lib.Tv/Store.cs": `namespace Lib.Tv;

using System.IO;

public class Store
{
    public virtual void Update(string item)
    {
        File.WriteAllText("store.json", item);
    }
}
`,
		"Lib.Tv/Timers.cs": `namespace Lib.Tv;

using System.Collections.Generic;

public class Timers : Store
{
    public override void Update(string item)
    {
        base.Update(item);
    }

    public void UpdateAll(List<string> items)
    {
        foreach (var item in items)
        {
            Update(item);
        }
    }
}
`,
	})
	update := csSym(t, ff, "Timers.Update")
	if !csCallsSuffix(update, "Store.Update") {
		t.Errorf("Timers.Update must call Store.Update; calls: %v", csCalls(update))
	}
	if !csPerformsIO(update) {
		t.Error("Timers.Update writes the store through its base and must be performs_io")
	}
	if !csPerformsIO(csSym(t, ff, "Timers.UpdateAll")) {
		t.Error("UpdateAll reaches the write and must be performs_io")
	}
}
