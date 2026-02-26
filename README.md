# Samplellama

Samplellama is a bridge that lets tools built for the
[Ollama API](https://github.com/ollama/ollama/blob/main/docs/api.md) use any
LLM accessible through an [MCP](https://modelcontextprotocol.io/) host.

It runs as an MCP server and simultaneously exposes an Ollama-compatible HTTP
API. When an Ollama client sends a request, samplellama translates it into an
MCP sampling call, forwards it to the connected MCP host, and returns the
result in Ollama format.

```text
Ollama client  ──▶  samplellama  ──▶  MCP host  ──▶  LLM
               HTTP :11434       stdio / HTTP
```

## Building

Requires Go 1.23 or later.

```bash
go build -o samplellama
```

## Running

### stdio mode (default)

In stdio mode the MCP host launches samplellama as a subprocess and
communicates over stdin/stdout. Configure samplellama as an MCP server in
your host's settings. For example, in an MCP host config file:

```json
{
  "mcpServers": {
    "samplellama": {
      "command": "/path/to/samplellama",
      "args": ["-port", "11434", "-models", "llama3,codellama"]
    }
  }
}
```

Once the host connects, any Ollama-compatible tool can target
`http://localhost:11434`.

### Streamable HTTP mode

In HTTP mode samplellama runs a second HTTP server for MCP, allowing the
host to connect over the network:

```bash
./samplellama -mcp-transport http -mcp-port 8081
```

The MCP host connects to `http://localhost:8081/mcp` using the Streamable
HTTP transport.

## Command-Line Flags

| Flag                  | Default   | Description                            |
|-----------------------|-----------|----------------------------------------|
| `-port`               | `11434`   | Ollama HTTP listen port                |
| `-models`             | `default` | Comma-separated model names            |
| `-default-max-tokens` | `4096`    | Default max tokens for sampling        |
| `-mcp-transport`      | `stdio`   | MCP transport: `stdio` or `http`       |
| `-mcp-port`           | `8081`    | Port for MCP Streamable HTTP transport |

## Supported Ollama Endpoints

| Method | Path            | Description                          |
|--------|-----------------|--------------------------------------|
| GET    | `/`             | Health check (`Ollama is running`)   |
| GET    | `/api/version`  | Returns the samplellama version      |
| GET    | `/api/tags`     | Lists the advertised model names     |
| POST   | `/api/chat`     | Chat completion (multi-turn)         |
| POST   | `/api/generate` | Text generation (single prompt)      |

### Chat example

```bash
curl http://localhost:11434/api/chat -d '{
  "model": "llama3",
  "messages": [
    {"role": "system", "content": "You are helpful."},
    {"role": "user", "content": "Hello!"}
  ]
}'
```

### Generate example

```bash
curl http://localhost:11434/api/generate -d '{
  "model": "llama3",
  "prompt": "Tell me a joke",
  "system": "You are funny."
}'
```

### Disabling streaming

Both endpoints stream by default (NDJSON). To get a single JSON response,
set `"stream": false`:

```bash
curl http://localhost:11434/api/chat -d '{
  "model": "llama3",
  "messages": [{"role": "user", "content": "Hi"}],
  "stream": false
}'
```

### Supported options

The `options` object accepts:

| Field         | Type  | Description                          |
|---------------|-------|--------------------------------------|
| `num_predict` | int   | Maximum number of tokens to generate |
| `temperature` | float | Sampling temperature                 |

## How It Works

1. An Ollama client sends a `/api/chat` or `/api/generate` request.
2. Samplellama translates it into an MCP `sampling/createMessage` request:
   - Chat messages are split into system prompt + user/assistant messages.
   - The model name is forwarded as an MCP model hint.
   - `num_predict` and `temperature` are mapped to MCP parameters.
3. The request is sent to the connected MCP host session.
4. The MCP host forwards it to its configured LLM and returns the result.
5. Samplellama extracts the text content and maps the MCP stop reason
   (`endTurn` → `stop`, `maxTokens` → `length`).
6. The response is returned in Ollama format.

## Testing

```bash
go test ./...
```

## License

See repository for license information.
