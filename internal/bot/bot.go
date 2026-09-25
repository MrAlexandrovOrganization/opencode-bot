package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"opencode-bot/internal/backend"
	"opencode-bot/internal/config"
	"opencode-bot/internal/whisper"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// maxMessageLen is the safe Telegram message length (actual limit is 4096).
const maxMessageLen = 4000

// maxQueuedTextMessages ограничивает очередь обычных текстовых запросов в памяти.
// Вложения и голосовые в неё намеренно не попадают: повтор проиграл бы скачивание
// или транскрибацию спустя непредсказуемое время.
const maxQueuedTextMessages = 5

// stallTimeout — если от шлюза нет событий дольше этого времени и финальное
// message.updated так и не пришло, превью принудительно завершается, чтобы
// сообщение с курсором не висело в чате вечно (иллюзия зависания). Значение
// намеренно заметно больше обычных пауз (долгий тихий tool без streaming,
// медленная модель), чтобы не рушить живые запросы; потерянный финал
// дофинализируется из истории шлюза в resolveStall.
const stallTimeout = 5 * time.Minute

// Bot is the main application struct.
type Bot struct {
	api        *telego.Bot
	backend    *backend.Client
	whisper    *whisper.Client // nil if Whisper is not configured
	cfg        *config.Config
	httpClient *http.Client // для скачивания файлов из Telegram (с таймаутом)

	mu        sync.Mutex
	sessionID string
	chatID    int64 // telegram chat of the authorized user
	busy      bool  // true while an opencode request is in flight
	stream    *Stream
	model     *backend.ModelRef
	agent     string
	userMsgID string              // messageID user-эха текущего запроса (части игнорируются)
	perms     map[string]*permAsk // permissionID -> pending "ask" prompt
	pendingQ  *pendingQuestions   // question the agent is awaiting an answer to
	queued    []queuedText        // текстовые запросы, пришедшие во время выполнения

	// activeJob — активная транскрибация Whisper, чтобы пользователь мог
	// отменить её кнопкой «Отменить» (как в transcriber-bot). Единственная,
	// т.к. бот однопользовательский и серийный (busy-флаг).
	activeJob *transcriptionJob

	// reqCtx/reqCancel — контекст текущего запроса. Отменяется при
	// завершении (finishStream) или сбросе (release), чтобы прервать
	// долгие операции внешних сервисов (например, опрос транскрибации
	// Whisper), которые иначе продолжались бы до своего собственного
	// дедлайна, удерживая busy-флаг.
	reqCtx    context.Context
	reqCancel context.CancelFunc
}

type queuedText struct {
	chatID int64
	text   string
}

// Stream tracks the in-flight assistant message so the WebSocket bus can
// update the Telegram placeholder with live partial text or the current
// agent activity (tool calls / reasoning).
type Stream struct {
	chatID       int64
	messageID    int
	mu           sync.Mutex
	partial      string   // streamed assistant text
	reasoning    string   // reasoning of the current step
	reasoningLog []string // reasoning of all completed steps
	status       string   // transient "what the agent is doing now" line
	log          []string // completed tool calls (✓/✗ lines)

	done         chan struct{} // closed when the response is finalizing
	stopped      chan struct{} // closed by the preview loop when it exits
	finalizeOnce sync.Once
	started      time.Time // время начала запроса — для привязки ответа в истории
	lastActivity time.Time // последнее событие от шлюза (part), для страховки зависания
}

// touch фиксирует активность шлюза, сбрасывая таймер зависания.
func (st *Stream) touch() {
	st.mu.Lock()
	st.lastActivity = time.Now()
	st.mu.Unlock()
}

// stalled возвращает true, если давно (stallTimeout) не было событий от шлюза
// и финал так и не пришёл. Ожидание ответа пользователя (permission/question)
// зависанием не считается: это легитимная пауза, событий от шлюза в ней и не
// должно быть.
func (b *Bot) stalled(st *Stream) bool {
	b.mu.Lock()
	waitingOnUser := len(b.perms) > 0 || b.pendingQ != nil
	b.mu.Unlock()
	if waitingOnUser {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.lastActivity.IsZero() {
		return false
	}
	return time.Since(st.lastActivity) > stallTimeout
}

// toolPartState mirrors the "tool" part state of the opencode server.
type toolPartState struct {
	Status string         `json:"status"`
	Title  string         `json:"title"`
	Input  map[string]any `json:"input"`
	Error  string         `json:"error"`
}

// permAsk is a pending permission prompt sent to Telegram.
type permAsk struct {
	created time.Time
}

// New creates a new Bot.
func New(api *telego.Bot, backendClient *backend.Client, whisperClient *whisper.Client, cfg *config.Config) *Bot {
	return &Bot{
		api:        api,
		backend:    backendClient,
		whisper:    whisperClient,
		cfg:        cfg,
		httpClient: &http.Client{Timeout: 2 * time.Minute},
		agent:      cfg.DefaultAgent,
		perms:      make(map[string]*permAsk),
	}
}

// ── Session management ───────────────────────────────────────────────────────

// sessionIDFor returns the current opencode session, reusing an existing empty
// session (после рестарта бота) or creating a new one lazily.
func (b *Bot) sessionIDFor(ctx context.Context) (string, error) {
	b.mu.Lock()
	id := b.sessionID
	b.mu.Unlock()
	if id != "" {
		return id, nil
	}
	if id, ok := b.reuseEmptySession(ctx); ok {
		return id, nil
	}
	return b.newSession(ctx)
}

// reuseEmptySession переиспользует существующую пустую сессию (без сообщений),
// чтобы после рестарта бота не плодить новые сессии. Среди пустых выбирает
// самую свежую; вернёт ok=false, если подходящей нет или шлюз недоступен.
func (b *Bot) reuseEmptySession(ctx context.Context) (string, bool) {
	sessions, err := b.backend.ListSessions(ctx)
	if err != nil {
		slog.Warn("list sessions for reuse", "error", err)
		return "", false
	}
	// Сессии отсортированы по CreatedAt (возрастание) — идём от самых свежих.
	for i := len(sessions) - 1; i >= 0; i-- {
		sess := sessions[i]
		msgs, err := b.backend.ListMessages(ctx, sess.ID)
		if err != nil {
			continue
		}
		if len(msgs) > 0 {
			continue
		}
		b.mu.Lock()
		b.sessionID = sess.ID
		b.mu.Unlock()
		slog.Info("reusing empty session", "id", sess.ID, "directory", sess.Directory)
		return sess.ID, true
	}
	return "", false
}

func (b *Bot) newSession(ctx context.Context) (string, error) {
	s, err := b.backend.CreateSession(ctx, "telegram-bot")
	if err != nil {
		return "", err
	}
	b.mu.Lock()
	b.sessionID = s.ID
	b.mu.Unlock()
	slog.Info("session created", "id", s.ID, "directory", s.Directory)
	return s.ID, nil
}

// ── Concurrency ──────────────────────────────────────────────────────────────

// beginRequest захватывает busy-флаг и создаёт отменяемый контекст запроса,
// который будет отменён в release() (при финализации стрима или ошибке).
// Возвращает ok=false, если бот уже занят. Полученный ctx следует
// передавать во внешние вызовы (backend, Whisper, скачивание файлов), чтобы
// их можно было прервать при /abort, таймауте или завершении запроса.
func (b *Bot) beginRequest() (context.Context, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Файл или голосовое не должны обгонять уже принятые текстовые запросы.
	if b.busy || len(b.queued) > 0 {
		return nil, false
	}
	b.busy = true
	ctx, cancel := context.WithCancel(context.Background())
	b.reqCtx, b.reqCancel = ctx, cancel
	return ctx, true
}

// beginTextRequest либо сразу резервирует запрос, либо добавляет текст в FIFO-
// очередь. Решение атомарно с busy, поэтому новое сообщение не обгонит уже
// поставленное в очередь.
func (b *Bot) beginTextRequest(chatID int64, text string) (ctx context.Context, start bool, position int, full bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.busy || len(b.queued) > 0 {
		if len(b.queued) >= maxQueuedTextMessages {
			return nil, false, 0, true
		}
		b.queued = append(b.queued, queuedText{chatID: chatID, text: text})
		return nil, false, len(b.queued), false
	}
	b.busy = true
	ctx, cancel := context.WithCancel(context.Background())
	b.reqCtx, b.reqCancel = ctx, cancel
	return ctx, true, 0, false
}

// takeQueuedTextRequest резервирует самый ранний текст из очереди. Вынесен из
// dispatchNextQueued, чтобы FIFO и блокировки проверялись без Telegram и backend I/O.
func (b *Bot) takeQueuedTextRequest() (context.Context, queuedText, int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.busy || len(b.queued) == 0 {
		return nil, queuedText{}, 0, false
	}
	item := b.queued[0]
	b.queued = b.queued[1:]
	b.busy = true
	ctx, cancel := context.WithCancel(context.Background())
	b.reqCtx, b.reqCancel = ctx, cancel
	return ctx, item, len(b.queued), true
}

func (b *Bot) clearQueuedText() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(b.queued)
	b.queued = nil
	return n
}

// queuedTextSnapshot возвращает копию очереди для отображения без удержания
// блокировки на время работы Telegram API.
func (b *Bot) queuedTextSnapshot() []queuedText {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]queuedText(nil), b.queued...)
}

// requestContext возвращает контекст текущего запроса (или Background).
func (b *Bot) requestContext() context.Context {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.reqCtx != nil {
		return b.reqCtx
	}
	return context.Background()
}

func (b *Bot) release() {
	b.releaseRequest(true)
}

// releaseRequest освобождает активный запрос. finishStream передаёт dispatch=false
// до отправки финального ответа в Telegram, чтобы следующий placeholder не
// обгонял предыдущий ответ в чате.
func (b *Bot) releaseRequest(dispatch bool) {
	b.mu.Lock()
	b.busy = false
	if b.reqCancel != nil {
		b.reqCancel()
		b.reqCancel = nil
		b.reqCtx = nil
	}
	b.mu.Unlock()
	if dispatch {
		go b.dispatchNextQueued()
	}
}

func (b *Bot) beginStream(chatID int64, messageID int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stream = &Stream{
		chatID:       chatID,
		messageID:    messageID,
		started:      time.Now(),
		lastActivity: time.Now(),
	}
}

func (b *Bot) endStream() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stream = nil
}

// currentStream returns the active stream, or nil.
func (b *Bot) currentStream() *Stream {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stream
}

// ── Run ──────────────────────────────────────────────────────────────────────

// Run starts long polling and the SSE event listener until ctx is cancelled.
func (b *Bot) Run(ctx context.Context) {
	go b.eventLoop(ctx)

	updates, err := b.api.UpdatesViaLongPolling(ctx, nil)
	if err != nil {
		slog.Error("long polling", "error", err)
		return
	}
	slog.Info("bot started", "username", b.api.Username(), "agent", b.agent)
	for update := range updates {
		go b.handleUpdateContext(ctx, update)
	}
}

// eventLoop keeps a single WebSocket subscription alive, reconnecting with
// exponential backoff + jitter (cap 15s) so повторные обрывы не спамят шлюз.
func (b *Bot) eventLoop(ctx context.Context) {
	backoff := time.Second
	for {
		err := b.backend.Events(ctx, b.handleEvent)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("event stream dropped", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		// backoff *= 2 с джиттером, но не более 15s.
		jitter := time.Duration(rand.Int63n(int64(backoff) / 2))
		backoff = min(backoff*2+jitter, 15*time.Second)
	}
}

// handleEvent dispatches WebSocket events of interest.
func (b *Bot) handleEvent(ev backend.Event) {
	switch ev.Type {
	case "message.part.updated":
		b.onMessagePartUpdated(ev)
	case "message.updated":
		b.onMessageUpdated(ev)
	case "permission.asked":
		b.onPermissionAsked(ev)
	case "question.asked":
		b.onQuestionAsked(ev)
	}
}

func (b *Bot) onMessagePartUpdated(ev backend.Event) {
	var props struct {
		Part struct {
			ID        string        `json:"id"`
			Type      string        `json:"type"`
			Text      string        `json:"text"`
			Tool      string        `json:"tool"`
			State     toolPartState `json:"state"`
			MessageID string        `json:"messageID"`
			SessionID string        `json:"sessionID"`
		} `json:"part"`
		Delta string `json:"delta"`
	}
	if err := unmarshalProps(ev, &props); err != nil {
		return
	}
	if props.Part.SessionID != b.currentSessionID() {
		return
	}
	// Части user-эха (копия запроса пользователя) не должны попадать в стрим.
	if props.Part.MessageID != "" && props.Part.MessageID == b.userEchoID() {
		return
	}
	st := b.currentStream()
	if st == nil {
		return
	}
	st.touch()
	st.mu.Lock()
	defer st.mu.Unlock()
	switch props.Part.Type {
	case "step-start":
		if r := strings.TrimSpace(st.reasoning); r != "" {
			st.reasoningLog = append(st.reasoningLog, r)
			if len(st.reasoningLog) > 200 {
				st.reasoningLog = st.reasoningLog[len(st.reasoningLog)-200:]
			}
		}
		st.status = ""
		st.reasoning = ""
	case "text":
		st.status = ""
		st.reasoning = ""
		if props.Delta != "" {
			st.partial += props.Delta
		} else {
			st.partial = props.Part.Text
		}
	case "tool":
		switch props.Part.State.Status {
		case "pending", "running":
			st.status = toolStatus(props.Part.Tool, props.Part.State)
		case "completed":
			st.status = ""
			st.appendLog("✓ " + toolDesc(props.Part.Tool, props.Part.State))
		case "error":
			st.status = ""
			line := "✗ " + toolDesc(props.Part.Tool, props.Part.State)
			if errMsg := props.Part.State.Error; errMsg != "" {
				line += " — " + shortLine(errMsg, 80)
			}
			st.appendLog(line)
		}
	case "reasoning":
		st.status = ""
		if props.Delta != "" {
			st.reasoning += props.Delta
		} else {
			st.reasoning = props.Part.Text
		}
	}
}

func (b *Bot) userEchoID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.userMsgID
}

// onMessageUpdated handles the final message of the current request. The
// backend forwards message.updated both for the user echo, intermediate
// assistant steps (including finish="tool-calls") and the completed response.
// Only the gateway's own publish carries a top-level sessionID and fires once,
// after the final message is saved to history — so we finalize exclusively on
// it.
func (b *Bot) onMessageUpdated(ev backend.Event) {
	var props struct {
		SessionID string          `json:"sessionID"`
		Info      backend.Message `json:"info"`
	}
	if err := unmarshalProps(ev, &props); err != nil {
		return
	}
	sess := props.SessionID
	if sess == "" {
		sess = props.Info.SessionID
	}
	if sess != b.currentSessionID() {
		return
	}
	st := b.currentStream()
	if st == nil {
		return
	}
	// Запоминаем user-эхо, чтобы не собирать его части в стрим.
	if props.Info.Role == "user" && props.Info.ID != "" {
		b.mu.Lock()
		b.userMsgID = props.Info.ID
		b.mu.Unlock()
		return
	}
	// События глобальной шины (промежуточные шаги, tool-calls, финальные
	// дубликаты) не имеют top-level sessionID — не финализируем на них.
	if props.SessionID == "" {
		return
	}
	if msg := props.Info.MessageError(); msg != "" {
		b.finishStream(st, props.Info, "❌ "+msg)
		return
	}
	b.finishStream(st, props.Info, "")
}

// appendLog records a finished tool call, keeping a bounded history.
func (st *Stream) appendLog(line string) {
	st.log = append(st.log, shortLine(line, 100))
	if len(st.log) > 500 {
		st.log = st.log[len(st.log)-500:]
	}
}

// toolDesc renders "tool: input" — the part of the status/log line without
// the status icon. The server provides a human-readable title for running
// tools; fall back to a compact summary of the tool input otherwise.
func toolDesc(tool string, state toolPartState) string {
	desc := state.Title
	if desc == "" {
		for _, key := range []string{"command", "filePath", "query", "pattern"} {
			if v, ok := state.Input[key]; ok {
				desc = fmt.Sprint(v)
				break
			}
		}
		if desc == "" && len(state.Input) > 0 {
			desc = fmt.Sprintf("%v", state.Input)
		}
	}
	if desc == "" {
		return tool
	}
	return tool + ": " + desc
}

// toolStatus renders a short "what the agent is doing" line for a tool call.
func toolStatus(tool string, state toolPartState) string {
	return "⚙️ " + shortLine(toolDesc(tool, state), 60)
}

// shortLine truncates a line to at most n runes, adding "..." when cut.
func shortLine(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 3 {
		return string(r[:n])
	}
	return string(r[:n-3]) + "..."
}

func (b *Bot) currentSessionID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessionID
}

// ── Request flow ─────────────────────────────────────────────────────────────

// startRequest sends the request to the backend asynchronously (202 +
// messageID) and streams partial text into a "💭" placeholder. The response
// is finalized by onMessageUpdated when the backend emits message.updated,
// or by the watchdog on timeout. busy is released exactly once on finish.
func (b *Bot) startRequest(ctx context.Context, chatID int64, req backend.MessageRequest) {
	_ = b.api.SendChatAction(ctx, &telego.SendChatActionParams{
		ChatID: telego.ChatID{ID: chatID},
		Action: "typing",
	})

	placeholder, err := b.api.SendMessage(ctx, tu.Message(tu.ID(chatID), "💭 Работаю…"))
	if err != nil {
		b.release()
		return
	}

	b.beginStream(chatID, placeholder.MessageID)
	st := b.currentStream()
	st.done = make(chan struct{})
	st.stopped = make(chan struct{})
	// previewLoop стартуем сразу: он единственный, кто закрывает st.stopped,
	// на который ждёт finishStream в путях ошибок (CreateSession/SendMessage).
	go b.previewLoop(st, chatID)

	sessionID, err := b.sessionIDFor(ctx)
	if err != nil {
		b.finishStream(st, backend.Message{}, "❌ "+err.Error())
		return
	}

	if _, err := b.backend.SendMessage(ctx, sessionID, req); err != nil {
		b.finishStream(st, backend.Message{}, "❌ "+err.Error())
		return
	}

	go b.timeoutLoop(st)
}

// finishStream stops the live preview and finalizes the placeholder. errText,
// when non-empty, replaces the placeholder with an error; otherwise the
// accumulated partial text is rendered together with the tool log, reasoning
// and the token/cost footer. Idempotent — runs exactly once per request.
func (b *Bot) finishStream(st *Stream, info backend.Message, errText string) {
	st.finalizeOnce.Do(func() {
		toolLog := b.toolLog()
		reasoning := b.reasoningLog()
		var text string
		if errText == "" {
			text = b.finalText(st, info)
			if text == "" {
				text = b.partialText()
			}
			if text == "" {
				text = "✅ Готово."
			}
		}
		b.finishCommon(st)
		if errText != "" {
			b.editMessage(context.Background(), st.chatID, st.messageID, truncate(errText))
			b.dispatchNextQueued()
			return
		}
		b.sendFinalResponse(context.Background(), st.chatID, st.messageID, text, info, toolLog, reasoning)
		b.dispatchNextQueued()
	})
}

// finishStalled финализирует превью после «догонялки» за потерянным финалом:
// текст и метаданные ответа уже получены из истории шлюза, а не из стрима.
// Idempotent, как и finishStream — гонку с поздним message.updated выигрывает
// первый финализатор.
func (b *Bot) finishStalled(st *Stream, text string, info backend.Message, errText string) {
	st.finalizeOnce.Do(func() {
		toolLog := b.toolLog()
		reasoning := b.reasoningLog()
		if errText == "" {
			if strings.TrimSpace(text) == "" {
				text = b.partialText()
			}
			if strings.TrimSpace(text) == "" {
				text = "✅ Готово."
			}
		}
		b.finishCommon(st)
		if errText != "" {
			b.editMessage(context.Background(), st.chatID, st.messageID, truncate(errText))
			b.dispatchNextQueued()
			return
		}
		b.sendFinalResponse(context.Background(), st.chatID, st.messageID, text, info, toolLog, reasoning)
		b.dispatchNextQueued()
	})
}

// finishCommon выполняет общую часть финализации: останавливает превью и
// освобождает состояние запроса ровно один раз.
func (b *Bot) finishCommon(st *Stream) {
	if st.done != nil {
		close(st.done)
	}
	if st.stopped != nil {
		<-st.stopped
	}
	b.endStream()
	b.releaseRequest(false)
	// Запрос завершён (в т.ч. по таймауту/ошибке): любые висящие вопрос
	// и запросы разрешений устарели, чтобы не съедать следующее
	// сообщение пользователя как «ответ» на мёртвый вопрос.
	b.mu.Lock()
	b.pendingQ = nil
	b.perms = make(map[string]*permAsk)
	b.mu.Unlock()
}

// dispatchNextQueued запускает один текстовый запрос из очереди после полной
// отрисовки предыдущего ответа. Следующий стартует его финализацией, поэтому
// два запроса к OpenCode не пересекаются.
func (b *Bot) dispatchNextQueued() {
	ctx, item, left, ok := b.takeQueuedTextRequest()
	if !ok {
		return
	}
	b.send(item.chatID, fmt.Sprintf("📤 Выполняю сообщение из очереди (осталось: %d).", left))
	req := b.newMessageRequest()
	req.AddText(item.text)
	b.startRequest(ctx, item.chatID, req)
}

// previewLoop keeps the placeholder up to date with the stream state.
func (b *Bot) previewLoop(st *Stream, chatID int64) {
	defer close(st.stopped)
	ticker := time.NewTicker(1200 * time.Millisecond)
	defer ticker.Stop()
	var last string
	for {
		select {
		case <-st.done:
			return
		case <-ticker.C:
			// Heartbeat: держим индикатор «печатает…» даже в тихие паузы
			// (когда шлюз не шлёт событий), чтобы сообщение не выглядело
			// зависшим.
			_ = b.api.SendChatAction(context.Background(), &telego.SendChatActionParams{
				ChatID: telego.ChatID{ID: chatID},
				Action: "typing",
			})
			curr := b.previewText()
			if curr != last && curr != "" {
				// Явная метка «черновик», без двусмысленного курсора ▌.
				// Метка и динамический текст уходят в HTML-режиме, поэтому
				// live-контент экранируем, чтобы `<i>` не печатался как есть.
				b.editMessageHTML(context.Background(), chatID, st.messageID,
					"💭 <i>выполняется…</i>\n\n"+escapeHTML(truncate(curr)))
				last = curr
			}
			// Страховка: шлюз давно не отвечал и финал не пришёл. Сначала
			// «догоняемся» за историей шлюза — обычно финал потерялся на сети
			// (обрыв WS/SSE без реплея) и ответ уже сохранён, — и только если
			// готового ответа нет, завершаем превью предупреждением (иначе
			// сообщение с курсором осталось бы в чате навсегда).
			if b.stalled(st) {
				go b.resolveStall(st)
				return
			}
		}
	}
}

// timeoutLoop aborts the request if the backend does not respond within the
// configured timeout (covers waiting for the user on permissions/questions).
func (b *Bot) timeoutLoop(st *Stream) {
	timer := time.NewTimer(b.cfg.RequestTimeout)
	defer timer.Stop()
	select {
	case <-st.done:
		return
	case <-timer.C:
		b.finishStream(st, backend.Message{}, "❌ Превышено время ожидания ответа")
	}
}

// resolveStall вызывается, когда шлюз молчит дольше stallTimeout и финальный
// message.updated так и не пришёл. Обычно финал теряется на сети (обрыв
// WebSocket/SSE без реплея), и ответ при этом уже сохранён в истории шлюза —
// дофинализируем его настоящим текстом, а не ошибкой. Если после нескольких
// попыток готового ответа в истории нет — запрос завершается штатным
// предупреждением.
func (b *Bot) resolveStall(st *Stream) {
	if sessionID := b.currentSessionID(); sessionID != "" {
		for i := 0; i < 10; i++ {
			msgs, err := b.backend.ListMessages(context.Background(), sessionID)
			if err == nil {
				if m := lastAssistantResult(msgs, st.started); m != nil {
					var info backend.Message
					if len(m.Info) > 0 {
						_ = json.Unmarshal(m.Info, &info)
					}
					if m.Status == "completed" {
						slog.Info("stall resolved: response completed in history", "message_id", m.ID)
						b.finishStalled(st, m.Text(), info, "")
					} else {
						errMsg := info.MessageError()
						if errMsg == "" {
							errMsg = "Ответ не сохранился в истории шлюза"
						}
						b.finishStalled(st, "", backend.Message{}, "❌ "+errMsg)
					}
					return
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
	}
	b.finishStalled(st, "", backend.Message{},
		"⚠️ Ответ не финализирован шлюзом (нет события message.updated) — превью остановлено.")
}

// lastAssistantResult возвращает последнее завершённое (или упавшее) сообщение
// ассистента в истории, созданное после начала запроса. Бот серийный, поэтому
// такое сообщение принадлежит именно текущему запросу. nil — ответа ещё нет.
func lastAssistantResult(msgs []backend.StoredMessage, since time.Time) *backend.StoredMessage {
	var best *backend.StoredMessage
	for i := range msgs {
		m := &msgs[i]
		if m.Role != "assistant" || m.Status == "pending" {
			continue
		}
		if m.CreatedAt.Before(since) {
			continue
		}
		if best == nil || m.CreatedAt.After(best.CreatedAt) {
			best = m
		}
	}
	return best
}

func (b *Bot) partialText() string {
	if st := b.currentStream(); st != nil {
		st.mu.Lock()
		defer st.mu.Unlock()
		return st.partial
	}
	return ""
}

// finalText возвращает окончательный текст ответа. Для завершённого сообщения
// ассистента берём полный текст из истории шлюза (там он сохранён до публикации
// message.updated), а не из потокового partial: финальная текстовая часть может
// прийти после события message.updated. Ретраи — страховка на случай гонки
// между сохранением и событием; при недоступности истории — fallback на partial.
func (b *Bot) finalText(st *Stream, info backend.Message) string {
	sessionID := b.currentSessionID()
	if info.ID != "" && sessionID != "" {
		for i := 0; i < 15; i++ {
			msg, err := b.backend.GetMessage(context.Background(), sessionID, info.ID)
			if err == nil {
				if t := msg.Text(); t != "" {
					return t
				}
				return b.partialText()
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	return b.partialText()
}

// previewText returns what should be shown in the live placeholder right now:
// recent tool activity, then either the current tool / reasoning, or text.
func (b *Bot) previewText() string {
	st := b.currentStream()
	if st == nil {
		return ""
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	var sb strings.Builder
	start := 0
	if n := len(st.log); n > 6 {
		start = n - 6
	}
	for _, l := range st.log[start:] {
		sb.WriteString(l)
		sb.WriteByte('\n')
	}
	switch {
	case st.status != "":
		sb.WriteString(st.status)
	case st.partial != "":
		sb.WriteString(st.partial)
	case st.reasoning != "":
		sb.WriteString("🧠 " + shortLine(st.reasoning, 200))
	}
	return sb.String()
}

// toolLog returns a copy of the finished tool-call lines for the request.
func (b *Bot) toolLog() []string {
	st := b.currentStream()
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return append([]string(nil), st.log...)
}

// reasoningLog returns the reasoning of all steps of the request, including
// the still-pending reasoning of the current step.
func (b *Bot) reasoningLog() []string {
	st := b.currentStream()
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	log := append([]string(nil), st.reasoningLog...)
	if r := strings.TrimSpace(st.reasoning); r != "" {
		log = append(log, r)
	}
	return log
}

// ── Send helpers ─────────────────────────────────────────────────────────────

func (b *Bot) send(chatID int64, text string) {
	if _, err := b.api.SendMessage(context.Background(), tu.Message(tu.ID(chatID), text)); err != nil {
		slog.Error("send", "error", err)
	}
}

func (b *Bot) sendHTML(chatID int64, text string, kb *telego.InlineKeyboardMarkup) {
	params := tu.Message(tu.ID(chatID), text).WithParseMode(telego.ModeHTML)
	if kb != nil {
		params = params.WithReplyMarkup(kb)
	}
	if _, err := b.api.SendMessage(context.Background(), params); err != nil {
		slog.Error("sendHTML", "error", err)
	}
}

func (b *Bot) editMessage(ctx context.Context, chatID int64, msgID int, text string) {
	params := tu.EditMessageText(tu.ID(chatID), msgID, truncate(text))
	if _, err := b.api.EditMessageText(ctx, params); err != nil {
		slog.Debug("editMessage", "error", err)
	}
}

// editMessageWithKeyboard редактирует статус-сообщение, опционально добавляя
// inline-клавиатуру (например, кнопку «Отменить» при расшифровке).
func (b *Bot) editMessageWithKeyboard(ctx context.Context, chatID int64, msgID int, text string, kb *telego.InlineKeyboardMarkup) {
	params := tu.EditMessageText(tu.ID(chatID), msgID, truncate(text))
	if kb != nil {
		params = params.WithReplyMarkup(kb)
	}
	if _, err := b.api.EditMessageText(ctx, params); err != nil {
		slog.Debug("editMessageWithKeyboard", "error", err)
	}
}

// setActiveJob/clearActiveJob регистрируют активную транскрибацию, чтобы
// inline-кнопка «Отменить» могла её прервать.
func (b *Bot) setActiveJob(j *transcriptionJob) {
	b.mu.Lock()
	b.activeJob = j
	b.mu.Unlock()
}

func (b *Bot) clearActiveJob() {
	b.mu.Lock()
	b.activeJob = nil
	b.mu.Unlock()
}

func (b *Bot) editMessageHTML(ctx context.Context, chatID int64, msgID int, html string) {
	params := tu.EditMessageText(tu.ID(chatID), msgID, html).WithParseMode(telego.ModeHTML)
	if _, err := b.api.EditMessageText(ctx, params); err != nil {
		slog.Debug("editMessageHTML", "error", err)
	}
}

// sendFinalResponse replaces the placeholder with the activity summary
// (tool log + reasoning) and sends the answer across as many Telegram
// messages as needed, splitting on line boundaries (runes as a fallback for
// over-long lines). The token/cost footer is appended to the last message
// when it fits, otherwise sent as its own message.
func (b *Bot) sendFinalResponse(ctx context.Context, chatID int64, msgID int, text string, info backend.Message, toolLog, reasoning []string) {
	chunks := buildFinalChunks(text, info, toolLog, reasoning)
	if len(chunks) == 0 {
		b.editMessageHTML(ctx, chatID, msgID, "✅ Готово.")
		return
	}
	b.editMessageHTML(ctx, chatID, msgID, chunks[0])
	for _, chunk := range chunks[1:] {
		b.sendHTML(chatID, chunk, nil)
	}
}

// buildFinalChunks assembles the final Telegram messages: first the full
// activity summary (tool log + reasoning), then the markdown-formatted
// answer, then the footer (appended to the last message when it fits).
// Every chunk stays within maxMessageLen runes.
func buildFinalChunks(text string, info backend.Message, toolLog, reasoning []string) []string {
	var chunks []string
	if act := splitActivityChunks(toolLog, reasoning); len(act) > 0 {
		chunks = append(chunks, act...)
	}

	answer := strings.TrimSpace(text)
	if answer == "" {
		answer = "✅ Готово."
	}
	if ac := splitMarkdownChunks(answer); len(ac) > 0 {
		chunks = append(chunks, ac...)
	}

	if footer := formatFooter(info); footer != "" {
		last := len(chunks) - 1
		if last >= 0 && len([]rune(chunks[last]))+len([]rune(footer)) <= maxMessageLen {
			chunks[last] += footer
		} else {
			chunks = append(chunks, strings.TrimSpace(footer))
		}
	}
	return chunks
}

// splitActivityChunks packs the tool log and reasoning lines into self
// contained <i>…</i> HTML chunks, splitting only at line boundaries. An
// over-long line is split by runes into several <i>…</i> pieces.
func splitActivityChunks(toolLog, reasoning []string) []string {
	lines := make([]string, 0, len(toolLog)+len(reasoning))
	for _, l := range toolLog {
		lines = append(lines, l)
	}
	for _, r := range reasoning {
		lines = append(lines, "🧠 "+r)
	}

	var chunks []string
	var sb strings.Builder
	flush := func() {
		if sb.Len() > 0 {
			chunks = append(chunks, sb.String())
			sb.Reset()
		}
	}
	add := func(html string) {
		if sb.Len() > 0 && len([]rune(sb.String()))+len([]rune(html))+1 > maxMessageLen {
			flush()
		}
		if sb.Len() > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(html)
	}

	for _, line := range lines {
		wrapped := "<i>" + escapeHTML(line) + "</i>"
		if len([]rune(wrapped)) > maxMessageLen {
			for _, piece := range splitRunes(line, maxMessageLen-7) {
				add("<i>" + escapeHTML(piece) + "</i>")
			}
			continue
		}
		add(wrapped)
	}
	flush()
	return chunks
}

// splitMarkdownChunks splits a markdown document into HTML chunks. It first
// breaks the text into block-level pieces (paragraphs, code fences,
// blockquotes, list runs) that each render to well-formed HTML, then packs
// them greedily into maxMessageLen-sized chunks. A block that is too big on
// its own is split: code fences by inner lines, everything else by runes.
func splitMarkdownChunks(text string) []string {
	var chunks []string
	var buf strings.Builder
	bufLen := 0
	flush := func() {
		if buf.Len() > 0 {
			chunks = append(chunks, strings.TrimRight(buf.String(), "\n"))
			buf.Reset()
			bufLen = 0
		}
	}
	add := func(html string) {
		for _, piece := range splitHTMLChunks(html, maxMessageLen) {
			n := len([]rune(piece))
			if bufLen > 0 && bufLen+n > maxMessageLen {
				flush()
			}
			if bufLen > 0 {
				buf.WriteByte('\n')
			}
			buf.WriteString(piece)
			bufLen += n
		}
	}

	for _, block := range markdownBlocks(text) {
		html := markdownToHTML(block)
		if len([]rune(html)) <= maxMessageLen {
			add(html)
			continue
		}
		for _, piece := range splitOversizedBlock(block) {
			add(markdownToHTML(piece))
		}
	}
	flush()
	return chunks
}

// markdownBlocks breaks markdown into block-level pieces that each render to
// balanced HTML: paragraphs, fenced code blocks, blockquote runs, headers,
// horizontal rules and list items.
func markdownBlocks(s string) []string {
	var blocks []string
	lines := strings.Split(s, "\n")
	for i := 0; i < len(lines); {
		trimmed := strings.TrimSpace(lines[i])
		switch {
		case trimmed == "":
			i++
		case strings.HasPrefix(trimmed, "```"):
			j := i + 1
			for j < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[j]), "```") {
				j++
			}
			if j < len(lines) {
				j++ // closing fence
			}
			blocks = append(blocks, strings.Join(lines[i:j], "\n"))
			i = j
		case strings.HasPrefix(trimmed, ">"):
			j := i
			for j < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[j]), ">") {
				j++
			}
			blocks = append(blocks, strings.Join(lines[i:j], "\n"))
			i = j
		case isMarkdownHeader(trimmed) || isMarkdownHRule(trimmed) || isMarkdownListItem(trimmed):
			blocks = append(blocks, trimmed)
			i++
		default:
			j := i
			var block []string
			blen := 0
			for j < len(lines) {
				t := lines[j]
				trim := strings.TrimSpace(t)
				if trim == "" || strings.HasPrefix(trim, "```") || strings.HasPrefix(trim, ">") ||
					isMarkdownHeader(trim) || isMarkdownHRule(trim) || isMarkdownListItem(trim) {
					break
				}
				// Cap paragraph blocks so their rendered HTML stays within the
				// message limit; splitting happens on complete lines.
				if blen > 0 && blen+len([]rune(t))+1 > maxMessageLen-200 {
					break
				}
				block = append(block, t)
				blen += len([]rune(t)) + 1
				j++
			}
			blocks = append(blocks, strings.Join(block, "\n"))
			i = j
		}
	}
	return blocks
}

// splitOversizedBlock splits a single markdown block that exceeds the message
// limit. Fenced code blocks are cut between their inner lines (each piece
// re-fenced), everything else is cut by runes.
func splitOversizedBlock(block string) []string {
	lines := strings.Split(block, "\n")
	if len(lines) >= 2 && strings.HasPrefix(lines[0], "```") {
		lang := strings.TrimSpace(strings.TrimPrefix(lines[0], "```"))
		inner := lines[1:]
		if n := len(inner); n > 0 && strings.HasPrefix(strings.TrimSpace(inner[n-1]), "```") {
			inner = inner[:n-1]
		}
		var pieces []string
		var buf []string
		cur := 0
		target := maxMessageLen - 120 // leave room for the <pre><code> tags
		for _, line := range inner {
			overhead := len([]rune(lang)) + 6
			if cur+len([]rune(line))+1+overhead > target {
				pieces = append(pieces, codeBlock(lang, buf))
				buf = nil
				cur = 0
			}
			buf = append(buf, line)
			cur += len([]rune(line)) + 1
		}
		if len(buf) > 0 {
			pieces = append(pieces, codeBlock(lang, buf))
		}
		return pieces
	}
	var pieces []string
	for _, piece := range splitRunes(block, maxMessageLen) {
		pieces = append(pieces, piece)
	}
	return pieces
}

func codeBlock(lang string, lines []string) string {
	return "```" + lang + "\n" + strings.Join(lines, "\n") + "\n```"
}

// splitRunes splits s into pieces of at most n runes each (never splitting
// a multi-byte rune in half).
func splitRunes(s string, n int) []string {
	if n <= 0 {
		n = 1
	}
	runes := []rune(s)
	var pieces []string
	for i := 0; i < len(runes); i += n {
		end := i + n
		if end > len(runes) {
			end = len(runes)
		}
		pieces = append(pieces, string(runes[i:end]))
	}
	return pieces
}

// splitHTMLChunks splits an already-rendered Telegram HTML fragment into
// pieces of at most n runes each. В отличие от наивного splitRunes, он не
// разрезает тег посередине и сохраняет баланс вложенности тегов между
// кусками: открытые теги закрываются в конце куска и переоткрываются в
// начале следующего. Используется, когда один блок markdown рендерится в
// HTML длиннее лимита сообщения (иначе Telegram отверг бы кусок с
// незакрытым/разорванным тегом).
func splitHTMLChunks(s string, n int) []string {
	if n <= 0 {
		n = 1
	}
	runes := []rune(s)
	if len(runes) <= n {
		return []string{s}
	}

	var out []string
	var buf strings.Builder
	var open []string // стек имён открытых тегов

	closeOpen := func() {
		for i := len(open) - 1; i >= 0; i-- {
			buf.WriteString("</" + open[i] + ">")
		}
	}
	reopenOpen := func() {
		for _, t := range open {
			buf.WriteString("<" + t + ">")
		}
	}
	flush := func() {
		if buf.Len() == 0 {
			return
		}
		closeOpen()
		out = append(out, buf.String())
		buf.Reset()
		reopenOpen()
	}
	tagName := func(tag string) string {
		name := strings.TrimPrefix(tag, "</")
		name = strings.TrimPrefix(name, "<")
		if i := strings.IndexAny(name, " \t>"); i >= 0 {
			name = name[:i]
		}
		return name
	}

	i := 0
	for i < len(runes) {
		if runes[i] == '<' {
			j := i + 1
			for j < len(runes) && runes[j] != '>' {
				j++
			}
			if j >= len(runes) {
				// Незакрытый '<' — трактуем как обычный текст.
				buf.WriteRune(runes[i])
				i++
				continue
			}
			tag := string(runes[i : j+1])
			buf.WriteString(tag)
			name := tagName(tag)
			if name != "" && !strings.HasSuffix(tag, "/>") {
				if strings.HasPrefix(tag, "</") {
					for k := len(open) - 1; k >= 0; k-- {
						if open[k] == name {
							open = open[:k]
							break
						}
					}
				} else if name != "br" && name != "img" {
					open = append(open, name)
				}
			}
			i = j + 1
			continue
		}

		// Текстовый кусок до следующего '<' или лимита.
		start := i
		for i < len(runes) && runes[i] != '<' {
			if len([]rune(buf.String())) >= n && i > start {
				flush()
				break
			}
			buf.WriteRune(runes[i])
			i++
		}
		if len([]rune(buf.String())) >= n {
			flush()
		}
	}
	flush()
	if len(out) == 0 {
		// Весь текст оказался внутри одного гигантского неразбиваемого тега.
		return []string{truncate(s)}
	}
	return out
}

func isMarkdownHeader(s string) bool {
	return reHeader.MatchString(s)
}

func isMarkdownHRule(s string) bool {
	return s == "---" || s == "***" || s == "___"
}

func isMarkdownListItem(s string) bool {
	return strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "* ") || reOrderedList.MatchString(s)
}

func formatFooter(info backend.Message) string {
	if info.Cost == 0 && info.Tokens.Input == 0 && info.Tokens.Output == 0 {
		return ""
	}
	return fmt.Sprintf("\n\n<i>💸 %s · ⤴ %d · ⤵ %d</i>",
		formatCost(info.Cost), info.Tokens.Input, info.Tokens.Output)
}

func formatCost(c float64) string {
	if c == 0 {
		return "$0.00"
	}
	s := strings.TrimRight(fmt.Sprintf("%.4f", c), "0")
	return "$" + strings.TrimRight(s, ".")
}

func unmarshalProps(ev backend.Event, out any) error {
	return json.Unmarshal(ev.Properties, out)
}

func truncate(s string) string {
	r := []rune(s)
	if len(r) <= maxMessageLen {
		return s
	}
	return string(r[:maxMessageLen-3]) + "..."
}
