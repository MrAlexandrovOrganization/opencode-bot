package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"github.com/coder/websocket"
)

// Ошибки, которые бот должен различать.
var (
	ErrBusy            = errors.New("сессия занята другим запросом")
	ErrSessionNotFound = errors.New("сессия не найдена")
)

// Client — клиент REST + WebSocket к opencode-backend.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New создаёт клиент шлюза. token — BACKEND_TOKEN (admin-токен шлюза).
func New(baseURL, token string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{}}
}

// Health проверяет доступность шлюза.
func (c *Client) Health(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/healthz", nil, nil, http.StatusOK)
	return err
}

// CreateSession создаёт сессию opencode через шлюз.
func (c *Client) CreateSession(ctx context.Context, title string) (*Session, error) {
	var out Session
	_, err := c.do(ctx, http.MethodPost, "/api/v1/sessions", map[string]string{"title": title}, &out, http.StatusCreated)
	return &out, err
}

// GetSession возвращает сессию по ID.
func (c *Client) GetSession(ctx context.Context, id string) (*Session, error) {
	var out Session
	_, err := c.do(ctx, http.MethodGet, "/api/v1/sessions/"+id, nil, &out, http.StatusOK)
	return &out, err
}

// GetMessage возвращает сообщение из истории шлюза.
func (c *Client) GetMessage(ctx context.Context, sessionID, messageID string) (*StoredMessage, error) {
	var out StoredMessage
	_, err := c.do(ctx, http.MethodGet, "/api/v1/sessions/"+sessionID+"/messages/"+messageID, nil, &out, http.StatusOK)
	return &out, err
}

// DeleteSession удаляет сессию.
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodDelete, "/api/v1/sessions/"+id, nil, nil, http.StatusNoContent)
	return err
}

// SendMessage отправляет сообщение асинхронно. Возвращает локальный messageID
// шлюза; прогресс и финальный ответ приходят через WebSocket-события.
func (c *Client) SendMessage(ctx context.Context, sessionID string, req MessageRequest) (string, error) {
	var out struct {
		MessageID string `json:"messageID"`
	}
	_, err := c.do(ctx, http.MethodPost, "/api/v1/sessions/"+sessionID+"/messages", req, &out, http.StatusAccepted)
	return out.MessageID, err
}

// AbortSession прерывает выполняющийся запрос сессии.
func (c *Client) AbortSession(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodPost, "/api/v1/sessions/"+id+"/abort", nil, nil, http.StatusAccepted)
	return err
}

// ReplyPermission отвечает на запрос разрешения.
func (c *Client) ReplyPermission(ctx context.Context, sessionID, permissionID, response string) error {
	_, err := c.do(ctx, http.MethodPost,
		"/api/v1/sessions/"+sessionID+"/permissions/"+permissionID,
		map[string]string{"response": response}, nil, http.StatusNoContent)
	return err
}

// ReplyQuestion отправляет накопленные ответы на серию вопросов агента.
func (c *Client) ReplyQuestion(ctx context.Context, requestID string, answers [][]string) error {
	_, err := c.do(ctx, http.MethodPost, "/api/v1/questions/"+requestID,
		map[string][][]string{"answers": answers}, nil, http.StatusNoContent)
	return err
}

// UploadFile загружает файл через шлюз и возвращает file:// URL, видимый
// opencode-серверу (рабочая директория шлюза).
func (c *Client) UploadFile(ctx context.Context, filename, mime string, r io.Reader) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(fw, r); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/files", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var out struct {
		URL string `json:"url"`
	}
	if _, err := c.perform(req, &out, http.StatusCreated); err != nil {
		return "", err
	}
	return out.URL, nil
}

// Events держит единственную WebSocket-подписку на события пользователя и
// вызывает fn для каждого. Возвращает ошибку при обрыве соединения.
func (c *Client) Events(ctx context.Context, fn func(Event)) error {
	wsURL := "ws" + strings.TrimPrefix(c.baseURL, "http") + "/api/v1/ws?session=*"
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.token}},
	})
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	defer conn.CloseNow()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var raw struct {
			Type    string          `json:"type"`
			Session string          `json:"session"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}
		fn(Event{Type: raw.Type, Session: raw.Session, Properties: raw.Payload})
	}
}

// ── Утилиты ─────────────────────────────────────────────────────────────────

// do строит запрос с токеном, выполняет его и мапит коды на ошибки шлюза.
func (c *Client) do(ctx context.Context, method, path string, body, out any, wantStatus int) (int, error) {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.perform(req, out, wantStatus)
}

// perform выполняет запрос с авторизацией и разбирает ответ.
func (c *Client) perform(req *http.Request, out any, wantStatus int) (int, error) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != wantStatus {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		switch resp.StatusCode {
		case http.StatusConflict:
			return resp.StatusCode, ErrBusy
		case http.StatusNotFound:
			return resp.StatusCode, ErrSessionNotFound
		}
		if e.Error != "" {
			return resp.StatusCode, errors.New(e.Error)
		}
		return resp.StatusCode, fmt.Errorf("%s %s: status %d", req.Method, req.URL.Path, resp.StatusCode)
	}
	if out != nil {
		_ = json.Unmarshal(data, out)
	}
	return resp.StatusCode, nil
}
