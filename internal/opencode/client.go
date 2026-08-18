// Package opencode is a thin HTTP client for the opencode server
// (https://opencode.ai/docs/server). It covers just the endpoints the
// Telegram bot needs: sessions, messages, permissions and the SSE event bus.
package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// maxEventBytes caps the size of a single SSE event payload.
const maxEventBytes = 8 << 20

// ModelRef references a model by provider and model ID.
type ModelRef struct {
	ProviderID string `json:"providerID"`
	ModelID    string `json:"modelID"`
}

// PartInput is a single content part sent to the server.
// It is either a text part or a file part (see AddText/AddFile).
type PartInput struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Mime     string `json:"mime,omitempty"`
	Filename string `json:"filename,omitempty"`
	URL      string `json:"url,omitempty"`
}

// MessageRequest is the body of POST /session/{id}/message.
type MessageRequest struct {
	Model *ModelRef   `json:"model,omitempty"`
	Agent string      `json:"agent,omitempty"`
	Parts []PartInput `json:"parts"`
}

// AddText appends a plain text part.
func (m *MessageRequest) AddText(text string) {
	m.Parts = append(m.Parts, PartInput{Type: "text", Text: text})
}

// AddFile appends a local file part. url should be a file:// URL.
func (m *MessageRequest) AddFile(mime, filename, url string) {
	m.Parts = append(m.Parts, PartInput{Type: "file", Mime: mime, Filename: filename, URL: url})
}

// Session mirrors the opencode Session type.
type Session struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Directory string `json:"directory"`
	ProjectID string `json:"projectID"`
	Time      struct {
		Created int64 `json:"created"`
		Updated int64 `json:"updated"`
	} `json:"time"`
}

// MessagePart is a part of an assistant response. Only text parts carry content.
type MessagePart struct {
	ID   string `json:"id,omitempty"`
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// MessageResponse is the body of POST /session/{id}/message.
type MessageResponse struct {
	Info  AssistantInfo `json:"info"`
	Parts []MessagePart `json:"parts"`
}

// Text concatenates all text parts of the response.
func (r *MessageResponse) Text() string {
	var sb strings.Builder
	for _, p := range r.Parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// AssistantInfo carries message metadata, including errors.
type AssistantInfo struct {
	ID     string  `json:"id"`
	Finish string  `json:"finish"`
	Cost   float64 `json:"cost"`
	Time   struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed,omitempty"`
	} `json:"time"`
	Tokens struct {
		Input  int `json:"input"`
		Output int `json:"output"`
	} `json:"tokens"`
	Error *struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

// MessageError returns the user-facing error message if the response failed.
func (r *MessageResponse) MessageError() string {
	if r.Info.Error == nil {
		return ""
	}
	if r.Info.Error.Name == "MessageAbortedError" {
		return "Отменено."
	}
	if msg := r.Info.Error.Data.Message; msg != "" {
		return msg
	}
	return r.Info.Error.Name
}

// PermissionAsked is emitted by the server (event type "permission.asked")
// when a tool needs approval.
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

// QuestionOption is a selectable option of an assistant question.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// Question is a single question with selectable options.
type Question struct {
	Question string           `json:"question"`
	Header   string           `json:"header"`
	Options  []QuestionOption `json:"options"`
}

// QuestionAsked is emitted by the server (event type "question.asked")
// when the agent asks the user a question via the question tool.
type QuestionAsked struct {
	ID        string     `json:"id"`
	SessionID string     `json:"sessionID"`
	Questions []Question `json:"questions"`
	Tool      struct {
		MessageID string `json:"messageID"`
		CallID    string `json:"callID"`
	} `json:"tool"`
}

// Event is a single event from the SSE bus (/event). Properties is left
// raw so the caller can unmarshal per event type.
type Event struct {
	Type       string          `json:"type"`
	Properties json.RawMessage `json:"properties"`
}

// Client is a thin HTTP client for the opencode server.
type Client struct {
	baseURL  string
	username string
	password string
	http     *http.Client
}

// New creates a client for the given server URL.
func New(baseURL, username, password string) *Client {
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 30 * time.Second,
	}
	return &Client{
		baseURL:  strings.TrimRight(baseURL, "/"),
		username: username,
		password: password,
		http:     &http.Client{Transport: transport},
	}
}

func (c *Client) setAuth(req *http.Request) {
	if c.password == "" {
		return
	}
	token := base64.StdEncoding.EncodeToString([]byte(c.username + ":" + c.password))
	req.Header.Set("Authorization", "Basic "+token)
}

// do performs a JSON request and decodes the response into out (if any).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}
		reader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("opencode: %s %s: status %d: %s",
			method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Health checks that the server is reachable.
func (c *Client) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/global/health", nil, nil)
}

// CreateSession creates a new session on the server.
func (c *Client) CreateSession(ctx context.Context, title string) (*Session, error) {
	var s Session
	err := c.do(ctx, http.MethodPost, "/session", struct {
		Title string `json:"title"`
	}{title}, &s)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// GetSession fetches a session by ID.
func (c *Client) GetSession(ctx context.Context, id string) (*Session, error) {
	var s Session
	if err := c.do(ctx, http.MethodGet, "/session/"+id, nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// AbortSession aborts an in-flight request.
func (c *Client) AbortSession(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/session/"+id+"/abort", nil, nil)
}

// SendMessage sends a prompt and blocks until the response is ready.
// The caller must supply a generous timeout via ctx.
func (c *Client) SendMessage(ctx context.Context, id string, req MessageRequest) (*MessageResponse, error) {
	var resp MessageResponse
	if err := c.do(ctx, http.MethodPost, "/session/"+id+"/message", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ReplyPermission answers a pending permission request.
// response is one of "once", "always" or "reject".
func (c *Client) ReplyPermission(ctx context.Context, sessionID, permissionID, response string) error {
	return c.do(ctx, http.MethodPost,
		fmt.Sprintf("/session/%s/permissions/%s", sessionID, permissionID),
		struct {
			Response string `json:"response"`
		}{response}, nil)
}

// ReplyQuestion answers a pending question request from the assistant.
// answers contains one entry per question (in order), each an array of
// selected labels.
func (c *Client) ReplyQuestion(ctx context.Context, requestID string, answers [][]string) error {
	return c.do(ctx, http.MethodPost,
		"/question/"+requestID+"/reply",
		struct {
			Answers [][]string `json:"answers"`
		}{answers}, nil)
}

// Events subscribes to the SSE event bus and calls fn for each event.
// It blocks until ctx is cancelled or the stream drops.
func (c *Client) Events(ctx context.Context, fn func(Event)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/event", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("opencode: events: status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), maxEventBytes)
	var data strings.Builder
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Text()
		switch {
		case line == "":
			if data.Len() > 0 {
				var ev Event
				if err := json.Unmarshal([]byte(data.String()), &ev); err == nil {
					fn(ev)
				}
				data.Reset()
			}
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return err
	}
	return nil
}
