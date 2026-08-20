package bot

import (
	"context"
	"testing"

	"opencode-bot/internal/backend"
)

// TestReuseEmptySession — при пустой памяти (после рестарта) переиспользуется
// существующая пустая сессия, новая не создаётся.
func TestReuseEmptySession(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	fb.sessions = []backend.Session{{ID: "sess_empty", Title: "telegram-bot", Directory: "/workspace"}}
	fb.msgCount["sess_empty"] = 0
	b := newTestBot(t, ft, fb)

	id, err := b.sessionIDFor(context.Background())
	if err != nil {
		t.Fatalf("sessionIDFor: %v", err)
	}
	if id != "sess_empty" {
		t.Fatalf("id = %q, want sess_empty", id)
	}
	if got := b.currentSessionID(); got != "sess_empty" {
		t.Fatalf("currentSessionID = %q, want sess_empty", got)
	}
	if n := fb.createdSessions(); n != 0 {
		t.Fatalf("создано сессий %d, хотя есть пустая", n)
	}
}

// TestReuseEmptySessionSkipsUsed — сессия с сообщениями не переиспользуется;
// берётся пустая (даже если она старше).
func TestReuseEmptySessionSkipsUsed(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	fb.sessions = []backend.Session{
		{ID: "sess_used", Title: "telegram-bot", Directory: "/workspace"},
		{ID: "sess_empty", Title: "telegram-bot", Directory: "/workspace"},
	}
	fb.msgCount["sess_used"] = 2
	fb.msgCount["sess_empty"] = 0
	b := newTestBot(t, ft, fb)

	id, err := b.sessionIDFor(context.Background())
	if err != nil {
		t.Fatalf("sessionIDFor: %v", err)
	}
	if id != "sess_empty" {
		t.Fatalf("id = %q, want sess_empty", id)
	}
	if n := fb.createdSessions(); n != 0 {
		t.Fatalf("создано сессий %d, хотя есть пустая", n)
	}
}

// TestNoEmptySessionCreatesNew — если пустых сессий нет, создаётся новая.
func TestNoEmptySessionCreatesNew(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	fb.sessions = []backend.Session{{ID: "sess_used", Title: "telegram-bot", Directory: "/workspace"}}
	fb.msgCount["sess_used"] = 5
	b := newTestBot(t, ft, fb)

	id, err := b.sessionIDFor(context.Background())
	if err != nil {
		t.Fatalf("sessionIDFor: %v", err)
	}
	if id != testSessID {
		t.Fatalf("id = %q, want %q", id, testSessID)
	}
	if n := fb.createdSessions(); n != 1 {
		t.Fatalf("создано сессий %d, want 1", n)
	}
}

// TestSessionIDCached — после установки сессия не ищется заново.
func TestSessionIDCached(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	b := newTestBot(t, ft, fb)

	if _, err := b.sessionIDFor(context.Background()); err != nil {
		t.Fatalf("sessionIDFor: %v", err)
	}
	fb.sessions = nil // сломали бы список, если бы снова искали
	if _, err := b.sessionIDFor(context.Background()); err != nil {
		t.Fatalf("повторный sessionIDFor: %v", err)
	}
	if n := fb.createdSessions(); n != 1 {
		t.Fatalf("создано сессий %d, want 1", n)
	}
}
