package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMessageResponseText(t *testing.T) {
	resp := &MessageResponse{
		Parts: []MessagePart{
			{Type: "text", Text: "Привет"},
			{Type: "tool", Text: "ignored"},
			{Type: "text", Text: ", мир"},
		},
	}
	if got := resp.Text(); got != "Привет, мир" {
		t.Fatalf("Text() = %q, want %q", got, "Привет, мир")
	}
}

func TestMessageError(t *testing.T) {
	resp := &MessageResponse{
		Info: AssistantInfo{Error: &struct {
			Name string `json:"name"`
			Data struct {
				Message string `json:"message"`
			} `json:"data"`
		}{Name: "ProviderAuthError", Data: struct {
			Message string `json:"message"`
		}{Message: "no api key"}}},
	}
	if got := resp.MessageError(); got != "no api key" {
		t.Fatalf("MessageError() = %q, want %q", got, "no api key")
	}

	aborted := &MessageResponse{Info: AssistantInfo{Error: &struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	}{Name: "MessageAbortedError"}}}
	if got := aborted.MessageError(); got != "Отменено." {
		t.Fatalf("MessageError() = %q, want %q", got, "Отменено.")
	}
}

func TestEventsSSE(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/event" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		flusher.Flush()
		for _, ev := range []string{
			`data: {"type":"server.connected","properties":{}}` + "\n\n",
			`data: {"type":"permission.asked","properties":{"id":"p1","sessionID":"s1","permission":"external_directory","patterns":["/home/user/other/*"]}}` + "\n\n",
			`data: {"type":"message.part.updated","properties":{"part":{"id":"part1","type":"text","messageID":"m1","sessionID":"s1","text":""},"delta":"hel"}}` + "\n\n",
			`data: {"type":"message.part.updated","properties":{"part":{"id":"part1","type":"text","messageID":"m1","sessionID":"s1","text":""},"delta":"lo"}}` + "\n\n",
		} {
			w.Write([]byte(ev))
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "opencode", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	textParts := 0
	var permission *PermissionAsked
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Events(ctx, func(ev Event) {
			count.Add(1)
			switch ev.Type {
			case "permission.asked":
				var p PermissionAsked
				if err := unmarshalTestProps(ev, &p); err == nil {
					permission = &p
				}
			case "message.part.updated":
				var props struct {
					Delta string `json:"delta"`
				}
				_ = unmarshalTestProps(ev, &props)
				if props.Delta != "" {
					textParts++
				}
			}
		})
	}()
	<-done

	if got := count.Load(); got != 4 {
		t.Fatalf("received %d events, want 4", got)
	}
	if permission == nil || permission.ID != "p1" || permission.Permission != "external_directory" ||
		len(permission.Patterns) != 1 || permission.Patterns[0] != "/home/user/other/*" {
		t.Fatalf("permission not parsed: %+v", permission)
	}
	if textParts != 2 {
		t.Fatalf("got %d text deltas, want 2", textParts)
	}
}

func TestEventsAuth(t *testing.T) {
	var sawAuth atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Basic b3BlbmNvZGU6c2VjcmV0" {
			sawAuth.Store(true)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "opencode", "secret")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = c.Health(ctx)

	if !sawAuth.Load() {
		t.Fatal("expected basic auth header on request")
	}
}

func unmarshalTestProps(ev Event, out any) error {
	return json.Unmarshal(ev.Properties, out)
}

func TestEventsTextDeltas(t *testing.T) {
	events := []string{
		`data: {"type":"message.part.updated","properties":{"part":{"id":"p","type":"text","messageID":"m","sessionID":"s","text":"xy"},"delta":"xy"}}` + "\n\n",
	}
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, ev := range events {
			w.Write([]byte(ev))
		}
		flusher.Flush()
	}))
	defer srv.Close()

	c := New(srv.URL, "opencode", "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = c.Events(ctx, func(ev Event) {
		if ev.Type == "message.part.updated" {
			var props struct {
				Delta string `json:"delta"`
			}
			_ = unmarshalTestProps(ev, &props)
			got = props.Delta
		}
	})
	if got != "xy" {
		t.Fatalf("delta = %q, want %q", got, "xy")
	}
}

func TestQuestionAskedParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		flusher.Flush()
		ev := `data: {"type":"question.asked","properties":{"id":"que_abc","sessionID":"s1","questions":[{"question":"What to do?","header":"Task","options":[{"label":"Fix","description":"fix it"},{"label":"Add","description":"add it"}]}],"tool":{"messageID":"m1","callID":"c1"}}}` + "\n\n"
		w.Write([]byte(ev))
		flusher.Flush()
	}))
	defer srv.Close()

	c := New(srv.URL, "opencode", "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var got *QuestionAsked
	_ = c.Events(ctx, func(ev Event) {
		if ev.Type != "question.asked" {
			return
		}
		var q QuestionAsked
		if err := unmarshalTestProps(ev, &q); err == nil {
			got = &q
		}
	})

	if got == nil || got.ID != "que_abc" || got.SessionID != "s1" {
		t.Fatalf("question not parsed: %+v", got)
	}
	if len(got.Questions) != 1 || got.Questions[0].Header != "Task" ||
		len(got.Questions[0].Options) != 2 || got.Questions[0].Options[0].Label != "Fix" {
		t.Fatalf("question content wrong: %+v", got.Questions)
	}
}

func TestReplyQuestion(t *testing.T) {
	var (
		path, body string
		gotMethod  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		path = r.URL.Path
		data, _ := io.ReadAll(r.Body)
		body = string(data)
		w.Write([]byte("true"))
	}))
	defer srv.Close()

	c := New(srv.URL, "opencode", "")
	if err := c.ReplyQuestion(context.Background(), "que_abc", [][]string{{"Fix"}}); err != nil {
		t.Fatalf("ReplyQuestion: %v", err)
	}
	if gotMethod != http.MethodPost || path != "/question/que_abc/reply" {
		t.Fatalf("got %s %s", gotMethod, path)
	}
	if body != `{"answers":[["Fix"]]}` {
		t.Fatalf("body = %s", body)
	}
}

func TestEventsDisconnectReturns(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		flusher.Flush()
	}))
	defer srv.Close()

	c := New(srv.URL, "opencode", "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Stream that ends immediately should return (EOF) rather than hang.
	if err := c.Events(ctx, func(ev Event) {}); err != nil && err != context.Canceled {
		if !strings.Contains(err.Error(), "EOF") && err.Error() != "" {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}
