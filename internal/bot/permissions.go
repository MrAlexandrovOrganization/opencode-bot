package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"opencode-bot/internal/backend"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// onPermissionAsked handles a permission request from the server.
func (b *Bot) onPermissionAsked(ev backend.Event) {
	var p backend.PermissionAsked
	if err := unmarshalProps(ev, &p); err != nil {
		slog.Warn("parse permission", "error", err)
		return
	}
	if p.ID == "" || p.SessionID == "" {
		return
	}

	switch b.cfg.PermissionMode {
	case "allow":
		_ = b.backend.ReplyPermission(context.Background(), p.SessionID, p.ID, "always")
	case "deny":
		_ = b.backend.ReplyPermission(context.Background(), p.SessionID, p.ID, "reject")
	default:
		b.askPermission(p)
	}
}

// askPermission forwards a permission request to Telegram with inline buttons.
func (b *Bot) askPermission(p backend.PermissionAsked) {
	b.mu.Lock()
	if _, ok := b.perms[p.ID]; ok {
		b.mu.Unlock()
		return
	}
	b.perms[p.ID] = &permAsk{created: time.Now(), sessionID: p.SessionID}
	chatID := b.chatID
	b.mu.Unlock()

	if chatID == 0 {
		slog.Warn("permission before chat known", "permission_id", p.ID)
		return
	}

	kb := telego.InlineKeyboardMarkup{InlineKeyboard: [][]telego.InlineKeyboardButton{
		{
			{Text: "✅ Один раз", CallbackData: "perm:once:" + p.ID},
			{Text: "🟢 Всегда", CallbackData: "perm:always:" + p.ID},
			{Text: "❌ Отклонить", CallbackData: "perm:reject:" + p.ID},
		},
	}}

	desc := escapeHTML(p.Permission)
	switch {
	case len(p.Patterns) > 0:
		desc += ": " + htmlCode(strings.Join(p.Patterns, ", "))
	case p.Pattern != "":
		desc += ": " + htmlCode(p.Pattern)
	case p.Metadata.Filepath != "":
		desc += ": " + htmlCode(p.Metadata.Filepath)
	}
	text := fmt.Sprintf("🔐 <b>Запрос разрешения</b>\n\n<blockquote>%s</blockquote>", desc)
	if _, err := b.api.SendMessage(context.Background(),
		tu.Message(tu.ID(chatID), text).WithParseMode(telego.ModeHTML).WithReplyMarkup(&kb),
	); err != nil {
		slog.Error("send permission ask", "error", err)
	}
}

// handleCallback processes inline keyboard callbacks (permission replies).
func (b *Bot) handleCallback(query *telego.CallbackQuery) {
	if strings.HasPrefix(query.Data, "cancel:") {
		b.handleCancelCallback(query)
		return
	}
	if strings.HasPrefix(query.Data, "qans:") {
		b.handleQuestionAnswer(query)
		return
	}
	if strings.HasPrefix(query.Data, "sess:") {
		b.handleSessionSwitch(query)
		return
	}
	parts := strings.SplitN(query.Data, ":", 3)
	if len(parts) != 3 || parts[0] != "perm" {
		return
	}
	response := parts[1]
	permissionID := parts[2]

	switch response {
	case "once", "always", "reject":
	default:
		return
	}

	b.mu.Lock()
	permission, ok := b.perms[permissionID]
	b.mu.Unlock()
	if !ok {
		_ = b.api.AnswerCallbackQuery(context.Background(), &telego.AnswerCallbackQueryParams{
			CallbackQueryID: query.ID,
			Text:            "Запрос уже обработан",
		})
		return
	}

	label := map[string]string{
		"once":   "✅ Разрешено один раз",
		"always": "🟢 Разрешено (всегда)",
		"reject": "❌ Отклонено",
	}[response]

	err := b.backend.ReplyPermission(context.Background(), permission.sessionID, permissionID, response)
	if err != nil {
		slog.Error("reply permission", "error", err)
		label = "❌ Ошибка: " + err.Error()
	} else {
		// Снимаем запрос только после успешного ответа, иначе пользователь
		// сможет повторить нажатие после восстановления сервера.
		b.mu.Lock()
		delete(b.perms, permissionID)
		b.mu.Unlock()
	}

	_ = b.api.AnswerCallbackQuery(context.Background(), &telego.AnswerCallbackQueryParams{
		CallbackQueryID: query.ID,
		Text:            label,
	})
	if msg, ok := query.Message.(*telego.Message); ok {
		_, _ = b.api.EditMessageText(context.Background(),
			tu.EditMessageText(tu.ID(msg.Chat.ID), msg.MessageID, label),
		)
	}
}

// handleCancelCallback отменяет активную транскрибацию Whisper по нажатию
// inline-кнопки «Отменить», как в transcriber-bot. Отменяет контекст запроса
// (цикл опроса в transcribeVoice поймает ctx.Done() и сообщит об отмене) и
// явно просит бэкенд снять задание.
func (b *Bot) handleCancelCallback(query *telego.CallbackQuery) {
	b.mu.Lock()
	job := b.activeJob
	b.mu.Unlock()
	if job == nil {
		_ = b.api.AnswerCallbackQuery(context.Background(), &telego.AnswerCallbackQueryParams{
			CallbackQueryID: query.ID,
			Text:            "Расшифровка уже завершена",
		})
		return
	}

	b.mu.Lock()
	if b.reqCancel != nil {
		b.reqCancel()
	}
	b.mu.Unlock()

	if b.whisper != nil {
		if _, cerr := b.whisper.Cancel(job.jobID); cerr != nil {
			slog.Warn("cancel job on backend", "job_id", job.jobID, "error", cerr)
		} else {
			slog.Info("cancel sent to backend", "job_id", job.jobID)
		}
	}

	_ = b.api.AnswerCallbackQuery(context.Background(), &telego.AnswerCallbackQueryParams{
		CallbackQueryID: query.ID,
		Text:            "❌ Отменяю расшифровку…",
	})
}
