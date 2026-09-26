package bot

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"

	"opencode-bot/internal/backend"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// maxSessionsInList — сколько сессий показываем в /sessions (кнопок с каждой).
const maxSessionsInList = 10

// sessionTitleLen — максимальная длина названия сессии, выводимого в списке и
// в кнопках переключения.
const sessionTitleLen = 48

// genericSessionTitles — заглушки, которые бот и opencode ставят сессии по
// умолчанию. Они одинаковы у всех сессий и не говорят, о чём диалог, поэтому
// в списках и кнопках заменяются первым сообщением пользователя.
var genericSessionTitles = map[string]bool{
	"":             true,
	"telegram-bot": true,
	"opencode":     true,
	"new session":  true,
}

// isGenericSessionTitle — настоящее ли это название сессии, а не заглушка.
func isGenericSessionTitle(title string) bool {
	return genericSessionTitles[strings.ToLower(strings.TrimSpace(title))]
}

// titleFromText превращает текст в однострочное название сессии.
func titleFromText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	return shortLine(s, sessionTitleLen)
}

// titleFromMessages извлекает название сессии из её истории: первое текстовое
// сообщение пользователя, иначе имя первого вложения.
func titleFromMessages(msgs []backend.StoredMessage) string {
	var firstFile string
	for i := range msgs {
		m := &msgs[i]
		if m.Role != "user" {
			continue
		}
		if t := titleFromText(m.Text()); t != "" {
			return t
		}
		if files := m.Files(); len(files) > 0 && firstFile == "" {
			firstFile = files[0]
		}
	}
	if firstFile != "" {
		return "📎 " + shortLine(firstFile, sessionTitleLen-2)
	}
	return ""
}

// sessionTitleFromRequest извлекает название сессии из первого запроса:
// первый непустой текст, иначе имя вложения (фото без подписи и т.п.).
func sessionTitleFromRequest(req backend.MessageRequest) string {
	for _, p := range req.Parts {
		if p.Type == "text" {
			if t := titleFromText(p.Text); t != "" {
				return t
			}
		}
	}
	for _, p := range req.Parts {
		if p.Type == "file" && strings.TrimSpace(p.Filename) != "" {
			return "📎 " + shortLine(strings.TrimSpace(p.Filename), sessionTitleLen-2)
		}
	}
	return ""
}

// displaySessionTitle — название сессии для списка и кнопок: осмысленное из
// шлюза, иначе первое сообщение пользователя из уже загруженной истории.
func displaySessionTitle(s backend.Session, msgs []backend.StoredMessage) string {
	title := strings.TrimSpace(s.Title)
	if !isGenericSessionTitle(title) {
		return title
	}
	if derived := titleFromMessages(msgs); derived != "" {
		return derived
	}
	if title != "" {
		return title
	}
	return shortLine(s.ID, 24)
}

// displayTitle — то же, но историю загружает сам (когда её ещё нет под рукой).
func (b *Bot) displayTitle(ctx context.Context, s *backend.Session) string {
	if title := strings.TrimSpace(s.Title); !isGenericSessionTitle(title) {
		return title
	}
	if msgs, err := b.backend.ListMessages(ctx, s.ID); err == nil {
		if derived := titleFromMessages(msgs); derived != "" {
			return derived
		}
	}
	if title := strings.TrimSpace(s.Title); title != "" {
		return title
	}
	return shortLine(s.ID, 24)
}

// sessionTitle — понятное название сессии по её ID (для уведомлений о фоновых
// сессиях, /detach и /session).
func (b *Bot) sessionTitle(ctx context.Context, sessionID string) string {
	s, err := b.backend.GetSession(ctx, sessionID)
	if err != nil {
		return shortLine(sessionID, 24)
	}
	return b.displayTitle(ctx, s)
}

// ensureSessionTitle называет свежую сессию (только что созданную или взятую
// пустой) по первому запросу, если у неё всё ещё заглушка. Вызывается один раз
// — перед первой отправкой сообщения, — поэтому списки, кнопки и уведомления
// о фоновых сессиях показывают осмысленное название, а не «telegram-bot».
func (b *Bot) ensureSessionTitle(ctx context.Context, sessionID, fallback string) {
	fallback = strings.TrimSpace(fallback)
	if sessionID == "" || fallback == "" {
		return
	}
	s, err := b.backend.GetSession(ctx, sessionID)
	if err != nil {
		slog.Debug("auto title: get session", "id", sessionID, "error", err)
		return
	}
	if !isGenericSessionTitle(s.Title) {
		return
	}
	if _, err := b.backend.RenameSession(ctx, sessionID, fallback); err != nil {
		slog.Debug("auto title: rename", "id", sessionID, "error", err)
		return
	}
	slog.Info("session auto-titled", "id", sessionID, "title", fallback)
}

// cmdSessions показывает список сессий с кнопками переключения.
func (b *Bot) cmdSessions(msg *telego.Message) {
	ctx := context.Background()
	sessions, err := b.backend.ListSessions(ctx)
	if err != nil {
		b.send(msg.Chat.ID, "Не удалось получить список сессий: "+err.Error())
		return
	}
	if len(sessions) == 0 {
		b.sendHTML(msg.Chat.ID, "Сессий пока нет — напиши первое сообщение, и она создастся автоматически.", nil)
		return
	}
	activities, err := b.backend.ListSessionActivities(ctx)
	if err != nil {
		slog.Warn("list session activities", "error", err)
	}
	activityBySession := make(map[string]backend.SessionActivity, len(activities))
	for _, activity := range activities {
		activityBySession[activity.SessionID] = activity
	}

	// Свежие сверху, показываем не более maxSessionsInList.
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].CreatedAt.After(sessions[j].CreatedAt) })
	if len(sessions) > maxSessionsInList {
		sessions = sessions[:maxSessionsInList]
	}

	// Загружаем историю каждой сессии (конкурентно — их немного): она нужна
	// и для счётчика сообщений, и для названия, если у сессии ещё заглушка.
	histories := make([][]backend.StoredMessage, len(sessions))
	var wg sync.WaitGroup
	for i, s := range sessions {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			if msgs, err := b.backend.ListMessages(ctx, id); err == nil {
				histories[i] = msgs
			}
		}(i, s.ID)
	}
	wg.Wait()

	titles := make([]string, len(sessions))
	for i, s := range sessions {
		titles[i] = displaySessionTitle(s, histories[i])
	}

	current := b.currentSessionID()
	var sb strings.Builder
	sb.WriteString("<b>Сессии</b>\n\n")
	for i, s := range sessions {
		line := fmt.Sprintf("%s %s · 💬 %d · %s",
			sessionStateIcon(activityBySession[s.ID]), htmlBold(titles[i]), len(histories[i]), s.CreatedAt.Format("02.01 15:04"))
		if status := activityBySession[s.ID].Status; status != "" {
			line += " · " + escapeHTML(shortLine(status, 48))
		}
		if s.ID == current {
			sb.WriteString("✅ ")
			sb.WriteString(line)
			sb.WriteString(" <b>(активна)</b>\n")
		} else {
			sb.WriteString(line)
			sb.WriteString("\n")
		}
	}

	var rows [][]telego.InlineKeyboardButton
	for i, s := range sessions {
		if s.ID == current {
			continue
		}
		// Название в кнопке — то же, что и в строке списка; длину ограничиваем,
		// чтобы Telegram не отклонил всю клавиатуру (лимит 64 символа).
		rows = append(rows, []telego.InlineKeyboardButton{{
			Text:         shortLine("Переключиться: "+titles[i], 64),
			CallbackData: "sess:switch:" + s.ID,
		}})
	}
	var kb *telego.InlineKeyboardMarkup
	if len(rows) > 0 {
		kb = &telego.InlineKeyboardMarkup{InlineKeyboard: rows}
	} else {
		sb.WriteString("\nСейчас активна единственная сессия.")
	}

	b.sendHTML(msg.Chat.ID, sb.String(), kb)
}

func sessionStateIcon(activity backend.SessionActivity) string {
	switch activity.State {
	case "running":
		return "🟡"
	case "waiting_permission":
		return "🔐"
	case "waiting_question":
		return "❓"
	default:
		return "▫️"
	}
}

// cmdRename переименовывает текущую сессию (title для списка /sessions).
func (b *Bot) cmdRename(msg *telego.Message) {
	_, _, args := tu.ParseCommandPayload(msg.Text)
	args = strings.TrimSpace(args)
	if args == "" {
		b.sendHTML(msg.Chat.ID, "Формат: <code>/rename название</code> — переименовать текущую сессию.", nil)
		return
	}
	id := b.currentSessionID()
	if id == "" {
		b.send(msg.Chat.ID, "Сессия ещё не создана.")
		return
	}
	if _, err := b.backend.RenameSession(context.Background(), id, args); err != nil {
		b.send(msg.Chat.ID, "Не удалось переименовать: "+err.Error())
		return
	}
	b.sendHTML(msg.Chat.ID, "✅ Сессия переименована: "+htmlCode(args), nil)
}

// handleSessionSwitch обрабатывает нажатие кнопки «Переключиться» в /sessions.
func (b *Bot) handleSessionSwitch(query *telego.CallbackQuery) {
	parts := strings.SplitN(query.Data, ":", 3)
	if len(parts) != 3 || parts[1] != "switch" {
		_ = b.answerCallback(query, "Неизвестное действие")
		return
	}
	sessionID := parts[2]

	b.mu.Lock()
	busy := b.busy
	current := b.sessionID
	b.mu.Unlock()

	if sessionID == current {
		_ = b.answerCallback(query, "Эта сессия уже активна")
		return
	}
	if busy {
		_ = b.answerCallback(query, "⏳ Дождись завершения текущего запроса")
		return
	}

	ctx := context.Background()
	s, err := b.backend.ResumeSession(ctx, sessionID)
	if err != nil {
		_ = b.answerCallback(query, "❌ "+err.Error())
		return
	}

	// Историю берём сразу: по ней же считаем название сессии, если у неё ещё
	// заглушка, и сразу восстанавливаем диалог в чате.
	msgs, histErr := b.backend.ListMessages(ctx, sessionID)
	title := displaySessionTitle(*s, msgs)

	// У каждой сессии свой pending-вопрос: при переключении восстанавливаем
	// именно её состояние, не ломая фоновые сессии. Если целевая сессия
	// работала в фоне — её окно сразу превращается в foreground-стрим, чтобы
	// пользователь снова видел действия агента, а не статичную подпись.
	b.mu.Lock()
	b.sessionID = sessionID
	b.userMsgID = ""
	b.pendingQ = b.pendingQs[sessionID]
	var promoted *Stream
	if bg := b.background[sessionID]; bg != nil {
		promoted = b.attachBackgroundLocked(bg, sessionID)
	}
	b.mu.Unlock()
	slog.Info("session switched", "id", sessionID, "promoted", promoted != nil)

	_ = b.answerCallback(query, "✅ Сессия активна")
	if msg, ok := query.Message.(*telego.Message); ok {
		chatID := msg.Chat.ID
		b.sendHTML(chatID, "Теперь активна сессия: "+htmlBold(title), nil)
		if histErr != nil {
			slog.Warn("session history", "session", sessionID, "error", histErr)
			b.sendHTML(chatID, "⚠️ Не удалось загрузить историю сессии.", nil)
		} else {
			b.sendSessionHistory(chatID, title, msgs)
		}
	}

	if promoted != nil {
		// Превью и watchdog запроса — как у обычного стрима. Первую отрисовку
		// делаем сразу: не ждём тикер, чтобы накопленная активность была видна
		// сразу после переключения.
		go b.previewLoop(promoted, promoted.chatID)
		go b.timeoutLoop(promoted)
		if preview := b.previewText(); preview != "" {
			b.editMessageHTML(context.Background(), promoted.chatID, promoted.messageID,
				"💭 <i>выполняется…</i>\n\n"+escapeHTML(truncate(preview)))
		}
	}
}

func (b *Bot) answerCallback(query *telego.CallbackQuery, text string) error {
	return b.api.AnswerCallbackQuery(context.Background(), &telego.AnswerCallbackQueryParams{
		CallbackQueryID: query.ID,
		Text:            text,
	})
}
