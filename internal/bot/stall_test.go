package bot

import (
	"encoding/json"
	"testing"
	"time"

	"opencode-bot/internal/backend"
)

func TestLastAssistantResult(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	since := now.Add(-10 * time.Minute)

	stored := func(id, role, status string, created time.Time) backend.StoredMessage {
		m := backend.StoredMessage{ID: id, Role: role, Status: status, CreatedAt: created}
		if role == "assistant" && status == "completed" {
			m.Parts = json.RawMessage(`[{"type":"text","text":"ответ"}]`)
		}
		return m
	}

	tests := []struct {
		name  string
		msgs  []backend.StoredMessage
		since time.Time
		want  string // ожидаемый ID результата; "" — nil
	}{
		{
			name: "empty history",
			want: "",
		},
		{
			name: "only user messages",
			msgs: []backend.StoredMessage{
				stored("u1", "user", "completed", now),
			},
			want: "",
		},
		{
			name: "assistant still pending",
			msgs: []backend.StoredMessage{
				stored("a1", "assistant", "pending", now),
			},
			want: "",
		},
		{
			name: "old assistant before request start",
			msgs: []backend.StoredMessage{
				stored("a0", "assistant", "completed", since.Add(-time.Minute)),
			},
			since: since,
			want:  "",
		},
		{
			name: "picks latest assistant after request start",
			msgs: []backend.StoredMessage{
				stored("u1", "user", "completed", now.Add(-2*time.Minute)),
				stored("a0", "assistant", "completed", now.Add(-time.Minute)),
				stored("a1", "assistant", "completed", now),
			},
			since: since,
			want:  "a1",
		},
		{
			name: "error result is also a candidate",
			msgs: []backend.StoredMessage{
				stored("u1", "user", "completed", now.Add(-2*time.Minute)),
				{ID: "a1", Role: "assistant", Status: "error", CreatedAt: now,
					Info: json.RawMessage(`{"error":{"name":"Error","data":{"message":"упало"}}}`)},
			},
			since: since,
			want:  "a1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := lastAssistantResult(tt.msgs, tt.since)
			if tt.want == "" {
				if got != nil {
					t.Fatalf("lastAssistantResult() = %+v, want nil", got)
				}
				return
			}
			if got == nil || got.ID != tt.want {
				t.Fatalf("lastAssistantResult() = %+v, want ID %q", got, tt.want)
			}
		})
	}
}
