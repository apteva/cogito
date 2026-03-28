package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// GoogleModel holds metadata for a Gemini model.
type GoogleModel struct {
	ID              string
	InputPer1M      float64
	CachedPer1M     float64
	OutputPer1M     float64
	MaxOutputTokens int
}

// Available Gemini models (March 2026)
var geminiModels = map[string]GoogleModel{
	// Gemini 3.1 series
	"gemini-3.1-pro-preview": {
		ID: "gemini-3.1-pro-preview", InputPer1M: 2.00, CachedPer1M: 0.20, OutputPer1M: 12.00, MaxOutputTokens: 65536,
	},
	"gemini-3.1-flash-lite-preview": {
		ID: "gemini-3.1-flash-lite-preview", InputPer1M: 0.25, CachedPer1M: 0.025, OutputPer1M: 1.50, MaxOutputTokens: 65536,
	},
	// Gemini 3 series
	"gemini-3-flash-preview": {
		ID: "gemini-3-flash-preview", InputPer1M: 0.50, CachedPer1M: 0.05, OutputPer1M: 3.00, MaxOutputTokens: 65536,
	},
	// Gemini 2.5 series
	"gemini-2.5-pro": {
		ID: "gemini-2.5-pro", InputPer1M: 1.00, CachedPer1M: 0.10, OutputPer1M: 10.00, MaxOutputTokens: 65536,
	},
	"gemini-2.5-flash": {
		ID: "gemini-2.5-flash", InputPer1M: 0.30, CachedPer1M: 0.03, OutputPer1M: 2.50, MaxOutputTokens: 65536,
	},
}

// GeminiModelOrder defines the cycle order for model switching in the TUI.
var GeminiModelOrder = []string{
	"gemini-3.1-pro-preview",
	"gemini-3-flash-preview",
	"gemini-3.1-flash-lite-preview",
	"gemini-2.5-pro",
	"gemini-2.5-flash",
}

type GoogleProvider struct {
	apiKey       string
	models       map[ModelTier]string
	activeModel  string // current model ID for cost tracking
}

func NewGoogleProvider(apiKey string) LLMProvider {
	return &GoogleProvider{
		apiKey: apiKey,
		models: map[ModelTier]string{
			ModelLarge: "gemini-3.1-pro-preview",
			ModelSmall: "gemini-3-flash-preview",
		},
		activeModel: "gemini-3.1-pro-preview",
	}
}

func (p *GoogleProvider) Name() string                 { return "google" }
func (p *GoogleProvider) Models() map[ModelTier]string  { return p.models }

func (p *GoogleProvider) CostPer1M() (float64, float64, float64) {
	if m, ok := geminiModels[p.activeModel]; ok {
		return m.InputPer1M, m.CachedPer1M, m.OutputPer1M
	}
	// Fallback to gemini-3.1-pro-preview pricing
	return 2.00, 0.20, 12.00
}

// SetModel updates the active model. Called from TUI model cycling.
func (p *GoogleProvider) SetModel(modelID string) {
	if _, ok := geminiModels[modelID]; ok {
		p.activeModel = modelID
		p.models[ModelLarge] = modelID
		p.models[ModelSmall] = modelID
	}
}

// ActiveModel returns the current model ID.
func (p *GoogleProvider) ActiveModel() string { return p.activeModel }

// AvailableModels returns all supported Gemini model IDs in cycle order.
func (p *GoogleProvider) AvailableModels() []string { return GeminiModelOrder }

// Gemini API request format
type geminiRequest struct {
	Contents         []geminiContent        `json:"contents"`
	SystemInstruction *geminiContent        `json:"systemInstruction,omitempty"`
	GenerationConfig  map[string]any        `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text       string          `json:"text,omitempty"`
	InlineData *geminiInline   `json:"inlineData,omitempty"`
}

type geminiInline struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// Gemini streaming response
type geminiStreamResponse struct {
	Candidates []struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
}

func (p *GoogleProvider) Chat(messages []Message, model string, onChunk func(string)) (string, TokenUsage, error) {
	// Track active model for cost calculation
	p.activeModel = model

	// Convert messages to Gemini format
	// Gemini requires: one optional systemInstruction, then strictly alternating user/model turns.
	// We merge consecutive same-role messages and fold extra system messages into user context.
	var systemParts []geminiPart
	var contents []geminiContent

	for _, m := range messages {
		if m.Role == "system" {
			// First system message becomes systemInstruction; subsequent ones merge into next user turn
			if len(contents) == 0 && len(systemParts) == 0 {
				systemParts = append(systemParts, geminiPart{Text: m.TextContent()})
			} else {
				// Fold into a user message as context
				text := "[system context] " + m.TextContent()
				if len(contents) > 0 && contents[len(contents)-1].Role == "user" {
					// Merge into existing user turn
					contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, geminiPart{Text: text})
				} else {
					contents = append(contents, geminiContent{
						Role:  "user",
						Parts: []geminiPart{{Text: text}},
					})
				}
			}
			continue
		}
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}

		var parts []geminiPart
		if m.HasParts() {
			parts = toGeminiParts(m.Parts)
		} else {
			parts = []geminiPart{{Text: m.Content}}
		}

		// Merge consecutive same-role messages (Gemini requirement)
		if len(contents) > 0 && contents[len(contents)-1].Role == role {
			contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, parts...)
		} else {
			contents = append(contents, geminiContent{Role: role, Parts: parts})
		}
	}

	if len(contents) == 0 {
		contents = append(contents, geminiContent{
			Role:  "user",
			Parts: []geminiPart{{Text: "Begin."}},
		})
	}

	// Ensure conversation starts with user (Gemini requirement)
	if contents[0].Role == "model" {
		contents = append([]geminiContent{{Role: "user", Parts: []geminiPart{{Text: "Begin."}}}}, contents...)
	}
	// Ensure conversation ends with user (Gemini requirement)
	if contents[len(contents)-1].Role == "model" {
		contents = append(contents, geminiContent{Role: "user", Parts: []geminiPart{{Text: "Continue."}}})
	}

	var systemContent *geminiContent
	if len(systemParts) > 0 {
		systemContent = &geminiContent{Parts: systemParts}
	}

	// Log turn structure for debugging
	var turnLog strings.Builder
	for i, c := range contents {
		partLen := 0
		for _, p := range c.Parts {
			partLen += len(p.Text)
		}
		if i > 0 {
			turnLog.WriteString(", ")
		}
		turnLog.WriteString(fmt.Sprintf("%s(%d)", c.Role, partLen))
	}
	logMsg("GEMINI", fmt.Sprintf("model=%s msgs=%d contents=%d sys=%d turns=[%s]", model, len(messages), len(contents), len(systemParts), turnLog.String()))

	// Use model-specific max output tokens
	maxTokens := 65536
	if m, ok := geminiModels[model]; ok {
		maxTokens = m.MaxOutputTokens
	}

	reqBody := geminiRequest{
		Contents:          contents,
		SystemInstruction: systemContent,
		GenerationConfig: map[string]any{
			"maxOutputTokens": maxTokens,
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", TokenUsage{}, err
	}

	url := fmt.Sprintf("https://generativelanguage.googleapis.com/v1beta/models/%s:streamGenerateContent?alt=sse&key=%s", model, p.apiKey)

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return "", TokenUsage{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", TokenUsage{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		logMsg("GEMINI", fmt.Sprintf("ERROR %d: %s", resp.StatusCode, string(respBody)))
		return "", TokenUsage{}, fmt.Errorf("Gemini API error %d: %s", resp.StatusCode, string(respBody))
	}
	logMsg("GEMINI", "streaming response started")

	var full strings.Builder
	var usage TokenUsage
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")

		var event geminiStreamResponse
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}

		for _, candidate := range event.Candidates {
			for _, part := range candidate.Content.Parts {
				if part.Text != "" {
					full.WriteString(part.Text)
					if onChunk != nil {
						onChunk(part.Text)
					}
				}
			}
		}

		if event.UsageMetadata != nil {
			usage.PromptTokens = event.UsageMetadata.PromptTokenCount
			usage.CompletionTokens = event.UsageMetadata.CandidatesTokenCount
			usage.CachedTokens = event.UsageMetadata.CachedContentTokenCount
		}
	}

	response := full.String()
	preview := response
	if len(preview) > 200 {
		preview = preview[:200] + "..."
	}
	logMsg("GEMINI", fmt.Sprintf("done tokens_in=%d tokens_out=%d len=%d response=%q", usage.PromptTokens, usage.CompletionTokens, len(response), preview))
	return response, usage, nil
}

// toGeminiParts converts our ContentParts to Gemini parts.
func toGeminiParts(parts []ContentPart) []geminiPart {
	var out []geminiPart
	for _, p := range parts {
		switch p.Type {
		case "text":
			out = append(out, geminiPart{Text: p.Text})
		case "image_url":
			if p.ImageURL != nil && strings.HasPrefix(p.ImageURL.URL, "data:") {
				// data:image/png;base64,iVBOR... → extract mime and data
				segments := strings.SplitN(p.ImageURL.URL, ",", 2)
				mimeType := strings.TrimPrefix(strings.TrimSuffix(segments[0], ";base64"), "data:")
				data := ""
				if len(segments) > 1 {
					data = segments[1]
				}
				out = append(out, geminiPart{InlineData: &geminiInline{MimeType: mimeType, Data: data}})
			} else if p.ImageURL != nil {
				// URL images — Gemini needs inlineData, so we note it as text
				// In production, you'd fetch and base64-encode the image
				out = append(out, geminiPart{Text: fmt.Sprintf("[image: %s]", p.ImageURL.URL)})
			}
		case "input_audio":
			if p.InputAudio != nil {
				mimeType := "audio/wav"
				if p.InputAudio.Format == "mp3" {
					mimeType = "audio/mp3"
				}
				out = append(out, geminiPart{InlineData: &geminiInline{MimeType: mimeType, Data: p.InputAudio.Data}})
			}
		}
	}
	if len(out) == 0 {
		out = append(out, geminiPart{Text: ""})
	}
	return out
}
