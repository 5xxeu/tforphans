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
	if !colored(w) || text == "" || tone == "" {
		return text
	}
	return "\x1b[" + string(tone) + "m" + text + "\x1b[0m"
}

// Write accents diagnostic labels without buffering progress or changing its
// timing. Data in the message is already sanitized by the caller. A label is
// recognized only at the start of a write: callers print each diagnostic line
// with one call.
func (w *colorWriter) Write(p []byte) (int, error) {
	s := string(p)
	opening, prefix := 0, 0
	// Complete with exceptions ("complete for what can be listed; not
	// checked: ...") is not the all-clear.
	complete := Attention
	if s == "coverage: complete\n" {
		complete = Good
	}
	for _, label := range []struct {
		prefix string
		tone   Tone
	}{
		{"error:", Failure}, {"warning:", Attention}, {"note:", Heading},
		{"plan:", Heading}, {"coverage: INCOMPLETE", Attention},
		{"coverage: complete", complete},
	} {
		if strings.HasPrefix(s, label.prefix) {
			opening, prefix = len("\x1b["+string(label.tone)+"m"), len(label.prefix)
			s = Accent(w, label.tone, label.prefix) + s[len(label.prefix):]
			break
		}
	}
	n, err := io.WriteString(w.Writer, s)
	if n < len(s) && err == nil {
		err = io.ErrShortWrite
	}
	if opening > 0 {
		// Report consumed input bytes, excluding the inserted SGR sequences.
		n = min(max(n-opening, 0), prefix) + max(n-opening-prefix-len("\x1b[0m"), 0)
	}
	return n, err
}

func colored(w io.Writer) bool {
	switch w := w.(type) {
	case *colorWriter:
		return true
	case *lazySection:
		return colored(w.w)
	}
	return false
}

// Section separates report blocks in both terminal output and plain logs.
func Section(w io.Writer, title string) {
	fmt.Fprintln(w, SectionLine(w, title))
}

// SectionLine is the heading Section prints, for callers that check the
// write.
func SectionLine(w io.Writer, title string) string {
	return Accent(w, Heading, "== "+title+" "+strings.Repeat("=", max(1, 56-len(title))))
}

// LazySection prints a blank line and the section heading before the first
// write, so that a block with nothing to say has no heading and nothing
// written there goes without one.
func LazySection(w io.Writer, title string) io.Writer {
	return &lazySection{w: w, title: title}
}

type lazySection struct {
	w       io.Writer
	title   string
	started bool
}

func (s *lazySection) Write(p []byte) (int, error) {
	if !s.started {
		s.started = true
		if _, err := fmt.Fprint(s.w, "\n"+SectionLine(s.w, s.title)+"\n"); err != nil {
			return 0, err
		}
	}
	return s.w.Write(p)
}

// writeTable adds accents after tab expansion, so ANSI codes never alter the
// column widths. Only generated headers, totals and coverage labels are styled;
// totals and coverage only in count tables, where the first column is a type or
// scope and the last one is coverage. Separator rows lose their padding.
func writeTable(w io.Writer, text string, counts bool) error {
	for i, line := range strings.SplitAfter(text, "\n") {
		if line == "" {
			continue
		}
		if strings.TrimSpace(line) == "" {
			line = "\n"
		} else if i == 0 || counts && strings.HasPrefix(strings.TrimSpace(line), "total ") {
			line = Accent(w, Heading, strings.TrimSuffix(line, "\n")) + "\n"
		} else if counts {
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
