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
	case "new":
		b.cmdNew(msg)
	case "session":
		b.cmdSession(msg)
	case "sessions":
		b.cmdSessions(msg)
	case "rename":
		b.cmdRename(msg)
	case "abort":
		b.cmdAbort(msg)
	case "detach":
		b.cmdDetach(msg)
	case "queue":
		b.cmdQueue(msg)
	case "model":
		b.cmdModel(msg)
	case "agent":
		b.cmdAgent(msg)
	case "commands":
		b.cmdOpenCodeCommands(msg)
	default:
		b.cmdOpenCodeCommand(cmd, msg)
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
/new — новая сессия, не удаляя прежнюю (как в OpenCode)
/session — информация о текущей сессии
/sessions — список сессий и переключение между ними
/rename <i>[название]</i> — переименовать текущую сессию
/abort — прервать текущий запрос
/detach — оставить текущую сессию работать в фоне
/queue — показать ожидающие сообщения
/model <i>[provider/model]</i> — показать или задать модель
/agent <i>[name]</i> — показать или задать агента (build, plan, general, explore)
/commands — команды, доступные в текущем OpenCode

<b>Сообщения</b>

📝 <b>Текст</b> — отправляется агенту, ответ стримится в сообщение
Если агент занят, до 5 обычных текстовых сообщений ставятся в очередь и
выполняются по порядку. Посмотреть их можно командой /queue; /abort и
успешный /reset очищают очередь.
🖼 <b>Фото / документ</b> — прикрепляется как вложение, подпись — вопрос
🎬 <b>Видео / стикер / GIF / аудио</b> — прикрепляется как файл,
подпись — вопрос
🎙 <b>Голос / видео-кружок</b> — транскрибируется через сервис
<code>whisper</code> (<code>WHISPER_GRPC_HOST</code>), затем текст
передаётся агенту

<b>Разрешения</b>

Когда агенту нужно выполнить команду или изменить файл, в режиме
<code>ask</code> приходит запрос с кнопками: разрешить один раз, всегда
или отклонить. Поведение задаётся через <code>PERMISSION_MODE</code>
(<code>ask</code> | <code>allow</code> | <code>deny</code>).`, nil)
}

// cmdNew создаёт и выбирает новую сессию, сохраняя историю прежних — это
// соответствует действию session.new в OpenCode. Для старого поведения
// удаления предыдущей сессии остаётся /reset.
func (b *Bot) cmdNew(msg *telego.Message) {
	b.mu.Lock()
	busy := b.busy
	b.mu.Unlock()
	if busy {
		b.send(msg.Chat.ID, "⏳ Дождись завершения текущего запроса, затем /new.")
		return
	}
	id, err := b.newSession(context.Background())
	if err != nil {
		b.send(msg.Chat.ID, "Не удалось создать сессию: "+err.Error())
		return
	}
	b.sendHTML(msg.Chat.ID, "Новая сессия создана: <code>"+escapeHTML(id)+"</code>", nil)
}

func (b *Bot) cmdOpenCodeCommands(msg *telego.Message) {
	commands, err := b.backend.ListCommands(context.Background())
	if err != nil {
		b.send(msg.Chat.ID, "Не удалось получить команды OpenCode: "+err.Error())
		return
	}
	if len(commands) == 0 {
		b.send(msg.Chat.ID, "В текущей конфигурации OpenCode нет пользовательских slash-команд.")
		return
	}
	var out strings.Builder
	out.WriteString("<b>Команды OpenCode</b>")
	for _, command := range commands {
		out.WriteString("\n/" + escapeHTML(command.Name))
		if desc := strings.TrimSpace(command.Description); desc != "" {
			out.WriteString(" — " + escapeHTML(shortLine(desc, 180)))
		}
	}
	b.sendHTML(msg.Chat.ID, out.String(), nil)
}

// cmdOpenCodeCommand передаёт незарезервированную Telegram-команду в OpenCode.
// Это поддерживает команды из opencode.json без их дублирования в коде
// Telegram-бота.
func (b *Bot) cmdOpenCodeCommand(command string, msg *telego.Message) {
	_, arguments, _ := tu.ParseCommand(msg.Text)
	ctx, ok := b.beginRequest()
	if !ok {
		b.send(msg.Chat.ID, "⏳ Сессия занята. Дождись ответа или используй /detach.")
		return
	}
	go b.startCommandRequest(ctx, msg.Chat.ID, command, strings.TrimSpace(arguments))
}

func (b *Bot) cmdReset(msg *telego.Message) {
	// Нельзя сбрасывать сессию, пока идёт запрос: удаление активной сессии
	// посреди стрима сломало бы финализацию ответа.
	b.mu.Lock()
	busy := b.busy
	b.mu.Unlock()
	if busy {
		b.send(msg.Chat.ID, "⏳ Дождись завершения текущего запроса, затем /reset.")
		return
	}
	ctx := context.Background()
	old := b.currentSessionID()
	if old != "" {
		// Если текущая сессия пустая (в неё ещё не отправляли сообщений),
		// не плодим новую пустую — оставляем текущую.
		empty, err := b.backend.IsSessionEmpty(ctx, old)
		if err == nil && empty {
			b.clearQueuedText()
			b.sendHTML(msg.Chat.ID,
				fmt.Sprintf("Сессия <code>%s</code> пустая — новая не создана.", escapeHTML(old)), nil)
			return
		}
	}

	// Сначала создаём новую сессию, и только при успехе удаляем старую.
	// Иначе при ошибке создания бот терял бы сессию (указывал на удалённую).
	id, err := b.newSession(ctx)
	if err != nil {
		slog.Error("reset", "error", err)
		b.send(msg.Chat.ID, "Не удалось создать сессию: "+err.Error())
		return
	}
	if old != "" && old != id {
		_ = b.backend.DeleteSession(ctx, old)
	}
	b.clearQueuedText()
	b.sendHTML(msg.Chat.ID, fmt.Sprintf("Новая сессия создана: <code>%s</code>", escapeHTML(id)), nil)
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
			"🕐 <b>Создана:</b> %s\n\n"+
			"Переключиться между сессиями — /sessions, переименовать — /rename",
		escapeHTML(s.ID),
		escapeHTML(s.Directory),
		escapeHTML(agent),
		model,
		s.CreatedAt.Format(time.RFC1123),
	)
	b.sendHTML(msg.Chat.ID, text, nil)
}

func (b *Bot) cmdAbort(msg *telego.Message) {
	cleared := b.clearQueuedText()
	id := b.currentSessionID()
	if id == "" {
		if cleared > 0 {
			b.send(msg.Chat.ID, "Очередь сообщений очищена.")
		}
		b.send(msg.Chat.ID, "Нет активной сессии.")
		return
	}
	if err := b.backend.AbortSession(context.Background(), id); err != nil {
		text := "Не удалось прервать: " + err.Error()
		if cleared > 0 {
			text += fmt.Sprintf(" Очередь очищена: %d.", cleared)
		}
		b.send(msg.Chat.ID, text)
		return
	}
	text := "⏹ Запрос прерван."
	if cleared > 0 {
		text += fmt.Sprintf(" Очередь очищена: %d.", cleared)
	}
	b.send(msg.Chat.ID, text)
}

// cmdDetach прекращает только отображение текущего запроса. Сам запрос уже
// принят backend и продолжает работать; его завершение придёт уведомлением.
func (b *Bot) cmdDetach(msg *telego.Message) {
	b.mu.Lock()
	st := b.stream
	sessionID := b.sessionID
	busy := b.busy
	b.mu.Unlock()
	if !busy || st == nil || sessionID == "" {
		b.send(msg.Chat.ID, "Нет выполняющейся сессии, которую можно оставить в фоне.")
		return
	}

	title := sessionID
	if sess, err := b.backend.GetSession(context.Background(), sessionID); err == nil && strings.TrimSpace(sess.Title) != "" {
		title = sess.Title
	}

	detached := false
	st.finalizeOnce.Do(func() {
		detached = true
		// Оставляем то же сообщение в роли живого индикатора фоновой работы.
		// Карта допускает несколько таких сессий одновременно.
		b.mu.Lock()
		b.background[sessionID] = &backgroundStream{
			chatID: st.chatID, messageID: st.messageID, title: title,
		}
		b.mu.Unlock()
		if st.done != nil {
			close(st.done)
		}
		if st.stopped != nil {
			<-st.stopped
		}
		b.endStream()
		b.releaseRequest(false)
		b.clearQueuedText()
		b.mu.Lock()
		b.sessionID = ""
		b.userMsgID = ""
		b.pendingQ = nil
		b.perms = make(map[string]*permAsk)
		b.mu.Unlock()
		b.editMessageHTML(context.Background(), st.chatID, st.messageID,
			"📎 Сессия <b>"+escapeHTML(title)+"</b> продолжает работу в фоне.")
	})
	if !detached {
		b.send(msg.Chat.ID, "Запрос уже завершился; открой /sessions для выбора сессии.")
		return
	}
	b.sendHTML(msg.Chat.ID, "📎 Открепил сессию <b>"+escapeHTML(title)+"</b>. "+
		"Открой /sessions, чтобы выбрать другую; о завершении фоновой задачи сообщу отдельно.", nil)
}

// cmdQueue показывает снимок ожидающих текстовых запросов в порядке их запуска.
func (b *Bot) cmdQueue(msg *telego.Message) {
	items := b.queuedTextSnapshot()
	if len(items) == 0 {
		b.send(msg.Chat.ID, "Очередь пуста.")
		return
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "<b>Очередь сообщений</b> · %d/%d\n", len(items), maxQueuedTextMessages)
	for i, item := range items {
		fmt.Fprintf(&sb, "\n%d. <blockquote>%s</blockquote>", i+1, escapeHTML(shortLine(strings.Join(strings.Fields(item.text), " "), 240)))
	}
	b.sendHTML(msg.Chat.ID, sb.String(), nil)
}

func (b *Bot) cmdModel(msg *telego.Message) {
	_, args, _ := tu.ParseCommand(msg.Text)
	args = strings.TrimSpace(args)

	if args == "" {
		b.mu.Lock()
		model := b.model
		b.mu.Unlock()
		if model == nil {
			b.sendHTML(msg.Chat.ID, "Модель: <i>(по умолчанию из конфига сервера)</i>", nil)
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
	b.sendHTML(msg.Chat.ID, "✅ Модель: <code>"+escapeHTML(args)+"</code>", nil)
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
	b.sendHTML(msg.Chat.ID, "✅ Агент: <code>"+escapeHTML(args)+"</code>", nil)
}

// ── Formatting ───────────────────────────────────────────────────────────────

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	s = strings.ReplaceAll(s, "'", "&#39;")
	return s
}
