package bot

import (
	"context"
	"log/slog"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

func (b *Bot) handleUpdate(update telego.Update) {
	if update.CallbackQuery != nil {
		// Кнопки (вопросы/разрешения) доступны только авторизованному
		// пользователю, как и обычные сообщения.
		if update.CallbackQuery.From.ID != b.cfg.RootID {
			_ = b.api.AnswerCallbackQuery(context.Background(), &telego.AnswerCallbackQueryParams{
				CallbackQueryID: update.CallbackQuery.ID,
				Text:            "Доступ запрещён",
			})
			return
		}
		b.handleCallback(update.CallbackQuery)
		return
	}
	if update.Message == nil {
		return
	}

	msg := update.Message
	if msg.From == nil || msg.From.ID != b.cfg.RootID {
		slog.Warn("unauthorized", "user_id", func() int64 {
			if msg.From != nil {
				return msg.From.ID
			}
			return 0
		}())
		return
	}

	b.mu.Lock()
	if b.chatID == 0 {
		b.chatID = msg.Chat.ID
	}
	b.mu.Unlock()

	switch {
	case msg.Photo != nil:
		b.handlePhoto(msg)
	case msg.Document != nil:
		b.handleDocument(msg)
	case msg.Voice != nil || msg.VideoNote != nil:
		b.handleVoice(msg)
	case msg.Animation != nil:
		b.handleAnimation(msg)
	case msg.Audio != nil:
		b.handleAudio(msg)
	case msg.Video != nil:
		b.handleVideo(msg)
	case msg.Sticker != nil:
		b.handleSticker(msg)
	case msg.Text != "":
		cmd, _, _ := tu.ParseCommand(msg.Text)
		if cmd != "" {
			b.handleCommand(cmd, msg)
		} else if b.answerQuestionText(msg) {
			// plain text consumed as the answer to a pending question
		} else {
			b.handleText(msg)
		}
	}
}
