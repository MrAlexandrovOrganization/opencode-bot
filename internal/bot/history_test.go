package bot

import (
	"encoding/json"
	"strings"
	"testing"

	"opencode-bot/internal/backend"
)

func storedMsg(role, status, parts string) backend.StoredMessage {
	return backend.StoredMessage{ID: "m1", Role: role, Status: status, Parts: json.RawMessage(parts)}
}

// TestDisplaySessionTitle — в списке и кнопках сессия показывается по
// осмысленному названию, а заглушка заменяется первым сообщением пользователя.
func TestDisplaySessionTitle(t *testing.T) {
	msgs := []backend.StoredMessage{
		storedMsg("user", "pending", `[{"type":"text","text":"  Почини\n баг в CI "}]`),
		storedMsg("assistant", "completed", `[{"type":"text","text":"Готово"}]`),
	}

	if got := displaySessionTitle(backend.Session{ID: "sid", Title: "telegram-bot"}, msgs); got != "Почини баг в CI" {
		t.Fatalf("заглушка не заменена историей: %q", got)
	}
	if got := displaySessionTitle(backend.Session{ID: "sid", Title: "Бэкенд"}, msgs); got != "Бэкенд" {
		t.Fatalf("своё название важнее истории: %q", got)
	}
	if got := displaySessionTitle(backend.Session{ID: "sid", Title: ""}, nil); got != "sid" {
		t.Fatalf("пустая сессия без сообщений: %q, хотим id", got)
	}
	if got := displaySessionTitle(backend.Session{ID: "sid", Title: "telegram-bot"}, nil); got != "telegram-bot" {
		t.Fatalf("без сообщений остаётся название из шлюза: %q", got)
	}
}

// TestTitleFromMessagesFallsBackToFileName — сессия, начатая с вложения без
// подписи, подписывается именем файла.
func TestTitleFromMessagesFallsBackToFileName(t *testing.T) {
	msgs := []backend.StoredMessage{
		storedMsg("assistant", "completed", `[{"type":"text","text":"что-то"}]`),
		storedMsg("user", "pending", `[{"type":"file","filename":"report.pdf"}]`),
	}
	if got := titleFromMessages(msgs); got != "📎 report.pdf" {
		t.Fatalf("titleFromMessages = %q, хотим имя файла", got)
	}
	if got := titleFromMessages(nil); got != "" {
		t.Fatalf("без сообщений ожидали пустую строку, got %q", got)
	}
}

// TestSessionTitleFromRequest — название свежей сессии берётся из первого
// запроса: текст, иначе вложение.
func TestSessionTitleFromRequest(t *testing.T) {
	var withText backend.MessageRequest
	withText.AddFile("image/png", "photo.png", "file:///tmp/photo.png")
	withText.AddText("  проверь\n деплой ")
	if got := sessionTitleFromRequest(withText); got != "проверь деплой" {
		t.Fatalf("текст запроса: %q", got)
	}

	var fileOnly backend.MessageRequest
	fileOnly.AddFile("application/pdf", "report.pdf", "file:///tmp/report.pdf")
	if got := sessionTitleFromRequest(fileOnly); got != "📎 report.pdf" {
		t.Fatalf("вложение без подписи: %q", got)
	}

	if got := sessionTitleFromRequest(backend.MessageRequest{}); got != "" {
		t.Fatalf("пустой запрос: %q", got)
	}
}

func TestTitleFromTextTruncates(t *testing.T) {
	got := titleFromText(strings.Repeat("а", 100))
	if r := []rune(got); len(r) != sessionTitleLen {
		t.Fatalf("длина названия = %d, хотим %d (%q)", len(r), sessionTitleLen, got)
	}
}

// TestHistoryEntryHTML — реплики обеих сторон диалога: пользователь в цитате,
// ответ агента в markdown; незавершённый ответ не показывается.
func TestHistoryEntryHTML(t *testing.T) {
	user := storedMsg("user", "pending", `[{"type":"text","text":"сделай <фичу>"}]`)
	chunks := historyEntryHTML(&user)
	if len(chunks) != 1 {
		t.Fatalf("user-кусков: %d, хотим 1 (%q)", len(chunks), chunks)
	}
	if !strings.Contains(chunks[0], "🧑 <b>Вы</b>") || !strings.Contains(chunks[0], "<blockquote>") {
		t.Fatalf("нет заголовка или цитаты: %q", chunks[0])
	}
	if !strings.Contains(chunks[0], "сделай &lt;фичу&gt;") {
		t.Fatalf("текст не экранирован: %q", chunks[0])
	}

	assistant := storedMsg("assistant", "completed", `[{"type":"text","text":"Фича **готова**"}]`)
	chunks = historyEntryHTML(&assistant)
	if len(chunks) != 1 || !strings.Contains(chunks[0], "🤖 <b>Агент</b>") || !strings.Contains(chunks[0], "<b>готова</b>") {
		t.Fatalf("ответ агента отрендерен неверно: %q", chunks)
	}

	// Ответ ещё в полёте — показывать нечего.
	pending := storedMsg("assistant", "pending", `[]`)
	if chunks := historyEntryHTML(&pending); len(chunks) != 0 {
		t.Fatalf("незавершённый ответ не должен показываться: %q", chunks)
	}

	failed := backend.StoredMessage{
		ID: "m2", Role: "assistant", Status: "error",
		Info: json.RawMessage(`{"error":"упало"}`),
	}
	chunks = historyEntryHTML(&failed)
	if len(chunks) == 0 || !strings.Contains(chunks[0], "❌ упало") {
		t.Fatalf("ошибка не показана: %q", chunks)
	}
}

// TestHistoryEntryHTMLSplitsLongText — длинный ответ агента режется на куски
// в пределах лимита Telegram.
func TestHistoryEntryHTMLSplitsLongText(t *testing.T) {
	long := ""
	for i := 0; i < 400; i++ {
		long += "строка довольно длинного ответа агента номер " + string(rune('a'+i%26)) + "\n"
	}
	msg := backend.StoredMessage{ID: "m1", Role: "assistant", Status: "completed", Parts: mustParts(long)}
	chunks := historyEntryHTML(&msg)
	if len(chunks) < 2 {
		t.Fatalf("ожидалась нарезка на несколько сообщений, got %d", len(chunks))
	}
	for i, c := range chunks {
		if n := len([]rune(c)); n > maxMessageLen {
			t.Fatalf("кусок %d длиннее лимита: %d", i, n)
		}
	}
}

// TestHistoryUserChunksPacksTextAndFile — текст и вложение одной реплики
// уходят одним сообщением, а не разбрасываются по отдельным.
func TestHistoryUserChunksPacksTextAndFile(t *testing.T) {
	parts, err := json.Marshal([]map[string]string{
		{"type": "text", "text": "прочитай лог"},
		{"type": "file", "filename": "app.log"},
	})
	if err != nil {
		t.Fatal(err)
	}
	msg := backend.StoredMessage{ID: "m1", Role: "user", Status: "pending", Parts: parts}

	chunks := historyEntryHTML(&msg)
	if len(chunks) != 1 {
		t.Fatalf("кусков: %d, хотим 1 (%q)", len(chunks), chunks)
	}
	if !strings.Contains(chunks[0], "<blockquote>прочитай лог</blockquote>") {
		t.Fatalf("нет цитаты: %q", chunks[0])
	}
	if !strings.Contains(chunks[0], "📎 <code>app.log</code>") {
		t.Fatalf("нет вложения: %q", chunks[0])
	}
}

// TestHistoryAssistantFooter — в истории видно те же метрики, что и в живом
// ответе; у записей без метрик футера нет.
func TestHistoryAssistantFooter(t *testing.T) {
	withInfo := backend.StoredMessage{
		ID: "m1", Role: "assistant", Status: "completed",
		Parts: mustParts("Готово"),
		Info:  json.RawMessage(`{"cost":0.0012,"tokens":{"input":500,"output":60}}`),
	}
	chunks := historyEntryHTML(&withInfo)
	if len(chunks) == 0 || !strings.Contains(chunks[0], "💸 $0.0012 · ⤴ 500 · ⤵ 60") {
		t.Fatalf("нет футера: %q", chunks)
	}

	withoutInfo := backend.StoredMessage{ID: "m2", Role: "assistant", Status: "completed", Parts: mustParts("Готово")}
	if chunks := historyEntryHTML(&withoutInfo); len(chunks) != 1 || strings.Contains(chunks[0], "💸") {
		t.Fatalf("лишний футер: %q", chunks)
	}
}

// TestHistoryHead — шапка истории называет сессию и честно сообщает, сколько
// сообщений показано и сколько старых пропущено.
func TestHistoryHead(t *testing.T) {
	got := historyHead("<бот>", 4, 4, 0)
	if !strings.Contains(got, "<b>&lt;бот&gt;</b>") {
		t.Fatalf("название не экранировано и не выделено: %q", got)
	}
	if strings.Contains(got, "пропущено") {
		t.Fatalf("при полной истории не должно быть пометки о пропуске: %q", got)
	}

	got = historyHead("Бэкенд", 40, 30, 10)
	if !strings.Contains(got, "40 сообщений") || !strings.Contains(got, "Показаны последние 30, пропущено старых: 10.") {
		t.Fatalf("нет счётчиков обрезки: %q", got)
	}
}

func mustParts(text string) json.RawMessage {
	data, err := json.Marshal([]map[string]string{{"type": "text", "text": text}})
	if err != nil {
		panic(err)
	}
	return data
}

func TestPlural(t *testing.T) {
	cases := []struct {
		n              int
		one, few, many string
		want           string
	}{
		{1, "сообщение", "сообщения", "сообщений", "1 сообщение"},
		{2, "сообщение", "сообщения", "сообщений", "2 сообщения"},
		{5, "сообщение", "сообщения", "сообщений", "5 сообщений"},
		{11, "сообщение", "сообщения", "сообщений", "11 сообщений"},
		{21, "сообщение", "сообщения", "сообщений", "21 сообщение"},
		{102, "сообщение", "сообщения", "сообщений", "102 сообщения"},
	}
	for _, tc := range cases {
		if got := plural(tc.n, tc.one, tc.few, tc.many); got != tc.want {
			t.Errorf("plural(%d) = %q, хотим %q", tc.n, got, tc.want)
		}
	}
}
