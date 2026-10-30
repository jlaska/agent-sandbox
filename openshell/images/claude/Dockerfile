# syntax=docker/dockerfile:1
#
# agent-claude: shared base + Claude Code (pinned).
# BASE_IMAGE is supplied by build.sh (a local build of ../base), not a registry
# dependency, so Renovate must ignore it.
ARG BASE_IMAGE=localhost/agent-base:local
FROM ${BASE_IMAGE}

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# renovate: datasource=npm depName=@anthropic-ai/claude-code
ARG CLAUDE_VERSION=2.1.311
ARG REVISION=unknown
ARG CREATED=unknown

USER 0
# Native installer (it verifies the download against Anthropic's signed manifest).
# Install into a throwaway HOME, then materialise the real binary (not a symlink)
# at a stable path so OpenShell's real-path binary matching works.
RUN set -eux; \
    export HOME=/tmp/claude-home; mkdir -p "$HOME"; \
    curl -fsSL --retry 5 --retry-all-errors https://claude.ai/install.sh | bash -s "${CLAUDE_VERSION}"; \
    install -m 0755 "$(readlink -f "$HOME/.local/bin/claude")" /usr/local/bin/claude; \
    rm -rf "$HOME"; \
    /usr/local/bin/claude --version

ENV DISABLE_AUTOUPDATER=1

LABEL org.opencontainers.image.source="https://github.com/jlaska/agent-sandbox" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}" \
      org.opencontainers.image.version="${CLAUDE_VERSION}" \
      org.opencontainers.image.title="agent-claude" \
      io.github.jlaska.agent-sandbox.harness="claude" \
      io.github.jlaska.agent-sandbox.harness-version="${CLAUDE_VERSION}"

USER 1500:1500
