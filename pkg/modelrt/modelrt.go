// Package modelrt implements the ModelRuntime: provider registry, model
// catalog, auth resolution, thinking-level clamping, and the StreamFn
// contract over raw HTTP/SSE transports (core-spec §4.3, D10).
package modelrt

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"fuji/pkg/config"
	"fuji/pkg/messages"
)

// API identifies a provider wire protocol family.
type API string

// API families.
const (
	APIAnthropic         API = "anthropic"
	APIOpenAICompletions API = "openai-completions"
	APIOpenAIResponses   API = "openai-responses"
)

// Provider is a named provider configuration.
type Provider struct {
	ID      string
	API     API
	BaseURL string
	APIKey  string
	Models  []Model
}

// Model describes a model's capabilities.
type Model struct {
	ID              string
	ProviderID      string
	API             API
	ContextWindow   int
	MaxOutput       int
	ThinkingLevels  []string // nil = no thinking support
	DefaultThinking string
}

// SupportsThinking reports whether level is valid for this model.
func (m Model) SupportsThinking(level string) bool {
	for _, l := range m.ThinkingLevels {
		if l == level {
			return true
		}
	}
	return false
}

// ClampThinkingLevel returns the requested level if supported, else the
// model's default (or "off" when unsupported).
func (m Model) ClampThinkingLevel(level string) string {
	if level == "" {
		level = config.DefaultThinkingLevel
	}
	if m.SupportsThinking(level) {
		return level
	}
	if len(m.ThinkingLevels) == 0 {
		return "off"
	}
	for _, l := range m.ThinkingLevels {
		if l == m.DefaultThinking {
			return l
		}
	}
	return m.ThinkingLevels[0]
}

// ToolSpec is the provider-facing tool shape.
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Context is the request payload for a stream call.
type Context struct {
	SystemPrompt string
	Messages     []messages.AgentMessage
	Tools        []ToolSpec
}

// StreamOptions carries per-call options.
type StreamOptions struct {
	ThinkingLevel string
	MaxTokens     int
	Timeout       time.Duration
	Headers       map[string]string
	APIKey        string // overrides provider key
}

// Runtime is the provider registry + auth + streaming.
type Runtime struct {
	providers         map[string]*Provider
	getenv            func(string) string
	keys              map[string]string // providerID → API key (config-provided)
	httpClientTimeout time.Duration
}

// New builds a Runtime from merged config, registering the built-in
// providers. Provider keys come from env (ANTHROPIC_API_KEY, OPENAI_API_KEY)
// or the keys map.
func New(cfg config.Config, getenv func(string) string, keys map[string]string) *Runtime {
	r := &Runtime{
		providers:         map[string]*Provider{},
		getenv:            getenv,
		keys:              keys,
		httpClientTimeout: cfg.TurnTimeout,
	}
	agentDir := cfg.AgentDir
	_ = agentDir
	r.registerBuiltins(cfg)
	return r
}

func (r *Runtime) registerBuiltins(cfg config.Config) {
	// Anthropic.
	r.providers["anthropic"] = &Provider{
		ID:      "anthropic",
		API:     APIAnthropic,
		BaseURL: defaultOr(cfg.BaseURL, "https://api.anthropic.com"),
		APIKey:  r.resolveKey("anthropic", "ANTHROPIC_API_KEY"),
		Models: []Model{
			{ID: "claude-sonnet-4-5", ContextWindow: 200_000, MaxOutput: 64_000,
				ThinkingLevels: []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}, DefaultThinking: "medium"},
			{ID: "claude-opus-4-1", ContextWindow: 200_000, MaxOutput: 32_000,
				ThinkingLevels: []string{"off", "low", "medium", "high", "xhigh", "max"}, DefaultThinking: "medium"},
			{ID: "claude-haiku-4-5", ContextWindow: 200_000, MaxOutput: 8_192,
				ThinkingLevels: []string{"off", "low", "medium", "high"}, DefaultThinking: "medium"},
			{ID: "claude-3-7-sonnet", ContextWindow: 200_000, MaxOutput: 64_000,
				ThinkingLevels: []string{"off", "low", "medium", "high", "xhigh"}, DefaultThinking: "medium"},
		},
	}
	// OpenAI (chat completions).
	r.providers["openai"] = &Provider{
		ID:      "openai",
		API:     APIOpenAICompletions,
		BaseURL: defaultOr(cfg.BaseURL, "https://api.openai.com/v1"),
		APIKey:  r.resolveKey("openai", "OPENAI_API_KEY"),
		Models: []Model{
			{ID: "gpt-5", ContextWindow: 400_000, MaxOutput: 128_000,
				ThinkingLevels: []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}, DefaultThinking: "medium"},
			{ID: "gpt-5-mini", ContextWindow: 400_000, MaxOutput: 128_000,
				ThinkingLevels: []string{"off", "low", "medium", "high"}, DefaultThinking: "medium"},
		},
	}
	// OpenAI-compatible: user-configured provider for custom base URLs and
	// arbitrary model ids (e.g. deepseek). Base URL must be explicit.
	if cfg.BaseURL != "" {
		id := "openai-compatible"
		if cfg.Provider != "" && cfg.Provider != "anthropic" && cfg.Provider != "openai" {
			id = cfg.Provider
		}
		envKey := strings.ToUpper(id) + "_API_KEY"
		r.providers[id] = &Provider{
			ID:      id,
			API:     APIOpenAICompletions,
			BaseURL: cfg.BaseURL,
			APIKey:  r.resolveKey(id, envKey),
			Models: []Model{
				{ID: defaultOr(cfg.ModelID, "model"), ContextWindow: cfg.CompactionWindow, MaxOutput: 16_384,
					ThinkingLevels: []string{"off", "low", "medium", "high"}, DefaultThinking: "medium"},
			},
		}
	}
	// Merge the configured provider/model into the catalog.
	if cfg.Provider != "" {
		if p, ok := r.providers[cfg.Provider]; ok {
			if cfg.ModelID != "" {
				if _, found := r.Model(cfg.Provider, cfg.ModelID); !found {
					p.Models = append(p.Models, Model{
						ID: cfg.ModelID, ProviderID: cfg.Provider, API: p.API,
						ContextWindow: cfg.CompactionWindow, MaxOutput: 16_384,
						ThinkingLevels: []string{"off", "low", "medium", "high"}, DefaultThinking: "medium",
					})
				}
			}
			if cfg.BaseURL != "" {
				p.BaseURL = cfg.BaseURL
			}
		}
	}
}

func (r *Runtime) resolveKey(providerID, envKey string) string {
	if r.keys != nil {
		if k, ok := r.keys[providerID]; ok && k != "" {
			return k
		}
	}
	if r.getenv != nil {
		if k := r.getenv(envKey); k != "" {
			return k
		}
	}
	return ""
}

func defaultOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Provider returns a provider by id.
func (r *Runtime) Provider(id string) (*Provider, bool) {
	p, ok := r.providers[id]
	return p, ok
}

// Providers returns all providers.
func (r *Runtime) Providers() []Provider {
	out := make([]Provider, 0, len(r.providers))
	for _, p := range r.providers {
		out = append(out, *p)
	}
	return out
}

// Models returns the models of a provider.
func (r *Runtime) Models(providerID string) []Model {
	if p, ok := r.providers[providerID]; ok {
		return p.Models
	}
	return nil
}

// Model returns a model by provider+id.
func (r *Runtime) Model(providerID, modelID string) (Model, bool) {
	p, ok := r.providers[providerID]
	if !ok {
		return Model{}, false
	}
	// An explicit model id must be in the catalog; never silently swap it
	// for the provider default (D14: --model is authoritative). Unknown ids
	// are appended to the catalog by registerBuiltins before resolution.
	if modelID != "" {
		for _, m := range p.Models {
			if m.ID == modelID {
				m.ProviderID = providerID
				m.API = p.API
				return m, true
			}
		}
		return Model{}, false
	}
	// No model requested: fall back to the provider's first model.
	if len(p.Models) > 0 {
		m := p.Models[0]
		m.ProviderID = providerID
		m.API = p.API
		return m, true
	}
	return Model{}, false
}

// CheckAuth verifies a provider has an API key configured.
func (r *Runtime) CheckAuth(providerID string) error {
	p, ok := r.providers[providerID]
	if !ok {
		return fmt.Errorf("unknown provider %q (known: %s)", providerID, r.providerNames())
	}
	if p.APIKey == "" {
		return fmt.Errorf("no API key configured for provider %q (set %s_API_KEY)", providerID, strings.ToUpper(providerID))
	}
	return nil
}

func (r *Runtime) providerNames() string {
	names := make([]string, 0, len(r.providers))
	for id := range r.providers {
		names = append(names, id)
	}
	return strings.Join(names, ", ")
}

// --- LLM message conversion -----------------------------------------------

// Compaction summary wrappers.
const (
	CompactionSummaryPrefix = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n"
	CompactionSummarySuffix = "\n</summary>"
	BranchSummaryPrefix     = "The following is a summary of a branch that this conversation came back from:\n\n<summary>\n"
	BranchSummarySuffix     = "</summary>"
)

// BashExecutionToText renders a bash execution message as text.
func BashExecutionToText(msg messages.AgentMessage) string {
	var b strings.Builder
	b.WriteString("Ran `")
	b.WriteString(msg.Command)
	b.WriteString("`\n")
	if msg.Output != "" {
		b.WriteString("```\n")
		b.WriteString(msg.Output)
		b.WriteString("\n```")
	} else {
		b.WriteString("(no output)")
	}
	if msg.Cancelled {
		b.WriteString("\n\n(command cancelled)")
	} else if msg.ExitCode != nil && *msg.ExitCode != 0 {
		fmt.Fprintf(&b, "\n\nCommand exited with code %d", *msg.ExitCode)
	}
	if msg.Truncated && msg.FullOutputPath != "" {
		b.WriteString("\n\n[Output truncated. Full output: " + msg.FullOutputPath + "]")
	}
	return b.String()
}

// ConvertToLLM converts AgentMessage[] to LLM messages.
// Unconvertible messages are filtered out (never throws).
func ConvertToLLM(msgs []messages.AgentMessage) []messages.AgentMessage {
	out := make([]messages.AgentMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case messages.RoleBashExecution:
			if m.ExcludeFromContext {
				continue
			}
			out = append(out, messages.UserMessage(
				messages.Content{Blocks: []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: BashExecutionToText(m)}}},
				m.Timestamp))
		case messages.RoleCustom:
			var content messages.Content
			if m.Content.IsText() {
				content = messages.Content{Blocks: []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: m.Content.Text}}}
			} else {
				content = m.Content
			}
			out = append(out, messages.UserMessage(content, m.Timestamp))
		case messages.RoleBranchSummary:
			out = append(out, messages.UserMessage(
				messages.Content{Blocks: []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: BranchSummaryPrefix + m.Summary + BranchSummarySuffix}}},
				m.Timestamp))
		case messages.RoleCompactionSummary:
			out = append(out, messages.UserMessage(
				messages.Content{Blocks: []messages.ContentBlock{messages.TextContent{Type: messages.ContentText, Text: CompactionSummaryPrefix + m.Summary + CompactionSummarySuffix}}},
				m.Timestamp))
		case messages.RoleUser, messages.RoleAssistant, messages.RoleToolResult:
			out = append(out, m)
		}
	}
	return out
}

// TransportRunner executes a provider stream, emitting events into s.
// Implemented by pkg/modelrt/transport and registered via RegisterTransport.
type TransportRunner interface {
	Run(p *Provider, m Model, ctx context.Context, c Context, opts StreamOptions, apiKey string, s *Stream)
}

var transportRegistry = map[API]TransportRunner{}

// RegisterTransport registers a runner for an API family (called from the
// transport package's init).
func RegisterTransport(api API, t TransportRunner) {
	transportRegistry[api] = t
}

// Stream opens a provider stream for the model. Per the StreamFn contract it
// never returns an error for request/model/runtime failures — those are
// encoded in the returned stream (first event is error/aborted).
func (r *Runtime) Stream(model Model, ctx context.Context, c Context, opts StreamOptions) *Stream {
	provider := r.providers[model.ProviderID]
	if provider == nil {
		return errorStream(fmt.Errorf("unknown provider %q", model.ProviderID))
	}
	transport, ok := transportRegistry[provider.API]
	if !ok {
		return errorStream(fmt.Errorf("no transport for API %q", provider.API))
	}
	key := opts.APIKey
	if key == "" {
		key = provider.APIKey
	}
	if key == "" {
		return errorStream(fmt.Errorf("no API key for provider %q (set %s_API_KEY)", model.ProviderID, strings.ToUpper(model.ProviderID)))
	}
	s := NewStream()
	go func() {
		defer s.Close()
		transport.Run(provider, model, ctx, c, opts, key, s)
	}()
	return s
}
