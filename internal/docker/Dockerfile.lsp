# Multi-stage Dockerfile для LSP-контейнера ragota-lsp
# Поддерживает: Go, TypeScript, JavaScript, Python, Java

# =============================================================================
# Stage 1: Go + gopls
# =============================================================================
FROM golang:1.26 AS go-stage
RUN go install golang.org/x/tools/gopls@latest

# =============================================================================
# Stage 2: Node.js + TypeScript/JavaScript LSP
# =============================================================================
FROM node:22 AS node-stage
RUN npm install -g typescript typescript-language-server

# =============================================================================
# Stage 3: Python + pyright
# =============================================================================
FROM node:22 AS python-stage
RUN npm install -g pyright

# =============================================================================
# Stage 4: Java + jdtls
# =============================================================================
FROM eclipse-temurin:21-jdk AS java-stage

# Устанавливаем зависимости для jdtls
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    && rm -rf /var/lib/apt/lists/*

# Скачиваем jdtls (последняя стабильная версия)
RUN mkdir -p /opt/jdtls && \
    curl -fsSL https://www.eclipse.org/downloads/download.php?file=/jdtls/snapshots/jdt-language-server-latest.tar.gz -o /tmp/jdtls.tar.gz && \
    tar -xzf /tmp/jdtls.tar.gz -C /opt/jdtls && \
    rm /tmp/jdtls.tar.gz

# Создаём скрипт-обёртку для jdtls
RUN echo '#!/bin/bash\nexec /opt/jdtls/bin/jdtls "$@"' > /usr/local/bin/jdtls && \
    chmod +x /usr/local/bin/jdtls

# =============================================================================
# Stage 5: Final image на базе Ubuntu
# =============================================================================
FROM ubuntu:24.04

# Устанавливаем базовые утилиты, Java Runtime и Python
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    openjdk-21-jre-headless \
    python3 \
    python3-pip \
    python-is-python3 \
    && rm -rf /var/lib/apt/lists/*

# Копируем Go инструменты и рантайм
COPY --from=go-stage /usr/local/go /usr/local/go
RUN ln -s /usr/local/go/bin/go /usr/local/bin/go && \
    ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt
COPY --from=go-stage /go/bin/gopls /usr/local/bin/gopls

# Копируем Node.js рантайм
COPY --from=node-stage /usr/local/bin/node /usr/local/bin/node
# Добавляем ссылку для совместимости (иногда ищут в /usr/bin)
RUN ln -s /usr/local/bin/node /usr/bin/node

# Копируем все глобальные node_modules
COPY --from=node-stage /usr/local/lib/node_modules /usr/local/lib/node_modules
COPY --from=python-stage /usr/local/lib/node_modules/pyright /usr/local/lib/node_modules/pyright

# Создаем скрипты-обертки для надежного запуска через node
# Используем /usr/bin/env node для переносимости, так как мы сделали ссылку /usr/bin/node
RUN echo '#!/bin/sh\nexec node /usr/local/lib/node_modules/typescript-language-server/lib/cli.mjs "$@"' > /usr/local/bin/typescript-language-server && \
    chmod +x /usr/local/bin/typescript-language-server

RUN echo '#!/bin/sh\nexec node /usr/local/lib/node_modules/pyright/langserver.index.js "$@"' > /usr/local/bin/pyright-langserver && \
    chmod +x /usr/local/bin/pyright-langserver

RUN echo '#!/bin/sh\nexec node /usr/local/lib/node_modules/pyright/index.js "$@"' > /usr/local/bin/pyright && \
    chmod +x /usr/local/bin/pyright

RUN ln -s /usr/local/lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm && \
    ln -s /usr/local/lib/node_modules/typescript/bin/tsc /usr/local/bin/tsc

# Копируем Java jdtls
COPY --from=java-stage /opt/jdtls /opt/jdtls
RUN ln -s /opt/jdtls/bin/jdtls /usr/local/bin/jdtls

# Настраиваем PATH
ENV PATH="/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/go/bin:/root/go/bin"
ENV GOPATH="/go"
ENV GOROOT="/usr/local/go"

# Рабочая директория
WORKDIR /workspace

# Команда по умолчанию - tail для долгой работы контейнера
CMD ["tail", "-f", "/dev/null"]
