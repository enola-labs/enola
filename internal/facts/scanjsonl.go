package facts

import (
	"bufio"
	"encoding/json"
	"io"
)

// ScanJSONL decodes a facts JSONL stream one fact at a time, handing each to fn.
//
// It exists for readers that need to SEE every fact but not KEEP them. ReadJSONL
// answers the other need — it builds a Store, with the four index maps, the interning
// table and a retained Fact per line — and a caller that only wants to reduce the
// stream to counts paid all of that: loading a kernel-sized previous snapshot to
// summarise it cost 386 MiB, at the end of a run, next to the snapshot it was being
// compared against.
//
// fn must not retain the Fact it is given beyond the call. The Props map and
// Relations slice belong to the decoder's scratch fact and are not reused between
// lines today, but nothing here promises that.
//
// Blank lines are skipped; a malformed line stops the scan and is returned. The
// buffer matches ReadJSONL's, so any line this package can write it can read back.
func ScanJSONL(r io.Reader, fn func(Fact) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var f Fact
		if err := json.Unmarshal(line, &f); err != nil {
			return err
		}
		dropWireIDs(&f)
		if err := fn(f); err != nil {
			return err
		}
	}
	return sc.Err()
}
