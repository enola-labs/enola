package check

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Sources says where the files a verdict names live on disk, and what each held
// when it was graded.
//
// It exists because the answer used to be the process's working directory. A
// check is routinely run on a repository it is not standing in (`enola check
// ../api`, a cluster config, the stop hook from an agent's subdirectory), and a
// relative read from the wrong directory is not merely empty: another checkout
// with the same file names printed its own line under a finding the graded tree
// raised, with nothing to say the quote came from somewhere else.
//
// The zero value knows nothing and falls back to the working directory, which is
// what a verdict built without AttachSources (a test, an older caller) always did.
type Sources struct {
	members []sourceMember
}

type sourceMember struct {
	label string
	dir   string
	// hashes is the SHA-256 of every file the snapshot read, keyed by its
	// repository-relative slash path. A file missing from it was not hashed
	// (not an indexed source) and is read unverified.
	hashes map[string]string
}

// AttachSources records where each graded repository lives. dirs are the
// directories the check indexed, in order; metaFor returns the snapshot meta the
// engine holds for one of them, which carries its label and its file hashes.
func AttachSources(v Verdict, dirs []string, metaFor func(string) facts.SnapshotMeta) Verdict {
	var s Sources
	for _, dir := range dirs {
		abs, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		meta := metaFor(dir)
		hashes := make(map[string]string, len(meta.FileHashes))
		for _, fh := range meta.FileHashes {
			hashes[filepath.ToSlash(fh.Path)] = fh.Hash
		}
		s.members = append(s.members, sourceMember{label: meta.Label(), dir: abs, hashes: hashes})
	}
	v.sources = s
	return v
}

// sourceFile is one file a finding names, resolved to the repository it came from.
type sourceFile struct {
	member *sourceMember
	// rel is the path inside member.dir, in slash form.
	rel string
}

func (f sourceFile) abs() string { return filepath.Join(f.member.dir, filepath.FromSlash(f.rel)) }

// locate resolves a fact's file to the repository it belongs to. In a union
// snapshot the file carries its repository's label as its first segment, and the
// label, not what happens to exist on disk, decides the repository. A lone
// repository's files are recorded as they are, unless a label prefix is the only
// reading its snapshot recorded.
func (s Sources) locate(file string) (sourceFile, bool) {
	if file == "" || len(s.members) == 0 {
		return sourceFile{}, false
	}
	label, rest, prefixed := strings.Cut(file, "/")
	if len(s.members) > 1 {
		if !prefixed {
			return sourceFile{}, false
		}
		for i := range s.members {
			if s.members[i].label == label {
				return sourceFile{member: &s.members[i], rel: rest}, true
			}
		}
		return sourceFile{}, false
	}
	m := &s.members[0]
	if prefixed && label == m.label {
		if _, asRecorded := m.hashes[file]; !asRecorded {
			if _, stripped := m.hashes[rest]; stripped {
				return sourceFile{member: m, rel: rest}, true
			}
		}
	}
	return sourceFile{member: m, rel: file}, true
}

// read returns a located file's content, and false when the file cannot be read
// or no longer holds what the snapshot hashed. A frame quoted from a file that
// changed after grading points at a line the finding was never about.
func (f sourceFile) read() ([]byte, bool) {
	src, err := os.ReadFile(f.abs()) //factpath:host
	if err != nil {
		return nil, false
	}
	if want, hashed := f.member.hashes[f.rel]; hashed {
		sum := sha256.Sum256(src)
		if hex.EncodeToString(sum[:]) != want {
			return nil, false
		}
	}
	return src, true
}

// frameSource returns the content a frame for file is quoted from.
func (s Sources) frameSource(file string) ([]byte, bool) {
	if len(s.members) == 0 {
		src, err := os.ReadFile(filepath.Join(frameRoot, filepath.FromSlash(repoRelative(file)))) //factpath:host
		return src, err == nil
	}
	f, ok := s.locate(file)
	if !ok {
		return nil, false
	}
	return f.read()
}

// hostPath is the path a host can open: the file's location relative to the
// directory the host runs in, which is where an annotation or a SARIF result is
// resolved. A file outside that directory is given relative to its own
// repository, and a file no graded repository claims is printed as recorded:
// an annotation cannot afford a guess, because a wrong path pins the finding to
// a file the reviewer does not have.
func (s Sources) hostPath(file string) string {
	if len(s.members) == 0 {
		return legacyHostPath(file)
	}
	f, ok := s.locate(file)
	if !ok {
		return file
	}
	if cwd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(resolved(cwd), resolved(f.abs())); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return filepath.ToSlash(rel)
		}
	}
	return f.rel
}

// resolved follows symlinks so two spellings of one directory compare equal
// (macOS reaches its temp and home trees through /var and /private/var). A path
// that cannot be resolved is compared as given.
func resolved(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	return path
}
