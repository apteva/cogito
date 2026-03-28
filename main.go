package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/joho/godotenv"
)

// ContentPart represents a multimodal content block (OpenAI Chat Completions format).
type ContentPart struct {
	Type       string      `json:"type"`                  // "text", "image_url", "input_audio"
	Text       string      `json:"text,omitempty"`        // type=text
	ImageURL   *ImageURL   `json:"image_url,omitempty"`   // type=image_url
	InputAudio *InputAudio `json:"input_audio,omitempty"` // type=input_audio
}

type ImageURL struct {
	URL    string `json:"url"`              // https:// or data:image/...;base64,...
	Detail string `json:"detail,omitempty"` // "low", "high", "auto"
}

type InputAudio struct {
	Data   string `json:"data"`   // base64
	Format string `json:"format"` // "wav", "mp3"
}

type Message struct {
	Role    string        `json:"role"`
	Content string        `json:"content"`
	Parts   []ContentPart `json:"parts,omitempty"` // when set, providers use this instead of Content
}

// TextContent returns the text content of a message, whether plain Content or from Parts.
func (m Message) TextContent() string {
	if len(m.Parts) == 0 {
		return m.Content
	}
	for _, p := range m.Parts {
		if p.Type == "text" {
			return p.Text
		}
	}
	return m.Content
}

// HasParts returns true if this message has multimodal content.
func (m Message) HasParts() bool {
	return len(m.Parts) > 0
}

type Request struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
}

type Delta struct {
	Content string `json:"content"`
}

type StreamChoice struct {
	Delta Delta `json:"delta"`
}

type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type Usage struct {
	PromptTokens        int                  `json:"prompt_tokens"`
	CompletionTokens    int                  `json:"completion_tokens"`
	TotalTokens         int                  `json:"total_tokens"`
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

type StreamEvent struct {
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"`
}

func main() {
	godotenv.Load()
	initLogger()

	cfg := NewConfig()

	provider, err := selectProvider(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	models := provider.Models()
	fmt.Fprintf(os.Stderr, "LLM provider: %s (large=%s, small=%s)\n", provider.Name(), models[ModelLarge], models[ModelSmall])

	// Keep apiKey for backward compat (memory embeddings use it)
	apiKey := os.Getenv("FIREWORKS_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("OPENAI_API_KEY")
	}

	thinker := NewThinker(apiKey, provider, cfg)
	go thinker.Run()

	apiPort := os.Getenv("API_PORT")
	if apiPort == "" {
		apiPort = "3210"
	}
	go startAPI(thinker, ":"+apiPort)

	// Check for --headless flag or NO_TUI env var
	headless := os.Getenv("NO_TUI") != ""
	for _, arg := range os.Args[1:] {
		if arg == "--headless" {
			headless = true
		}
	}

	if headless {
		fmt.Fprintf(os.Stderr, "apteva-core running headless (API on :%s)\n", apiPort)
		<-thinker.quit
	} else {
		p := tea.NewProgram(newModel(thinker), tea.WithAltScreen())
		if _, err := p.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
	}
}
