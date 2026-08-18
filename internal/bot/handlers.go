package bot

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"opencode-bot/internal/opencode"
	"opencode-bot/internal/whisper"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// newMessageRequest builds a request with the configured agent and model.
func (b *Bot) newMessageRequest() opencode.MessageRequest {
	b.mu.Lock()
	req := opencode.MessageRequest{Agent: b.agent}
	if b.model != nil {
		req.Model = b.model
	}
	b.mu.Unlock()
	return req
}

// handleText processes a plain text message: sends it to opencode and
// streams the response back into the placeholder.
func (b *Bot) handleText(msg *telego.Message) {
	if !b.tryAcquire() {
		b.send(msg.Chat.ID, "⏳ Подожди, я ещё думаю...")
		return
	}
	req := b.newMessageRequest()
	req.AddText(msg.Text)
	response, err := b.streamResponse(context.Background(), msg.Chat.ID, req)
	b.release()
	if err != nil {
		slog.Error("text", "error", err)
		return
	}
	slog.Info("response", "chat_id", msg.Chat.ID, "chars", len(response))
}

// handlePhoto downloads a photo, attaches it as a file part and sends it to
// opencode. The caption (if any) is used as the user's question.
func (b *Bot) handlePhoto(msg *telego.Message) {
	if !b.tryAcquire() {
		b.send(msg.Chat.ID, "⏳ Подожди, я ещё думаю...")
		return
	}
	defer b.release()

	ctx := context.Background()
	photo := msg.Photo[len(msg.Photo)-1]
	data, err := b.downloadFile(ctx, photo.FileID)
	if err != nil {
		b.send(msg.Chat.ID, "Не удалось скачать фото: "+err.Error())
		return
	}

	path, cleanup, err := writeTempFile(data, "photo.jpg")
	if err != nil {
		b.send(msg.Chat.ID, "Не удалось сохранить фото: "+err.Error())
		return
	}
	defer cleanup()

	req := b.newMessageRequest()
	req.AddFile("image/jpeg", "photo.jpg", "file://"+path)
	if caption := strings.TrimSpace(msg.Caption); caption != "" {
		req.AddText(caption)
	}
	if _, err := b.streamResponse(ctx, msg.Chat.ID, req); err != nil {
		slog.Error("photo", "error", err)
	}
}

// handleVoice transcribes a voice message or video note via the shared
// Whisper gRPC service, then sends the transcription to opencode.
func (b *Bot) handleVoice(msg *telego.Message) {
	if b.whisper == nil {
		b.sendHTML(msg.Chat.ID,
			"🎙 Голосовые сообщения требуют сервиса транскрибации.\n\n"+
				"Убедитесь, что в <code>docker-compose.yml</code> указаны "+
				"<code>WHISPER_GRPC_HOST</code> и <code>WHISPER_GRPC_PORT</code>, "+
				"а контейнер подключён к сети <code>whisper-net</code>.",
			nil,
		)
		return
	}

	if !b.tryAcquire() {
		b.send(msg.Chat.ID, "⏳ Подожди, я ещё думаю...")
		return
	}
	defer b.release()

	ctx := context.Background()

	var fileID, format string
	switch {
	case msg.Voice != nil:
		fileID, format = msg.Voice.FileID, "ogg"
	case msg.VideoNote != nil:
		fileID, format = msg.VideoNote.FileID, "mp4"
	default:
		return
	}

	statusMsg, err := b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), "⏳ Скачиваю аудио..."))
	if err != nil {
		slog.Error("send status", "error", err)
		return
	}

	data, err := b.downloadFile(ctx, fileID)
	if err != nil {
		b.editMessage(ctx, msg.Chat.ID, statusMsg.MessageID, "❌ Не удалось скачать аудио: "+err.Error())
		return
	}
	slog.Info("voice received", "file_id", fileID, "bytes", len(data))

	b.editMessage(ctx, msg.Chat.ID, statusMsg.MessageID, "⏳ Отправляю на расшифровку...")

	text, err := b.transcribeVoice(ctx, data, format, msg.Chat.ID, statusMsg.MessageID)
	if err != nil {
		slog.Error("transcribe", "error", err)
		b.editMessage(ctx, msg.Chat.ID, statusMsg.MessageID, "❌ Ошибка расшифровки: "+err.Error())
		return
	}

	slog.Info("transcribed", "chat_id", msg.Chat.ID, "chars", len(text))

	// Show the transcription, then hand it to the agent.
	b.editMessage(ctx, msg.Chat.ID, statusMsg.MessageID, "📝 "+truncate(text))
	req := b.newMessageRequest()
	req.AddText(text)
	if _, err := b.streamResponse(ctx, msg.Chat.ID, req); err != nil {
		slog.Error("voice", "error", err)
	}
}

// transcribeVoice submits audio to the Whisper gRPC service and polls for
// the result, updating the status message with progress.
func (b *Bot) transcribeVoice(ctx context.Context, data []byte, format string, chatID int64, statusMsgID int) (string, error) {
	jobID, pos, err := b.whisper.Submit(bytes.NewReader(data), format, nil)
	if err != nil {
		if _, ok := err.(*whisper.UnavailableError); ok {
			return "", fmt.Errorf("сервис транскрибации недоступен")
		}
		return "", fmt.Errorf("submit: %w", err)
	}
	slog.Info("whisper job submitted", "job_id", jobID, "queue_position", pos)

	if pos > 1 {
		b.editMessage(ctx, chatID, statusMsgID, fmt.Sprintf("⏳ В очереди (позиция %d), подожди немного...", pos))
	} else {
		b.editMessage(ctx, chatID, statusMsgID, "⏳ Расшифровываю...")
	}

	const pollInterval = 5 * time.Second
	const pollDeadline = 3 * time.Hour

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	deadline := time.After(pollDeadline)
	lastStatus := ""

	for {
		select {
		case <-ctx.Done():
			_, _ = b.whisper.Cancel(jobID)
			return "", ctx.Err()
		case <-deadline:
			_, _ = b.whisper.Cancel(jobID)
			return "", fmt.Errorf("превышено время ожидания расшифровки")
		case <-ticker.C:
		}

		result, err := b.whisper.GetStatus(jobID)
		if err != nil {
			return "", fmt.Errorf("get status: %w", err)
		}
		if result.IsFailed() {
			if result.Error == "cancelled" {
				return "", fmt.Errorf("расшифровка отменена")
			}
			return "", fmt.Errorf("whisper: %s", result.Error)
		}
		if result.IsDone() {
			if strings.TrimSpace(result.Text) == "" {
				return "", fmt.Errorf("whisper вернул пустой текст")
			}
			return strings.TrimSpace(result.Text), nil
		}

		statusText := "⏳ Расшифровываю..."
		if result.ProgressPercent > 0 {
			statusText = fmt.Sprintf("⏳ Расшифровываю... %.0f%%", result.ProgressPercent)
		}
		if statusText != lastStatus {
			b.editMessage(ctx, chatID, statusMsgID, statusText)
			lastStatus = statusText
		}
	}
}

// handleDocument downloads a document and attaches it as a file part.
func (b *Bot) handleDocument(msg *telego.Message) {
	if !b.tryAcquire() {
		b.send(msg.Chat.ID, "⏳ Подожди, я ещё думаю...")
		return
	}
	defer b.release()

	ctx := context.Background()
	data, err := b.downloadFile(ctx, msg.Document.FileID)
	if err != nil {
		b.send(msg.Chat.ID, "Не удалось скачать документ: "+err.Error())
		return
	}

	filename := msg.Document.FileName
	if filename == "" {
		filename = "document.bin"
	}
	mime := msg.Document.MimeType
	if mime == "" {
		mime = "application/octet-stream"
	}

	path, cleanup, err := writeTempFile(data, filename)
	if err != nil {
		b.send(msg.Chat.ID, "Не удалось сохранить документ: "+err.Error())
		return
	}
	defer cleanup()

	req := b.newMessageRequest()
	req.AddFile(mime, filename, "file://"+path)
	if caption := strings.TrimSpace(msg.Caption); caption != "" {
		req.AddText(caption)
	}
	if _, err := b.streamResponse(ctx, msg.Chat.ID, req); err != nil {
		slog.Error("document", "error", err)
	}
}

// ── File helpers ─────────────────────────────────────────────────────────────

// downloadFile fetches a Telegram file by its file ID.
func (b *Bot) downloadFile(ctx context.Context, fileID string) ([]byte, error) {
	file, err := b.api.GetFile(ctx, &telego.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("get file info: %w", err)
	}
	url := b.api.FileDownloadURL(file.FilePath)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// writeTempFile writes data to a temp file and returns its path plus a
// cleanup function.
func writeTempFile(data []byte, name string) (string, func(), error) {
	f, err := os.CreateTemp("", "opencode-*")
	if err != nil {
		return "", nil, err
	}
	path := f.Name() + filepath.Ext(name)
	if err := os.Rename(f.Name(), path); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", nil, err
	}
	return path, func() { os.Remove(path) }, nil
}
