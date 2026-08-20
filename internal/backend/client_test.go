package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

func TestClientAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "secret-token")
	_ = c.DeleteSession(context.Background(), "s1")
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
}

func TestCreateSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"s1","title":"тест","directory":"/workspace","createdAt":"2026-08-19T00:00:00Z"}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "t")
	sess, err := c.CreateSession(context.Background(), "тест")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.ID != "s1" || sess.Directory != "/workspace" {
		t.Fatalf("session = %+v", sess)
	}
	if sess.CreatedAt.IsZero() {
		t.Fatal("createdAt не распарсено")
	}
}

func TestSendMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions/s1/messages" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var req MessageRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Parts) != 1 || req.Parts[0].Text != "hi" {
			t.Errorf("parts = %+v", req.Parts)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, `{"messageID":"m-1"}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "t")
	var req MessageRequest
	req.AddText("hi")
	msgID, err := c.SendMessage(context.Background(), "s1", req)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if msgID != "m-1" {
		t.Fatalf("messageID = %q", msgID)
	}
}

func TestErrorMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/sessions/s-busy/messages":
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"сессия занята"}`)
		case "/api/v1/sessions/s-missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"сессия не найдена"}`)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "t")
	if _, err := c.SendMessage(context.Background(), "s-busy", MessageRequest{Parts: []PartInput{{Type: "text", Text: "x"}}}); err != ErrBusy {
		t.Fatalf("busy: %v, want ErrBusy", err)
	}
	if _, err := c.GetSession(context.Background(), "s-missing"); err != ErrSessionNotFound {
		t.Fatalf("missing: %v, want ErrSessionNotFound", err)
	}
}

func TestReplyPermissionAndQuestion(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(srv.URL, "t")
	if err := c.ReplyPermission(context.Background(), "s1", "p1", "once"); err != nil {
		t.Fatal(err)
	}
	if err := c.ReplyQuestion(context.Background(), "q1", [][]string{{"да"}}); err != nil {
		t.Fatal(err)
	}
	want := []string{"/api/v1/sessions/s1/permissions/p1", "/api/v1/questions/q1"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths = %v", paths)
	}
}

func TestUploadFile(t *testing.T) {
	var gotField, gotFilename string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Errorf("form file: %v", err)
			return
		}
		defer file.Close()
		gotField = header.Filename
		data, _ := io.ReadAll(file)
		gotFilename = string(data)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"url":"file:///workspace/.opencode-backend/uploads/admin/a.jpg"}`)
	}))
	defer srv.Close()

	c := New(srv.URL, "t")
	url, err := c.UploadFile(context.Background(), "a.jpg", "image/jpeg", strings.NewReader("photo"))
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if url != "file:///workspace/.opencode-backend/uploads/admin/a.jpg" {
		t.Fatalf("url = %q", url)
	}
	if gotField != "a.jpg" || gotFilename != "photo" {
		t.Fatalf("uploaded file = %q (%q)", gotField, gotFilename)
	}
}

func TestEvents(t *testing.T) {
	var mu sync.Mutex
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		if r.URL.Path != "/api/v1/ws" || r.URL.Query().Get("session") != "*" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for _, ev := range []Event{
			{Type: "message.part.updated", Session: "s1", Properties: json.RawMessage(`{"delta":"при"}`)},
			{Type: "permission.asked", Session: "s1", Properties: json.RawMessage(`{"id":"p1"}`)},
		} {
			data, _ := json.Marshal(struct {
				Type    string          `json:"type"`
				Session string          `json:"session"`
				Payload json.RawMessage `json:"payload"`
			}{Type: ev.Type, Session: ev.Session, Payload: ev.Properties})
			if err := conn.Write(context.Background(), websocket.MessageText, data); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "ws-token")
	var got []Event
	err := c.Events(context.Background(), func(ev Event) {
		got = append(got, ev)
		if len(got) == 2 {
			srv.Close()
		}
	})
	if err == nil {
		// соединение разорвано тестом — ошибка ожидаема
	}
	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer ws-token" {
		t.Fatalf("ws Authorization = %q", gotAuth)
	}
	if len(got) != 2 {
		t.Fatalf("events = %d, want 2", len(got))
	}
	if got[0].Type != "message.part.updated" || strings.TrimSpace(string(got[0].Properties)) != `{"delta":"при"}` {
		t.Fatalf("event[0] = %+v", got[0])
	}
	if got[1].Type != "permission.asked" {
		t.Fatalf("event[1] = %+v", got[1])
	}
}
