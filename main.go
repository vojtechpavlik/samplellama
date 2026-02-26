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

// sessionHolder provides thread-safe access to MCP client sessions.
// In stdio mode there is one session; in HTTP mode there may be multiple.
type sessionHolder struct {
	mu       sync.RWMutex
	sessions map[string]*mcp.ServerSession
	latest   *mcp.ServerSession
}

func newSessionHolder() *sessionHolder {
	return &sessionHolder{
		sessions: make(map[string]*mcp.ServerSession),
	}
}

func (h *sessionHolder) set(session *mcp.ServerSession) {
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

func (h *sessionHolder) get() *mcp.ServerSession {
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
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

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
	mux.HandleFunc("POST /api/chat", handleChat(holder, *defaultMaxTokens, logger))
	mux.HandleFunc("POST /api/generate", handleGenerate(holder, *defaultMaxTokens, logger))

	ollamaAddr := fmt.Sprintf(":%d", *port)
	ollamaServer := &http.Server{
		Addr:    ollamaAddr,
		Handler: mux,
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
				ModifiedAt: time.Now(),
				Size:       0,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(TagsResponse{Models: infos})
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

		params := chatToCreateMessage(req, defaultMaxTokens)

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
