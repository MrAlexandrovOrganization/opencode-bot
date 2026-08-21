#!/bin/sh
set -e

# Configure git identity for the agent from environment (GIT_USER_NAME /
# GIT_USER_EMAIL, passed from .env via docker-compose). Empty values are
# ignored so an unset identity never breaks the container.
if [ -n "${GIT_USER_NAME:-}" ]; then
	git config --global user.name "${GIT_USER_NAME}"
fi
if [ -n "${GIT_USER_EMAIL:-}" ]; then
	git config --global user.email "${GIT_USER_EMAIL}"
fi

# ── Git push access (optional) ───────────────────────────────────────────────
# Агент может пушить в GitHub двумя способами (оба опциональны, пустые
# переменные ничего не меняют):
#   1) SSH deploy-ключ: смонтируй приватный ключ в контейнер и укажи путь в
#      GIT_DEPLOY_KEY_PATH (например, через volume). Ключ должен быть без
#      парольной фразы и давать write-доступ к нужным репозиториям.
#   2) GitHub PAT: задай GH_TOKEN (fine-grained токен с contents:write на
#      целевые репозитории) — git-URL'ы переписываются на HTTPS автоматически,
#      SSH-клиент при этом не нужен.
if [ -n "${GIT_DEPLOY_KEY_PATH:-}" ]; then
	if [ ! -f "${GIT_DEPLOY_KEY_PATH}" ]; then
		echo "GIT_DEPLOY_KEY_PATH задан, но файл не найден: ${GIT_DEPLOY_KEY_PATH}" >&2
	else
		mkdir -p "${HOME}/.ssh"
		cp "${GIT_DEPLOY_KEY_PATH}" "${HOME}/.ssh/id_ed25519"
		chmod 600 "${HOME}/.ssh/id_ed25519"
		cat >> "${HOME}/.ssh/config" <<'EOF'
Host github.com
	StrictHostKeyChecking accept-new
	IdentityFile ~/.ssh/id_ed25519
	IdentitiesOnly yes
EOF
		ssh-keyscan -H github.com >> "${HOME}/.ssh/known_hosts" 2>/dev/null || true
	fi
fi

if [ -n "${GH_TOKEN:-}" ]; then
	git config --global url."https://x-access-token:${GH_TOKEN}@github.com/".insteadOf "git@github.com:"
	git config --global url."https://x-access-token:${GH_TOKEN}@github.com/".insteadOf "https://github.com/"
fi

exec opencode "$@"