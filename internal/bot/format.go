package bot

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	reHeader      = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)
	reOrderedList = regexp.MustCompile(`^(\d+[.)])\s+(.+)$`)
	reInlineCode  = regexp.MustCompile("`([^`]+)`")
	reBold        = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reItalic      = regexp.MustCompile(`\*([^*\s][^*]*)\*`)
	reLink        = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
)

// markdownToHTML converts markdown into a Telegram-safe HTML fragment
// (Telegram HTML parse mode supports b/i/code/pre/blockquote/a). Everything
// is HTML-escaped first, so the output is always valid for editMessageText.
func markdownToHTML(s string) string {
	var sb strings.Builder
	lines := strings.Split(s, "\n")
	var quote []string
	flushQuote := func() {
		if len(quote) == 0 {
			return
		}
		sb.WriteString("<blockquote>")
		for j, l := range quote {
			if j > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(inlineHTML(l))
		}
		sb.WriteString("</blockquote>\n")
		quote = nil
	}

	i := 0
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])

		// Fenced code block.
		if strings.HasPrefix(trimmed, "```") {
			flushQuote()
			lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			i++
			var code []string
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				code = append(code, lines[i])
				i++
			}
			i++ // closing fence
			sb.WriteString("<pre><code")
			if lang != "" {
				sb.WriteString(" class=\"language-")
				sb.WriteString(escapeHTML(lang))
				sb.WriteString("\"")
			}
			sb.WriteString(">")
			sb.WriteString(escapeHTML(strings.Join(code, "\n")))
			sb.WriteString("</code></pre>\n")
			continue
		}

		// Blockquote (contiguous "> " lines).
		if strings.HasPrefix(trimmed, ">") {
			quote = append(quote, strings.TrimSpace(strings.TrimPrefix(trimmed, ">")))
			i++
			continue
		}

		// Blank line.
		if trimmed == "" {
			flushQuote()
			sb.WriteByte('\n')
			i++
			continue
		}

		flushQuote()

		// Headers -> bold.
		if m := reHeader.FindStringSubmatch(trimmed); m != nil {
			sb.WriteString("<b>")
			sb.WriteString(inlineHTML(m[2]))
			sb.WriteString("</b>\n")
			i++
			continue
		}

		// Horizontal rule.
		if trimmed == "---" || trimmed == "***" || trimmed == "___" {
			sb.WriteString("────────────\n")
			i++
			continue
		}

		// Bullet / ordered list items.
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
			sb.WriteString("• ")
			sb.WriteString(inlineHTML(strings.TrimSpace(trimmed[2:])))
			sb.WriteByte('\n')
			i++
			continue
		}
		if m := reOrderedList.FindStringSubmatch(trimmed); m != nil {
			sb.WriteString(m[1])
			sb.WriteString(" ")
			sb.WriteString(inlineHTML(strings.TrimSpace(m[2])))
			sb.WriteByte('\n')
			i++
			continue
		}

		// Plain paragraph line.
		sb.WriteString(inlineHTML(trimmed))
		sb.WriteByte('\n')
		i++
	}
	flushQuote()
	return strings.TrimRight(sb.String(), "\n")
}

// inlineHTML applies inline formatting (code, links, bold, italic) to a
// single already-HTML-escaped line.
func inlineHTML(s string) string {
	s = escapeHTML(s)

	// Hold inline code spans aside so links/bold/italic can't touch them.
	var codes []string
	withCodes := reInlineCode.ReplaceAllStringFunc(s, func(m string) string {
		codes = append(codes, m[1:len(m)-1])
		return fmt.Sprintf("\x00C%d\x00", len(codes)-1)
	})

	out := reLink.ReplaceAllStringFunc(withCodes, func(m string) string {
		parts := reLink.FindStringSubmatch(m)
		href := strings.ReplaceAll(parts[2], `"`, "%22")
		return "<a href=\"" + href + "\">" + parts[1] + "</a>"
	})
	out = reBold.ReplaceAllString(out, "<b>$1</b>")
	out = reItalic.ReplaceAllString(out, "<i>$1</i>")

	for j := len(codes) - 1; j >= 0; j-- {
		out = strings.ReplaceAll(out, fmt.Sprintf("\x00C%d\x00", j), "<code>"+codes[j]+"</code>")
	}
	return out
}
