# syntax=docker/dockerfile:1
#
# agent-pi: shared base + pi coding agent (pinned) + pi-provider-litellm extension (pinned).
# BASE_IMAGE is supplied by build.sh (a local build of ../base), not a registry
# dependency, so Renovate must ignore it.
ARG BASE_IMAGE=localhost/agent-base:local
FROM ${BASE_IMAGE}

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# The base image's npm cooldown (min-release-age, DEP_COOLDOWN_DAYS) applies to these exact pins:
# a version younger than the cooldown fails the build with ETARGET. Renovate waits the same
# 3 days (minimumReleaseAge in .github/renovate.json5) before proposing a bump.
# renovate: datasource=npm depName=@earendil-works/pi-coding-agent
ARG PI_VERSION=1.2.0
# renovate: datasource=npm depName=pi-provider-litellm
ARG PI_LITELLM_VERSION=4.2.0
ARG REVISION=unknown
ARG CREATED=unknown

# pi is a node script: the policy-relevant binary is the real node path (/usr/local/bin/node).
USER 0
RUN npm install -g --prefix /usr/local "@earendil-works/pi-coding-agent@${PI_VERSION}" \
    && npm cache clean --force \
    && pi --version

# Pre-install the extension for the sandbox user so no npm egress is needed at runtime.
USER 1500:1500
RUN pi install "npm:pi-provider-litellm@${PI_LITELLM_VERSION}" \
    && rm -rf /sandbox/.npm

LABEL org.opencontainers.image.source="https://github.com/jlaska/agent-sandbox" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.created="${CREATED}" \
      org.opencontainers.image.version="${PI_VERSION}" \
      org.opencontainers.image.title="agent-pi" \
      io.github.jlaska.agent-sandbox.harness="pi" \
      io.github.jlaska.agent-sandbox.harness-version="${PI_VERSION}"
