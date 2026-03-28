package main

import (
	"fmt"
	"os"
)

// LLMProvider abstracts the LLM API call.
// All thinking, threading, tool handling stays in the Thinker.
// The provider only handles: send messages → get streaming response.
type LLMProvider interface {
	// Chat sends messages and streams the response.
	// onChunk is called for each token chunk as it arrives.
	// Returns the full response text, token usage, and any error.
	Chat(messages []Message, model string, onChunk func(string)) (string, TokenUsage, error)

	// Models returns model IDs for each tier.
	Models() map[ModelTier]string

	// Name returns the provider name for display/telemetry.
	Name() string

	// CostPer1M returns pricing per 1M tokens: (input, cached, output).
	CostPer1M() (float64, float64, float64)
}

// createProviderByName creates a provider by name, returning nil if the required API key is missing.
func createProviderByName(name string) LLMProvider {
	switch name {
	case "fireworks":
		if key := os.Getenv("FIREWORKS_API_KEY"); key != "" {
			return NewFireworksProvider(key)
		}
	case "openai":
		if key := os.Getenv("OPENAI_API_KEY"); key != "" {
			return NewOpenAIProvider(key)
		}
	case "anthropic":
		if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
			return NewAnthropicProvider(key)
		}
	case "google":
		if key := os.Getenv("GOOGLE_API_KEY"); key != "" {
			return NewGoogleProvider(key)
		}
	case "ollama":
		host := os.Getenv("OLLAMA_HOST")
		if host == "" {
			host = "http://localhost:11434"
		}
		return NewOllamaProvider(host)
	}
	return nil
}

// applyModelOverrides sets model overrides on a provider from a config map.
func applyModelOverrides(provider LLMProvider, models map[string]string) {
	if models == nil {
		return
	}
	large := models["large"]
	small := models["small"]

	switch p := provider.(type) {
	case *GoogleProvider:
		if large != "" {
			p.SetModel(large) // sets both large+small + active
		}
		if small != "" {
			p.models[ModelSmall] = small
		}
	case *OpenAICompatProvider:
		if large != "" {
			p.models[ModelLarge] = large
		}
		if small != "" {
			p.models[ModelSmall] = small
		}
	case *AnthropicProvider:
		if large != "" {
			p.models[ModelLarge] = large
		}
		if small != "" {
			p.models[ModelSmall] = small
		}
	}
}

// selectProvider picks the best available LLM provider.
// Priority: CORE_PROVIDER env → config.json provider → auto-detect from API keys.
// Model overrides: CORE_MODEL_LARGE/CORE_MODEL_SMALL env → config.json provider.models → provider defaults.
func selectProvider(cfg *Config) (LLMProvider, error) {
	var provider LLMProvider

	// 1. Explicit env var (highest priority)
	if explicit := os.Getenv("CORE_PROVIDER"); explicit != "" {
		provider = createProviderByName(explicit)
		if provider == nil {
			return nil, fmt.Errorf("provider %q requested via CORE_PROVIDER but required API key not set", explicit)
		}
	}

	// 2. Config file
	if provider == nil {
		if pc := cfg.GetProvider(); pc != nil && pc.Name != "" {
			provider = createProviderByName(pc.Name)
			// Apply config model overrides
			if provider != nil {
				applyModelOverrides(provider, pc.Models)
			}
		}
	}

	// 3. Auto-detect from API keys
	if provider == nil {
		for _, name := range []string{"fireworks", "openai", "anthropic", "google", "ollama"} {
			if p := createProviderByName(name); p != nil {
				provider = p
				break
			}
		}
	}

	if provider == nil {
		return nil, fmt.Errorf("no LLM provider configured — set FIREWORKS_API_KEY, OPENAI_API_KEY, ANTHROPIC_API_KEY, GOOGLE_API_KEY, or OLLAMA_HOST")
	}

	// 4. Env model overrides (highest priority for models)
	envModels := map[string]string{}
	if v := os.Getenv("CORE_MODEL_LARGE"); v != "" {
		envModels["large"] = v
	}
	if v := os.Getenv("CORE_MODEL_SMALL"); v != "" {
		envModels["small"] = v
	}
	if len(envModels) > 0 {
		applyModelOverrides(provider, envModels)
	}

	return provider, nil
}

// availableProviders returns all providers that have credentials configured.
func availableProviders() []LLMProvider {
	var providers []LLMProvider
	if key := os.Getenv("FIREWORKS_API_KEY"); key != "" {
		providers = append(providers, NewFireworksProvider(key))
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		providers = append(providers, NewOpenAIProvider(key))
	}
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		providers = append(providers, NewAnthropicProvider(key))
	}
	if key := os.Getenv("GOOGLE_API_KEY"); key != "" {
		providers = append(providers, NewGoogleProvider(key))
	}
	if host := os.Getenv("OLLAMA_HOST"); host != "" {
		providers = append(providers, NewOllamaProvider(host))
	}
	return providers
}

// calculateCostForProvider computes cost using the provider's pricing.
func calculateCostForProvider(provider LLMProvider, usage TokenUsage) float64 {
	inputPer1M, cachedPer1M, outputPer1M := provider.CostPer1M()
	uncached := usage.PromptTokens - usage.CachedTokens
	if uncached < 0 {
		uncached = 0
	}
	return (float64(uncached)*inputPer1M +
		float64(usage.CachedTokens)*cachedPer1M +
		float64(usage.CompletionTokens)*outputPer1M) / 1_000_000
}
