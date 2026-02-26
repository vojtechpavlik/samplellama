package main

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestChatToCreateMessage(t *testing.T) {
	req := ChatRequest{
		Model: "llama3",
		Messages: []OllamaMessage{
			{Role: "system", Content: "You are helpful."},
			{Role: "user", Content: "Hello"},
			{Role: "assistant", Content: "Hi there!"},
			{Role: "user", Content: "How are you?"},
		},
	}

	result := chatToCreateMessage(req, 4096)

	if len(result.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(result.Messages))
	}

	if result.SystemPrompt != "You are helpful." {
		t.Errorf("expected system prompt 'You are helpful.', got %q", result.SystemPrompt)
	}

	if result.ModelPreferences == nil || len(result.ModelPreferences.Hints) != 1 || result.ModelPreferences.Hints[0].Name != "llama3" {
		t.Error("expected model hint 'llama3'")
	}

	if result.MaxTokens != 4096 {
		t.Errorf("expected max tokens 4096, got %d", result.MaxTokens)
	}

	if result.Messages[0].Role != mcp.Role("user") {
		t.Errorf("expected first message role 'user', got %q", result.Messages[0].Role)
	}
	if result.Messages[1].Role != mcp.Role("assistant") {
		t.Errorf("expected second message role 'assistant', got %q", result.Messages[1].Role)
	}
	if result.Messages[2].Role != mcp.Role("user") {
		t.Errorf("expected third message role 'user', got %q", result.Messages[2].Role)
	}
}

func TestChatToCreateMessageWithOptions(t *testing.T) {
	temp := 0.7
	req := ChatRequest{
		Model: "codellama",
		Messages: []OllamaMessage{
			{Role: "user", Content: "Write code"},
		},
		Options: &Options{
			NumPredict:  2048,
			Temperature: &temp,
		},
	}

	result := chatToCreateMessage(req, 4096)

	if result.MaxTokens != 2048 {
		t.Errorf("expected max tokens 2048, got %d", result.MaxTokens)
	}
	if result.Temperature != 0.7 {
		t.Errorf("expected temperature 0.7, got %f", result.Temperature)
	}
}

func TestChatToCreateMessageMultipleSystemMessages(t *testing.T) {
	req := ChatRequest{
		Messages: []OllamaMessage{
			{Role: "system", Content: "First system."},
			{Role: "system", Content: "Second system."},
			{Role: "user", Content: "Hello"},
		},
	}

	result := chatToCreateMessage(req, 4096)

	if result.SystemPrompt != "First system.\nSecond system." {
		t.Errorf("expected concatenated system prompt, got %q", result.SystemPrompt)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result.Messages))
	}
}

func TestChatToCreateMessageNoSystem(t *testing.T) {
	req := ChatRequest{
		Messages: []OllamaMessage{
			{Role: "user", Content: "Hello"},
		},
	}

	result := chatToCreateMessage(req, 4096)

	if result.SystemPrompt != "" {
		t.Errorf("expected empty system prompt, got %q", result.SystemPrompt)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result.Messages))
	}
}

func TestGenerateToCreateMessage(t *testing.T) {
	req := GenerateRequest{
		Model:  "llama3",
		Prompt: "Tell me a joke",
		System: "You are funny.",
	}

	result := generateToCreateMessage(req, 4096)

	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result.Messages))
	}

	if result.Messages[0].Role != mcp.Role("user") {
		t.Errorf("expected role 'user', got %q", result.Messages[0].Role)
	}

	tc, ok := result.Messages[0].Content.(*mcp.TextContent)
	if !ok {
		t.Fatal("expected *TextContent")
	}
	if tc.Text != "Tell me a joke" {
		t.Errorf("expected prompt text, got %q", tc.Text)
	}

	if result.SystemPrompt != "You are funny." {
		t.Errorf("expected system prompt, got %q", result.SystemPrompt)
	}

	if result.ModelPreferences == nil || result.ModelPreferences.Hints[0].Name != "llama3" {
		t.Error("expected model hint 'llama3'")
	}
}

func TestGenerateToCreateMessageWithOptions(t *testing.T) {
	temp := 0.5
	req := GenerateRequest{
		Prompt: "Hello",
		Options: &Options{
			NumPredict:  512,
			Temperature: &temp,
		},
	}

	result := generateToCreateMessage(req, 4096)

	if result.MaxTokens != 512 {
		t.Errorf("expected max tokens 512, got %d", result.MaxTokens)
	}
	if result.Temperature != 0.5 {
		t.Errorf("expected temperature 0.5, got %f", result.Temperature)
	}
}

func TestMcpStopReason(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"endTurn", "stop"},
		{"maxTokens", "length"},
		{"unknown", "stop"},
		{"", "stop"},
	}

	for _, tt := range tests {
		got := mcpStopReason(tt.input)
		if got != tt.expected {
			t.Errorf("mcpStopReason(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestExtractTextContent(t *testing.T) {
	// *TextContent
	tc := &mcp.TextContent{Text: "hello"}
	if got := extractTextContent(tc); got != "hello" {
		t.Errorf("expected 'hello', got %q", got)
	}

	// nil
	if got := extractTextContent(nil); got != "" {
		t.Errorf("expected empty string for nil, got %q", got)
	}

	// non-text content
	ic := &mcp.ImageContent{Data: []byte("base64"), MIMEType: "image/png"}
	if got := extractTextContent(ic); got != "" {
		t.Errorf("expected empty string for non-text content, got %q", got)
	}
}

func TestChatToCreateMessageUnknownRole(t *testing.T) {
	req := ChatRequest{
		Messages: []OllamaMessage{
			{Role: "unknown", Content: "Hello"},
		},
	}

	result := chatToCreateMessage(req, 4096)

	if len(result.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(result.Messages))
	}
	if result.Messages[0].Role != mcp.Role("user") {
		t.Errorf("expected unknown role to map to 'user', got %q", result.Messages[0].Role)
	}
}
