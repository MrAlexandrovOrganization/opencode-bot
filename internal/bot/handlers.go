package bot

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"opencode-bot/internal/backend"
	"opencode-bot/internal/whisper"

	"github.com/mymmrac/telego"
	tu "github.com/mymmrac/telego/telegoutil"
)

// newMessageRequest builds a request with the configured agent and model.
func (b *Bot) newMessageRequest() backend.MessageRequest {
	b.mu.Lock()
	req := backend.MessageRequest{Agent: b.agent}
	if b.model != nil {
		req.Model = b.model
	}
	b.mu.Unlock()
	return req
}

// handleText processes a plain text message: sends it to the backend and
// streams the response back into the placeholder.
func (b *Bot) handleText(msg *telego.Message) {
	ctx, ok := b.beginRequest()
	if !ok {
		b.send(msg.Chat.ID, "⏳ Подожди, я ещё думаю...")
		return
	}
	req := b.newMessageRequest()
	req.AddText(msg.Text)
	b.startRequest(ctx, msg.Chat.ID, req)
}

// handlePhoto downloads a photo, uploads it through the backend and sends it
// to opencode. The caption (if any) is used as the user's question.
func (b *Bot) handlePhoto(msg *telego.Message) {
	ctx, ok := b.beginRequest()
	if !ok {
		b.send(msg.Chat.ID, "⏳ Подожди, я ещё думаю...")
		return
	}
	photo := msg.Photo[len(msg.Photo)-1]
	b.sendAttachment(ctx, msg.Chat.ID, photo.FileID, "photo.jpg", "image/jpeg", msg.Caption)
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

	ctx, ok := b.beginRequest()
	if !ok {
		b.send(msg.Chat.ID, "⏳ Подожди, я ещё думаю...")
		return
	}

	var fileID, format, noun string
	switch {
	case msg.Voice != nil:
		fileID, format, noun = msg.Voice.FileID, "ogg", "голосовое"
	case msg.VideoNote != nil:
		fileID, format, noun = msg.VideoNote.FileID, "mp4", "кружочек"
	default:
		b.release()
		return
	}

	statusMsg, err := b.api.SendMessage(ctx, tu.Message(tu.ID(msg.Chat.ID), "⏳ Скачиваю "+noun+"..."))
	if err != nil {
		b.release()
		slog.Error("send status", "error", err)
		return
	}

	lastStatus := ""
	data, err := b.downloadFile(ctx, fileID, func(downloaded, total int64) {
		statusText := formatDownloadStatus(downloaded, total, noun)
		if statusText == lastStatus {
			return
		}
		b.editMessage(ctx, msg.Chat.ID, statusMsg.MessageID, statusText)
		lastStatus = statusText
	})
	if err != nil {
		b.release()
		b.editMessage(ctx, msg.Chat.ID, statusMsg.MessageID, "❌ Не удалось скачать "+noun+": "+err.Error())
		return
	}
	slog.Info("voice received", "file_id", fileID, "bytes", len(data))

	// Нормализуем аудио в WAV 16k mono через ffmpeg: faster-whisper (и его
	// VAD-фильтр) надёжно декодирует PCM-WAV, тогда как «сырой» OGG/Opus от
	// Telegram в ряде случаев падает с «End of file» при расшифровке.
	audio := data
	outFormat := format
	if wav, werr := convertAudioToWAV(ctx, data); werr != nil {
		slog.Warn("normalize audio failed, send as-is", "error", werr)
	} else if len(wav) == 0 {
		slog.Warn("normalize audio produced empty result, send as-is")
	} else {
		audio = wav
		outFormat = "wav"
	}

	b.editMessage(ctx, msg.Chat.ID, statusMsg.MessageID, "⏳ Отправляю на расшифровку...")

	text, err := b.transcribeVoice(ctx, audio, outFormat, msg.Chat.ID, statusMsg.MessageID)
	if err != nil {
		b.release()
		slog.Error("transcribe", "error", err)
		b.editMessage(ctx, msg.Chat.ID, statusMsg.MessageID, "❌ Ошибка расшифровки: "+err.Error())
		return
	}

	slog.Info("transcribed", "chat_id", msg.Chat.ID, "chars", len(text))

	// Show the transcription, then hand it to the agent.
	b.editMessage(ctx, msg.Chat.ID, statusMsg.MessageID, "📝 "+truncate(text))
	req := b.newMessageRequest()
	req.AddText(text)
	b.startRequest(ctx, msg.Chat.ID, req)
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

// handleDocument downloads a document, uploads it through the backend and
// attaches it as a file part. The caption (if any) is used as the question.
func (b *Bot) handleDocument(msg *telego.Message) {
	ctx, ok := b.beginRequest()
	if !ok {
		b.send(msg.Chat.ID, "⏳ Подожди, я ещё думаю...")
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
	b.sendAttachment(ctx, msg.Chat.ID, msg.Document.FileID, filename, mime, msg.Caption)
}

// handleSticker downloads a sticker and uploads it through the backend as a
// file attachment. The sticker format depends on its type: regular (webp),
// animated (tgs), or video (webm).
func (b *Bot) handleSticker(msg *telego.Message) {
	ctx, ok := b.beginRequest()
	if !ok {
		b.send(msg.Chat.ID, "⏳ Подожди, я ещё думаю...")
		return
	}
	filename, mime := "sticker.webp", "image/webp"
	switch {
	case msg.Sticker.IsAnimated:
		filename, mime = "sticker.tgs", "application/x-tgsticker"
	case msg.Sticker.IsVideo:
		filename, mime = "sticker.webm", "video/webm"
	}
	b.sendAttachment(ctx, msg.Chat.ID, msg.Sticker.FileID, filename, mime, "")
}

// handleVideo downloads a video and uploads it through the backend as a
// file attachment. The caption (if any) is used as the user's question.
func (b *Bot) handleVideo(msg *telego.Message) {
	ctx, ok := b.beginRequest()
	if !ok {
		b.send(msg.Chat.ID, "⏳ Подожди, я ещё думаю...")
		return
	}
	filename := msg.Video.FileName
	if filename == "" {
		filename = "video.mp4"
	}
	mime := msg.Video.MimeType
	if mime == "" {
		mime = "video/mp4"
	}
	b.sendAttachment(ctx, msg.Chat.ID, msg.Video.FileID, filename, mime, msg.Caption)
}

// handleAnimation downloads an animation (GIF) and uploads it through the
// backend as a file attachment. The caption (if any) is used as the user's
// question.
func (b *Bot) handleAnimation(msg *telego.Message) {
	ctx, ok := b.beginRequest()
	if !ok {
		b.send(msg.Chat.ID, "⏳ Подожди, я ещё думаю...")
		return
	}
	filename := msg.Animation.FileName
	if filename == "" {
		filename = "animation.gif"
	}
	mime := msg.Animation.MimeType
	if mime == "" {
		mime = "image/gif"
	}
	b.sendAttachment(ctx, msg.Chat.ID, msg.Animation.FileID, filename, mime, msg.Caption)
}

// handleAudio downloads an audio file and uploads it through the backend as
// a file attachment. The caption (if any) is used as the user's question.
func (b *Bot) handleAudio(msg *telego.Message) {
	ctx, ok := b.beginRequest()
	if !ok {
		b.send(msg.Chat.ID, "⏳ Подожди, я ещё думаю...")
		return
	}
	filename := msg.Audio.FileName
	if filename == "" {
		filename = "audio.ogg"
	}
	mime := msg.Audio.MimeType
	if mime == "" {
		mime = "audio/ogg"
	}
	b.sendAttachment(ctx, msg.Chat.ID, msg.Audio.FileID, filename, mime, msg.Caption)
}

// sendAttachment скачивает файл из Telegram, загружает его через шлюз и
// отправляет агенту как вложение. Подпись (caption) добавляется как текст
// вопроса, если не пуста. busy-флаг освобождается только при успешном
// старте запроса (finishStream) либо при ошибке скачивания/загрузки.
func (b *Bot) sendAttachment(ctx context.Context, chatID int64, fileID, filename, mime, caption string) {
	data, err := b.downloadFile(ctx, fileID, nil)
	if err != nil {
		b.release()
		b.send(chatID, "Не удалось скачать файл: "+err.Error())
		return
	}

	url, err := b.backend.UploadFile(ctx, filename, mime, bytes.NewReader(data))
	if err != nil {
		b.release()
		b.send(chatID, "Не удалось загрузить файл: "+err.Error())
		return
	}

	req := b.newMessageRequest()
	req.AddFile(mime, filename, url)
	if caption := strings.TrimSpace(caption); caption != "" {
		req.AddText(caption)
	}
	b.startRequest(ctx, chatID, req)
}

// ── File helpers ─────────────────────────────────────────────────────────────

// downloadFile fetches a Telegram file by its file ID.
//
// Сначала пробуем скачать через сконфигурированный Bot API (локальный сервер
// при TELEGRAM_LOCAL_API_URL). Если он отдаёт 404 (типично для локального
// Bot API в --local-режиме, когда файл не попал в локальное хранилище, либо
// локальный сервер — просто прокси, а файл лежит в облаке), повторяем
// запрос напрямую к api.telegram.org.
//
// onProgress (если не nil) вызывается в ходе HTTP-загрузки с числом скачанных
// и общих байт, что позволяет показывать прогресс в статус-сообщении.
func (b *Bot) downloadFile(ctx context.Context, fileID string, onProgress func(downloaded, total int64)) ([]byte, error) {
	file, err := b.api.GetFile(ctx, &telego.GetFileParams{FileID: fileID})
	if err != nil {
		return nil, fmt.Errorf("get file info: %w", err)
	}
	if file.FilePath == "" {
		return nil, fmt.Errorf("get file info: пустой путь к файлу")
	}

	// Локальный Bot API-сервер (TELEGRAM_LOCAL_API_URL, --local режим) хранит
	// скачанные файлы на диске. HTTP-эндпоинт /file/ в этом режиме отдаёт 404,
	// поэтому, как в transcriber-bot, читаем файл напрямую со смонтированного
	// volume (telegram-bot-api-data → /var/lib/telegram-bot-api).
	if b.cfg != nil && b.cfg.TelegramLocalAPIURL != "" {
		if data, rerr := os.ReadFile(file.FilePath); rerr == nil {
			if onProgress != nil {
				onProgress(int64(len(data)), int64(len(data)))
			}
			return data, nil
		} else {
			slog.Warn("локальное чтение файла не удалось, пробуем HTTP", "path", file.FilePath, "error", rerr)
		}
	}

	urls := []string{b.api.FileDownloadURL(file.FilePath)}
	if b.cfg != nil && b.cfg.TelegramLocalAPIURL != "" &&
		!strings.HasPrefix(strings.ToLower(b.cfg.TelegramLocalAPIURL), "https://api.telegram.org") {
		urls = append(urls, "https://api.telegram.org/file/bot"+b.api.Token()+"/"+file.FilePath)
	}

	var lastErr error
	for _, url := range urls {
		data, derr := b.downloadFromURL(ctx, url, file.FileSize, onProgress)
		if derr == nil {
			return data, nil
		}
		lastErr = derr
		slog.Warn("скачивание файла не удалось", "url", url, "error", derr)
	}
	return nil, lastErr
}

// downloadFromURL выполняет GET-запрос и возвращает тело ответа либо ошибку
// с HTTP-статусом при неуспехе. totalHint — размер файла из метаданных
// Telegram (FileSize), используется, когда сервер не отдаёт Content-Length.
// onProgress (если не nil) вызывается по мере скачивания.
func (b *Bot) downloadFromURL(ctx context.Context, url string, totalHint int64, onProgress func(downloaded, total int64)) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}
	resp, err := b.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("download: неожиданный статус %d: %s", resp.StatusCode, string(body))
	}

	total := resp.ContentLength
	if total <= 0 && totalHint > 0 {
		total = int64(totalHint)
	}

	reader := &progressReader{r: resp.Body, total: total, onProgress: onProgress}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	return data, nil
}

// progressReader обёртывает io.Reader и сообщает о прогрессе скачивания, как
// в transcriber-bot. Обновления дедуплицируются (~5 МиБ), чтобы не спамить
// редактированием статус-сообщения в Telegram.
type progressReader struct {
	r            io.Reader
	total        int64
	onProgress   func(downloaded, total int64)
	downloaded   int64
	lastReported int64
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	if n > 0 {
		p.downloaded += int64(n)
		if p.onProgress != nil {
			if p.total <= 0 || p.downloaded-p.lastReported >= 5*1024*1024 || p.downloaded == p.total {
				p.lastReported = p.downloaded
				p.onProgress(p.downloaded, p.total)
			}
		}
	}
	return n, err
}

// convertAudioToWAV перегоняет скачанное аудио в WAV 16k mono через ffmpeg.
// На вход принимает любой контейнер (OGG/Opus, MP4/AAC и т.п.), на выходе —
// PCM-WAV, который faster-whisper декодирует без ошибок вида «End of file».
func convertAudioToWAV(ctx context.Context, data []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-i", "pipe:0",
		"-ar", "16000", "-ac", "1",
		"-f", "wav", "pipe:1",
	)
	cmd.Stdin = bytes.NewReader(data)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %w: %s", err, stderr.String())
	}
	return out.Bytes(), nil
}

// formatDownloadStatus формирует текст статуса скачивания для голосового,
// кружочка и прочих вложений, как в transcriber-bot.
func formatDownloadStatus(downloaded, total int64, noun string) string {
	if total > 0 {
		percent := float64(downloaded) / float64(total) * 100
		return fmt.Sprintf("⏳ Скачиваю %s... %.0f%%", noun, percent)
	}
	return fmt.Sprintf("⏳ Скачиваю %s... %s", noun, formatBytes(downloaded))
}

func formatBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}
