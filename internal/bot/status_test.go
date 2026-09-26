package bot

import (
	"strings"
	"testing"
)

func TestToolStatus(t *testing.T) {
	tests := []struct {
		name  string
		tool  string
		state toolPartState
		want  string
	}{
		{
			name: "bash with title",
			tool: "bash",
			state: toolPartState{
				Status: "running",
				Title:  "npm test",
			},
			want: "⚙️ bash: npm test",
		},
		{
			name: "bash falls back to input command",
			tool: "bash",
			state: toolPartState{
				Status: "running",
				Input:  map[string]any{"command": "ls -la"},
			},
			want: "⚙️ bash: ls -la",
		},
		{
			name: "grep falls back to pattern",
			tool: "grep",
			state: toolPartState{
				Status: "running",
				Input:  map[string]any{"pattern": "func main"},
			},
			want: "⚙️ grep: func main",
		},
		{
			name:  "no title or input",
			tool:  "webfetch",
			state: toolPartState{Status: "running"},
			want:  "⚙️ webfetch",
		},
		{
			name: "long input is truncated",
			tool: "bash",
			state: toolPartState{
				Status: "running",
				Input:  map[string]any{"command": strings.Repeat("x", 100)},
			},
			want: "⚙️ bash: " + strings.Repeat("x", 51) + "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolStatus(tt.tool, tt.state); got != tt.want {
				t.Errorf("toolStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestRenderPreviewHTML — живое превью и foreground-стрима, и фонового окна:
// последние завершённые тулы, затем текущая активность в приоритете
// статус тул-вызова > черновик ответа > reasoning. Журнал и статус отдаются
// обычным текстом, черновик и reasoning — markdown.
func TestRenderPreviewHTML(t *testing.T) {
	log := []string{"✓ read: a.go", "✗ bash: make test — exit 1"}

	tests := []struct {
		name      string
		log       []string
		status    string
		partial   string
		reasoning string
		want      string
	}{
		{
			name:      "status has priority over partial and reasoning",
			log:       log,
			status:    "⚙️ bash: make check",
			partial:   "Чиню очередь",
			reasoning: "думаю",
			want:      "✓ read: a.go\n✗ bash: make test — exit 1\n⚙️ bash: make check",
		},
		{
			name:      "partial without status",
			log:       log,
			partial:   "Чиню очередь",
			reasoning: "думаю",
			want:      "✓ read: a.go\n✗ bash: make test — exit 1\nЧиню очередь",
		},
		{
			name:      "reasoning is prefixed and shortened",
			log:       nil,
			reasoning: "обычный ход мысли",
			want:      "🧠 обычный ход мысли",
		},
		{name: "nothing to show", log: nil, want: ""},
		{
			name: "only the last six tool lines are kept",
			log:  []string{"1", "2", "3", "4", "5", "6", "7", "8"},
			want: "3\n4\n5\n6\n7\n8\n",
		},
		{
			name:    "partial is rendered as markdown",
			partial: "## Итог\n\n**готово** и `код`",
			want:    "<b>Итог</b>\n\n<b>готово</b> и <code>код</code>",
		},
		{
			name: "tool line is escaped, not markdown",
			log:  []string{"✓ glob: *.go *.md"},
			want: "✓ glob: *.go *.md\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderPreviewHTML(tt.log, tt.status, tt.partial, tt.reasoning); got != tt.want {
				t.Errorf("renderPreviewHTML() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestBackgroundProgressHTML — фоновое окно без активности показывает
// «Выполняется…», с активностью — заголовок сессии и общий превью-текст.
func TestBackgroundProgressHTML(t *testing.T) {
	if got := backgroundProgressHTML("Моя сессия", ""); !strings.Contains(got, "💭 Выполняется…") {
		t.Errorf("пустое окно: %q", got)
	}
	got := backgroundProgressHTML("Моя сессия", "✓ bash: make check")
	want := "📎 Фоновая сессия <b>Моя сессия</b>\n✓ bash: make check"
	if got != want {
		t.Errorf("backgroundProgressHTML() = %q, want %q", got, want)
	}
}
