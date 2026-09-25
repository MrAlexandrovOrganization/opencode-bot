// Package backend — HTTP + WebSocket клиент к opencode-backend (шлюзу).
// Бот — единственный фронтенд, всё общение с opencode идёт через шлюз:
// сессии, асинхронная отправка сообщений (202 + messageID), прогресс через
// WebSocket-события, permissions, вопросы и загрузка файлов.
package backend

import (
	"encoding/json"
	"strings"
	"time"
)

// ModelRef — модель в запросе к шлюзу.
type ModelRef struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

// PartInput — часть пользовательского сообщения: текст или файл.
type PartInput struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Mime     string `json:"mime,omitempty"`
	Filename string `json:"filename,omitempty"`
	URL      string `json:"url,omitempty"`
}

// MessageRequest — тело POST /api/v1/sessions/{id}/messages.
type MessageRequest struct {
	Model *ModelRef   `json:"model,omitempty"`
	Agent string      `json:"agent,omitempty"`
	Parts []PartInput `json:"parts"`
}

// Command — slash-команда, доступная в конфигурации OpenCode.
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Agent       string `json:"agent,omitempty"`
	Model       string `json:"model,omitempty"`
}

// AddText добавляет текстовую часть.
func (m *MessageRequest) AddText(text string) {
	m.Parts = append(m.Parts, PartInput{Type: "text", Text: text})
}

// AddFile добавляет файловую часть. url — file:// URL, видимый opencode-серверу.
func (m *MessageRequest) AddFile(mime, filename, url string) {
	m.Parts = append(m.Parts, PartInput{Type: "file", Mime: mime, Filename: filename, URL: url})
}

// Session — сессия в терминах шлюза (store.Session).
type Session struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Directory string    `json:"directory"`
	CreatedAt time.Time `json:"createdAt"`
}

// SessionActivity — сводный статус сессии, которым backend делится со всеми
// фронтендами. State отражает состояние шлюза, а OCStatus — сервер OpenCode.
type SessionActivity struct {
	SessionID   string    `json:"sessionID"`
	State       string    `json:"state"`
	Busy        bool      `json:"busy"`
	Status      string    `json:"status"`
	Partial     string    `json:"partial"`
	Permissions []string  `json:"permissions"`
	Question    bool      `json:"question"`
	OCStatus    string    `json:"ocStatus"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// PermissionAsked — запрос разрешения от агента.
type PermissionAsked struct {
	ID         string   `json:"id"`
	SessionID  string   `json:"sessionID"`
	Permission string   `json:"permission"`
	Pattern    string   `json:"pattern"`
	Patterns   []string `json:"patterns"`
	Metadata   struct {
		Filepath  string `json:"filepath"`
		ParentDir string `json:"parentDir"`
		Directory string `json:"directory"`
	} `json:"metadata"`
	Tool struct {
		MessageID string `json:"messageID"`
		CallID    string `json:"callID"`
	} `json:"tool"`
}

// QuestionOption — вариант ответа на вопрос агента.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Question — вопрос агента с опциями.
type Question struct {
	Question string           `json:"question"`
	Header   string           `json:"header"`
	Options  []QuestionOption `json:"options"`
	Custom   *bool            `json:"custom"` // false = без «своего ответа»
}

// QuestionAsked — серия вопросов, ждущих ответа пользователя.
type QuestionAsked struct {
	ID        string     `json:"id"`
	SessionID string     `json:"sessionID"`
	Questions []Question `json:"questions"`
	Tool      struct {
		MessageID string `json:"messageID"`
		CallID    string `json:"callID"`
	} `json:"tool"`
}

// Event — нормализованное событие шлюза (WebSocket).
type Event struct {
	Type       string          `json:"type"`
	Session    string          `json:"session"`
	Properties json.RawMessage `json:"payload"`
}

// Message — финальное сообщение ассистента (событие message.updated).
type Message struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
	Role      string `json:"role"`
	Error     *struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
	Time struct {
		Created   int64  `json:"created"`
		Completed *int64 `json:"completed,omitempty"`
	} `json:"time"`
	Cost   float64 `json:"cost"`
	Tokens struct {
		Input  int `json:"input"`
		Output int `json:"output"`
	} `json:"tokens"`
	Finish string `json:"finish"`
}

// MessageError возвращает пользовательское сообщение об ошибке, если ответ не удался.
func (m *Message) MessageError() string {
	if m == nil || m.Error == nil {
		return ""
	}
	if m.Error.Name == "MessageAbortedError" {
		return "Отменено."
	}
	if msg := m.Error.Data.Message; msg != "" {
		return msg
	}
	return m.Error.Name
}

// StoredMessage — сообщение из истории шлюза (GET /api/v1/sessions/{id}/messages/{mid}).
type StoredMessage struct {
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	Status    string          `json:"status"`
	Parts     json.RawMessage `json:"parts"`
	Info      json.RawMessage `json:"info,omitempty"`
	CreatedAt time.Time       `json:"createdAt"`
}

// Text возвращает склеенный текст текстовых частей сообщения.
func (m *StoredMessage) Text() string {
	if m == nil || len(m.Parts) == 0 {
		return ""
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(m.Parts, &parts); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(p.Text)
		}
	}
	return strings.TrimSpace(sb.String())
}
