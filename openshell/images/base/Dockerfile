# syntax=docker/dockerfile:1
#
# Shared base for the agent-sandbox harness images (claude, pi, opencode).
# Built first by build.sh as localhost/agent-base:<tag>; never published on its own.
#
# OpenShell v0.1.4 custom-image requirements (docs/how-it-works/sandboxes/overview.mdx and
# examples/bring-your-own-container/Dockerfile at tag v0.1.4):
#   - iproute2 is required (network namespaces); nftables is optional (bypass detection)
#   - the supervisor is injected by the driver; CMD/ENTRYPOINT are ignored
#   - the image's non-root USER is the agent identity on the Docker/Podman drivers
#   - bash should be present (sandbox exec uses a login shell: bash -lc)
#
# apt packages are intentionally unpinned: the base image digest below pins the
# Ubuntu archive snapshot closely enough, and Renovate bumps the digest.
FROM nvcr.io/nvidia/base/ubuntu:26.04@sha256:3cacfd2cba1b5fe1b80c424b7863dad960d75e2edca076c18deb5bd32b3c0740

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

ENV DEBIAN_FRONTEND=noninteractive

# hadolint ignore=DL3008
RUN apt-get update && apt-get install -y --no-install-recommends \
        bash \
        ca-certificates \
        curl \
        git \
        iproute2 \
        jq \
        libatomic1 \
        make \
        nftables \
        openssh-sftp-server \
        procps \
        python3 \
        xz-utils \
    && rm -rf /var/lib/apt/lists/*

# node (official tarball, SHASUMS256 verified). npm ships inside the tarball.
# renovate: datasource=node-version depName=node
ARG NODE_VERSION=24.22.2
# Download scratch space for the tool installs below.
WORKDIR /tmp
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in amd64) narch=x64 ;; arm64) narch=arm64 ;; *) echo "unsupported arch $arch" >&2; exit 1 ;; esac; \
    f="node-v${NODE_VERSION}-linux-${narch}.tar.xz"; \
    curl -fsSLO --retry 5 --retry-all-errors "https://nodejs.org/dist/v${NODE_VERSION}/${f}"; \
    curl -fsSL --retry 5 --retry-all-errors "https://nodejs.org/dist/v${NODE_VERSION}/SHASUMS256.txt" | grep " ${f}\$" | sha256sum -c -; \
    tar -xJf "$f" -C /usr/local --strip-components=1 --exclude='*.md' --exclude=LICENSE --exclude=CHANGELOG.md; \
    rm -f "$f" /usr/local/share/doc/node/* 2>/dev/null || true; \
    node --version

# mise (single binary; checksum from the release's SHASUMS256.txt). CalVer: use loose versioning.
# renovate: datasource=github-releases depName=jdx/mise versioning=loose
ARG MISE_VERSION=2026.10.25
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in amd64) march=x64 ;; arm64) march=arm64 ;; *) echo "unsupported arch $arch" >&2; exit 1 ;; esac; \
    f="mise-v${MISE_VERSION}-linux-${march}"; \
    curl -fsSLO --retry 5 --retry-all-errors "https://github.com/jdx/mise/releases/download/v${MISE_VERSION}/${f}"; \
    curl -fsSL --retry 5 --retry-all-errors "https://github.com/jdx/mise/releases/download/v${MISE_VERSION}/SHASUMS256.txt" | grep " \./${f}\$" | sed 's# \./# #' | sha256sum -c -; \
    install -m 0755 "$f" /usr/local/bin/mise; \
    rm -f "$f"; \
    mise --version

# uv (checksum from the release's .sha256 sidecar)
# renovate: datasource=github-releases depName=astral-sh/uv
ARG UV_VERSION=0.13.6
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in amd64) uarch=x86_64 ;; arm64) uarch=aarch64 ;; *) echo "unsupported arch $arch" >&2; exit 1 ;; esac; \
    f="uv-${uarch}-unknown-linux-gnu.tar.gz"; \
    curl -fsSLO --retry 5 --retry-all-errors "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/${f}"; \
    curl -fsSLO --retry 5 --retry-all-errors "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/${f}.sha256"; \
    sha256sum -c "${f}.sha256"; \
    tar -xzf "$f"; \
    install -m 0755 "uv-${uarch}-unknown-linux-gnu/uv" /usr/local/bin/uv; \
    install -m 0755 "uv-${uarch}-unknown-linux-gnu/uvx" /usr/local/bin/uvx; \
    rm -rf "$f" "${f}.sha256" "uv-${uarch}-unknown-linux-gnu"; \
    uv --version

# gh (checksum from gh_<ver>_checksums.txt)
# renovate: datasource=github-releases depName=cli/cli
ARG GH_VERSION=2.103.3
RUN set -eux; \
    arch="$(dpkg --print-architecture)"; \
    f="gh_${GH_VERSION}_linux_${arch}.tar.gz"; \
    curl -fsSLO --retry 5 --retry-all-errors "https://github.com/cli/cli/releases/download/v${GH_VERSION}/${f}"; \
    curl -fsSL --retry 5 --retry-all-errors "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_checksums.txt" | grep " ${f}\$" | sha256sum -c -; \
    tar -xzf "$f"; \
    install -m 0755 "gh_${GH_VERSION}_linux_${arch}/bin/gh" /usr/local/bin/gh; \
    rm -rf "$f" "gh_${GH_VERSION}_linux_${arch}"; \
    gh --version

# User and group. UID/GID 1500 follows OpenShell's bring-your-own-container example.
RUN groupadd --gid 1500 sandbox \
    && useradd --uid 1500 --gid 1500 --home-dir /sandbox --shell /bin/bash --no-create-home sandbox \
    && install -d -o sandbox -g sandbox /sandbox /sandbox/.config /sandbox/.config/mise \
        /sandbox/.mise-cache /sandbox/.mise-cache/data /sandbox/.mise-cache/cache

ENV MISE_DATA_DIR=/sandbox/.mise-cache/data \
    MISE_CACHE_DIR=/sandbox/.mise-cache/cache \
    MISE_CONFIG_DIR=/sandbox/.config/mise \
    PATH=/sandbox/.mise-cache/data/shims:$PATH

# Dependency cooldown: package versions published less than DEP_COOLDOWN_DAYS ago are not
# installed, so a freshly published malicious release has time to be caught and yanked first.
#   npm   min-release-age (days, npm >= 11.10) in the global npmrc ($PREFIX/etc/npmrc; root-owned)
#   uv    UV_EXCLUDE_NEWER relative duration (uv >= 0.9.17; also covers mise's pipx: backend)
#   mise  MISE_MINIMUM_RELEASE_AGE for the tool versions mise resolves (exact pins bypass it)
# Per-command opt-out: npm --min-release-age=0, uv --exclude-newer false, mise --minimum-release-age 0s.
ARG DEP_COOLDOWN_DAYS=3
ENV MISE_MINIMUM_RELEASE_AGE=${DEP_COOLDOWN_DAYS}d \
    UV_EXCLUDE_NEWER="${DEP_COOLDOWN_DAYS} days"
RUN install -d /usr/local/etc \
    && printf 'min-release-age=%s\n' "${DEP_COOLDOWN_DAYS}" > /usr/local/etc/npmrc \
    && [ "$(npm config get min-release-age)" = "${DEP_COOLDOWN_DAYS}" ]

# Login shells (sandbox exec uses bash -lc) reset PATH via /etc/profile, so re-export the shims.
# hadolint ignore=SC2016
RUN printf '%s\n' \
        'export PATH="/sandbox/.mise-cache/data/shims:$PATH"' \
        'export MISE_DATA_DIR=/sandbox/.mise-cache/data MISE_CACHE_DIR=/sandbox/.mise-cache/cache MISE_CONFIG_DIR=/sandbox/.config/mise' \
        "export MISE_MINIMUM_RELEASE_AGE=${DEP_COOLDOWN_DAYS}d UV_EXCLUDE_NEWER='${DEP_COOLDOWN_DAYS} days'" \
        > /sandbox/.bashrc \
    && printf '%s\n' '[ -f ~/.bashrc ] && . ~/.bashrc' > /sandbox/.profile \
    && chown sandbox:sandbox /sandbox/.bashrc /sandbox/.profile

WORKDIR /sandbox
# Numeric so runtimes can verify a non-root user without resolving names (uid/gid 1500 = sandbox).
USER 1500:1500
