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

	// Считаем сообщения в каждой сессии (конкурентно — их немного).
	counts := make([]int, len(sessions))
	var wg sync.WaitGroup
	for i, s := range sessions {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			if msgs, err := b.backend.ListMessages(ctx, id); err == nil {
				counts[i] = len(msgs)
			}
		}(i, s.ID)
	}
	wg.Wait()

	current := b.currentSessionID()
	var sb strings.Builder
	sb.WriteString("<b>Сессии</b>\n\n")
	for i, s := range sessions {
		title := strings.TrimSpace(s.Title)
		if title == "" {
			title = shortLine(s.ID, 24)
		}
		line := fmt.Sprintf("%s <code>%s</code> · 💬 %d · %s",
			sessionStateIcon(activityBySession[s.ID]), escapeHTML(title), counts[i], s.CreatedAt.Format("02.01 15:04"))
		if status := activityBySession[s.ID].Status; status != "" {
			line += " · " + escapeHTML(shortLine(status, 48))
		}
		if s.ID == current {
			sb.WriteString("✅ " + line + " <b>(активна)</b>\n")
		} else {
			sb.WriteString(line + "\n")
		}
	}

	var rows [][]telego.InlineKeyboardButton
	for _, s := range sessions {
		if s.ID == current {
			continue
		}
		title := strings.TrimSpace(s.Title)
		if title == "" {
			title = shortLine(s.ID, 24)
		}
		rows = append(rows, []telego.InlineKeyboardButton{{
			Text:         "Переключиться: " + title,
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
	b.sendHTML(msg.Chat.ID, "✅ Сессия переименована: <code>"+escapeHTML(args)+"</code>", nil)
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

	s, err := b.backend.ResumeSession(context.Background(), sessionID)
	if err != nil {
		_ = b.answerCallback(query, "❌ "+err.Error())
		return
	}

	// Переключаемся только при busy=false, поэтому транзиентного состояния
	// (стрим, вопрос, разрешение) быть не должно — сбрасываем на всякий случай.
	b.mu.Lock()
	b.sessionID = sessionID
	b.userMsgID = ""
	b.perms = map[string]*permAsk{}
	b.pendingQ = nil
	b.mu.Unlock()
	slog.Info("session switched", "id", sessionID)

	title := strings.TrimSpace(s.Title)
	if title == "" {
		title = shortLine(sessionID, 24)
	}
	_ = b.answerCallback(query, "✅ Сессия активна")
	if msg, ok := query.Message.(*telego.Message); ok {
		b.sendHTML(msg.Chat.ID, "Теперь активна сессия: <b>"+escapeHTML(title)+"</b>", nil)
	}
}

func (b *Bot) answerCallback(query *telego.CallbackQuery, text string) error {
	return b.api.AnswerCallbackQuery(context.Background(), &telego.AnswerCallbackQueryParams{
		CallbackQueryID: query.ID,
		Text:            text,
	})
}
