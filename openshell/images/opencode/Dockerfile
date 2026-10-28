# syntax=docker/dockerfile:1
#
# agent-opencode: shared base + opencode release binary (pinned, sha256-verified).
# BASE_IMAGE is supplied by build.sh (a local build of ../base), not a registry
# dependency, so Renovate must ignore it.
ARG BASE_IMAGE=localhost/agent-base:local
FROM ${BASE_IMAGE}

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# Upstream moved to github.com/anomalyco/opencode. Its releases publish no checksum file, only
# GitHub's per-asset sha256 digest, so the per-arch digests below are pinned here and MUST be
# refreshed together with OPENCODE_VERSION by running
#   openshell/images/refresh-checksums.sh   (CI runs it with --check). Manual equivalent:
#   gh api repos/anomalyco/opencode/releases/tags/v<ver> --jq '.assets[]|select(.name|test("^opencode-linux-(x64|arm64)\\.tar\\.gz$"))|.name+" "+.digest'
# renovate: datasource=github-releases depName=anomalyco/opencode
ARG OPENCODE_VERSION=1.19.4
ARG OPENCODE_SHA256_AMD64=13a8fb2978302071b86f9d7e00da9c8895f9cd846978379c3bdc6a5333536c9b
ARG OPENCODE_SHA256_ARM64=7900818a6a98881edbbadab4b7525940eff3b9750040c200481f9f56cacee405
ARG REVISION=unknown
ARG CREATED=unknown

USER 0
WORKDIR /tmp
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      amd64) oarch=x64; sha="${OPENCODE_SHA256_AMD64}" ;; \
      arm64) oarch=arm64; sha="${OPENCODE_SHA256_ARM64}" ;; \
      *) echo "unsupported arch $arch" >&2; exit 1 ;; \
    esac; \
    f="opencode-linux-${oarch}.tar.gz"; \
    curl -fsSLO --retry 5 --retry-all-errors "https://github.com/anomalyco/opencode/releases/download/v${OPENCODE_VERSION}/${f}"; \
    echo "${sha}  ${f}" | sha256sum -c -; \
    tar -xzf "$f" opencode; \
    install -m 0755 opencode /usr/local/bin/opencode; \
    rm -f "$f" opencode; \
    /usr/local/bin/opencode --version

# Autoupdate off two ways: env var (documented in packages/web/src/content/docs/cli.mdx at
# v1.19.3, "OPENCODE_DISABLE_AUTOUPDATE") and the global config key `autoupdate: false`
# (docs config.mdx, "Autoupdate").
ENV OPENCODE_DISABLE_AUTOUPDATE=1
WORKDIR /sandbox
# hadolint ignore=SC2016
RUN install -d -o sandbox -g sandbox /sandbox/.config/opencode \
    && printf '%s\n' '{' '  "$schema": "https://opencode.ai/config.json",' '  "autoupdate": false' '}' \
        > /sandbox/.config/opencode/opencode.json \
    && chown sandbox:sandbox /sandbox/.config/opencode/opencode.json

LABEL org.opencontainers.image.source="https://github.com/jlaska/agent-sandbox" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}" \
      org.opencontainers.image.version="${OPENCODE_VERSION}" \
      org.opencontainers.image.title="agent-opencode" \
      io.github.jlaska.agent-sandbox.harness="opencode" \
      io.github.jlaska.agent-sandbox.harness-version="${OPENCODE_VERSION}"

USER 1500:1500
