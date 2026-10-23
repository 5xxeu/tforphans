package report

import (
	"fmt"
	"io"
	"strings"
)

// Tone gives terminal text a semantic accent. Labels also carry the meaning
// when colors are disabled or cannot be distinguished.
type Tone string

const (
	Heading   Tone = "1;36"
	Good      Tone = "32"
	Attention Tone = "1;33"
	Failure   Tone = "1;31"
	Muted     Tone = "2"
)

type colorWriter struct{ io.Writer }

// WithColor enables terminal presentation for a human-readable stream only.
// Callers decide whether its destination is a terminal and honor NO_COLOR.
func WithColor(w io.Writer, enabled bool) io.Writer {
	if !enabled {
		return w
	}
	return &colorWriter{w}
}

// Accent leaves plain writers unchanged.
func Accent(w io.Writer, tone Tone, text string) string {
	if _, ok := w.(*colorWriter); !ok || text == "" {
		return text
	}
	return "\x1b[" + string(tone) + "m" + text + "\x1b[0m"
}

// Write accents diagnostic labels without buffering progress or changing its
// timing. Data in the message is already sanitized by the caller.
func (w *colorWriter) Write(p []byte) (int, error) {
	s := string(p)
	for _, label := range []struct {
		prefix string
		tone   Tone
	}{
		{"error:", Failure}, {"warning:", Attention}, {"note:", Heading},
		{"plan:", Heading}, {"coverage: INCOMPLETE", Attention},
		{"coverage: complete", Good},
	} {
		if strings.HasPrefix(s, label.prefix) {
			s = Accent(w, label.tone, label.prefix) + s[len(label.prefix):]
			break
		}
	}
	n, err := io.WriteString(w.Writer, s)
	if err != nil {
		return 0, err
	}
	if n != len(s) {
		return 0, io.ErrShortWrite
	}
	return len(p), nil
}

// Section separates report blocks in both terminal output and plain logs.
func Section(w io.Writer, title string) {
	fmt.Fprintln(w, Accent(w, Heading, "== "+title+" "+strings.Repeat("=", max(1, 56-len(title)))))
}

// writeTable adds accents after tab expansion, so ANSI codes never alter the
// column widths. Only generated headers, totals and coverage labels are styled.
func writeTable(w io.Writer, text string) error {
	for i, line := range strings.SplitAfter(text, "\n") {
		if line == "" {
			continue
		}
		if i == 0 || strings.HasPrefix(strings.TrimSpace(line), "total ") {
			line = Accent(w, Heading, strings.TrimSuffix(line, "\n")) + "\n"
		} else {
			// Summary tables carry coverage in their final column.
			trimmed := strings.TrimRight(line, " \n")
			for _, status := range []struct {
				word string
				tone Tone
			}{{Incomplete, Attention}, {Complete, Good}} {
				if strings.HasSuffix(trimmed, "  "+status.word) {
					at := len(trimmed) - len(status.word)
					line = line[:at] + Accent(w, status.tone, status.word) + line[len(trimmed):]
					break
				}
			}
		}
		if _, err := io.WriteString(w, line); err != nil {
			return err
		}
	}
	return nil
}
