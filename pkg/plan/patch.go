package plan

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/workspace"
)

const (
	OpCreate = "create"
	OpDelete = "delete"
	OpModify = "modify"
)

type PatchFile struct {
	Path         string
	Op           string
	hunks        []hunk
	oldNoNewline bool
	newNoNewline bool
}

type hunk struct {
	oldStart, oldCount int
	newStart, newCount int
	lines              []hunkLine
}

type hunkLine struct {
	op   byte
	text string
}

func ParsePatch(data []byte) ([]PatchFile, error) {
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	var files []PatchFile
	i := 0
	for i < len(lines) {
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "rename from "), strings.HasPrefix(line, "rename to "),
			strings.HasPrefix(line, "copy from "), strings.HasPrefix(line, "copy to "):
			return nil, fmt.Errorf("patch line %d: rename/copy is not supported — express it as a delete plus a create", i+1)
		case strings.HasPrefix(line, "GIT binary patch"), strings.HasPrefix(line, "Binary files "):
			return nil, fmt.Errorf("patch line %d: binary patches are not supported", i+1)
		case strings.HasPrefix(line, "--- "):
			file, next, err := parseFileSection(lines, i)
			if err != nil {
				return nil, err
			}
			files = append(files, file)
			i = next
		default:
			i++
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("the patch contains no file changes (no ---/+++ headers found)")
	}
	seen := map[string]bool{}
	for _, f := range files {
		if seen[f.Path] {
			return nil, fmt.Errorf("the patch names %s twice — fold the changes into one file section", f.Path)
		}
		seen[f.Path] = true
	}
	return files, nil
}

func parseFileSection(lines []string, start int) (PatchFile, int, error) {
	oldName := parsePatchName(strings.TrimPrefix(lines[start], "--- "))
	if start+1 >= len(lines) || !strings.HasPrefix(lines[start+1], "+++ ") {
		return PatchFile{}, 0, fmt.Errorf("patch line %d: --- header without a +++ header", start+1)
	}
	newName := parsePatchName(strings.TrimPrefix(lines[start+1], "+++ "))

	var file PatchFile
	switch {
	case oldName == "" && newName == "":
		return PatchFile{}, 0, fmt.Errorf("patch line %d: both sides are /dev/null", start+1)
	case oldName == "":
		file = PatchFile{Path: newName, Op: OpCreate}
	case newName == "":
		file = PatchFile{Path: oldName, Op: OpDelete}
	case oldName != newName:
		return PatchFile{}, 0, fmt.Errorf("patch line %d: renames %s to %s — rename is not supported; express it as a delete plus a create", start+1, oldName, newName)
	default:
		file = PatchFile{Path: oldName, Op: OpModify}
	}

	i := start + 2
	for i < len(lines) && strings.HasPrefix(lines[i], "@@ ") {
		h, next, err := parseHunk(lines, i, &file)
		if err != nil {
			return PatchFile{}, 0, err
		}
		file.hunks = append(file.hunks, h)
		i = next
	}
	if len(file.hunks) == 0 {
		return PatchFile{}, 0, fmt.Errorf("patch line %d: %s declares no hunks", start+1, file.Path)
	}
	return file, i, nil
}

func parsePatchName(header string) string {
	name, _, _ := strings.Cut(header, "\t")
	name = strings.TrimSpace(name)
	if name == "/dev/null" {
		return ""
	}
	if rest, ok := strings.CutPrefix(name, "a/"); ok {
		return rest
	}
	if rest, ok := strings.CutPrefix(name, "b/"); ok {
		return rest
	}
	return name
}

func parseHunk(lines []string, start int, file *PatchFile) (hunk, int, error) {
	header := lines[start]
	h, err := parseHunkHeader(header)
	if err != nil {
		return hunk{}, 0, fmt.Errorf("patch line %d: %v", start+1, err)
	}
	oldLeft, newLeft := h.oldCount, h.newCount
	i := start + 1
	var lastOp byte
	for i < len(lines) && (oldLeft > 0 || newLeft > 0) {
		line := lines[i]
		if strings.HasPrefix(line, `\ No newline at end of file`) {
			markNoNewline(file, lastOp)
			i++
			continue
		}
		op, text := byte(' '), ""
		if line != "" {
			op, text = line[0], line[1:]
		}
		switch op {
		case ' ':
			if oldLeft == 0 || newLeft == 0 {
				return hunk{}, 0, fmt.Errorf("patch line %d: context line exceeds the counts declared by %q", i+1, header)
			}
			oldLeft--
			newLeft--
		case '-':
			if oldLeft == 0 {
				return hunk{}, 0, fmt.Errorf("patch line %d: removed line exceeds the counts declared by %q", i+1, header)
			}
			oldLeft--
		case '+':
			if newLeft == 0 {
				return hunk{}, 0, fmt.Errorf("patch line %d: added line exceeds the counts declared by %q", i+1, header)
			}
			newLeft--
		default:
			return hunk{}, 0, fmt.Errorf("patch line %d: %q is not a hunk line (expected ' ', '-', '+' or a no-newline marker)", i+1, line)
		}
		h.lines = append(h.lines, hunkLine{op: op, text: text})
		lastOp = op
		i++
	}
	if oldLeft > 0 || newLeft > 0 {
		return hunk{}, 0, fmt.Errorf("patch line %d: hunk %q ends before its declared counts are satisfied", start+1, header)
	}
	if i < len(lines) && strings.HasPrefix(lines[i], `\ No newline at end of file`) {
		markNoNewline(file, lastOp)
		i++
	}
	return h, i, nil
}

func markNoNewline(file *PatchFile, lastOp byte) {
	switch lastOp {
	case '-':
		file.oldNoNewline = true
	case '+':
		file.newNoNewline = true
	case ' ':
		file.oldNoNewline = true
		file.newNoNewline = true
	}
}

func parseHunkHeader(header string) (hunk, error) {
	body, ok := strings.CutPrefix(header, "@@ -")
	if !ok {
		return hunk{}, fmt.Errorf("%q is not a hunk header", header)
	}
	body, _, ok = strings.Cut(body, " @@")
	if !ok {
		return hunk{}, fmt.Errorf("%q is not a hunk header (missing closing @@)", header)
	}
	oldPart, newPart, ok := strings.Cut(body, " +")
	if !ok {
		return hunk{}, fmt.Errorf("%q is not a hunk header (missing new-side range)", header)
	}
	var h hunk
	var err error
	if h.oldStart, h.oldCount, err = parseRange(oldPart); err != nil {
		return hunk{}, fmt.Errorf("%q: old side: %v", header, err)
	}
	if h.newStart, h.newCount, err = parseRange(newPart); err != nil {
		return hunk{}, fmt.Errorf("%q: new side: %v", header, err)
	}
	return h, nil
}

func parseRange(s string) (start, count int, err error) {
	count = 1
	startPart, countPart, hasCount := strings.Cut(s, ",")
	if start, err = strconv.Atoi(startPart); err != nil {
		return 0, 0, fmt.Errorf("%q is not a line number", startPart)
	}
	if hasCount {
		if count, err = strconv.Atoi(countPart); err != nil {
			return 0, 0, fmt.Errorf("%q is not a line count", countPart)
		}
	}
	return start, count, nil
}

func ValidatePatchScope(files []PatchFile, outputDir string) error {
	outDir := path.Clean(filepath.ToSlash(outputDir))
	for _, f := range files {
		p := filepath.ToSlash(f.Path)
		cleaned := path.Clean(p)
		switch {
		case p == "" || cleaned == ".":
			return fmt.Errorf("the patch carries an empty file path")
		case path.IsAbs(p) || filepath.IsAbs(f.Path):
			return fmt.Errorf("the patch targets absolute path %s — outside the snapshot's scope, which is repo-relative", f.Path)
		case cleaned == ".." || strings.HasPrefix(cleaned, "../"):
			return fmt.Errorf("the patch targets %s, which escapes the repository root — outside the snapshot's scope", f.Path)
		case outDir != "." && (cleaned == outDir || strings.HasPrefix(cleaned, outDir+"/")):
			return fmt.Errorf("the patch targets %s inside the snapshot output directory %s — snapshot artifacts are outside the snapshot's scope", f.Path, outputDir)
		}
	}
	return nil
}

func applyPatch(root string, files []PatchFile) error {
	for _, f := range files {
		target := filepath.Join(root, filepath.FromSlash(f.Path))
		switch f.Op {
		case OpCreate:
			if err := applyCreate(target, f); err != nil {
				return err
			}
		case OpDelete:
			if err := applyDelete(target, f); err != nil {
				return err
			}
		default:
			if err := applyModify(target, f); err != nil {
				return err
			}
		}
	}
	return nil
}

func applyCreate(target string, f PatchFile) error {
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("the patch creates %s, but it already exists", f.Path)
	}
	if len(f.hunks) != 1 || f.hunks[0].oldCount != 0 {
		return fmt.Errorf("the patch creates %s but its hunk removes lines — does not apply", f.Path)
	}
	var created []string
	for _, hl := range f.hunks[0].lines {
		if hl.op != '+' {
			return fmt.Errorf("the patch creates %s but its hunk carries a %q line — does not apply", f.Path, string(hl.op))
		}
		created = append(created, hl.text)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.WriteFile(target, []byte(joinLines(created, !f.newNoNewline)), 0o644)
}

func applyDelete(target string, f PatchFile) error {
	content, err := os.ReadFile(target)
	if err != nil {
		return fmt.Errorf("the patch deletes %s, but it cannot be read: %v", f.Path, err)
	}
	current, hadNewline := splitLines(string(content))
	var expected []string
	for _, h := range f.hunks {
		for _, hl := range h.lines {
			if hl.op == '+' {
				return fmt.Errorf("the patch deletes %s but its hunk adds lines — does not apply", f.Path)
			}
			expected = append(expected, hl.text)
		}
	}
	if err := matchLines(f.Path, current, expected); err != nil {
		return err
	}
	if f.oldNoNewline == hadNewline {
		return fmt.Errorf("the patch deletes %s, but the file's trailing-newline state does not match — does not apply", f.Path)
	}
	return os.Remove(target)
}

func applyModify(target string, f PatchFile) error {
	content, err := os.ReadFile(target)
	if err != nil {
		return fmt.Errorf("the patch modifies %s, but it cannot be read: %v", f.Path, err)
	}
	current, hadNewline := splitLines(string(content))
	if f.oldNoNewline && hadNewline {
		return fmt.Errorf("the patch expects %s to end without a newline, but it ends with one — does not apply", f.Path)
	}
	delta := 0
	for n, h := range f.hunks {
		pos := h.oldStart - 1 + delta
		if h.oldCount == 0 {
			pos = h.oldStart + delta
		}
		if pos < 0 || pos > len(current) {
			return fmt.Errorf("hunk %d of %s starts at line %d, beyond the file's %d lines — does not apply", n+1, f.Path, h.oldStart, len(current))
		}
		oldIdx := pos
		var replacement []string
		for _, hl := range h.lines {
			switch hl.op {
			case ' ', '-':
				if oldIdx >= len(current) {
					return fmt.Errorf("hunk %d of %s expects %q at line %d, but the file ends there — does not apply", n+1, f.Path, hl.text, oldIdx+1)
				}
				if current[oldIdx] != hl.text {
					return fmt.Errorf("hunk %d of %s does not apply at line %d: expected %q, found %q", n+1, f.Path, oldIdx+1, hl.text, current[oldIdx])
				}
				if hl.op == ' ' {
					replacement = append(replacement, hl.text)
				}
				oldIdx++
			case '+':
				replacement = append(replacement, hl.text)
			}
		}
		next := make([]string, 0, len(current)+len(replacement))
		next = append(next, current[:pos]...)
		next = append(next, replacement...)
		next = append(next, current[oldIdx:]...)
		current = next
		delta += h.newCount - h.oldCount
	}
	resultNewline := hadNewline
	if f.newNoNewline {
		resultNewline = false
	} else if f.oldNoNewline {
		resultNewline = true
	}
	return os.WriteFile(target, []byte(joinLines(current, resultNewline)), 0o644)
}

func matchLines(path string, current, expected []string) error {
	if len(current) != len(expected) {
		return fmt.Errorf("the patch deletes %s as %d line(s), but the file has %d — does not apply", path, len(expected), len(current))
	}
	for i := range expected {
		if current[i] != expected[i] {
			return fmt.Errorf("the patch deletes %s, but line %d differs: expected %q, found %q — does not apply", path, i+1, expected[i], current[i])
		}
	}
	return nil
}

func splitLines(content string) ([]string, bool) {
	if content == "" {
		return nil, false
	}
	hadNewline := strings.HasSuffix(content, "\n")
	if hadNewline {
		content = content[:len(content)-1]
	}
	return strings.Split(content, "\n"), hadNewline
}

func joinLines(lines []string, trailingNewline bool) string {
	if len(lines) == 0 {
		return ""
	}
	joined := strings.Join(lines, "\n")
	if trailingNewline {
		joined += "\n"
	}
	return joined
}

func copyTree(src, dst string, skipRel map[string]bool) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		slashRel := filepath.ToSlash(rel)
		if d.IsDir() && (d.Name() == ".git" || skipRel[slashRel] || (rel != "." && workspace.IsLinkedWorktree(p))) {
			if rel == "." {
				return nil
			}
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case !d.Type().IsRegular():
			return nil
		default:
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.WriteFile(target, data, info.Mode().Perm())
		}
	})
}
