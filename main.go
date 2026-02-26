package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
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

	logger := log.New(os.Stderr, "[samplellama] ", log.LstdFlags)
	slogger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	holder := newSessionHolder()

	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "samplellama",
		Version: version,
	}, &mcp.ServerOptions{
		Logger: slogger,
		InitializedHandler: func(ctx context.Context, req *mcp.InitializedRequest) {
			holder.set(req.Session)
			logger.Printf("MCP session initialized: %s", req.Session.ID())
			go func() {
				req.Session.Wait()
				holder.remove(req.Session.ID())
				logger.Printf("MCP session closed: %s", req.Session.ID())
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
		logger.Printf("Ollama-compatible API listening on %s", ollamaAddr)
		if err := ollamaServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("Ollama HTTP server error: %v", err)
		}
	}()

	// Start MCP transport (blocks until context is cancelled or transport closes).
	switch *mcpTransport {
	case "stdio":
		logger.Println("Starting MCP stdio transport")
		ss, err := mcpServer.Connect(ctx, &mcp.StdioTransport{}, nil)
		if err != nil {
			logger.Fatalf("MCP stdio connect error: %v", err)
		}
		holder.set(ss)
		logger.Printf("MCP stdio session: %s", ss.ID())
		ss.Wait()
	case "http":
		mcpAddr := fmt.Sprintf(":%d", *mcpPort)
		logger.Printf("Starting MCP Streamable HTTP transport on %s", mcpAddr)
		httpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
			return mcpServer
		}, nil)

		mcpHTTPServer := &http.Server{
			Addr:    mcpAddr,
			Handler: httpHandler,
		}
		go func() {
			if err := mcpHTTPServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Fatalf("MCP HTTP server error: %v", err)
			}
		}()
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := mcpHTTPServer.Shutdown(shutdownCtx); err != nil {
			logger.Printf("MCP HTTP shutdown error: %v", err)
		}
	default:
		logger.Fatalf("Unknown MCP transport: %s (use 'stdio' or 'http')", *mcpTransport)
	}

	// Graceful shutdown of Ollama server.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := ollamaServer.Shutdown(shutdownCtx); err != nil {
		logger.Printf("Ollama HTTP shutdown error: %v", err)
	}
	logger.Println("Shutdown complete")
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

func handleChat(holder *sessionHolder, defaultMaxTokens int, logger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
			return
		}

		session := holder.get()
		if session == nil {
			writeError(w, http.StatusServiceUnavailable, "MCP host not connected")
			return
		}

		params := chatToCreateMessage(req, defaultMaxTokens)

		result, err := session.CreateMessage(r.Context(), params)
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("sampling failed: %v", err))
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

func handleGenerate(holder *sessionHolder, defaultMaxTokens int, logger *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req GenerateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON: %v", err))
			return
		}

		session := holder.get()
		if session == nil {
			writeError(w, http.StatusServiceUnavailable, "MCP host not connected")
			return
		}

		params := generateToCreateMessage(req, defaultMaxTokens)

		result, err := session.CreateMessage(r.Context(), params)
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("sampling failed: %v", err))
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

func writeError(w http.ResponseWriter, status int, msg string) {
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
