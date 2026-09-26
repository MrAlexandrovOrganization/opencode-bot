package bot

import (
	"encoding/json"
	"testing"

	"opencode-bot/internal/backend"

	"github.com/mymmrac/telego"
)

func newPendingQ() *pendingQuestions {
	return &pendingQuestions{
		sessionID: "sess1",
		requestID: "que_test01",
		questions: []backend.Question{
			{Question: "Что сделать?", Header: "Выбор", Options: []backend.QuestionOption{{Label: "да"}, {Label: "нет"}}},
		},
		answers: make([][]string, 1),
	}
}

func TestBackgroundQuestionIsKeptBySession(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	b := newTestBot(t, ft, fb)
	b.mu.Lock()
	b.chatID = testChatID
	b.sessionID = "foreground"
	b.mu.Unlock()

	b.onQuestionAsked(backend.Event{Properties: json.RawMessage(`{
        "id":"question-background","sessionID":"background",
        "questions":[{"question":"Продолжить?","options":[{"label":"да"}]}]
    }`)})

	ft.waitText(t, "Продолжить?")
	b.mu.Lock()
	p := b.pendingQs["background"]
	foreground := b.pendingQ
	b.mu.Unlock()
	if p == nil || p.requestID != "question-background" {
		t.Fatalf("фоновый вопрос не сохранён: %+v", p)
	}
	if foreground != nil {
		t.Fatalf("фоновый вопрос стал вопросом выбранной сессии: %+v", foreground)
	}
}

func callback(data string) *telego.CallbackQuery {
	return &telego.CallbackQuery{ID: "q1", From: telego.User{ID: testRootID}, Data: data}
}

// TestQuestionAnswerValidOption — нажатие валидного варианта записывает ответ.
func TestQuestionAnswerValidOption(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	b := newTestBot(t, ft, fb)
	b.mu.Lock()
	b.pendingQ = newPendingQ()
	b.pendingQs[b.pendingQ.sessionID] = b.pendingQ
	b.mu.Unlock()

	b.handleQuestionAnswer(callback("qans:que_test01:0:1"))

	waitFor(t, func() bool { return len(fb.questionReplies()) > 0 })
	got := fb.questionReplies()[0]
	if len(got) != 1 || len(got[0]) != 1 || got[0][0] != "нет" {
		t.Fatalf("отправленный ответ = %v, want [[нет]]", got)
	}
	b.mu.Lock()
	p := b.pendingQ
	b.mu.Unlock()
	if p != nil {
		t.Fatalf("pendingQ не обнулён после ответа: %+v", p)
	}
}

// TestQuestionAnswerStaleRequest — кнопка старого запроса не трогает текущий вопрос.
func TestQuestionAnswerStaleRequest(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	b := newTestBot(t, ft, fb)
	b.mu.Lock()
	b.pendingQ = newPendingQ()
	b.pendingQs[b.pendingQ.sessionID] = b.pendingQ
	b.mu.Unlock()

	b.handleQuestionAnswer(callback("qans:que_other00:0:0"))

	b.mu.Lock()
	p := b.pendingQ
	b.mu.Unlock()
	if p == nil || len(p.answers[0]) != 0 {
		t.Fatalf("старая кнопка изменила состояние вопроса: %+v", p)
	}
}

// TestQuestionAnswerCustom — «Свой ответ» не записывает ответ, ждёт текст.
func TestQuestionAnswerCustom(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	b := newTestBot(t, ft, fb)
	b.mu.Lock()
	b.pendingQ = newPendingQ()
	b.pendingQs[b.pendingQ.sessionID] = b.pendingQ
	b.mu.Unlock()

	b.handleQuestionAnswer(callback("qans:que_test01:0:-1"))

	b.mu.Lock()
	p := b.pendingQ
	b.mu.Unlock()
	if p == nil || p.idx != 0 || len(p.answers[0]) != 0 {
		t.Fatalf("«Свой ответ» изменил состояние: %+v", p)
	}
}

// TestQuestionAnswerOutOfRange — вариант вне диапазона отклоняется.
func TestQuestionAnswerOutOfRange(t *testing.T) {
	ft := newFakeTelegram(t)
	fb := newFakeBackend(t)
	b := newTestBot(t, ft, fb)
	b.mu.Lock()
	b.pendingQ = newPendingQ()
	b.pendingQs[b.pendingQ.sessionID] = b.pendingQ
	b.mu.Unlock()

	b.handleQuestionAnswer(callback("qans:que_test01:0:99"))

	b.mu.Lock()
	p := b.pendingQ
	b.mu.Unlock()
	if p == nil || len(p.answers[0]) != 0 {
		t.Fatalf("вариант вне диапазона изменил состояние: %+v", p)
	}
}

// TestQuestionNoCustomButton — при custom=false кнопка «Свой ответ» не рисуется.
func TestQuestionNoCustomButton(t *testing.T) {
	noCustom := false
	p := newPendingQ()
	p.questions[0].Custom = &noCustom
	if customButtonVisible(p.questions[0]) {
		t.Fatal("custom=false, но кнопка «Свой ответ» показывается")
	}
	if !customButtonVisible(newPendingQ().questions[0]) {
		t.Fatal("custom по умолчанию, а кнопки «Свой ответ» нет")
	}
}

func customButtonVisible(q backend.Question) bool {
	return q.Custom == nil || *q.Custom
}
