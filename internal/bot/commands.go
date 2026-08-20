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

// handleCommand dispatches bot commands.
func (b *Bot) handleCommand(cmd string, msg *telego.Message) {
	switch cmd {
	case "start":
		b.cmdStart(msg)
	case "help":
		b.cmdHelp(msg)
	case "reset":
		b.cmdReset(msg)
	case "session":
		b.cmdSession(msg)
	case "abort":
		b.cmdAbort(msg)
	case "model":
		b.cmdModel(msg)
	case "agent":
		b.cmdAgent(msg)
	default:
		b.send(msg.Chat.ID, "Неизвестная команда. /help — список команд.")
	}
}

func (b *Bot) cmdStart(msg *telego.Message) {
	b.sendHTML(msg.Chat.ID,
		"Привет! Я <b>opencode-бот</b> 🤖\n\n"+
			"Я проксирую твои сообщения в <a href=\"https://opencode.ai\">opencode</a> — агента, "+
			"который умеет читать и редактировать файлы, выполнять команды и работать с git.\n\n"+
			"Отправь /help для списка команд.",
		nil,
	)
}

func (b *Bot) cmdHelp(msg *telego.Message) {
	b.sendHTML(msg.Chat.ID, `<b>Команды</b>

/start — приветствие
/help — этот список
/reset — начать новую сессию opencode
/session — информация о текущей сессии
/abort — прервать текущий запрос
/model <i>[provider/model]</i> — показать или задать модель
/agent <i>[name]</i> — показать или задать агента (build, plan, general, explore)

<b>Сообщения</b>

📝 <b>Текст</b> — отправляется агенту, ответ стримится в сообщение
🖼 <b>Фото / документ</b> — прикрепляется как вложение, подпись — вопрос
🎙 <b>Голос / видео-кружок</b> — транскрибируется через сервис
<code>whisper</code> (<code>WHISPER_GRPC_HOST</code>), затем текст
передаётся агенту

<b>Разрешения</b>

Когда агенту нужно выполнить команду или изменить файл, в режиме
<code>ask</code> приходит запрос с кнопками: разрешить один раз, всегда
или отклонить. Поведение задаётся через <code>PERMISSION_MODE</code>
(<code>ask</code> | <code>allow</code> | <code>deny</code>).`, nil)
}

func (b *Bot) cmdReset(msg *telego.Message) {
	ctx := context.Background()
	if old := b.currentSessionID(); old != "" {
		_ = b.backend.DeleteSession(ctx, old)
	}
	id, err := b.newSession(ctx)
	if err != nil {
		slog.Error("reset", "error", err)
		b.send(msg.Chat.ID, "Не удалось создать сессию: "+err.Error())
		return
	}
	b.send(msg.Chat.ID, fmt.Sprintf("Новая сессия создана: <code>%s</code>", escapeHTML(id)))
}

func (b *Bot) cmdSession(msg *telego.Message) {
	ctx := context.Background()
	id := b.currentSessionID()
	if id == "" {
		b.send(msg.Chat.ID, "Сессия ещё не создана — напиши первое сообщение.")
		return
	}
	s, err := b.backend.GetSession(ctx, id)
	if err != nil {
		b.send(msg.Chat.ID, "Не удалось получить сессию: "+err.Error())
		return
	}

	b.mu.Lock()
	model := "<i>(по умолчанию)</i>"
	if b.model != nil {
		model = b.model.ProviderID + "/" + b.model.ModelID
	}
	agent := b.agent
	b.mu.Unlock()

	text := fmt.Sprintf(
		"<b>Сессия</b>\n\n"+
			"🆔 <code>%s</code>\n"+
			"📁 <code>%s</code>\n"+
			"🤖 <b>Агент:</b> %s\n"+
			"⚙️ <b>Модель:</b> %s\n"+
			"🕐 <b>Создана:</b> %s",
		escapeHTML(s.ID),
		escapeHTML(s.Directory),
		escapeHTML(agent),
		model,
		s.CreatedAt.Format(time.RFC1123),
	)
	b.sendHTML(msg.Chat.ID, text, nil)
}

func (b *Bot) cmdAbort(msg *telego.Message) {
	id := b.currentSessionID()
	if id == "" {
		b.send(msg.Chat.ID, "Нет активной сессии.")
		return
	}
	if err := b.backend.AbortSession(context.Background(), id); err != nil {
		b.send(msg.Chat.ID, "Не удалось прервать: "+err.Error())
		return
	}
	b.send(msg.Chat.ID, "⏹ Запрос прерван.")
}

func (b *Bot) cmdModel(msg *telego.Message) {
	_, args, _ := tu.ParseCommand(msg.Text)
	args = strings.TrimSpace(args)

	if args == "" {
		b.mu.Lock()
		model := b.model
		b.mu.Unlock()
		if model == nil {
			b.send(msg.Chat.ID, "Модель: <i>(по умолчанию из конфига сервера)</i>")
			return
		}
		b.send(msg.Chat.ID, "Модель: "+model.ProviderID+"/"+model.ModelID)
		return
	}

	parts := strings.SplitN(args, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		b.send(msg.Chat.ID, "Формат: /model provider/model, например /model anthropic/claude-sonnet-4")
		return
	}

	b.mu.Lock()
	b.model = &backend.ModelRef{ProviderID: parts[0], ModelID: parts[1]}
	b.mu.Unlock()
	b.send(msg.Chat.ID, "✅ Модель: <code>"+escapeHTML(args)+"</code>")
}

func (b *Bot) cmdAgent(msg *telego.Message) {
	_, args, _ := tu.ParseCommand(msg.Text)
	args = strings.TrimSpace(args)

	if args == "" {
		b.mu.Lock()
		agent := b.agent
		b.mu.Unlock()
		b.send(msg.Chat.ID, "Агент: "+agent)
		return
	}

	b.mu.Lock()
	b.agent = args
	b.mu.Unlock()
	b.send(msg.Chat.ID, "✅ Агент: <code>"+escapeHTML(args)+"</code>")
}

// ── Formatting ───────────────────────────────────────────────────────────────

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
