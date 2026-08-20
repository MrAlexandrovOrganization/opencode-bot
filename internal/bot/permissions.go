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
	if p.ID == "" || p.SessionID != b.currentSessionID() {
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
	b.perms[p.ID] = &permAsk{created: time.Now()}
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
		desc = fmt.Sprintf("%s: <code>%s</code>", escapeHTML(p.Permission), escapeHTML(strings.Join(p.Patterns, ", ")))
	case p.Pattern != "":
		desc = fmt.Sprintf("%s: <code>%s</code>", escapeHTML(p.Permission), escapeHTML(p.Pattern))
	case p.Metadata.Filepath != "":
		desc = fmt.Sprintf("%s: <code>%s</code>", escapeHTML(p.Permission), escapeHTML(p.Metadata.Filepath))
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
	if strings.HasPrefix(query.Data, "qans:") {
		b.handleQuestionAnswer(query)
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
	_, ok := b.perms[permissionID]
	delete(b.perms, permissionID)
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

	err := b.backend.ReplyPermission(context.Background(), b.currentSessionID(), permissionID, response)
	if err != nil {
		slog.Error("reply permission", "error", err)
		label = "❌ Ошибка: " + err.Error()
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
