// cmd/tui layout policy: keep every rendered frame line within the live
// terminal width so the dashboard stays readable on narrow terminals (#466).
//
// SPEC scope: §2.2 lists "prescribing a specific dashboard or terminal UI
// implementation" as a non-goal and §13.4 makes the terminal status surface
// OPTIONAL and implementation-defined, so the narrow-width policy here is an
// implementation choice, not a deviation. The /api/v1/state data contract
// (§13.7.2) is untouched. cellRight restores upstream parity: the Elixir
// reference renders the TOKENS column with format_cell(.., :right), which
// truncates, but the Go port previously right-padded without truncating.
package main

import (
	"fmt"
	"strings"
)

// truncateCell trims a column value to width, replacing the overflow with "..."
// — the shared body of cell (left aligned) and cellRight (right aligned),
// mirroring truncate_plain/2 in status_dashboard.ex.
func truncateCell(s string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	if width <= 3 {
		return string(runes[:width])
	}
	return string(runes[:width-3]) + "..."
}

// cellRight right-aligns a value to width, truncating with "..." when it
// overflows. Mirrors format_cell(value, width, :right): a long token count must
// not push the EVENT column out of alignment.
func cellRight(s string, width int) string {
	return fmt.Sprintf("%*s", width, truncateCell(sanitize(s), width))
}

// clipFrame joins the frame lines, first clipping each to the live terminal
// width so a header line or table row can never wrap or word-split on a narrow
// terminal (#466). Clipping is applied only when a real terminal width is
// measured (TIOCGWINSZ); piped or redirected output stays full-fidelity because
// the consumer (a file, a pager, `less -S`) is not a fixed-width wrapping
// surface and may scroll horizontally. It clips lines in place, so callers pass
// a freshly built slice (every call site does).
func clipFrame(lines []string) string {
	if width, ok := liveTerminalWidth(); ok {
		for i := range lines {
			lines[i] = truncateToWidth(lines[i], width)
		}
	}
	return strings.Join(lines, "\n")
}

// truncateToWidth clips a single rendered line to at most width visible columns.
// ANSI escape sequences (colour codes) are passed through verbatim and never
// counted toward the width or split mid-sequence; when the line is cut a reset
// is appended so a truncation inside a colourised span can't bleed colour into
// the rest of the screen. A single-column ellipsis marks the cut.
func truncateToWidth(line string, width int) string {
	if width <= 0 {
		return line
	}
	runes := []rune(line)
	if visibleColumns(runes) <= width {
		return line
	}
	var b strings.Builder
	visible := 0
	for i := 0; i < len(runes); {
		if runes[i] == 0x1b {
			j := ansiSeqLen(runes, i)
			b.WriteString(string(runes[i:j]))
			i = j
			continue
		}
		if visible >= width-1 {
			break
		}
		b.WriteRune(runes[i])
		visible++
		i++
	}
	b.WriteRune('…')
	b.WriteString(ansiReset)
	return b.String()
}

// visibleColumns counts the display columns of runes, skipping ANSI escape
// sequences. Like the rest of this file it assumes one column per visible rune,
// matching the column-width bookkeeping in format_cell/3.
func visibleColumns(runes []rune) int {
	visible := 0
	for i := 0; i < len(runes); {
		if runes[i] == 0x1b {
			i = ansiSeqLen(runes, i)
			continue
		}
		visible++
		i++
	}
	return visible
}

// ansiSeqLen returns the index just past the escape sequence starting at
// runes[i] (which must be ESC, 0x1b). It recognises CSI sequences (ESC '[' …
// final byte 0x40-0x7e) and bare two-rune escapes, matching the reCSISeq /
// reEscapeSeq patterns used by sanitize.
func ansiSeqLen(runes []rune, i int) int {
	n := len(runes)
	if i+1 < n && runes[i+1] == '[' {
		j := i + 2
		for j < n {
			r := runes[j]
			j++
			if r >= 0x40 && r <= 0x7e {
				break
			}
		}
		return j
	}
	if i+1 < n {
		return i + 2
	}
	return i + 1
}
