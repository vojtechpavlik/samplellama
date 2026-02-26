package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const version = "samplellama-0.1.0"

// SamplingSession defines the interface for MCP sessions that support sampling.
type SamplingSession interface {
	ID() string
	CreateMessage(context.Context, *mcp.CreateMessageParams) (*mcp.CreateMessageResult, error)
}

// sessionHolder provides thread-safe access to MCP client sessions.
// In stdio mode there is one session; in HTTP mode there may be multiple.
type sessionHolder struct {
	mu       sync.RWMutex
	sessions map[string]SamplingSession
	latest   SamplingSession
}

func newSessionHolder() *sessionHolder {
	return &sessionHolder{
		sessions: make(map[string]SamplingSession),
	}
}

func (h *sessionHolder) set(session SamplingSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions[session.ID()] = session
	h.latest = session
}

func (h *sessionHolder) remove(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, sessionID)
	if h.latest != nil && h.latest.ID() == sessionID {
		h.latest = nil
		for _, s := range h.sessions {
			h.latest = s
			break
		}
	}
}

func (h *sessionHolder) get() SamplingSession {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.latest
}

func main() {
	port := flag.Int("port", 11434, "Ollama HTTP listen port")
	models := flag.String("models", "default", "Comma-separated model names to advertise")
	defaultMaxTokens := flag.Int("default-max-tokens", 4096, "Default max tokens for sampling")
	mcpTransport := flag.String("mcp-transport", "stdio", "MCP transport: stdio or http")
	mcpPort := flag.Int("mcp-port", 8081, "Port for MCP Streamable HTTP transport")
	logFile := flag.String("log-file", "", "Log to file instead of stderr")
	verbose := flag.Bool("verbose", false, "Enable verbose request logging")
	flag.Parse()

	logOutput := os.Stderr
	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to open log file: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		logOutput = f
	}
	logLevel := slog.LevelInfo
	if !*verbose {
		logLevel = slog.LevelWarn
	}
	logger := slog.New(slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: logLevel}))

	holder := newSessionHolder()

	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "samplellama",
		Version: version,
	}, &mcp.ServerOptions{
		Logger: logger,
		InitializedHandler: func(ctx context.Context, req *mcp.InitializedRequest) {
			holder.set(req.Session)
			logger.Info("MCP session initialized", "session_id", req.Session.ID())
			go func() {
				req.Session.Wait()
				holder.remove(req.Session.ID())
				logger.Info("MCP session closed", "session_id", req.Session.ID())
			}()
		},
	})

	modelList := parseModels(*models)

	// Set up Ollama HTTP server.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleHealth)
	mux.HandleFunc("HEAD /{$}", handleHealth)
	mux.HandleFunc("GET /api/version", handleVersion)
	mux.HandleFunc("GET /api/tags", handleTags(modelList))
	mux.HandleFunc("POST /api/show", handleShow(modelList))
	mux.HandleFunc("POST /api/pull", handlePull(modelList))
	mux.HandleFunc("POST /api/chat", handleChat(holder, *defaultMaxTokens, logger))
	mux.HandleFunc("POST /api/generate", handleGenerate(holder, *defaultMaxTokens, logger))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		logger.Warn("Unhandled request", "method", r.Method, "path", r.URL.Path)
		http.NotFound(w, r)
	})

	logged := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.Info("HTTP request", "method", r.Method, "path", r.URL.Path)
		mux.ServeHTTP(w, r)
	})

	ollamaAddr := fmt.Sprintf(":%d", *port)
	ollamaServer := &http.Server{
		Addr:    ollamaAddr,
		Handler: logged,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Start Ollama HTTP server in background.
	go func() {
		logger.Info("Ollama-compatible API listening", "addr", ollamaAddr)
		if err := ollamaServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("Ollama HTTP server error", "error", err)
			os.Exit(1)
		}
	}()

	// Start MCP transport (blocks until context is cancelled or transport closes).
	switch *mcpTransport {
	case "stdio":
		logger.Info("Starting MCP stdio transport")
		ss, err := mcpServer.Connect(ctx, &mcp.StdioTransport{}, nil)
		if err != nil {
			logger.Error("MCP stdio connect error", "error", err)
			os.Exit(1)
		}
		holder.set(ss)
		logger.Info("MCP stdio session", "session_id", ss.ID())
		ss.Wait()
	case "http":
		mcpAddr := fmt.Sprintf(":%d", *mcpPort)
		logger.Info("Starting MCP Streamable HTTP transport", "addr", mcpAddr)
		httpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
			return mcpServer
		}, nil)

		mcpHTTPServer := &http.Server{
			Addr:    mcpAddr,
			Handler: httpHandler,
		}
		go func() {
			if err := mcpHTTPServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("MCP HTTP server error", "error", err)
				os.Exit(1)
			}
		}()
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := mcpHTTPServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("MCP HTTP shutdown error", "error", err)
		}
	default:
		logger.Error("Unknown MCP transport", "transport", *mcpTransport)
		os.Exit(1)
	}

	// Graceful shutdown of Ollama server.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := ollamaServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("Ollama HTTP shutdown error", "error", err)
	}
	logger.Info("Shutdown complete")
}

func parseModels(s string) []string {
	parts := strings.Split(s, ",")
	var models []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			models = append(models, p)
		}
	}
	return models
}

// HTTP Handlers

func handleHealth(w http.ResponseWriter, r *http.Request) {
	fmt.Fprint(w, "Ollama is running")
}

func handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(VersionResponse{Version: version})
}

func handleTags(models []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var infos []ModelInfo
		for _, m := range models {
			infos = append(infos, ModelInfo{
				Name:       m,
				Model:      m,
				ModifiedAt: time.Now(),
				Size:       0,
				Digest:     "sha256:000000000000",
				Details: ModelDetails{
					Format: "mcp",
					Family: "mcp",
				},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(TagsResponse{Models: infos})
	}
}

func handleShow(models []string) http.HandlerFunc {
	known := make(map[string]bool, len(models))
	for _, m := range models {
		known[m] = true
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req ShowRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(ErrorResponse{Error: fmt.Sprintf("invalid JSON: %v", err)})
			return
		}
		name := req.Model
		if name == "" {
			name = req.Name
		}
		if !known[name] {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(ErrorResponse{Error: fmt.Sprintf("model %q not found", name)})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ShowResponse{
			Modelfile:  fmt.Sprintf("# Modelfile generated by samplellama\nFROM %s\n", name),
			Parameters: "",
			Template:   "{{ .Prompt }}",
			Details: ModelDetails{
				Format: "mcp",
				Family: "mcp",
			},
			ModifiedAt: time.Now(),
		})
	}
}

func handlePull(models []string) http.HandlerFunc {
	known := make(map[string]bool, len(models))
	for _, m := range models {
		known[m] = true
	}
	return func(w http.ResponseWriter, r *http.Request) {
		var req PullRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(ErrorResponse{Error: fmt.Sprintf("invalid JSON: %v", err)})
			return
		}
		name := req.Model
		if name == "" {
			name = req.Name
		}
		if !known[name] {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(ErrorResponse{Error: fmt.Sprintf("model %q not found", name)})
			return
		}
		// Model is "already available" — just report success.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(ProgressResponse{Status: "success"})
	}
}

func handleChat(holder *sessionHolder, defaultMaxTokens int, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, logger, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
			return
		}

		session := holder.get()
		if session == nil {
			writeError(w, logger, http.StatusServiceUnavailable, "MCP host not connected")
			return
		}

		logger.Info("Ollama chat request",
			"model", req.Model,
			"num_messages", len(req.Messages),
		)
		for i, msg := range req.Messages {
			logger.Info("  message", "index", i, "role", msg.Role, "content_len", len(msg.Content), "content_preview", truncate(msg.Content, 100))
		}

		params := chatToCreateMessage(req, defaultMaxTokens)

		paramsJSON, _ := json.Marshal(params)
		logger.Info("CreateMessage request", "params", string(paramsJSON))

		if len(params.Messages) == 0 {
			// Ollama sends an empty request to preload the model; respond with an empty done message.
			logger.Info("Empty message list, returning preload response")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ChatResponse{
				Model:     req.Model,
				CreatedAt: time.Now(),
				Message:   OllamaMessage{Role: "assistant", Content: ""},
				Done:      true,
			})
			return
		}

		result, err := session.CreateMessage(r.Context(), params)
		if err != nil {
			logger.Error("CreateMessage failed", "error", err)
			writeError(w, logger, http.StatusBadGateway, fmt.Sprintf("sampling failed: %v", err))
			return
		}

		text := extractTextContent(result.Content)
		now := time.Now()
		stopReason := mcpStopReason(result.StopReason)

		model := req.Model
		if model == "" {
			model = "default"
		}

		streaming := req.Stream == nil || *req.Stream // default true
		if streaming {
			w.Header().Set("Content-Type", "application/x-ndjson")
			writeNDJSON(w, ChatResponse{
				Model:     model,
				CreatedAt: now,
				Message:   OllamaMessage{Role: "assistant", Content: text},
				Done:      false,
			})
			writeNDJSON(w, ChatResponse{
				Model:      model,
				CreatedAt:  now,
				Message:    OllamaMessage{Role: "assistant", Content: ""},
				Done:       true,
				DoneReason: stopReason,
				EvalCount:  len(text),
			})
		} else {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(ChatResponse{
				Model:      model,
				CreatedAt:  now,
				Message:    OllamaMessage{Role: "assistant", Content: text},
				Done:       true,
				DoneReason: stopReason,
				EvalCount:  len(text),
			})
		}
	}
}

func handleGenerate(holder *sessionHolder, defaultMaxTokens int, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req GenerateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, logger, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
			return
		}

		session := holder.get()
		if session == nil {
			writeError(w, logger, http.StatusServiceUnavailable, "MCP host not connected")
			return
		}

		params := generateToCreateMessage(req, defaultMaxTokens)

		paramsJSON, _ := json.Marshal(params)
		logger.Info("Generate CreateMessage request", "prompt_len", len(req.Prompt), "params", string(paramsJSON))

		if len(params.Messages) == 0 || req.Prompt == "" {
			logger.Info("Empty prompt, returning preload response")
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(GenerateResponse{
				Model:     req.Model,
				CreatedAt: time.Now(),
				Response:  "",
				Done:      true,
			})
			return
		}

		result, err := session.CreateMessage(r.Context(), params)
		if err != nil {
			writeError(w, logger, http.StatusBadGateway, fmt.Sprintf("sampling failed: %v", err))
			return
		}

		text := extractTextContent(result.Content)
		now := time.Now()
		stopReason := mcpStopReason(result.StopReason)

		model := req.Model
		if model == "" {
			model = "default"
		}

		streaming := req.Stream == nil || *req.Stream
		if streaming {
			w.Header().Set("Content-Type", "application/x-ndjson")
			writeNDJSON(w, GenerateResponse{
				Model:     model,
				CreatedAt: now,
				Response:  text,
				Done:      false,
			})
			writeNDJSON(w, GenerateResponse{
				Model:      model,
				CreatedAt:  now,
				Response:   "",
				Done:       true,
				DoneReason: stopReason,
				EvalCount:  len(text),
			})
		} else {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(GenerateResponse{
				Model:      model,
				CreatedAt:  now,
				Response:   text,
				Done:       true,
				DoneReason: stopReason,
				EvalCount:  len(text),
			})
		}
	}
}

func writeError(w http.ResponseWriter, logger *slog.Logger, status int, msg string) {
	logger.Error("HTTP error", "status", status, "message", msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(ErrorResponse{Error: msg})
}

func writeNDJSON(w http.ResponseWriter, v any) {
	json.NewEncoder(w).Encode(v)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
