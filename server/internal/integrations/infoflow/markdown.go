package infoflow

import (
	"regexp"
	"strings"
)

// markdownPatterns enumerate the syntax shapes we treat as evidence that the
// agent's reply is markdown rather than prose. Copied from the Lark adapter
// (lark/markdown_detect.go) so the two channels agree on what counts as
// markdown; the infoflow package must NOT import lark, so the detection lives
// here independently.
//
// Each pattern is intentionally conservative — better to false-positive (route
// plain text through the markdown element, which still renders fine) than to
// false-negative (leave `**bold**` characters visible in the user's transcript).
var markdownPatterns = []*regexp.Regexp{
	regexp.MustCompile("(?m)^#{1,6}[ \t]"),                        // headings: # H1, ###### H6
	regexp.MustCompile("(?m)^[ \t]*[-*+][ \t]"),                   // unordered list
	regexp.MustCompile("(?m)^[ \t]*\\d+\\.[ \t]"),                 // ordered list
	regexp.MustCompile("(?m)^>[ \t]"),                             // blockquote
	regexp.MustCompile("(?m)^[ \t]*(?:---|\\*\\*\\*|___)[ \t]*$"), // hr / thematic break
	regexp.MustCompile("\\*\\*[^*\\n]+\\*\\*"),                    // **bold**
	regexp.MustCompile("__[^_\\n]+__"),                            // __bold__
	regexp.MustCompile("(?m)^[ \t]*\\|.+\\|[ \t]*$"),              // table row (must have | on both ends)
	regexp.MustCompile("\\[[^\\]\\n]+\\]\\([^)\\n]+\\)"),          // [text](url) link / image
}

// containsMarkdown reports whether the body almost certainly contains markdown
// syntax that a plain-text `msgtype=TEXT` renderer would leave un-rendered
// (showing raw asterisks, hashes, pipes, etc.). On true, the group send path
// upgrades the element to a markdown element with header `msgtype=MD`.
//
// Fast-path tokens (backtick, fenced code) are checked first; only on a hit do
// we run the slower regex pass. Empty strings short-circuit to false.
func containsMarkdown(s string) bool {
	if s == "" {
		return false
	}
	// Fenced code block — strong markdown signal, cheap substring check.
	if strings.Contains(s, "```") {
		return true
	}
	// Inline code: only count `…` runs that look paired and contain a
	// non-space char. Bare backticks (e.g. quoting a single keystroke in prose)
	// shouldn't trigger.
	if i := strings.Index(s, "`"); i >= 0 {
		if j := strings.Index(s[i+1:], "`"); j > 0 {
			return true
		}
	}
	for _, re := range markdownPatterns {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// markdownTableRow matches a GFM table row: a line that starts (after optional
// indent) with `|` and also contains a `|` later, e.g. `| a | b |`.
var markdownTableRow = regexp.MustCompile(`^[ \t]*\|.*\|[ \t]*$`)

// markdownTableSeparator matches the `| --- | --- |` header separator line.
var markdownTableSeparator = regexp.MustCompile(`^[ \t]*\|[ \t:|-]+\|[ \t]*$`)

// markdownTaskItem matches a task-list line, capturing the checkbox state and
// the trailing text.
var markdownTaskItem = regexp.MustCompile(`^([ \t]*[-*+][ \t]+)\[([ xX])\]([ \t]+.*)?$`)

// normalizeForInfoflow rewrites the markdown constructs the InfoFlow client
// renders poorly into a shape it shows as readable text, leaving supported
// syntax untouched. It is applied to the reply BEFORE the markdown check and
// chunking, so the batches and the element type reflect the normalized body.
//
//   - GFM tables become one `- 列: 值；列: 值` line per data row (the header row
//     supplies the column labels; the `---` separator line is dropped).
//   - Task list items `- [x]` / `- [ ]` become `- 已完成` / `- 未完成`.
//   - Headings, bold, italic, inline code, links, blockquotes, ordered and
//     unordered lists, and fenced code blocks pass through unchanged.
func normalizeForInfoflow(s string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		// GFM table: a header row followed by a separator row. Collect the
		// whole block (header + separator + data rows) and rewrite it.
		if isTableRow(line) && i+1 < len(lines) && markdownTableSeparator.MatchString(lines[i+1]) {
			header := splitTableRow(line)
			i += 2 // skip header + separator
			for ; i < len(lines) && isTableRow(lines[i]); i++ {
				cells := splitTableRow(lines[i])
				out = append(out, tableRowToBullet(header, cells))
			}
			i-- // the loop's i++ advances past the last data row
			continue
		}
		if m := markdownTaskItem.FindStringSubmatch(line); m != nil {
			marker := "未完成"
			if strings.EqualFold(m[2], "x") {
				marker = "已完成"
			}
			out = append(out, m[1]+marker+trimSuffixSpace(m[3]))
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func isTableRow(line string) bool {
	return markdownTableRow.MatchString(line) && !markdownTableSeparator.MatchString(line)
}

// splitTableRow splits `| a | b |` into ["a", "b"], trimming the outer pipes and
// each cell's surrounding whitespace.
func splitTableRow(line string) []string {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimPrefix(trimmed, "|")
	trimmed = strings.TrimSuffix(trimmed, "|")
	parts := strings.Split(trimmed, "|")
	cells := make([]string, 0, len(parts))
	for _, p := range parts {
		cells = append(cells, strings.TrimSpace(p))
	}
	return cells
}

// tableRowToBullet renders one table row as a bullet. When the header row has
// usable labels the bullet reads `- 列: 值；列: 值`; otherwise the raw cells are
// joined with `；`.
func tableRowToBullet(header, cells []string) string {
	if len(cells) == 0 {
		return "- "
	}
	parts := make([]string, 0, len(cells))
	for i, cell := range cells {
		if i < len(header) && header[i] != "" {
			parts = append(parts, header[i]+": "+cell)
		} else {
			parts = append(parts, cell)
		}
	}
	return "- " + strings.Join(parts, "；")
}

// trimSuffixSpace preserves the exact leading spacing of a task item's text
// while ensuring the marker and text are separated by one space when the source
// had trailing content.
func trimSuffixSpace(rest string) string {
	if strings.TrimSpace(rest) == "" {
		return ""
	}
	return " " + strings.TrimSpace(rest)
}
