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

exec opencode "$@"