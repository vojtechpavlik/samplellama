package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestParseModels(t *testing.T) {
	tests := []struct {
		input    string
		expected []string
	}{
		{"llama3", []string{"llama3"}},
		{"llama3,codellama", []string{"llama3", "codellama"}},
		{"  llama3 ,  codellama  ", []string{"llama3", "codellama"}},
		{"", nil},
		{",,", nil},
		{"a,,b", []string{"a", "b"}},
	}

	for _, tt := range tests {
		got := parseModels(tt.input)
		if !reflect.DeepEqual(got, tt.expected) {
			t.Errorf("parseModels(%q) = %v, want %v", tt.input, got, tt.expected)
		}
	}
}

type mockSession struct {
	id                string
	createMessageFunc func(ctx context.Context, params *mcp.CreateMessageParams) (*mcp.CreateMessageResult, error)
}

func (m *mockSession) ID() string { return m.id }
func (m *mockSession) CreateMessage(ctx context.Context, params *mcp.CreateMessageParams) (*mcp.CreateMessageResult, error) {
	if m.createMessageFunc != nil {
		return m.createMessageFunc(ctx, params)
	}
	return &mcp.CreateMessageResult{}, nil
}

func TestSessionHolder(t *testing.T) {
	h := newSessionHolder()

	if h.get() != nil {
		t.Error("expected initial session to be nil")
	}

	s1 := &mockSession{id: "s1"}
	s2 := &mockSession{id: "s2"}

	h.set(s1)
	if h.get() != s1 {
		t.Error("expected latest session to be s1")
	}

	h.set(s2)
	if h.get() != s2 {
		t.Error("expected latest session to be s2")
	}

	h.remove("s2")
	if h.get() != s1 {
		t.Error("expected latest session to be s1 after removing s2")
	}

	h.remove("s1")
	if h.get() != nil {
		t.Error("expected latest session to be nil after removing all sessions")
	}
}

func TestHandleHealth(t *testing.T) {
	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	handleHealth(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	expected := "Ollama is running"
	if rr.Body.String() != expected {
		t.Errorf("handler returned unexpected body: got %v want %v", rr.Body.String(), expected)
	}
}

func TestHandleVersion(t *testing.T) {
	req := httptest.NewRequest("GET", "/api/version", nil)
	rr := httptest.NewRecorder()
	handleVersion(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	var resp VersionResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Version != version {
		t.Errorf("expected version %q, got %q", version, resp.Version)
	}
}

func TestHandleTags(t *testing.T) {
	models := []string{"llama3", "codellama"}
	handler := handleTags(models)

	req := httptest.NewRequest("GET", "/api/tags", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	var resp TagsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(resp.Models))
	}

	if resp.Models[0].Name != "llama3" || resp.Models[1].Name != "codellama" {
		t.Errorf("unexpected model names: %v, %v", resp.Models[0].Name, resp.Models[1].Name)
	}
}

func TestHandleChat(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("success non-streaming", func(t *testing.T) {
		h := newSessionHolder()
		mock := &mockSession{
			id: "s1",
			createMessageFunc: func(ctx context.Context, params *mcp.CreateMessageParams) (*mcp.CreateMessageResult, error) {
				return &mcp.CreateMessageResult{
					Content: &mcp.TextContent{Text: "Hello from MCP"},
					StopReason: "endTurn",
				}, nil
			},
		}
		h.set(mock)

		reqBody := `{"model": "llama3", "messages": [{"role": "user", "content": "hi"}], "stream": false}`
		req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(reqBody))
		rr := httptest.NewRecorder()

		handler := handleChat(h, 4096, logger)
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", rr.Code)
		}

		var resp ChatResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}

		if resp.Message.Content != "Hello from MCP" {
			t.Errorf("expected 'Hello from MCP', got %q", resp.Message.Content)
		}
		if resp.DoneReason != "stop" {
			t.Errorf("expected done_reason 'stop', got %q", resp.DoneReason)
		}
	})

	t.Run("no session", func(t *testing.T) {
		h := newSessionHolder()
		reqBody := `{"model": "llama3", "messages": [{"role": "user", "content": "hi"}]}`
		req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(reqBody))
		rr := httptest.NewRecorder()

		handler := handleChat(h, 4096, logger)
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("expected 503, got %d", rr.Code)
		}
	})

	t.Run("mcp error", func(t *testing.T) {
		h := newSessionHolder()
		mock := &mockSession{
			id: "s1",
			createMessageFunc: func(ctx context.Context, params *mcp.CreateMessageParams) (*mcp.CreateMessageResult, error) {
				return nil, errors.New("mcp failure")
			},
		}
		h.set(mock)

		reqBody := `{"model": "llama3", "messages": [{"role": "user", "content": "hi"}]}`
		req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(reqBody))
		rr := httptest.NewRecorder()

		handler := handleChat(h, 4096, logger)
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusBadGateway {
			t.Errorf("expected 502, got %d", rr.Code)
		}
	})
}

func TestHandleChatStreaming(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := newSessionHolder()
	mock := &mockSession{
		id: "s1",
		createMessageFunc: func(ctx context.Context, params *mcp.CreateMessageParams) (*mcp.CreateMessageResult, error) {
			return &mcp.CreateMessageResult{
				Content: &mcp.TextContent{Text: "Streaming response"},
				StopReason: "maxTokens",
			}, nil
		},
	}
	h.set(mock)

	reqBody := `{"model": "llama3", "messages": [{"role": "user", "content": "hi"}], "stream": true}`
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(reqBody))
	rr := httptest.NewRecorder()

	handler := handleChat(h, 4096, logger)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}

	// Should have two JSON objects
	dec := json.NewDecoder(rr.Body)
	var resp1 ChatResponse
	if err := dec.Decode(&resp1); err != nil {
		t.Fatal(err)
	}
	if resp1.Done {
		t.Error("expected first response to have done: false")
	}
	if resp1.Message.Content != "Streaming response" {
		t.Errorf("expected content, got %q", resp1.Message.Content)
	}

	var resp2 ChatResponse
	if err := dec.Decode(&resp2); err != nil {
		t.Fatal(err)
	}
	if !resp2.Done {
		t.Error("expected second response to have done: true")
	}
	if resp2.DoneReason != "length" {
		t.Errorf("expected done_reason 'length', got %q", resp2.DoneReason)
	}
}

func TestHandleGenerate(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := newSessionHolder()
	mock := &mockSession{
		id: "s1",
		createMessageFunc: func(ctx context.Context, params *mcp.CreateMessageParams) (*mcp.CreateMessageResult, error) {
			return &mcp.CreateMessageResult{
				Content: &mcp.TextContent{Text: "Generated text"},
				StopReason: "endTurn",
			}, nil
		},
	}
	h.set(mock)

	reqBody := `{"model": "llama3", "prompt": "tell me a joke", "stream": false}`
	req := httptest.NewRequest("POST", "/api/generate", strings.NewReader(reqBody))
	rr := httptest.NewRecorder()

	handler := handleGenerate(h, 4096, logger)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}

	var resp GenerateResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}

	if resp.Response != "Generated text" {
		t.Errorf("expected 'Generated text', got %q", resp.Response)
	}
}

func TestHandleChatMalformedJSON(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := newSessionHolder()
	reqBody := `{"model": "llama3", "messages": [` // incomplete
	req := httptest.NewRequest("POST", "/api/chat", strings.NewReader(reqBody))
	rr := httptest.NewRecorder()

	handler := handleChat(h, 4096, logger)
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rr.Code)
	}
}
