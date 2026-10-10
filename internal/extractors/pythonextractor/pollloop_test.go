package pythonextractor

import (
	"slices"
	"testing"
)

const roundLoops = `import time

from app.client import Client


def poll(client: Client, job):
    while True:
        status = client.status(job)
        if status == "done":
            break
        time.sleep(5)


def poll_no_sleep(client: Client, job):
    while True:
        status = client.status(job)
        if status == "done":
            return status


def retry(client: Client, url, retries):
    for attempt in range(retries):
        try:
            return client.fetch(url)
        except TimeoutError:
            time.sleep(2**attempt)


def retry_library(client: Client, url, retrying):
    for attempt in retrying:
        with attempt:
            client.fetch(url)


def backoff(client: Client, url, delays):
    for delay in delays:
        if client.fetch(url):
            return
        time.sleep(delay)


def cursor(client: Client, api):
    request = api.first()
    items = []
    while request is not None:
        response = client.fetch(request)
        items.extend(response)
        request = api.list_next(request, response)
    return items


def chunks(client: Client, f):
    while chunk := f.read(8192):
        client.send(chunk)


def page_then_items(client: Client, api):
    token = None
    while True:
        page = client.fetch(token)
        for item in page:
            client.send(item)
        token = page.next
        if not token:
            break


def retry_per_item(client: Client, urls, retrying):
    for url in urls:
        for attempt in retrying:
            with attempt:
                client.fetch(url)


# Walks: the call runs once per element.

def per_item(client: Client, urls):
    for url in urls:
        client.fetch(url)


def per_item_throttled(client: Client, urls):
    for url in urls:
        client.fetch(url)
        time.sleep(1)


def by_index(client: Client, urls):
    for i in range(len(urls)):
        client.fetch(urls[i])
        time.sleep(1)


def worklist(client: Client, queue):
    while queue:
        url = queue.pop()
        client.fetch(url)
        time.sleep(1)


def rows(client: Client, cur):
    while True:
        row = cur.fetchone()
        if row is None:
            break
        client.send(row)


def consumer(client: Client, inbox):
    while True:
        work = inbox.get()
        if work is None:
            return
        client.send(work)


def settings(client: Client, conf, job):
    while True:
        status = client.status(job)
        if status == conf.get("done"):
            return


def chain(client: Client, node):
    while node is not None:
        client.send(node)
        node = parent_of(node)


def walk_stops_early(client: Client, nodes, i):
    while i < len(nodes):
        found = client.fetch(nodes[i])
        if found:
            break
        i += 1
`

const roundClient = `class Client:
    def status(self, job):
        return open(job).read()

    def fetch(self, url):
        return open(url).read()

    def send(self, item):
        open(item, "w").close()
`

// A poll, a retry and a cursor go round on what they fetched: the call at their
// own level is not an N+1 candidate. A walk written the same way still is.
func TestRoundLoopCallsAreNotPerElement(t *testing.T) {
	ff := extractPyRepo(t, map[string]string{
		"app/__init__.py": "",
		"app/client.py":   roundClient,
		"app/loops.py":    roundLoops,
	})
	scaling := func(name string) []string {
		return pyStrings(pySym(t, ff, "loops."+name).PropAny("calls_in_scaling_loop"))
	}

	for _, name := range []string{"poll", "poll_no_sleep", "settings", "retry", "retry_library", "backoff", "cursor", "chunks"} {
		if got := scaling(name); len(got) != 0 {
			t.Errorf("%s is a round loop; calls_in_scaling_loop = %v, want none", name, got)
		}
		if d, _ := pySym(t, ff, "loops."+name).PropAny("scaling_loop_depth").(int); d != 0 {
			t.Errorf("%s: scaling_loop_depth = %d, want 0", name, d)
		}
	}

	// The loop inside a round walks what the round fetched.
	got := scaling("page_then_items")
	if !slices.Contains(got, "app/client.Client.send") {
		t.Errorf("page_then_items: the per-item send must stay a candidate; got %v", got)
	}
	if slices.Contains(got, "app/client.Client.fetch") {
		t.Errorf("page_then_items: the fetch is one per page; got %v", got)
	}

	// A retry around each element is still a request per element, once.
	retried := pySym(t, ff, "loops.retry_per_item")
	if got := pyStrings(retried.PropAny("calls_in_scaling_loop")); !slices.Contains(got, "app/client.Client.fetch") {
		t.Errorf("retry_per_item: calls_in_scaling_loop = %v, want Client.fetch", got)
	}
	if d, _ := retried.PropAny("scaling_loop_depth").(int); d != 1 {
		t.Errorf("retry_per_item: scaling_loop_depth = %d, want 1: the retry adds no nesting", d)
	}

	for name, call := range map[string]string{
		"per_item": "fetch", "per_item_throttled": "fetch", "by_index": "fetch", "worklist": "fetch",
		"rows": "send", "consumer": "send", "chain": "send", "walk_stops_early": "fetch",
	} {
		if got := scaling(name); !slices.Contains(got, "app/client.Client."+call) {
			t.Errorf("%s walks its input; calls_in_scaling_loop = %v, want Client.%s", name, got, call)
		}
	}
}
