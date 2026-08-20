package bot

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"opencode-bot/internal/backend"
	"opencode-bot/internal/config"

	"github.com/coder/websocket"
	"github.com/mymmrac/telego"
)

// e2e-тесты бота целиком: фейковый Telegram Bot API + фейковый шлюз
// (opencode-backend). Проверяют базовые сценарии общения пользователя с
// ботом через реальные клиенты (telego, backend.Client) и реальный WS.

const (
	testBotToken = "123456789:12345678901234567890123456789012345"
	testRootID   = int64(123)
	testChatID   = int64(456)
	testSessID   = "sess1"
)

// ── fake Telegram Bot API ────────────────────────────────────────────────────

type fakeTelegram struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	nextID   int
	messages map[int]*sentMessage
}

type sentMessage struct {
	text string
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	ft := &fakeTelegram{t: t, nextID: 100, messages: map[int]*sentMessage{}}
	ft.srv = httptest.NewServer(http.HandlerFunc(ft.route))
	t.Cleanup(ft.srv.Close)
	return ft
}

func (ft *fakeTelegram) route(w http.ResponseWriter, r *http.Request) {
	method := strings.TrimPrefix(r.URL.Path, "/bot"+testBotToken+"/")
	if method == r.URL.Path {
		http.NotFound(w, r)
		return
	}
	switch method {
	case "sendChatAction":
		writeJSON(w, map[string]any{"ok": true, "result": true})
	case "sendMessage":
		ft.handleSendMessage(w, r)
	case "editMessageText":
		ft.handleEditMessage(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (ft *fakeTelegram) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	var params struct {
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}
	_ = json.NewDecoder(r.Body).Decode(&params)
	ft.mu.Lock()
	ft.nextID++
	id := ft.nextID
	ft.messages[id] = &sentMessage{text: params.Text}
	ft.mu.Unlock()
	writeJSON(w, map[string]any{
		"ok": true,
		"result": map[string]any{
			"message_id": id,
			"chat":       map[string]any{"id": params.ChatID, "type": "private"},
			"text":       params.Text,
		},
	})
}

func (ft *fakeTelegram) handleEditMessage(w http.ResponseWriter, r *http.Request) {
	var params struct {
		MessageID int    `json:"message_id"`
		Text      string `json:"text"`
	}
	_ = json.NewDecoder(r.Body).Decode(&params)
	ft.mu.Lock()
	if m, ok := ft.messages[params.MessageID]; ok {
		m.text = params.Text
	}
	ft.mu.Unlock()
	writeJSON(w, map[string]any{
		"ok":     true,
		"result": map[string]any{"message_id": params.MessageID, "text": params.Text},
	})
}

// texts возвращает копию всех текущих текстов сообщений.
func (ft *fakeTelegram) texts() []string {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	var out []string
	for _, m := range ft.messages {
		out = append(out, m.text)
	}
	return out
}

// waitText ждёт, пока в каком-либо сообщении не появится substr, и возвращает
// все тексты. Падает по таймауту.
func (ft *fakeTelegram) waitText(t *testing.T, substr string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		texts := ft.texts()
		for _, s := range texts {
			if strings.Contains(s, substr) {
				return texts
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("telegram: сообщение, содержащее %q, не появилось; texts=%q", substr, ft.texts())
	return nil
}

// ── fake backend (opencode-backend) ──────────────────────────────────────────

type fakeBackend struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	gotMsg   []backend.MessageRequest // принятые send-message запросы
	stored   map[string]string        // messageID -> тело StoredMessage (JSON)
	replQ    [][][]string             // принятые answers на вопрос
	created  int                      // сколько раз создавалась сессия
	sessions []backend.Session        // выдаются в GET /api/v1/sessions
	msgCount map[string]int           // sessionID -> число сообщений

	pushCh  chan []byte
	wsReady chan struct{}
	wsOnce  sync.Once
}

func newFakeBackend(t *testing.T) *fakeBackend {
	fb := &fakeBackend{
		t:        t,
		pushCh:   make(chan []byte, 32),
		wsReady:  make(chan struct{}),
		stored:   map[string]string{},
		msgCount: map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/sessions", fb.handleCreateSession)
	mux.HandleFunc("GET /api/v1/sessions", fb.handleListSessions)
	mux.HandleFunc("GET /api/v1/sessions/{id}/messages", fb.handleListMessages)
	mux.HandleFunc("POST /api/v1/sessions/"+testSessID+"/messages", fb.handleSendMessage)
	mux.HandleFunc("GET /api/v1/sessions/"+testSessID+"/messages/asm1", fb.handleGetMessage)
	mux.HandleFunc("GET /api/v1/ws", fb.handleWS)
	mux.HandleFunc("POST /api/v1/questions/{qid}", fb.handleReplyQuestion)
	fb.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		close(fb.pushCh)
		fb.srv.Close()
	})
	return fb
}

func (fb *fakeBackend) handleListSessions(w http.ResponseWriter, r *http.Request) {
	fb.mu.Lock()
	sess := fb.sessions
	fb.mu.Unlock()
	writeJSON(w, sess)
}

func (fb *fakeBackend) handleListMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	fb.mu.Lock()
	n := fb.msgCount[id]
	fb.mu.Unlock()
	var out []map[string]any
	for i := 0; i < n; i++ {
		out = append(out, map[string]any{"id": "m" + id + string(rune('a'+i))})
	}
	writeJSON(w, out)
}

func (fb *fakeBackend) createdSessions() int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return fb.created
}

func (fb *fakeBackend) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer test-token" {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	w.WriteHeader(http.StatusCreated)
	fb.mu.Lock()
	fb.created++
	fb.mu.Unlock()
	writeJSON(w, map[string]any{
		"id": testSessID, "title": "telegram-bot", "directory": "/workspace",
		"createdAt": time.Now().UTC().Format(time.RFC3339),
	})
}

func (fb *fakeBackend) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	var req backend.MessageRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	fb.mu.Lock()
	fb.gotMsg = append(fb.gotMsg, req)
	fb.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]any{"messageID": "req-1"})
}

func (fb *fakeBackend) handleGetMessage(w http.ResponseWriter, r *http.Request) {
	fb.mu.Lock()
	body := fb.stored["asm1"]
	fb.mu.Unlock()
	if body == "" {
		writeJSON(w, map[string]any{"error": "сообщение не найдено"})
		w.WriteHeader(http.StatusNotFound)
		return
	}
	writeRaw(w, body)
}

func (fb *fakeBackend) handleReplyQuestion(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Answers [][]string `json:"answers"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	fb.mu.Lock()
	fb.replQ = append(fb.replQ, body.Answers)
	fb.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (fb *fakeBackend) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()
	fb.wsOnce.Do(func() { close(fb.wsReady) })
	for data := range fb.pushCh {
		if err := conn.Write(context.Background(), websocket.MessageText, data); err != nil {
			return
		}
	}
}

// push отправляет событие в WS-подписку бота.
func (fb *fakeBackend) push(t *testing.T, evType, payload string) {
	t.Helper()
	frame, err := json.Marshal(struct {
		Type    string          `json:"type"`
		Session string          `json:"session"`
		Payload json.RawMessage `json:"payload"`
	}{Type: evType, Session: testSessID, Payload: json.RawMessage(payload)})
	if err != nil {
		t.Fatal(err)
	}
	fb.pushCh <- frame
}

func (fb *fakeBackend) receivedMessages() int {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return len(fb.gotMsg)
}

func (fb *fakeBackend) questionReplies() [][][]string {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return append([][][]string(nil), fb.replQ...)
}

// ── помощники событий ────────────────────────────────────────────────────────

const (
	evUserEcho = `{"info":{"id":"user1","sessionID":"sess1","role":"user","finish":"","time":{"created":1000},"cost":0,"tokens":{"input":0,"output":0}}}`
	evUserPart = `{"part":{"id":"p-user","messageID":"user1","sessionID":"sess1","type":"text","text":"hello","tool":"","state":null},"delta":"hello"}`
	// Промежуточный шаг с тул-вызовом: глобальное событие без top-level sessionID.
	// Бот не должен финализировать на нём.
	evToolCall = `{"info":{"id":"toolmsg","sessionID":"sess1","role":"assistant","finish":"tool-calls","time":{"created":1001,"completed":1002},"cost":0,"tokens":{"input":1,"output":1}}}`
	// Финальное сообщение: публикация runMessage (top-level sessionID).
	evFinal = `{"sessionID":"sess1","info":{"id":"asm1","sessionID":"sess1","role":"assistant","finish":"stop","time":{"created":1003,"completed":1004},"cost":0.0005,"tokens":{"input":5,"output":10}}}`
	// Ошибка: публикация runMessage c error (top-level sessionID).
	evError = `{"sessionID":"sess1","info":{"id":"asm1","sessionID":"sess1","role":"assistant","error":{"name":"Error","data":{"message":"внутренняя ошибка"}},"finish":"","time":{"created":1003,"completed":1004},"cost":0,"tokens":{"input":0,"output":0}}}`
)

// ── сборка бота ──────────────────────────────────────────────────────────────

func newTestBot(t *testing.T, ft *fakeTelegram, fb *fakeBackend) *Bot {
	t.Helper()
	api, err := telego.NewBot(testBotToken,
		telego.WithAPIServer(ft.srv.URL),
		telego.WithDiscardLogger(),
	)
	if err != nil {
		t.Fatalf("NewBot: %v", err)
	}
	bc := backend.New(fb.srv.URL, "test-token")
	cfg := &config.Config{RootID: testRootID, DefaultAgent: "build", RequestTimeout: 30 * time.Second}
	return New(api, bc, nil, cfg)
}

func textUpdate(text string) telego.Update {
	return telego.Update{
		UpdateID: 1,
		Message: &telego.Message{
			MessageID: 1,
			From:      &telego.User{ID: testRootID},
			Chat:      telego.Chat{ID: testChatID, Type: "private"},
			Text:      text,
		},
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeRaw(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(s))
}

// waitFor поллит cond до истины или таймаута.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("условие не выполнилось за таймаут")
}

// ── тесты ────────────────────────────────────────────────────────────────────

// TestE2ETextMessage — базовый сценарий: текстовый запрос, стриминг, ответ.
// Включает регрессию: промежуточный шаг с finish="tool-calls" не должен
// завершать запрос; ответ берётся из истории шлюза, а user-эхо не попадает
// в вывод.
func TestE2ETextMessage(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	fb.stored["asm1"] = `{"id":"asm1","role":"assistant","status":"completed","parts":[{"type":"text","text":"Привет!"}]}`
	b := newTestBot(t, ft, fb)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.eventLoop(ctx)

	select {
	case <-fb.wsReady:
	case <-time.After(2 * time.Second):
		t.Fatal("ws-подписка не поднялась")
	}

	b.handleUpdate(textUpdate("hello"))
	waitFor(t, func() bool { return fb.receivedMessages() == 1 })

	fb.push(t, "message.updated", evUserEcho)
	fb.push(t, "message.part.updated", evUserPart)
	fb.push(t, "message.updated", evToolCall)
	fb.push(t, "message.updated", evFinal)

	texts := ft.waitText(t, "Привет!")
	joined := strings.Join(texts, "\n")
	if strings.Contains(joined, "hello") {
		t.Fatalf("user-эхо попало в вывод: %q", joined)
	}
	if !strings.Contains(joined, "⤴ 5 · ⤵ 10") {
		t.Fatalf("футер токенов отсутствует: %q", joined)
	}
}

// TestE2EBusyRejectsSecondRequest — пока запрос в полёте, новый отклоняется.
func TestE2EBusyRejectsSecondRequest(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	fb.stored["asm1"] = `{"id":"asm1","role":"assistant","status":"completed","parts":[{"type":"text","text":"Привет!"}]}`
	b := newTestBot(t, ft, fb)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.eventLoop(ctx)

	select {
	case <-fb.wsReady:
	case <-time.After(2 * time.Second):
		t.Fatal("ws-подписка не поднялась")
	}

	b.handleUpdate(textUpdate("first"))
	waitFor(t, func() bool { return fb.receivedMessages() == 1 })

	b.handleUpdate(textUpdate("second"))
	ft.waitText(t, "⏳ Подожди")

	// завершаем первый запрос, чтобы бот вернулся в не-busy состояние
	fb.push(t, "message.updated", evUserEcho)
	fb.push(t, "message.updated", evFinal)
	ft.waitText(t, "Привет!")
}

// TestE2EErrorFinal — финальный message.updated c ошибкой показывает ❌.
func TestE2EErrorFinal(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	b := newTestBot(t, ft, fb)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.eventLoop(ctx)

	select {
	case <-fb.wsReady:
	case <-time.After(2 * time.Second):
		t.Fatal("ws-подписка не поднялась")
	}

	b.handleUpdate(textUpdate("boom"))
	waitFor(t, func() bool { return fb.receivedMessages() == 1 })

	fb.push(t, "message.updated", evUserEcho)
	fb.push(t, "message.updated", evError)

	ft.waitText(t, "❌ внутренняя ошибка")
}
