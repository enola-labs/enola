// Package linescan reads text a line at a time, whatever the length of a line.
//
// bufio.Scanner has a maximum token size. A line longer than that ends the scan, and
// a loop that does not then ask Err reads a file with one long line as a file that
// stopped there: everything after it is silently missing. Source files do carry such
// lines (an embedded asset, generated code), so the extractors read with this instead.
package linescan

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// Scanner yields the lines of a reader. It is used as bufio.Scanner is:
//
//	sc := linescan.New(r)
//	for sc.Scan() {
//		line := sc.Text()
//	}
//	if err := sc.Err(); err != nil { ... }
//
// The only error it can report is the reader's own.
type Scanner struct {
	r    *bufio.Reader
	line string
	err  error
	done bool
}

// New returns a Scanner over r.
func New(r io.Reader) *Scanner {
	return &Scanner{r: bufio.NewReader(r)}
}

// Scan advances to the next line, which Text then returns. It returns false at the
// end of the input or on a read error. A final line with no line ending is a line.
func (s *Scanner) Scan() bool {
	if s.done {
		return false
	}
	line, err := s.r.ReadString('\n')
	if err != nil {
		s.done = true
		if !errors.Is(err, io.EOF) {
			s.err = err
			return false
		}
		if line == "" {
			return false
		}
	}
	s.line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	return true
}

// Text returns the line Scan advanced to, without its line ending.
func (s *Scanner) Text() string { return s.line }

// Err returns the read error that ended the scan, or nil at the end of the input.
func (s *Scanner) Err() error { return s.err }
