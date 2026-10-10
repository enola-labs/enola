package javaextractor

import (
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func javaMethod(t *testing.T, src, suffix string) facts.Fact {
	t.Helper()
	for _, f := range extractAll(t, map[string]string{"svc/Sync.java": src}) {
		if f.Kind == facts.KindSymbol && strings.HasSuffix(f.Name, suffix) {
			return f
		}
	}
	t.Fatalf("no symbol ending in %s", suffix)
	return facts.Fact{}
}

func javaScalingCalls(f facts.Fact) []string {
	s, _ := f.PropAny("calls_in_scaling_loop").([]string)
	return s
}

func javaScalingDepth(t *testing.T, f facts.Fact) int {
	t.Helper()
	d, ok := f.PropAny("scaling_loop_depth").(int)
	if !ok {
		t.Fatalf("%s: no scaling_loop_depth; props=%v", f.Name, f.Props)
	}
	return d
}

// One query per page, drained row by row: one pass over the rows. The fetch is
// the batched call; the save in the drain loop is still once per row.
func TestJavaBatchLoop_DoWhilePaging(t *testing.T) {
	f := javaMethod(t, `package svc;

public class Sync {
    void run(PageLink pageLink) {
        PageData<Device> page;
        do {
            page = deviceService.findDevices(pageLink);
            for (Device d : page.getData()) {
                deviceService.save(d);
            }
            pageLink = pageLink.nextPageLink();
        } while (page.hasNext());
    }
}
`, "Sync.run")
	if d := javaScalingDepth(t, f); d != 1 {
		t.Errorf("scaling_loop_depth = %d, want 1: pages times rows-of-page is the rows", d)
	}
	calls := javaScalingCalls(f)
	if slices.Contains(calls, "deviceService.findDevices") {
		t.Errorf("the page fetch is listed as a per-element call: %v", calls)
	}
	if !slices.Contains(calls, "deviceService.save") {
		t.Errorf("the per-row save is not listed: %v", calls)
	}
}

// The same with while(true), a break on an empty batch, and a callback as the drain.
func TestJavaBatchLoop_WhileTrueWithStreamDrain(t *testing.T) {
	f := javaMethod(t, `package svc;

public class Sync {
    void run(long cursor) {
        while (true) {
            var batch = repo.findNextBatch(cursor, 500);
            if (batch.isEmpty()) {
                break;
            }
            batch.forEach(row -> sink.write(row));
            cursor = batch.get(batch.size() - 1).getId();
        }
    }
}
`, "Sync.run")
	calls := javaScalingCalls(f)
	if slices.Contains(calls, "repo.findNextBatch") {
		t.Errorf("the batch fetch is listed as a per-element call: %v", calls)
	}
	if !slices.Contains(calls, "sink.write") {
		t.Errorf("the per-row write in the callback is not listed: %v", calls)
	}
}

// A walk that queries per element binds and drains too. It runs for as long as
// there are nodes, whatever a round fetched, so it is not paging and the query
// stays a per-element call.
func TestJavaBatchLoop_IteratorWalkIsNotPaging(t *testing.T) {
	f := javaMethod(t, `package svc;

public class Sync {
    void run(Iterator<Node> it) {
        while (it.hasNext()) {
            Node n = it.next();
            List<Node> kids = dao.findChildren(n.getId());
            for (Node k : kids) {
                index.add(k);
            }
        }
    }
}
`, "Sync.run")
	if calls := javaScalingCalls(f); !slices.Contains(calls, "dao.findChildren") {
		t.Errorf("the per-node query was taken for a page fetch: %v", calls)
	}
}

// A loop that fetches and never drains is a poll or a chain walk, not paging.
func TestJavaBatchLoop_FetchWithoutADrainIsNotPaging(t *testing.T) {
	f := javaMethod(t, `package svc;

public class Sync {
    void run(Job job) {
        Status s;
        do {
            s = client.getStatus(job);
        } while (s.isRunning());
    }
}
`, "Sync.run")
	if calls := javaScalingCalls(f); !slices.Contains(calls, "client.getStatus") {
		t.Errorf("a status poll was taken for a page fetch: %v", calls)
	}
}

// A callback over what the loop's element holds repeats without scaling, as the
// statement form of the same nest does.
func TestJavaStreamCallbackOverTheElementAddsNoDepth(t *testing.T) {
	f := javaMethod(t, `package svc;

public class Sync {
    void run(List<Schema> schemas) {
        for (Schema schema : schemas) {
            schema.getAllOf().forEach(part -> registry.store(part));
        }
    }
}
`, "Sync.run")
	if d := javaScalingDepth(t, f); d != 1 {
		t.Errorf("scaling_loop_depth = %d, want 1", d)
	}
	if calls := javaScalingCalls(f); !slices.Contains(calls, "registry.store") {
		t.Errorf("the per-part store is not listed: %v", calls)
	}
}
