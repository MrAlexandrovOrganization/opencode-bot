package bot

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"opencode-bot/internal/backend"
)

// maxHistoryMessages — сколько последних сообщений сессии показываем при
// восстановлении истории после переключения. Ограничение не даёт залить чат
// сотнями сообщений в длинных диалогах.
const maxHistoryMessages = 30

// sendSessionHistory отправляет в чат сохранённую историю сессии: реплики
// пользователя — и ответы агента. Шлюз пишет в историю обе стороны диалога,
// поэтому после переключения сразу видно, на чём остановился разговор.
// msgs — уже загруженные сообщения в порядке времени (от старых к новым).
func (b *Bot) sendSessionHistory(chatID int64, title string, msgs []backend.StoredMessage) {
	if len(msgs) == 0 {
		b.sendHTML(chatID, "📜 В этой сессии ещё нет сообщений.", nil)
		return
	}
	shown := msgs
	skipped := 0
	if len(msgs) > maxHistoryMessages {
		skipped = len(msgs) - maxHistoryMessages
		shown = msgs[len(msgs)-maxHistoryMessages:]
	}

	b.sendHTML(chatID, historyHead(title, len(msgs), len(shown), skipped), nil)

	// Длинная история уходит десятками сообщений: Telegram режет поток в один
	// чат быстрее ~30 msg/s, поэтому между пачками делаем паузу, чтобы
	// хвост не отвалился по 429.
	const batch = 15
	sent := 0
	for i := range shown {
		for _, chunk := range historyEntryHTML(&shown[i]) {
			if strings.TrimSpace(chunk) == "" {
				continue
			}
			b.sendHTML(chatID, chunk, nil)
			sent++
			if sent%batch == 0 {
				time.Sleep(700 * time.Millisecond)
			}
		}
	}
}

// historyHead — шапка восстановленной истории: название сессии, сколько в ней
// сообщений и сколько старых не показано (при обрезке длинных диалогов).
func historyHead(title string, total, shown, skipped int) string {
	text := "📜 <b>История сессии</b> " + htmlBold(title) + "\n" +
		htmlItalic(plural(total, "сообщение", "сообщения", "сообщений"))
	if skipped > 0 {
		text += "\n" + htmlItalic(fmt.Sprintf("Показаны последние %d, пропущено старых: %d.", shown, skipped))
	}
	return text
}

// historyEntryHTML рендерит одно сообщение истории в один или несколько
// HTML-кусков длиной не больше maxMessageLen. Заголовок «кто и когда» идёт
// перед телом сообщения.
func historyEntryHTML(m *backend.StoredMessage) []string {
	if m.Role == "user" {
		return historyUserChunks(historyHeader(m), m)
	}
	return historyAssistantChunks(historyHeader(m), m)
}

// historyHeader — строка «кто и когда» для сообщения истории.
func historyHeader(m *backend.StoredMessage) string {
	when := ""
	if !m.CreatedAt.IsZero() {
		when = " · " + htmlItalic(m.CreatedAt.Format("02.01 15:04"))
	}
	if m.Role == "user" {
		return "🧑 <b>Вы</b>" + when
	}
	return "🤖 <b>Агент</b>" + when
}

// historyUserChunks рендерит реплику пользователя: текст в цитате (текст
// режется как есть, экранирование — уже после разрезки), вложения — именами
// файлов. Куски пакуются в сообщения по порядку, пока помещаются в лимит.
func historyUserChunks(header string, m *backend.StoredMessage) []string {
	var bodies []string
	if text := strings.TrimSpace(m.Text()); text != "" {
		for _, piece := range splitRunes(text, maxMessageLen-200) {
			bodies = append(bodies, htmlBlockquote(piece))
		}
	}
	for _, f := range m.Files() {
		bodies = append(bodies, "📎 "+htmlCode(shortLine(f, 80)))
	}

	var chunks []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
	}
	for _, body := range bodies {
		if cur.Len() > 0 && len([]rune(cur.String()))+len([]rune(body))+1 > maxMessageLen {
			flush()
		}
		if cur.Len() > 0 {
			cur.WriteByte('\n')
		}
		cur.WriteString(body)
	}
	flush()
	return prependHistoryHeader(header, chunks)
}

// historyAssistantChunks рендерит ответ агента: markdown как в живом ответе
// (вместе с футером токенов/стоимости), ошибку — с ❌. Сообщение без текста и
// без ошибки (запрос ещё в полёте) не показываем — его продолжит стримить
// foreground-окно.
func historyAssistantChunks(header string, m *backend.StoredMessage) []string {
	var chunks []string
	if errText := storedErrorMessage(m); errText != "" {
		for _, piece := range splitRunes("❌ "+errText, maxMessageLen) {
			chunks = append(chunks, escapeHTML(piece))
		}
	} else {
		text := strings.TrimSpace(m.Text())
		if text == "" {
			return nil
		}
		chunks = splitMarkdownChunks(text)
		if footer := historyFooter(m); footer != "" {
			last := len(chunks) - 1
			if last >= 0 && len([]rune(chunks[last]))+len([]rune(footer)) <= maxMessageLen {
				chunks[last] += footer
			} else {
				chunks = append(chunks, strings.TrimSpace(footer))
			}
		}
	}
	return prependHistoryHeader(header, chunks)
}

// historyFooter — футер токенов и стоимости, как у живого ответа. Для записей
// без метрик (ошибки, slash-команды прошлых версий) возвращает пустую строку.
func historyFooter(m *backend.StoredMessage) string {
	if m == nil || len(m.Info) == 0 {
		return ""
	}
	var info backend.Message
	if err := json.Unmarshal(m.Info, &info); err != nil {
		return ""
	}
	return formatFooter(info)
}

// prependHistoryHeader вставляет заголовок в первый кусок тела, если он
// помещается в лимит сообщения, иначе отправляет заголовок отдельным сообщением.
func prependHistoryHeader(header string, chunks []string) []string {
	if header == "" {
		return chunks
	}
	if len(chunks) == 0 {
		return []string{header}
	}
	combined := header + "\n" + chunks[0]
	if len([]rune(combined)) <= maxMessageLen {
		chunks[0] = combined
		return chunks
	}
	return append([]string{header}, chunks...)
}

// storedErrorMessage достаёт текст ошибки из сохранённого сообщения: как из
// формата Message шлюза, так и из плоского {"error": "..."}.
func storedErrorMessage(m *backend.StoredMessage) string {
	if m == nil || m.Status != "error" {
		return ""
	}
	var info backend.Message
	if len(m.Info) > 0 {
		_ = json.Unmarshal(m.Info, &info)
	}
	if errText := info.MessageError(); errText != "" {
		return errText
	}
	var raw struct {
		Error string `json:"error"`
	}
	if len(m.Info) > 0 {
		_ = json.Unmarshal(m.Info, &raw)
	}
	if raw.Error != "" {
		return raw.Error
	}
	return "неизвестная ошибка"
}

// plural подбирает русскую форму множественного числа:
// 1 сообщение, 2 сообщения, 5 сообщений.
func plural(n int, one, few, many string) string {
	n10, n100 := n%10, n%100
	word := many
	switch {
	case n10 == 1 && n100 != 11:
		word = one
	case n10 >= 2 && n10 <= 4 && (n100 < 12 || n100 > 14):
		word = few
	}
	return fmt.Sprintf("%d %s", n, word)
}
