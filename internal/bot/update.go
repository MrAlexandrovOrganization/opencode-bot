package bot

import (
	"context"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

func (b *Bot) handleUpdate(update telego.Update) {
	b.handleUpdateContext(context.Background(), update)
}

// parseCommand разбирает текст апдейта на команду и её аргументы.
// tu.ParseCommand здесь не используется намеренно: на go1.26.6 его третий
// результат (args []string в исходнике telego) для этого пакета читается
// компилятором как string и приходит пустым — аргументы команды теряются.
// ParseCommandPayload отдаёт payload строкой и работает корректно.
func parseCommand(text string) (cmd, args string) {
	cmd, _, args = tu.ParseCommandPayload(text)
	return cmd, strings.TrimSpace(args)
}

func (b *Bot) handleUpdateContext(parent context.Context, update telego.Update) {
	ctx, span := otel.Tracer("opencode-bot").Start(parent, "telegram.update")
	defer span.End()
	if update.CallbackQuery != nil {
		// Кнопки (вопросы/разрешения) доступны только авторизованному
		// пользователю, как и обычные сообщения.
		if update.CallbackQuery.From.ID != b.cfg.RootID {
			_ = b.api.AnswerCallbackQuery(ctx, &telego.AnswerCallbackQueryParams{
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
		cmd, _ := parseCommand(msg.Text)
		if cmd != "" {
			b.handleCommand(cmd, msg)
		} else if b.answerQuestionText(msg) {
			// plain text consumed as the answer to a pending question
		} else {
			b.handleText(msg)
		}
	}
}
