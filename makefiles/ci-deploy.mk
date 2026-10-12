# Standalone entry point: CI may load this file from the checked Git revision
# before the VM checkout contains the new Makefile. Keep it self-contained.
export IMAGE_DEPLOY_LOCK := $(HOME)/.opencode-bot-images-deploy.lock

.PHONY: ci-deploy
ci-deploy:
	@set -eu; \
	test "$$(git rev-parse --show-toplevel)" = "$$(pwd -P)"; \
	test -n "$${IMAGE_MANIFEST_B64:-}"; \
	test -n "$${GHCR_USER:-}"; \
	test -n "$${GHCR_TOKEN:-}"; \
	test "$${#DEPLOY_SHA}" -eq 40; \
	case "$$DEPLOY_SHA" in *[!0-9a-f]*) exit 1 ;; esac; \
	exec 9>"$(IMAGE_DEPLOY_LOCK)"; \
	flock -w 1800 9; \
	test "$$(git branch --show-current)" = main; \
	git fetch --quiet origin refs/heads/main; \
	test "$$(git rev-parse FETCH_HEAD)" = "$$DEPLOY_SHA" || { echo 'Stale workflow: main changed' >&2; exit 1; }; \
	test -z "$$(git diff --cached --name-only)" || { echo 'Server has staged local changes; refusing deployment' >&2; exit 1; }; \
	test -z "$$(git ls-files --others --exclude-standard)" || { echo 'Server has untracked files; refusing deployment' >&2; exit 1; }; \
	dirty="$$(git diff --name-only)"; \
	if test -n "$$dirty"; then \
		tmp_dir=$$(mktemp -d); \
		git diff --binary > "$$tmp_dir/server.patch"; \
		git archive "$$DEPLOY_SHA" | tar -x -C "$$tmp_dir"; \
		patch -R --dry-run -p1 -d "$$tmp_dir" < "$$tmp_dir/server.patch" || { echo 'Server has local changes not included in deployment revision' >&2; exit 1; }; \
		rm -rf "$$tmp_dir"; \
		git reset --hard --quiet HEAD; \
	fi; \
	git merge --ff-only --quiet "$$DEPLOY_SHA"; \
	test "$$(git rev-parse HEAD)" = "$$DEPLOY_SHA"; \
	DOCKER_CONFIG=$$(mktemp -d); export DOCKER_CONFIG; \
	trap 'rm -rf "$$DOCKER_CONFIG"' EXIT; \
	printf '%s' "$$GHCR_TOKEN" | docker login ghcr.io -u "$$GHCR_USER" --password-stdin; \
	unset GHCR_TOKEN; \
	IMAGE_DEPLOY_LOCKED=1 DEPLOY_LOCKED=1 make -f Makefile image-deploy
