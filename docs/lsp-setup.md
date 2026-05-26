# Настройка LSP

`ragota` запускает LSP-серверы **локально** (дочерние процессы через stdio) в режиме `--env local`. В режиме `--env docker` — в Docker-контейнерах.

## Режимы запуска

| | Local (`--env local`) | Docker (`--env docker`) |
|---|---|---|
| LSP-серверы | Локальные процессы | Контейнер `ragota-lsp` |
| Qdrant | Docker | Docker |
| Ollama | Хост | Хост |
| Требует | Серверы в PATH | Dockerfile.lsp autobuild |

## Установка LSP-серверов

```bash
# Go
go install golang.org/x/tools/gopls@latest

# TypeScript/JavaScript
npm install -g typescript typescript-language-server

# Python
npm install -g pyright

# Java (macOS)
brew install jdtls
# Java (Linux) — скачайте https://github.com/eclipse-jdtls/eclipse.jdt.ls, положите jdtls в PATH
```

Для Java требуется **JDK 21+**. На JDK <17 jdtls не стартует, на JDK 24+ возможны WARNING — они безопасны.

## Конфигурация

Пример секции `lsp` в `config.yaml`:

```yaml
lsp:
  - language: go
    command: gopls
  - language: java
    command: jdtls
    args: ["-data", ".ragota/jdtls-data"]
  - language: typescript
    command: typescript-language-server
    args: ["--stdio"]
  - language: python
    command: pyright-langserver
    args: ["--stdio"]
```

Пример секции `docker` для режима `--env docker`:

```yaml
docker:
  network: ragota-net
  qdrant:
    name: ragota-qdrant
    image: qdrant/qdrant:latest
    ports: ["127.0.0.1:6333:6333"]
    volumes: [".ragota/qdrant_storage:/qdrant/storage"]
  lsp:
    image: ragota-lsp:latest
    volumes: [".:/workspace"]
```

## Troubleshooting

### jdtls возвращает `null` или пустой результат на `definition/hover/references`

**Причина:** jdtls долго импортирует Maven/Gradle проект (30–120с на первый запуск).

**Решение:** Подождать. Для больших проектов увеличить таймаут: `JDTLS_READY_TIMEOUT=240` (секунды).

**Лог:** `.ragota/logs/lsp-debug.log` — ищите `LSP java: ready signal received`. Если `ready timeout` — сервер не успел проиндексировать.

### `References` возвращает 0 результатов

**Причина:** Файл лежит вне source roots Maven/Gradle (в корне рядом с `pom.xml`, а не в `src/main/java/`). jdtls в режиме Maven индексирует только `src/main/java/**` и `src/test/java/**`.

**Решение:** Переместить файл в `src/main/java/`, либо удалить `pom.xml`/`build.gradle` (jdtls перейдёт в invisible-project режим).

### `process exited: signal: killed` сразу после ответа

**Причина:** OOM — jdtls не хватает памяти.

**Решение:** Увеличить `-Xmx` (по умолчанию `-Xmx4G` для Java) или память контейнера.

### `pyright` / `typescript-language-server` не находит определения

- Убедитесь, что в корне есть `pyrightconfig.json`/`pyproject.toml` (Python) или `tsconfig.json`/`package.json` (TS).
- Установлены ли пакеты проекта (`npm install`, `pip install -e .`) — без них типы из зависимостей не разрешаются.
- Для JS с `require` добавьте `jsconfig.json` в корень проекта.

### `LSP <lang>: client dead, recreating` повторяется

Откройте `.ragota/logs/lsp-debug.log`, найдите строку `process exited: ...`. `signal=killed` = OOM. Реальные Exception'ы jdtls видны в `stderr tail`.

### WARNING от JVM (`sun.misc.Unsafe`, `incubator modules`)

Шум от JDK 24+ (Guice/Plexus/Sisu). Не влияет на работу, отфильтровывается из логов.

### `jdtls --version` пишет `Could not load Gradle version information`

Безвреден. Для Maven-проектов не нужен.
