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
