# OpenCode Bot

Telegram frontend над отдельным `opencode-backend`; этот стек также содержит
opencode-server. Архитектура и конфигурация описаны в `AGENTS.md`.

HTTPS ingress использует отдельную внутреннюю external-сеть `caddy-opencode-net`
и alias `opencode-ingress`. Сеть создаёт стек `infra/network/caddy`; поднимите
его перед первым запуском этого Compose. Бот в ingress-сеть не подключается.
`whisper-net` и VM-specific VLESS override сохраняют свои назначения.
`make config-check` валидирует Compose без загрузки `.env` и запуска контейнеров.

## Версии сборки

Go задаётся в `go.mod`, образы и инструменты контейнера сервера — в `versions.mk`.
Существующий protoc-gen-go закреплён отдельно от runtime protobuf, чтобы
сохранить выбранную версию генератора. Poetry, ранее устанавливавшийся без
версии, теперь также закреплён.

- `make versions` — показать все значения.
- `make install` — установить генераторы для бота.
- `make compose-build` — собрать оба образа.
- `make up` — собрать и запустить стек.
- `make dev` — пересобрать и перезапустить только бота.
- `make check` — форматирование, vet, тесты и валидация Compose-конфигурации.

Make экспортирует версии в Compose и Dockerfile. Для изменения релиза
opencode-server редактируйте `OPENCODE_VERSION` в `versions.mk`.
CI берёт Go из `go.mod`; ссылки на GitHub Actions остаются в workflow.

## Доставка образов из CI

Рекомендуемый путь релиза: собирать образы после проверок в CI и разворачивать
опубликованные образы по digest. На VM не пересобирать приложения из checkout:
так запускаются те же артефакты, которые прошли проверки.

CI выполняет существующие проверки, затем `make image-build` собирает все собственные
образы из build-секций Compose без runtime-секретов и соседних checkout.
Push main публикует **эти же** образы в GHCR через `make image-publish`.
Тег содержит SHA, run ID и attempt; artifact `image-release` содержит digest manifest.
Платформа — `linux/amd64` (текущая VM). На PR публикации и доставки нет.
Версии build args по-прежнему приходят из Make, определения сборки — из Compose.

SSH CD передаёт SHA, manifest и временный token в `make ci-deploy`.
Эта цель из `makefiles/ci-deploy.mk` под одной блокировкой проверяет main и актуальность
workflow. Она отклоняет staged и untracked серверные изменения; tracked diff допускается
только когда `patch --dry-run` подтверждает, что его можно полностью обратить на дереве
целевого коммита, то есть эти же
изменения уже входят в доставляемый код. Затем checkout fast-forward обновляется до SHA,
временно авторизует Docker в GHCR и вызывает `make image-deploy` из нового Makefile.
Сначала скачиваются все образы, затем `image-activate` сохраняет подготовку/миграции
этого проекта и запускает Compose с `--no-build --pull never --wait`.
Доступ к registry удаляется и при ошибке. `ci-deploy` требует переменные
`DEPLOY_SHA`, `IMAGE_MANIFEST_B64`, `GHCR_USER`, `GHCR_TOKEN` из окружения;
не передавайте token аргументом Make или команды.

Короткий SSH-шаг делает fetch и загружает только `makefiles/ci-deploy.mk` через
`git show` из SHA этого workflow. Поэтому первый деплой работает и со старым
checkout без новой цели. Загруженный файл сам проверяет актуальность SHA и повторно
делает fetch после захвата блокировки; параллельные доставки не обновляют checkout
во время активации. Обычные `make up` и существующие `make deploy` сохраняют
свои прежние контракты; для CI служит отдельная цель `ci-deploy`.

Для CI разрешите GitHub Actions запись Packages. Новые GHCR packages по умолчанию
private; настройте наследование доступа от этого репозитория. Автоматический CD
использует временный `GITHUB_TOKEN` с `packages: read` и временный Docker config,
который удаляется после SSH-шагa. Для ручной доставки нужен отдельный read-only
доступ (`read:packages`) или публичный package; приложение секреты получает только на VM.
VM нужны Python 3.9+, Make, Docker Compose с `--wait`, Git и flock для SSH CD.

`.images/current.json` хранит последний успешный manifest, `previous.json` — предыдущий.
Для отката сначала согласуйте совместимость БД и конфигурации, верните checkout
к `revision` выбранного manifest и выполните `make image-deploy IMAGE_MANIFEST=...`.
Автоматического отката БД/образов нет; не удаляйте нужные digests в GHCR.
`make up` остаётся локальной сборкой из исходников: на VM для релиза используйте
`image-deploy`; `logs`/`ps` читают те же контейнеры. Сторонние образы не пересобираются,
их версии остаются в конфигурации проекта. Внешний registry-кеш между CI runs пока
не настроен. Успешный build не подтверждает runtime: проверяйте readiness и сценарий
сервиса после доставки.

Для диагностики и явных CLI-операций из активного образа используйте
`make image-compose IMAGE_ARGS='ps'`, `IMAGE_ARGS='logs -f'` или
`IMAGE_ARGS='run --rm SERVICE COMMAND'`. Последняя команда автоматически
запрещает сборку и pull. Она подходит для явных миграций/CLI, которые раньше
запускались через локальный Compose-образ. После неуспешной активации
`.images/requested.json` сохраняет manifest попытки; `current.json` остаётся
последним успешным и не гарантирует, что частичный rollout откатился.

Ручной `image-deploy` использует тот же host lock, что SSH CD; не запускайте
обходные Compose-операции одновременно с доставкой. При первом переходе на GHCR
`previous.json` ещё отсутствует: сохраните прежние локальные образы и конфигурацию
для восстановления по runbook до первого релиза.

## Container names

Compose uses explicit service names without replica numbers. Set
`CONTAINER_NAME_PREFIX` (for example `dev-`) for an additional local or CI stack
on the same Docker host. Keep Compose project/service labels and volume names
unchanged; clients address services or explicit network aliases.
