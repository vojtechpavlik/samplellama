package main

import (
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// chatToCreateMessage translates an Ollama chat request into an MCP CreateMessageParams.
func chatToCreateMessage(req ChatRequest, defaultMaxTokens int) *mcp.CreateMessageParams {
	var messages []*mcp.SamplingMessage
	var systemParts []string

	for _, msg := range req.Messages {
		if msg.Role == "system" {
			systemParts = append(systemParts, msg.Content)
			continue
		}
		role := mcp.Role("user")
		if msg.Role == "assistant" {
			role = mcp.Role("assistant")
		}
		messages = append(messages, &mcp.SamplingMessage{
			Role:    role,
			Content: &mcp.TextContent{Text: msg.Content},
		})
	}

	maxTokens := int64(defaultMaxTokens)
	if req.Options != nil && req.Options.NumPredict > 0 {
		maxTokens = int64(req.Options.NumPredict)
	}

	params := &mcp.CreateMessageParams{
		Messages:  messages,
		MaxTokens: maxTokens,
	}

	if len(systemParts) > 0 {
		params.SystemPrompt = strings.Join(systemParts, "\n")
	}

	if req.Model != "" {
		params.ModelPreferences = &mcp.ModelPreferences{
			Hints: []*mcp.ModelHint{{Name: req.Model}},
		}
	}

	if req.Options != nil && req.Options.Temperature != nil {
		params.Temperature = *req.Options.Temperature
	}

	return params
}

// generateToCreateMessage translates an Ollama generate request into an MCP CreateMessageParams.
func generateToCreateMessage(req GenerateRequest, defaultMaxTokens int) *mcp.CreateMessageParams {
	messages := []*mcp.SamplingMessage{
		{
			Role:    mcp.Role("user"),
			Content: &mcp.TextContent{Text: req.Prompt},
		},
	}

	maxTokens := int64(defaultMaxTokens)
	if req.Options != nil && req.Options.NumPredict > 0 {
		maxTokens = int64(req.Options.NumPredict)
	}

	params := &mcp.CreateMessageParams{
		Messages:  messages,
		MaxTokens: maxTokens,
	}

	if req.System != "" {
		params.SystemPrompt = req.System
	}

	if req.Model != "" {
		params.ModelPreferences = &mcp.ModelPreferences{
			Hints: []*mcp.ModelHint{{Name: req.Model}},
		}
	}

	if req.Options != nil && req.Options.Temperature != nil {
		params.Temperature = *req.Options.Temperature
	}

	return params
}

// mcpStopReason translates an MCP stop reason to an Ollama done_reason.
func mcpStopReason(stopReason string) string {
	switch stopReason {
	case "endTurn":
		return "stop"
	case "maxTokens":
		return "length"
	default:
		return "stop"
	}
}

// extractTextContent pulls the text string from an MCP content value.
func extractTextContent(content mcp.Content) string {
	switch c := content.(type) {
	case *mcp.TextContent:
		return c.Text
	}
	return ""
}
