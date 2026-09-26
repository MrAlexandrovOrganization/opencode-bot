package bot

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"opencode-bot/internal/backend"
)

func TestMarkdownToHTML(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "fenced code block",
			in:   "```go\nfunc main() {}\n```",
			want: "<pre><code class=\"language-go\">func main() {}</code></pre>",
		},
		{
			name: "inline code and html escape",
			in:   "Use `x < 3` here.",
			want: "Use <code>x &lt; 3</code> here.",
		},
		{
			name: "bold and italic",
			in:   "**bold** and *italic*",
			want: "<b>bold</b> and <i>italic</i>",
		},
		{
			name: "header becomes bold",
			in:   "## Title\n\ntext",
			want: "<b>Title</b>\n\ntext",
		},
		{
			name: "list items",
			in:   "- one\n- two",
			want: "• one\n• two",
		},
		{
			name: "link",
			in:   "see [docs](https://opencode.ai)",
			want: "see <a href=\"https://opencode.ai\">docs</a>",
		},
		{
			name: "blockquote",
			in:   "> quoted",
			want: "<blockquote>quoted</blockquote>",
		},
		{
			name: "ordered list",
			in:   "1. first",
			want: "1. first",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := markdownToHTML(tt.in); got != tt.want {
				t.Errorf("markdownToHTML(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestBuildFinalChunks(t *testing.T) {
	info := backend.Message{
		Cost: 0.0123,
		Tokens: struct {
			Input  int `json:"input"`
			Output int `json:"output"`
		}{Input: 100, Output: 50},
	}
	log := []string{"✓ bash: npm test", "✓ read: internal/bot/bot.go"}
	reasoning := []string{"Let me look at the files."}
	chunks := buildFinalChunks("```go\nok\n```", info, log, reasoning)

	var joined string
	for i, c := range chunks {
		if len([]rune(c)) > maxMessageLen {
			t.Errorf("chunk %d exceeds %d runes: %d", i, maxMessageLen, len([]rune(c)))
		}
		joined += c + "\n"
	}
	for _, want := range []string{
		"📋 <b>Ход работы</b>",
		"✓ bash: npm test",
		"✓ read: internal/bot/bot.go",
		"🧠 Let me look at the files.",
		"<pre><code class=\"language-go\">ok</code></pre>",
		"💸 $0.0123",
		"⤴ 100",
		"⤵ 50",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("buildFinalChunks() missing %q in:\n%s", want, joined)
		}
	}
	// Activity first, answer after, footer last.
	if !strings.Contains(chunks[0], "bash: npm test") {
		t.Errorf("first chunk should hold the tool log, got %q", chunks[0])
	}
	// Сопроводительное сообщение не должно быть сплошным курсивом — раньше
	// каждая его строка была обёрнута в <i>.
	if strings.Contains(chunks[0], "<i>") {
		t.Errorf("activity chunk must not be italic, got %q", chunks[0])
	}
	if !strings.Contains(chunks[len(chunks)-1], "💸") {
		t.Errorf("last chunk should hold the footer, got %q", chunks[len(chunks)-1])
	}
}

func TestSplitMarkdownChunksLongAnswer(t *testing.T) {
	lines := make([]string, 0, 300)
	for i := 0; i < 300; i++ {
		lines = append(lines, fmt.Sprintf("line %d of the answer", i))
	}
	answer := strings.Join(lines, "\n")
	chunks := splitMarkdownChunks(answer)

	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	seen := map[string]bool{}
	for _, c := range chunks {
		if len([]rune(c)) > maxMessageLen {
			t.Errorf("chunk exceeds %d runes", maxMessageLen)
		}
		for _, line := range lines {
			if strings.Contains(c, line) {
				seen[line] = true
			}
		}
	}
	for _, line := range lines {
		if !seen[line] {
			t.Errorf("content lost: %q", line)
		}
	}
}

func TestSplitMarkdownChunksCodeBlock(t *testing.T) {
	code := make([]string, 0, 500)
	for i := 0; i < 500; i++ {
		code = append(code, fmt.Sprintf("x := %d", i))
	}
	answer := "```go\n" + strings.Join(code, "\n") + "\n```"
	chunks := splitMarkdownChunks(answer)

	seen := map[string]bool{}
	for _, c := range chunks {
		if len([]rune(c)) > maxMessageLen {
			t.Errorf("chunk exceeds %d runes", maxMessageLen)
		}
		for _, line := range code {
			if strings.Contains(c, line) {
				seen[line] = true
			}
		}
	}
	for _, line := range code {
		if !seen[line] {
			t.Errorf("code line lost: %q", line)
		}
	}
}

func TestSplitRunesUnicode(t *testing.T) {
	s := "héllo ❤️ 日本語"
	pieces := splitRunes(s, 3)
	if got := strings.Join(pieces, ""); got != s {
		t.Fatalf("rejoin = %q, want %q", got, s)
	}
	for _, p := range pieces {
		if len([]rune(p)) > 3 {
			t.Fatalf("piece %q has %d runes, want <=3", p, len([]rune(p)))
		}
	}
}

func TestSplitHTMLChunks(t *testing.T) {
	// Большой блок с вложенными тегами и длинным текстом: не должен
	// разрывать тег посередине и должен держать баланс тегов в каждом куске.
	html := "<b>Заголовок</b>\n" + strings.Repeat("<i>очень длинный текст </i>подробности ", 200)
	pieces := splitHTMLChunks(html, 400)
	if len(pieces) < 2 {
		t.Fatalf("ожидалось дробление на несколько кусков, получили %d", len(pieces))
	}
	for _, p := range pieces {
		// Допускаем небольшой перерасход на переоткрывающие теги
		// (<i> и т.п.), добавляемые в начало куска для баланса вложенности.
		// В проде maxMessageLen=4000 при лимите Telegram 4096 это безопасно.
		if len([]rune(p)) > 400+16 {
			t.Errorf("кусок из %d рун превышает лимит 400+16", len([]rune(p)))
		}
		if !balancedTags(p) {
			t.Errorf("кусок с несбалансированными тегами: %q", p)
		}
	}
	if got := strings.Join(pieces, ""); stripTags(got) != stripTags(html) {
		t.Errorf("текст кусков потерян:\n got=%q\nwant=%q", stripTags(got), stripTags(html))
	}
}

// balancedTags проверяет, что открытые/закрытые теги b/i/code/pre/a
// сбалансированы (игнорируя br/img).
func balancedTags(s string) bool {
	var stack []string
	for i := 0; i < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		j := i + 1
		for j < len(s) && s[j] != '>' {
			j++
		}
		if j >= len(s) {
			return false
		}
		tag := s[i : j+1]
		name := strings.TrimPrefix(tag, "</")
		name = strings.TrimPrefix(name, "<")
		if k := strings.IndexAny(name, " \t>"); k >= 0 {
			name = name[:k]
		}
		if name == "" || strings.HasSuffix(tag, "/>") || name == "br" || name == "img" {
			i = j
			continue
		}
		if strings.HasPrefix(tag, "</") {
			if len(stack) == 0 || stack[len(stack)-1] != name {
				return false
			}
			stack = stack[:len(stack)-1]
		} else {
			stack = append(stack, name)
		}
		i = j
	}
	return len(stack) == 0
}

// stripTags убирает все HTML-теги, оставляя только текстовое содержимое.
func stripTags(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '<' {
			for i < len(s) && s[i] != '>' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func TestTruncate(t *testing.T) {
	// Короткая строка возвращается как есть.
	if got := truncate("коротко"); got != "коротко" {
		t.Errorf("truncate(short) = %q, want %q", got, "коротко")
	}
	// Много-байтовые руны не должны разрываться посередине: результат
	// должен быть валидной UTF-8 строкой без ошибок декодирования.
	long := "日本語のテキストが含まれる очень длинное сообщение " + strings.Repeat("x", 5000)
	got := truncate(long)
	wantLen := maxMessageLen
	if len([]rune(got)) != wantLen {
		t.Errorf("truncate() length = %d runes, want %d", len([]rune(got)), wantLen)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncate() should end with '...', got %q", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncate() produced invalid UTF-8: %q", got)
	}
}

func TestFormatCost(t *testing.T) {
	for in, want := range map[float64]string{
		0:      "$0.00",
		0.0123: "$0.0123",
		0.1:    "$0.1",
		2:      "$2",
		0.0005: "$0.0005",
	} {
		if got := formatCost(in); got != want {
			t.Errorf("formatCost(%v) = %q, want %q", in, got, want)
		}
	}
}

// TestHTMLHelpersEscape — обёртки форматирования всегда экранируют текст:
// в них нельзя получить сломанный или подделанный HTML.
func TestHTMLHelpersEscape(t *testing.T) {
	for _, tc := range []struct {
		got, want string
	}{
		{htmlBold("а<b"), "<b>а&lt;b</b>"},
		{htmlItalic("x&y"), "<i>x&amp;y</i>"},
		{htmlCode("</code>"), "<code>&lt;/code&gt;</code>"},
		{htmlBlockquote("<script>"), "<blockquote>&lt;script&gt;</blockquote>"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}
