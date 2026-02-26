# Samplellama Design

## Overview

Samplellama is a bridge daemon that connects the MCP (Model Context Protocol) Sampling
API with the Ollama HTTP API. It exposes two interfaces simultaneously:

1. An **Ollama-compatible HTTP API** that tools expecting Ollama can connect to.
2. An **MCP server** that an MCP host connects to (via stdio or Streamable HTTP).

When a tool sends a chat or generation request to the Ollama API, samplellama
translates it into an MCP `sampling/createMessage` call and forwards it to
the connected MCP host. The host routes the request to whatever LLM it is
configured with (Claude, GPT, a local model, etc.) and returns the result.
Samplellama translates the response back into Ollama format and returns it
to the calling tool.

```text
┌────────────┐  Ollama API  ┌─────────────┐  MCP Sampling  ┌──────────┐
│ Ollama Tool │ ──────────▶ │ samplellama │ ────────────▶  │ MCP Host │
│ (e.g. CLI)  │ ◀────────── │             │ ◀────────────  │  + LLM   │
└────────────┘  HTTP :11434 └─────────────┘ stdio / HTTP   └──────────┘
```

## Source Layout

| File                | Purpose                                               |
|---------------------|-------------------------------------------------------|
| `main.go`           | Entry point, flag parsing, HTTP server, MCP transport |
| `ollama.go`         | Ollama API request/response type definitions          |
| `translate.go`      | Ollama ↔ MCP request/response translation functions   |
| `translate_test.go` | Unit tests for translation logic                      |

## Architecture

### Session Management

`sessionHolder` maintains thread-safe access to MCP client sessions.

- In **stdio** mode a single session is created when the MCP host connects.
- In **HTTP** mode multiple sessions can be active; the most recently
  connected session is used for sampling.
- A background goroutine monitors each session and removes it from the
  holder when the session closes.

### Ollama HTTP Server

A standard `net/http` server exposes these endpoints:

| Method | Path            | Handler          | Description                  |
|--------|-----------------|------------------|------------------------------|
| GET    | `/`             | `handleHealth`   | Returns `"Ollama is running"`|
| HEAD   | `/`             | `handleHealth`   | Health check (head)          |
| GET    | `/api/version`  | `handleVersion`  | Returns samplellama version  |
| GET    | `/api/tags`     | `handleTags`     | Lists advertised model names |
| POST   | `/api/chat`     | `handleChat`     | Chat completion              |
| POST   | `/api/generate` | `handleGenerate` | Text generation              |

Both `/api/chat` and `/api/generate` follow the same flow:

1. Decode the Ollama JSON request body.
2. Obtain the current MCP session (503 if none connected).
3. Translate the request to `mcp.CreateMessageParams`.
4. Call `session.CreateMessage()`.
5. Extract text content and stop reason from the MCP result.
6. Return the response as NDJSON stream (default) or single JSON object.

### MCP Transport

Samplellama runs as an MCP **server** (not client). The MCP host is the
client that connects to it.

- **stdio** (default): The MCP server binds to stdin/stdout. The host
  launches samplellama as a subprocess. A single session is established
  and the process blocks until the session ends.
- **Streamable HTTP**: The MCP server listens on a configurable port.
  The host connects over HTTP, allowing multiple concurrent sessions.

### Request Translation

`translate.go` converts between the two API formats:

**`chatToCreateMessage`** — Ollama chat → MCP:

- System-role messages are extracted and concatenated into `SystemPrompt`.
- User/assistant messages become `SamplingMessage` entries.
- `options.num_predict` maps to `MaxTokens` (falls back to the default).
- `options.temperature` maps to `Temperature`.
- The `model` field is passed as a `ModelHint`.

**`generateToCreateMessage`** — Ollama generate → MCP:

- The prompt becomes a single user `SamplingMessage`.
- The `system` field maps to `SystemPrompt`.
- Options and model are handled identically to chat.

**`mcpStopReason`** — MCP stop reason → Ollama `done_reason`:

- `"endTurn"` → `"stop"`
- `"maxTokens"` → `"length"`
- anything else → `"stop"`

### Streaming

Streaming is **on by default** (matching Ollama behavior). When streaming:

- Content-Type is `application/x-ndjson`.
- Two JSON lines are sent: the content chunk (`done: false`), then the
  final marker (`done: true`) with the stop reason and token count.

When the client sends `"stream": false`, a single JSON response is returned.

Note: because MCP sampling returns the full response at once, samplellama
does not produce incremental token-by-token streaming. The "stream" mode
emits the complete text in one chunk followed by a done marker, which is
compatible with clients that expect the NDJSON framing.

### Error Handling

| HTTP Status | Condition                         |
|-------------|-----------------------------------|
| 400         | Malformed JSON in request body    |
| 502         | MCP `CreateMessage` call failed   |
| 503         | No MCP host session is connected  |

Errors are returned as `{"error": "..."}`.

### Graceful Shutdown

On SIGINT or SIGTERM:

1. The MCP transport is torn down (stdio session ends or HTTP server shuts
   down with a 5-second timeout).
2. The Ollama HTTP server shuts down with a 5-second timeout.

## Dependencies

- [`modelcontextprotocol/go-sdk`][go-sdk] v1.3.0 —
  Official MCP Go SDK for server, session, and transport types.
- Go standard library for HTTP serving, JSON encoding, flag parsing, and
  signal handling.

[go-sdk]: https://github.com/modelcontextprotocol/go-sdk
