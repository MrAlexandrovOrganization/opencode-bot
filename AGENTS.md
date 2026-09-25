# AGENTS.md

Руководство для агентов, работающих над этим репозиторием. Описывает текущее
состояние проекта, архитектуру и конвенции.

## Проект

**opencode-bot** — Telegram-бот на Go, который проксирует сообщения в
[opencode](https://opencode.ai) — агента, умеющего читать/редактировать файлы,
выполнять команды и работать с git. Бот — фронтенд над **opencode-backend**
(шлюз, репозиторий в `/home/maxim/projects/backends/opencode-backend`):
вся работа с `opencode-server` (сессии, асинхронные сообщения, WebSocket-события,
permissions, вопросы, загрузка файлов) идёт через его REST `/api/v1` + WS.

Язык интерфейсных строк, логов и комментариев — **русский**. Код — идиоматичный Go,
без внешних веб-фреймворков (только `telego`, `grpc`, `coder/websocket`,
стандартная библиотека).

## Как это запускается

Три сервиса в `docker-compose.yml`:

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
  генерируются внутри образа. Запускается с `env_file: .env`, ходит в
  **внешний** шлюз `opencode-backend` (`BACKEND_BASE_URL`, `BACKEND_TOKEN`).

Внешние сети: `telegram-net` (общий локальный Telegram Bot API-сервер) и
`whisper-net` (общий gRPC-сервис транскрибации; на ней же сидят `opencode-server`
и развернутый отдельно шлюз `opencode-backend`).

**`opencode-backend` (шлюз) здесь НЕ поднимается.** Он разрабатывается и
деплоится отдельно — свой репозиторий `backends/opencode-backend` со своим CI.
В `docker-compose.yml` этого бота сервиса `opencode-backend` нет; бот
подключается к уже запущенному шлюзу (сервис `opencode-backend` на сети
`whisper-net`, порт `8080`). Шлюз, в свою очередь, ходит в `opencode-server`
(тоже на `whisper-net`, порт `4096`), поэтому `opencode-server` остаётся
в этом стеке и общий для шлюза и бота.

Запуск: `make up` (сборка + старт opencode-server и opencode-bot),
`make server` (только opencode-server). Шлюз поднимается отдельно в его репозитории.

### Выборочный VPN на VM

`docker-compose.override.yml` автоматически подключает **opencode-server** к
внешней сети `vless-egress` (`10.245.77.2`, gateway priority 100) и задаёт DNS
`10.245.77.1`. Перед `make up` нужна подготовленная инфраструктура
`infra/network/vless-client`; её инструкции и откат — в соответствующем
`README.md`. Сеть `whisper-net` сохраняется. На хосте выборочная маршрутизация
направляет внешний трафик этой сети через `vless0`, а локальные сервисы остаются
доступны напрямую. Без VPN внешний трафик выбранной сети блокируется.
Для запуска без этого VM-specific override явно использовать
`docker compose -f docker-compose.yml ...`. Не заменять незакоммиченные
серверные правки основного Compose при применении VPN.

## Структура репозитория

```
cmd/bot/main.go              — точка входа: конфиг, HTTP-клиент, telegram, backend,
                               whisper, запуск Bot.Run(ctx)
internal/bot/                — ТЕЛЕГРАМ-ЛОГИКА + движок взаимодействия с opencode
  bot.go                     — состояние: sessionID, chatID, busy, Stream, perms,
                               pendingQ; WS event loop; стриминг ответа в placeholder;
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
internal/backend/client.go   — тонкий HTTP+WS-клиент к opencode-backend (шлюз):
                               Health, CreateSession, SendMessage (async → messageID),
                               Abort, Get/Delete, ReplyPermission, ReplyQuestion,
                               UploadFile (multipart), activity всех сессий,
                               Events (WebSocket)
internal/backend/types.go    — типы шлюза: Event, Message, Session, PermissionAsked,
                               QuestionAsked, MessageRequest и пр.
internal/whisper/client.go   — gRPC-клиент к сервису транскрибации (async-джобы)
internal/config/config.go    — конфиг из env
internal/telemetry/          — OpenTelemetry provider и OTLP/HTTP exporter
gen/whisper/                 — сгенерированные proto-стабы (не редактировать руками)
proto/whisper.proto          — канонический источник whisper.proto (копия из
                               backends/transcriber)
scripts/entrypoint.sh        — задаёт git identity в контейнере сервера
agents/                      — документы для дальнейшей работы (планы, фичи, решения)
```

`internal/opencode` в боте больше нет: вся работа с opencode-server идёт через
шлюз `opencode-backend` (`internal/backend`). Код шлюза живёт отдельно — в
`/home/maxim/projects/backends/opencode-backend`.

## Ключевые понятия

- **Сессия** — одна глобальная сессия opencode на весь бот (`b.sessionID`),
  живёт на шлюзе (создаётся лениво при первом сообщении через
  `POST /api/v1/sessions`, сбрасывается командой `/reset`). После рестарта
  бота сессия переживает: id восстанавливается из шлюза.
- **Busy-флаг** — `tryAcquire()/release()`: пока запрос в полёте, новый
  отклоняется («⏳ Подожди, я ещё думаю…»). Конкурентности внутри бота нет;
  дополнительно шлюз режет конкурентные запросы в одну сессию (409).
- **Stream** — состояние текущего запроса (partial, reasoning, статусы/лог
  тул-вызовов), наполняется из WS-событий, рендерится в «💭»-placeholder
  каждые 1.2s. Финализируется из `message.updated` идемпотентно
  (`finalizeOnce`), освобождая busy-флаг ровно один раз.
- **WS event loop** — `b.eventLoop` держит единственную подписку на
  `/api/v1/ws?session=*` с реконнектом. Обрабатываются типы:
  `message.part.updated`, `message.updated`, `permission.asked`,
  `question.asked`.
- **Permissions** — режимы `PERMISSION_MODE` (на шлюзе): `ask` (кнопки ✅ один раз /
  🟢 всегда / ❌ отклонить), `allow`, `deny`. Соответствие `permissionID`
  → вопрос в `b.perms`, ответ уходит в `POST /api/v1/sessions/{id}/permissions/{pid}`.
- **Questions** — вопросы агента (инструмент `question`): серия вопросов с
  опциями, отвечается по одному, ответы аккумулируются и отправляются разом
  в `POST /api/v1/questions/{id}/reply`.

## Конфигурация (env)

Все переменные — в `.env` / `.env.example`:

| Переменная | Назначение |
|---|---|
| `BOT_TOKEN` | токен Telegram-бота (обязателен) |
| `ROOT_ID` | telegram user ID единственного авторизованного пользователя (обязателен) |
| `BACKEND_BASE_URL` | URL внешнего шлюза opencode-backend (`http://opencode-backend:8080` в docker на сети whisper-net; для локального запуска — опубликованный порт, напр. `http://localhost:8091`) |
| `BACKEND_TOKEN` | токен шлюза = `ADMIN_TOKEN` сервиса opencode-backend (обязателен) |
| `OPENCODE_MODEL` | модель по умолчанию (пусто = серверная) |
| `OPENCODE_AGENT` | агент по умолчанию (default `build`) |
| `PERMISSION_MODE` | `ask` / `allow` / `deny` (default `ask`) |
| `OPENCODE_REQUEST_TIMEOUT` | таймаут одного запроса (default `30m`) |
| `TELEGRAM_LOCAL_API_URL` | локальный Telegram Bot API (пусто = api.telegram.org) |
| `WHISPER_GRPC_HOST/PORT` | сервис транскрибации (пусто = голос отключён) |
| `OPENCODE_USERNAME/PASSWORD` | Basic-auth к opencode-server (использует шлюз, а не бот) |
| `GIT_USER_NAME/EMAIL` | git identity агента (в сервере) |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP/HTTP collector; по умолчанию `http://jaeger:4318`, Compose подключает бота к `jaeger-net` |

## Команды

- `make up` / `make down` / `make logs` / `make restart` / `make deploy` — docker compose
- `make server` — пересборка и старт только opencode-server
- `make test` — юнит-тесты Go-пакетов
- `make fmt-check` — проверка форматирования без изменения файлов
- `make lint` — `go vet` по пакетам бота
- `make check` — полный локальный gate: форматирование, lint и тесты
- `make format` — `gofmt -w`
- `make cover` — покрытие
- `make proto` — синхронизация `proto/whisper.proto` из `backends/transcriber`
  и регенерация стабов (`make install` для плагинов + protoc)

**Перед сдачей изменений:** `make lint` и `make test`.

Размер входящих Telegram-файлов ограничен 100 МиБ до конвертации и загрузки в
шлюз. Стандартный Compose монтирует SSH deploy-ключ в контейнер сервера, если
ключ существует на хосте и задан `GIT_DEPLOY_KEY_PATH`.

## Конвенции

- Go 1.26, модуль `opencode-bot`. Сторонних веб-фреймворков не добавляем.
- `internal/backend` — только HTTP+WS-обёртка над opencode-backend, без логики
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
4. Состояние бота в памяти; сессия живёт на шлюзе (переживает рестарт бота,
   но не рестарт шлюза).
5. `internal/bot/update.go:47` — `answerQuestionText` перехватывает любой
   текст как ответ на вопрос (нет явного разделения).
6. Нет rate-limiting, квот и метрик. Есть JSON-логи и базовые OTel spans/
   propagation HTTP и WebSocket; наличие этой инструментации не подтверждает
   сквозной trace всех обработчиков Telegram и gRPC.

## Куда дальше

Направление развития — **backend-шлюз** как единая точка взаимодействия с
opencode (сессии, стриминг, permissions, questions), с несколькими
фронтендами (Telegram, web, приложение). Шлюз уже реализован в
`opencode-backend`; в этом репозитории бот — один из фронтендов.
Подробнее и задачи — в [`agents/`](agents/README.md).
