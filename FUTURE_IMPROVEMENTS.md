# Future Improvements

This document outlines potential enhancements and security improvements for Samplellama.

## Security & Resilience

- **HTTP Server Timeouts**: Add `ReadTimeout`, `WriteTimeout`, and `IdleTimeout` to the `http.Server` configurations for both the Ollama API and the MCP Streamable HTTP transport to mitigate slow-client DoS attacks.
- **CORS Support**: Implement Cross-Origin Resource Sharing (CORS) for the Ollama HTTP API to allow web-based tools (like Open WebUI) to connect to Samplellama.
- **Model Validation**: Validate that the `model` field in `/api/chat` and `/api/generate` requests matches one of the models advertised via the `-models` flag.
- **Authentication**: Consider adding a simple API key or token-based authentication for the Ollama API if it's exposed on non-localhost interfaces.

## Feature Completeness

- **MCP Content Types**: Enhance `extractTextContent` to support non-text MCP content types (e.g., `ImageContent` or `ResourceContent`) by converting them to appropriate textual representations or handling them according to Ollama's multi-modal capabilities.
- **Incremental Streaming**: Since MCP sampling returns the full response at once, Samplellama currently sends the entire response in a single NDJSON chunk. Implementing "simulated" streaming (splitting the response into smaller chunks) could provide a more native Ollama experience for clients.
- **Additional Ollama Fields**: Populate additional fields in the Ollama response, such as `total_duration`, `load_duration`, `prompt_eval_count`, and `eval_duration`. Basic timing measurements can be used for durations.

## Observability & Developer Experience

- **Log Level Control**: Add a command-line flag (e.g., `-log-level`) to control the verbosity of the `slog` output (e.g., `debug`, `info`, `warn`, `error`).
- **Structured Logging for Requests**: Log incoming Ollama requests and outgoing MCP calls with more detailed context for better auditability.
- **Metrics**: Add an `/metrics` endpoint (e.g., Prometheus format) to track request counts, latency, and session health.
