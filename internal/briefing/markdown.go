package briefing

import (
	"fmt"
	"strings"
)

// Markdown renders the briefing as the daily file. The layout is fixed
// (ARCHITECTURE.md section 14, Q12): frontmatter, Today, then Backlog.
func (b Briefing) Markdown() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "---\ndate: %s\ntoday: %d\nbacklog: %d\nfinal: %t\n---\n\n", b.Day, len(b.Today), len(b.Backlog), b.Final)
	fmt.Fprintf(&sb, "# Siftstr briefing %s\n", b.Day)
	section(&sb, "Today", b.Highlight, b.Today, "Nothing arrived today.")
	section(&sb, "Backlog", nil, b.Backlog, "Nothing carried over.")
	return sb.String()
}

// highlight writes the day's highlight as the first entry under Today.
func highlight(sb *strings.Builder, h *Highlight) {
	sb.WriteString("\n### Highlight\n\n")
	for _, line := range strings.Split(strings.TrimSpace(h.Text), "\n") {
		fmt.Fprintf(sb, "> %s\n", strings.TrimSpace(line))
	}
	if n := strings.TrimSpace(h.Note); n != "" {
		fmt.Fprintf(sb, "\nNote: %s\n", oneLine(n))
	}
	title := oneLine(h.Title)
	if title == "" {
		title = oneLine(h.Byline)
	}
	switch {
	case title != "" && h.URL != "":
		fmt.Fprintf(sb, "\nFrom [%s](%s)", escapeLink(title), linkURL(h.URL))
	case title != "":
		fmt.Fprintf(sb, "\nFrom %s", title)
	default:
		return
	}
	if by := oneLine(h.Byline); by != "" && by != title {
		fmt.Fprintf(sb, " · %s", by)
	}
	sb.WriteString("\n")
}

func section(sb *strings.Builder, name string, h *Highlight, entries []Entry, empty string) {
	fmt.Fprintf(sb, "\n## %s\n", name)
	if h != nil {
		highlight(sb, h)
	}
	if len(entries) == 0 {
		fmt.Fprintf(sb, "\n%s\n", empty)
		return
	}
	for _, e := range entries {
		sb.WriteString("\n### ")
		title := oneLine(e.Title)
		if e.URL != "" {
			fmt.Fprintf(sb, "[%s](%s)", escapeLink(title), linkURL(e.URL))
		} else {
			sb.WriteString(title)
		}
		meta := oneLine(e.Source)
		if e.MediaType != "" {
			meta += " · " + e.MediaType
		}
		if e.Outcome != "" {
			meta += " · **" + e.Outcome + "**"
		}
		fmt.Fprintf(sb, "\n\n%s\n", meta)
		if s := strings.TrimSpace(e.Summary); s != "" {
			fmt.Fprintf(sb, "\n%s\n", s)
		} else if x := strings.TrimSpace(e.Excerpt); x != "" {
			fmt.Fprintf(sb, "\n> %s\n", oneLine(x))
		}
	}
}

// oneLine keeps a title or source name on a single line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// escapeLink protects the brackets of a link label.
func escapeLink(s string) string {
	return strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`).Replace(s)
}

// linkURL keeps a URL from closing the link early.
func linkURL(u string) string {
	return strings.NewReplacer(" ", "%20", "(", "%28", ")", "%29", "<", "%3C", ">", "%3E").Replace(u)
}
