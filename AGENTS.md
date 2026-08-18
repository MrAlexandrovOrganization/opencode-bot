# AGENTS.md

Руководство для агентов, работающих над этим репозиторием. Описывает текущее
состояние проекта, архитектуру и конвенции.

## Проект

**opencode-bot** — Telegram-бот на Go, который проксирует сообщения в
[opencode](https://opencode.ai) — агента, умеющего читать/редактировать файлы,
выполнять команды и работать с git. Бот — единственный клиент `opencode-server`
(HTTP API + SSE event bus).

Язык интерфейсных строк, логов и комментариев — **русский**. Код — идиоматичный Go,
без внешних веб-фреймворков (только `telego`, `grpc`, стандартная библиотека).

## Как это запускается

Два сервиса в `docker-compose.yml`:

- **`opencode-server`** (`Dockerfile.server`) — сам opencode-сервер, бинарник из
  GitHub Releases (`anomalyco/opencode`), слушает порт `4096`. Mounts:
  - `/home/maxim:/home/maxim:ro` — домашняя директория только для чтения;
  - `/home/maxim/projects:/workspace` — перекрывающий rw-маунт, единственное
    место, где агент может править файлы;
  - XDG-директории opencode (`~/.config`, `~/.local/share`, `~/.cache`) — общее
    состояние, конфиги, провайдеры, БД.
  - Служит под `ubuntu` (UID 1000), git-identity задаётся из env через
    `scripts/entrypoint.sh`.
- **`opencode-bot`** (`Dockerfile`) — сам бот. Собирает Go-бинарник, proto-стабы
  генерируются внутри образа. Запускается с `env_file: .env`.

Внешние сети: `telegram-net` (общий локальный Telegram Bot API-сервер) и
`whisper-net` (общий gRPC-сервис транскрибации).

Запуск: `make up` (сборка + старт), `make server` (только сервер).

## Структура репозитория

```
cmd/bot/main.go              — точка входа: конфиг, HTTP-клиент, telegram, opencode,
                               whisper, запуск Bot.Run(ctx)
internal/bot/                — ТЕЛЕГРАМ-ЛОГИКА + движок взаимодействия с opencode
  bot.go                     — состояние: sessionID, chatID, busy, Stream, perms,
                               pendingQ; SSE event loop; стриминг ответа в placeholder;
                               форматирование markdown→HTML и нарезка сообщений
  update.go                  — разбор апдейтов, авторизация по ROOT_ID, маршрутизация
                               (text/photo/document/voice/command/callback)
  commands.go                — /start /help /reset /session /abort /model /agent
  handlers.go                — обработка текста/фото/документа/голоса, скачивание
                               файлов из Telegram
  permissions.go             — permission.asked: ask/allow/deny, inline-кнопки
  questions.go               — question.asked: вопрос с опциями, "свой ответ"
  format.go                  — markdown→HTML, разбивка на сообщения ≤4000
  *_test.go                  — юнит-тесты форматирования и статусов
internal/opencode/client.go  — тонкий HTTP-клиент к opencode-server:
                               /global/health, /session, /session/{id}/message,
                               /abort, /permissions, /question/{id}/reply, /event (SSE)
internal/whisper/client.go   — gRPC-клиент к сервису транскрибации (async-джобы)
internal/config/config.go    — конфиг из env
gen/whisper/                 — сгенерированные proto-стабы (не редактировать руками)
proto/whisper.proto          — канонический источник whisper.proto (копия из
                               backends/transcriber)
scripts/entrypoint.sh        — задаёт git identity в контейнере сервера
agents/                      — документы для дальнейшей работы (планы, фичи, решения)
```

## Ключевые понятия

- **Сессия** — одна глобальная сессия opencode на весь бот (`b.sessionID`),
  создаётся лениво при первом сообщении, сбрасывается командой `/reset`.
- **Busy-флаг** — `tryAcquire()/release()`: пока запрос в полёте, новый
  отклоняется («⏳ Подожди, я ещё думаю…»). Конкурентности внутри бота нет.
- **Stream** — состояние текущего запроса (partial, reasoning, статусы/лог
  тул-вызовов), наполняется из SSE, рендерится в «💭»-placeholder каждые 1.2s.
- **SSE event loop** — `b.eventLoop` держит единственную подписку на `/event`
  с реконнектом. Обрабатываются типы: `message.part.updated`,
  `permission.asked`, `question.asked`.
- **Permissions** — режимы `PERMISSION_MODE`: `ask` (кнопки ✅ один раз /
  🟢 всегда / ❌ отклонить), `allow`, `deny`. Соответствие `permissionID`
  → вопрос в `b.perms`.
- **Questions** — вопросы агента (инструмент `question`): серия вопросов с
  опциями, отвечается по одному, ответы аккумулируются и отправляются разом
  в `/question/{id}/reply`.

## Конфигурация (env)

Все переменные — в `.env` / `.env.example`:

| Переменная | Назначение |
|---|---|
| `BOT_TOKEN` | токен Telegram-бота (обязателен) |
| `ROOT_ID` | telegram user ID единственного авторизованного пользователя (обязателен) |
| `OPENCODE_BASE_URL` | URL opencode-сервера (`http://opencode-server:4096` в docker) |
| `OPENCODE_USERNAME/PASSWORD` | Basic-auth к серверу |
| `OPENCODE_MODEL` | модель по умолчанию (пусто = серверная) |
| `OPENCODE_AGENT` | агент по умолчанию (default `build`) |
| `PERMISSION_MODE` | `ask` / `allow` / `deny` (default `ask`) |
| `OPENCODE_REQUEST_TIMEOUT` | таймаут одного запроса (default `30m`) |
| `TELEGRAM_LOCAL_API_URL` | локальный Telegram Bot API (пусто = api.telegram.org) |
| `WHISPER_GRPC_HOST/PORT` | сервис транскрибации (пусто = голос отключён) |
| `GIT_USER_NAME/EMAIL` | git identity агента (в сервере) |

## Команды

- `make up` / `make down` / `make logs` / `make restart` / `make deploy` — docker compose
- `make server` — пересборка и старт только opencode-server
- `make test` — `go test ./cmd/... ./internal/...`
- `make lint` — проверка `gofmt -l` (не `go vet`, не golangci)
- `make format` — `gofmt -w`
- `make cover` — покрытие
- `make proto` — синхронизация `proto/whisper.proto` из `backends/transcriber`
  и регенерация стабов (`make install` для плагинов + protoc)

**Перед сдачей изменений:** `make lint` и `make test`.

## Конвенции

- Go 1.26, модуль `opencode-bot`. Сторонних веб-фреймворков не добавляем.
- `internal/opencode` — только HTTP-обёртка над opencode API, без логики
  Telegram. Telegram-специфика — только в `internal/bot`.
- Сгенерированные файлы (`gen/whisper`) руками не править.
- Ответы пользователю — на русском, эмодзи допустимы (существующий стиль).
- Тесты — стандартный `testing`, рядом с кодом (`*_test.go`).

## Известные ограничения (важно для планирования)

1. **Одна сессия на всех и глобальный busy-флаг** — бот однопользовательский
   и серийный.
2. **Один авторизованный пользователь** — проверка `msg.From.ID != ROOT_ID`.
3. **Вся «инженерия» зашита в `internal/bot`** — нельзя переиспользовать
   движок (сессии, стриминг, permissions, questions) для другого фронтенда.
4. Состояние в памяти — после рестарта сессия теряется.
5. `internal/bot/update.go:47` — `answerQuestionText` перехватывает любой
   текст как ответ на вопрос (нет явного разделения).
6. Нет rate-limiting, квот, логов запросов, метрик.

## Куда дальше

Направление развития — **backend-шлюз** как единая точка взаимодействия с
opencode (сессии, стриминг, permissions, questions), с несколькими
фронтендами (Telegram, web, приложение). Подробнее и задачи — в
[`agents/`](agents/README.md).