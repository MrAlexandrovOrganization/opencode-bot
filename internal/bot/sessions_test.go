package bot

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"opencode-bot/internal/backend"

	"github.com/mymmrac/telego"
)

// ── Переиспользование пустых сессий (sessionIDFor) ─────────────────────────

// TestReuseEmptySession — при пустой памяти (после рестарта) переиспользуется
// существующая пустая сессия, новая не создаётся.
func TestReuseEmptySession(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	fb.sessions = []backend.Session{{ID: "sess_empty", Title: "telegram-bot", Directory: "/workspace"}}
	fb.msgCount["sess_empty"] = 0
	b := newTestBot(t, ft, fb)

	id, fresh, err := b.sessionIDFor(context.Background())
	if err != nil {
		t.Fatalf("sessionIDFor: %v", err)
	}
	if !fresh {
		t.Fatal("взятая пустая сессия должна быть «свежей» (в неё не писали)")
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

	id, fresh, err := b.sessionIDFor(context.Background())
	if err != nil {
		t.Fatalf("sessionIDFor: %v", err)
	}
	if !fresh {
		t.Fatal("взятая пустая сессия должна быть «свежей» (в неё не писали)")
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

	id, fresh, err := b.sessionIDFor(context.Background())
	if err != nil {
		t.Fatalf("sessionIDFor: %v", err)
	}
	if !fresh {
		t.Fatal("только что созданная сессия должна быть «свежей»")
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

	if _, fresh, err := b.sessionIDFor(context.Background()); err != nil {
		t.Fatalf("sessionIDFor: %v", err)
	} else if !fresh {
		t.Fatal("первая сессия должна быть «свежей»")
	}
	fb.sessions = nil // сломали бы список, если бы снова искали
	if _, fresh, err := b.sessionIDFor(context.Background()); err != nil {
		t.Fatalf("повторный sessionIDFor: %v", err)
	} else if fresh {
		t.Fatal("закэшированная сессия не должна считаться «свежей»")
	}
	if n := fb.createdSessions(); n != 1 {
		t.Fatalf("создано сессий %d, want 1", n)
	}
}

// ── Переключение сессий (/sessions, /rename, callback) ─────────────────────

func twoSessions(t time.Time) []backend.Session {
	return []backend.Session{
		{ID: "sess1", Title: "telegram-bot", Directory: "/workspace", CreatedAt: t},
		{ID: "sess2", Title: "Бэкенд", Directory: "/workspace", CreatedAt: t.Add(time.Hour)},
	}
}

func TestCmdSessionsListsActiveAndCounts(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	fb.sessions = twoSessions(time.Now().Add(-24 * time.Hour))
	fb.msgCount["sess1"] = 5
	fb.msgCount["sess2"] = 3
	fb.activities = []backend.SessionActivity{{SessionID: "sess2", State: "running", Status: "проверяю тесты"}}
	b := newTestBot(t, ft, fb)
	b.mu.Lock()
	b.sessionID = "sess1"
	b.mu.Unlock()

	b.handleUpdate(textUpdate("/sessions"))

	texts := ft.waitText(t, "Сессии")
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "(активна)") {
		t.Fatalf("активная сессия не отмечена: %q", joined)
	}
	if !strings.Contains(joined, "Бэкенд") {
		t.Fatalf("в списке нет второй сессии: %q", joined)
	}
	if !strings.Contains(joined, "💬 3") {
		t.Fatalf("не посчитаны сообщения второй сессии: %q", joined)
	}
	if !strings.Contains(joined, "🟡") || !strings.Contains(joined, "проверяю тесты") {
		t.Fatalf("не показан фоновый activity-статус: %q", joined)
	}
}

func TestSwitchSessionSwitchesAndResumes(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	fb.sessions = twoSessions(time.Now())
	b := newTestBot(t, ft, fb)
	b.mu.Lock()
	b.sessionID = "sess1"
	b.mu.Unlock()

	query := &telego.CallbackQuery{
		ID:      "c1",
		Data:    "sess:switch:sess2",
		From:    telego.User{ID: testRootID},
		Message: &telego.Message{MessageID: 2, Chat: telego.Chat{ID: testChatID, Type: "private"}},
	}
	b.handleCallback(query)

	got := fb.resumedSessions()
	if len(got) != 1 || got[0] != "sess2" {
		t.Fatalf("resume не вызван для sess2: %v", got)
	}
	if b.currentSessionID() != "sess2" {
		t.Fatalf("сессия не переключилась: %q", b.currentSessionID())
	}
	ft.waitText(t, "Теперь активна сессия")
}

func TestSwitchSessionBusyRejected(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	fb.sessions = twoSessions(time.Now())
	b := newTestBot(t, ft, fb)
	b.mu.Lock()
	b.sessionID = "sess1"
	b.busy = true
	b.mu.Unlock()

	b.handleCallback(&telego.CallbackQuery{
		ID:   "c1",
		Data: "sess:switch:sess2",
		From: telego.User{ID: testRootID},
	})

	if b.currentSessionID() != "sess1" {
		t.Fatalf("busy-переключение изменило сессию: %q", b.currentSessionID())
	}
	if got := fb.resumedSessions(); len(got) != 0 {
		t.Fatalf("busy-переключение вызвало resume: %v", got)
	}
}

func TestSwitchSessionSameSessionNoop(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	fb.sessions = twoSessions(time.Now())
	b := newTestBot(t, ft, fb)
	b.mu.Lock()
	b.sessionID = "sess2"
	b.mu.Unlock()

	b.handleCallback(&telego.CallbackQuery{
		ID:   "c1",
		Data: "sess:switch:sess2",
		From: telego.User{ID: testRootID},
	})

	if b.currentSessionID() != "sess2" {
		t.Fatalf("сессия изменилась: %q", b.currentSessionID())
	}
	if got := fb.resumedSessions(); len(got) != 0 {
		t.Fatalf("resume вызван для активной сессии: %v", got)
	}
}

func TestCmdRenameCurrentSession(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	fb.sessions = twoSessions(time.Now())
	b := newTestBot(t, ft, fb)
	b.mu.Lock()
	b.sessionID = "sess1"
	b.mu.Unlock()

	b.handleUpdate(textUpdate("/rename Новый тайтл"))

	if got := fb.renamedSession("sess1"); got != "Новый тайтл" {
		t.Fatalf("rename не доехал до шлюза: %q", got)
	}
	ft.waitText(t, "переименована")
}

func TestCmdRenameWithoutArgsShowsUsage(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	b := newTestBot(t, ft, fb)

	b.handleUpdate(textUpdate("/rename"))

	ft.waitText(t, "Формат:")
}

// ── /reset не плодит пустые сессии ───────────────────────────────────────────

// TestResetEmptySessionReused — /reset на пустой (без сообщений) текущей
// сессии не создаёт новую и не удаляет старую, а оставляет текущую.
func TestResetEmptySessionReused(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	b := newTestBot(t, ft, fb)
	b.mu.Lock()
	b.sessionID = "sess_empty"
	b.mu.Unlock()
	fb.msgCount["sess_empty"] = 0

	b.handleUpdate(textUpdate("/reset"))

	ft.waitText(t, "пустая")
	if n := fb.createdSessions(); n != 0 {
		t.Fatalf("создано сессий %d, а пустая должна переиспользоваться", n)
	}
	if got := fb.deletedSessions(); len(got) != 0 {
		t.Fatalf("удалена(ы) сессия(и): %v, пустая не должна удаляться", got)
	}
	if b.currentSessionID() != "sess_empty" {
		t.Fatalf("currentSessionID = %q, хотим sess_empty", b.currentSessionID())
	}
}

// TestResetNonEmptySessionReplaced — /reset на непустой сессии удаляет её и
// создаёт новую.
func TestResetNonEmptySessionReplaced(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	b := newTestBot(t, ft, fb)
	b.mu.Lock()
	b.sessionID = "sess_used"
	b.mu.Unlock()
	fb.msgCount["sess_used"] = 3

	b.handleUpdate(textUpdate("/reset"))

	ft.waitText(t, "Новая сессия создана")
	if got := fb.deletedSessions(); len(got) != 1 || got[0] != "sess_used" {
		t.Fatalf("удалено: %v, хотим [sess_used]", got)
	}
	if n := fb.createdSessions(); n != 1 {
		t.Fatalf("создано сессий %d, хотим 1", n)
	}
}

// TestResetNoSessionCreates — /reset без текущей сессии создаёт новую.
func TestResetNoSessionCreates(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	b := newTestBot(t, ft, fb)

	b.handleUpdate(textUpdate("/reset"))

	ft.waitText(t, "Новая сессия создана")
	if n := fb.createdSessions(); n != 1 {
		t.Fatalf("создано сессий %d, хотим 1", n)
	}
	if got := fb.deletedSessions(); len(got) != 0 {
		t.Fatalf("удалено лишнее: %v", got)
	}
}

// ── Названия сессий и восстановление истории ────────────────────────────────

// TestCmdSessionsShowsTitlesFromHistory — сессии с заглушкой «telegram-bot»
// подписываются первым сообщением пользователя: и в строке списка, и в кнопке
// переключения, иначе все сессии неотличимы друг от друга.
func TestCmdSessionsShowsTitlesFromHistory(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	now := time.Now()
	fb.sessions = []backend.Session{
		{ID: "sess1", Title: "Бэкенд", Directory: "/workspace", CreatedAt: now},
		{ID: "sess2", Title: "telegram-bot", Directory: "/workspace", CreatedAt: now.Add(time.Hour)},
	}
	fb.hist["sess2"] = []backend.StoredMessage{
		{ID: "u1", Role: "user", Status: "pending",
			Parts: json.RawMessage(`[{"type":"text","text":"Почини баг в CI"}]`), CreatedAt: now},
		{ID: "a1", Role: "assistant", Status: "completed",
			Parts: json.RawMessage(`[{"type":"text","text":"Готово"}]`), CreatedAt: now},
	}
	b := newTestBot(t, ft, fb)
	b.mu.Lock()
	b.sessionID = "sess1"
	b.mu.Unlock()

	b.handleUpdate(textUpdate("/sessions"))

	texts := ft.waitText(t, "Сессии")
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "Почини баг в CI") {
		t.Fatalf("в списке нет названия из истории: %q", joined)
	}
	if !strings.Contains(joined, "Бэкенд") {
		t.Fatalf("потерялось своё название сессии: %q", joined)
	}
	if strings.Contains(joined, "<code>telegram-bot</code>") {
		t.Fatalf("заглушка не должна показываться: %q", joined)
	}
	var buttons string
	for _, text := range ft.buttonTexts() {
		buttons += text + "\n"
	}
	if !strings.Contains(buttons, "Почини баг в CI") {
		t.Fatalf("кнопка подписана не названием сессии: %q", buttons)
	}
}

// TestSwitchSessionRestoresHistory — после переключения в чат восстанавливается
// история диалога: и реплики пользователя, и ответы агента.
func TestSwitchSessionRestoresHistory(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	now := time.Now()
	fb.sessions = twoSessions(now)
	fb.hist["sess2"] = []backend.StoredMessage{
		{ID: "u1", Role: "user", Status: "pending",
			Parts: json.RawMessage(`[{"type":"text","text":"сделай фичу"}]`), CreatedAt: now},
		{ID: "a1", Role: "assistant", Status: "completed",
			Parts: json.RawMessage(`[{"type":"text","text":"Фича **готова**"}]`), CreatedAt: now.Add(time.Minute)},
	}
	b := newTestBot(t, ft, fb)
	b.mu.Lock()
	b.sessionID = "sess1"
	b.mu.Unlock()

	b.handleCallback(&telego.CallbackQuery{
		ID:      "c1",
		Data:    "sess:switch:sess2",
		From:    telego.User{ID: testRootID},
		Message: &telego.Message{MessageID: 2, Chat: telego.Chat{ID: testChatID, Type: "private"}},
	})

	texts := ft.waitText(t, "Фича <b>готова</b>")
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "История сессии") {
		t.Fatalf("нет заголовка истории: %q", joined)
	}
	if !strings.Contains(joined, "сделай фичу") {
		t.Fatalf("в истории нет сообщения пользователя: %q", joined)
	}
	if !strings.Contains(joined, "2 сообщения") {
		t.Fatalf("не посчитаны сообщения истории: %q", joined)
	}
}

// TestSwitchSessionWithEmptyHistory — переключение на сессию без сообщений
// честно сообщает, что восстанавливать нечего.
func TestSwitchSessionWithEmptyHistory(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	fb.sessions = twoSessions(time.Now())
	b := newTestBot(t, ft, fb)
	b.mu.Lock()
	b.sessionID = "sess1"
	b.mu.Unlock()

	b.handleCallback(&telego.CallbackQuery{
		ID:      "c1",
		Data:    "sess:switch:sess2",
		From:    telego.User{ID: testRootID},
		Message: &telego.Message{MessageID: 2, Chat: telego.Chat{ID: testChatID, Type: "private"}},
	})

	ft.waitText(t, "ещё нет сообщений")
}

// TestFirstMessageAutoTitlesSession — первое сообщение задаёт название свежей
// сессии, чтобы в списках не висела заглушка «telegram-bot».
func TestFirstMessageAutoTitlesSession(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	b := newTestBot(t, ft, fb)

	b.handleUpdate(textUpdate("Почини баг в CI"))

	waitFor(t, func() bool { return fb.receivedMessages() == 1 })
	if got := fb.renamedSession(testSessID); got != "Почини баг в CI" {
		t.Fatalf("название сессии = %q, хотим по первому сообщению", got)
	}
}

// TestSecondMessageDoesNotRenameSession — автоназвание ставится только один
// раз: повторные сообщения не переписывают название (и его можно менять через
// /rename).
func TestSecondMessageDoesNotRenameSession(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	b := newTestBot(t, ft, fb)

	b.handleUpdate(textUpdate("Почини баг в CI"))
	waitFor(t, func() bool { return fb.renamedSession(testSessID) != "" })
	if _, _, err := b.sessionIDFor(context.Background()); err != nil {
		t.Fatalf("sessionIDFor: %v", err)
	}
	b.handleUpdate(textUpdate("другое сообщение"))

	if got := fb.renamedSession(testSessID); got != "Почини баг в CI" {
		t.Fatalf("название перезаписано повторным сообщением: %q", got)
	}
}
