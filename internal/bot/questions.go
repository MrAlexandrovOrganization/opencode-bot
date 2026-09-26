package bot

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"opencode-bot/internal/backend"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// pendingQuestions tracks a question.asked event that the agent is waiting
// for the user to answer. Questions are presented one at a time; answers
// are accumulated in order and submitted to the server once all are known.
type pendingQuestions struct {
	sessionID string
	requestID string
	questions []backend.Question
	answers   [][]string
	idx       int // index of the question currently being asked
	chatID    int64
	msgID     int // telegram message id of the current question prompt
}

// onQuestionAsked handles a question from the agent (question tool).
func (b *Bot) onQuestionAsked(ev backend.Event) {
	var q backend.QuestionAsked
	if err := unmarshalProps(ev, &q); err != nil {
		slog.Warn("parse question", "error", err)
		return
	}
	if q.ID == "" || q.SessionID == "" || len(q.Questions) == 0 {
		return
	}

	b.mu.Lock()
	p := &pendingQuestions{
		sessionID: q.SessionID,
		requestID: q.ID,
		questions: q.Questions,
		answers:   make([][]string, len(q.Questions)),
	}
	b.pendingQs[q.SessionID] = p
	if q.SessionID == b.sessionID {
		b.pendingQ = p
	}
	b.mu.Unlock()

	b.askCurrentQuestion(p)
}

// askCurrentQuestion sends the current pending question to Telegram with its
// options as inline buttons.
func (b *Bot) askCurrentQuestion(p *pendingQuestions) {
	if p == nil || p.idx >= len(p.questions) {
		return
	}
	chatID := b.chatID
	if chatID == 0 {
		slog.Warn("question before chat known", "request_id", p.requestID)
		return
	}

	q := p.questions[p.idx]
	var sb strings.Builder
	sb.WriteString("❓ ")
	if q.Header != "" {
		sb.WriteString(htmlBold(q.Header))
		sb.WriteString("\n\n")
	}
	sb.WriteString(htmlBlockquote(q.Question))

	// Кнопки привязываем к requestID вопроса: нажатие кнопки старого
	// сообщения не должно отвечать на текущий вопрос.
	var rows [][]telego.InlineKeyboardButton
	for oi := range q.Options {
		rows = append(rows, []telego.InlineKeyboardButton{{
			Text:         shortLine(q.Options[oi].Label, 100),
			CallbackData: fmt.Sprintf("qans:%s:%d:%d", p.requestID, p.idx, oi),
		}})
	}
	if q.Custom == nil || *q.Custom {
		rows = append(rows, []telego.InlineKeyboardButton{{
			Text:         "✍️ Свой ответ",
			CallbackData: fmt.Sprintf("qans:%s:%d:-1", p.requestID, p.idx),
		}})
	}
	kb := telego.InlineKeyboardMarkup{InlineKeyboard: rows}

	msg, err := b.api.SendMessage(context.Background(),
		tu.Message(tu.ID(chatID), sb.String()).WithParseMode(telego.ModeHTML).WithReplyMarkup(&kb),
	)
	if err != nil {
		slog.Error("send question", "error", err)
		// Не бросаем пользователя в молчании: вопрос не показался, снимаем
		// его из pending — иначе следующий текст будет съеден как «ответ».
		b.mu.Lock()
		if b.pendingQs[p.sessionID] == p {
			delete(b.pendingQs, p.sessionID)
		}
		if b.pendingQ == p {
			b.pendingQ = nil
		}
		b.mu.Unlock()
		b.send(chatID, "❌ Не удалось показать вопрос: "+err.Error())
		return
	}
	b.mu.Lock()
	if b.pendingQs[p.sessionID] == p {
		p.chatID = chatID
		p.msgID = msg.MessageID
	}
	b.mu.Unlock()
}

// answerCurrent records the answer for question qidx (must be the current
// one) and, once all questions are answered, submits the reply to the server.
func (b *Bot) answerCurrent(p *pendingQuestions, qidx int, label string) {
	b.mu.Lock()
	if p == nil || b.pendingQs[p.sessionID] != p || p.idx != qidx || qidx >= len(p.questions) {
		b.mu.Unlock()
		return
	}
	p.answers[p.idx] = []string{label}
	msgID, chatID := p.msgID, p.chatID
	p.idx++
	done := p.idx >= len(p.questions)
	b.mu.Unlock()

	if msgID != 0 && chatID != 0 {
		b.editMessage(context.Background(), chatID, msgID, "✅ "+label)
	}
	if done {
		b.submitQuestionReply(p)
	} else {
		b.askCurrentQuestion(p)
	}
}

// submitQuestionReply posts the accumulated answers and clears the pending
// state, letting the agent continue.
func (b *Bot) submitQuestionReply(p *pendingQuestions) {
	err := b.backend.ReplyQuestion(context.Background(), p.requestID, p.answers)
	b.mu.Lock()
	if b.pendingQs[p.sessionID] == p {
		delete(b.pendingQs, p.sessionID)
	}
	if b.pendingQ == p {
		b.pendingQ = nil
	}
	b.mu.Unlock()
	if err != nil {
		slog.Error("reply question", "error", err)
	}
}

// handleQuestionAnswer processes an inline button click on a question prompt.
func (b *Bot) handleQuestionAnswer(query *telego.CallbackQuery) {
	parts := strings.Split(query.Data, ":")
	if len(parts) != 4 || parts[0] != "qans" {
		_ = b.api.AnswerCallbackQuery(context.Background(), &telego.AnswerCallbackQueryParams{
			CallbackQueryID: query.ID,
			Text:            "Сообщение устарело",
		})
		return
	}
	requestID := parts[1]
	qidx, errQ := strconv.Atoi(parts[2])
	oidx, errO := strconv.Atoi(parts[3])
	if errQ != nil || errO != nil {
		return
	}

	b.mu.Lock()
	var p *pendingQuestions
	for _, candidate := range b.pendingQs {
		if candidate.requestID == requestID {
			p = candidate
			break
		}
	}
	b.mu.Unlock()

	// Кнопка относится к старому вопросу (другой requestID) или уже
	// обработанному шагу серии — отклоняем, не трогая текущее состояние.
	if p == nil || p.requestID != requestID || p.idx != qidx {
		_ = b.api.AnswerCallbackQuery(context.Background(), &telego.AnswerCallbackQueryParams{
			CallbackQueryID: query.ID,
			Text:            "Вопрос уже обработан",
		})
		return
	}
	if oidx == -1 {
		_ = b.api.AnswerCallbackQuery(context.Background(), &telego.AnswerCallbackQueryParams{
			CallbackQueryID: query.ID,
			Text:            "Просто напиши ответ текстом",
		})
		return
	}
	if oidx < 0 || oidx >= len(p.questions[qidx].Options) {
		_ = b.api.AnswerCallbackQuery(context.Background(), &telego.AnswerCallbackQueryParams{
			CallbackQueryID: query.ID,
			Text:            "Вариант недоступен",
		})
		return
	}
	label := p.questions[qidx].Options[oidx].Label
	_ = b.api.AnswerCallbackQuery(context.Background(), &telego.AnswerCallbackQueryParams{
		CallbackQueryID: query.ID,
		Text:            "✅ " + label,
	})
	b.answerCurrent(p, qidx, label)
}

// answerQuestionText consumes a plain-text message as the answer to the
// pending question. Returns true if the text was used as an answer.
func (b *Bot) answerQuestionText(msg *telego.Message) bool {
	b.mu.Lock()
	p := b.pendingQ
	var qidx int
	if p != nil && p.idx < len(p.questions) {
		qidx = p.idx
	}
	b.mu.Unlock()
	if p == nil {
		return false
	}
	b.answerCurrent(p, qidx, msg.Text)
	return true
}
