// Package config implements fuji's settings model: a typed settings struct
// resolved through a strict precedence merge of CLI flags > env vars > project
// config file > user config file > built-in defaults (core-spec §4.7, D9).
//
// Secrets never live in config files — provider API keys come from the
// environment (e.g. ANTHROPIC_API_KEY) or are injected programmatically.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Defaults for the settings surface.
const (
	DefaultAgentDirName   = ".fuji"
	DefaultThinkingLevel  = "medium"
	DefaultProvider       = "anthropic"
	DefaultTurnTimeout    = 5 * time.Minute
	DefaultToolTimeout    = 5 * time.Minute
	DefaultBashTimeout    = 5 * time.Minute
	DefaultRetryMax       = 2
	DefaultRetryBackoff   = 2 * time.Second
	DefaultRetryMaxDelay  = 60 * time.Second
	DefaultCompactWindow  = 200_000 // target context window tokens
	DefaultCompactReserve = 10_000  // reserve tokens before auto-compaction
	DefaultKeepRecent     = 50_000  // tokens to keep after a cut
)

// Config is the fully-resolved settings surface (core-spec §4.7).
type Config struct {
	// Model selection.
	Provider      string `json:"provider"`
	ModelID       string `json:"model"`
	BaseURL       string `json:"baseUrl"`
	ThinkingLevel string `json:"thinkingLevel"`

	// Tools allow/deny. AllowedTools nil = all bundled tools allowed.
	AllowedTools  []string `json:"allowedTools"`
	ExcludedTools []string `json:"excludedTools"`

	// Timeout budgets (core-spec §5.3).
	TurnTimeout time.Duration `json:"turnTimeout"`
	ToolTimeout time.Duration `json:"toolTimeout"`
	BashTimeout time.Duration `json:"bashTimeout"`

	// Retry policy (core-spec §5.4).
	RetryMaxAttempts int           `json:"retryMaxAttempts"`
	RetryBackoff     time.Duration `json:"retryBackoff"`
	RetryMaxDelay    time.Duration `json:"retryMaxDelay"`

	// Auto-compaction (core-spec §4.9).
	AutoCompactEnabled bool `json:"autoCompactEnabled"`
	CompactionWindow   int  `json:"compactionWindow"`  // context window tokens
	CompactionReserve  int  `json:"compactionReserve"` // reserve before compacting
	KeepRecentTokens   int  `json:"keepRecentTokens"`  // tokens retained after a cut

	// App attribution (OpenRouter headers). Sends HTTP-Referer,
	// X-OpenRouter-Title, and X-OpenRouter-Categories so usage appears in
	// public rankings/analytics. HTTP-Referer (app.url) is required for a
	// rankings entry to be created.
	AppURL        string `json:"appUrl"`        // → HTTP-Referer
	AppTitle      string `json:"appTitle"`      // → X-OpenRouter-Title
	AppCategories string `json:"appCategories"` // → X-OpenRouter-Categories (comma-sep)

	// Paths.
	AgentDir   string `json:"agentDir"`   // default ~/.fuji (D11)
	SessionDir string `json:"sessionDir"` // override for the sessions root
	SkillsDir  string `json:"skillsDir"`  // explicit --skills override

	// Sources records where each scalar setting came from, for diagnostics
	// (highest-precedence source per key). Immaterial for behavior.
	Sources map[string]string `json:"-"`
}

// Default returns the built-in defaults layer.
func Default() Config {
	agentDir, _ := defaultAgentDir()
	return Config{
		Provider:           DefaultProvider,
		ThinkingLevel:      DefaultThinkingLevel,
		TurnTimeout:        DefaultTurnTimeout,
		ToolTimeout:        DefaultToolTimeout,
		BashTimeout:        DefaultBashTimeout,
		RetryMaxAttempts:   DefaultRetryMax,
		RetryBackoff:       DefaultRetryBackoff,
		RetryMaxDelay:      DefaultRetryMaxDelay,
		AutoCompactEnabled: true,
		CompactionWindow:   DefaultCompactWindow,
		CompactionReserve:  DefaultCompactReserve,
		KeepRecentTokens:   DefaultKeepRecent,
		AgentDir:           agentDir,
		Sources:            map[string]string{"*": "defaults"},
	}
}

// defaultAgentDir returns ~/.fuji using os.UserHomeDir.
func defaultAgentDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, DefaultAgentDirName), nil
}

// --- precedence merge -----------------------------------------------------

// Source is one layer of settings: a flat map of dotted keys to values.
// Keys are like "model.provider", "timeouts.turn", "tools.excluded".
type Source map[string]any

// Layer names for the Sources map / diagnostics.
const (
	LayerFlags    = "flags"
	LayerEnv      = "env"
	LayerProject  = "project"
	LayerUser     = "user"
	LayerDefaults = "defaults"
)

// Layer is a named settings layer for Resolve.
type Layer struct {
	Name string
	Src  Source
}

// Resolve merges layers where each earlier Layer has HIGHER precedence than
// later ones (flags > env > project > user > defaults). To honor that, layers
// are applied from lowest to highest precedence: later layers overwrite.
// Scalars from higher-precedence layers win; slices are taken wholesale from
// the highest-precedence layer that provides them.
func Resolve(layers ...Layer) (Config, error) {
	cfg := Default()
	seen := map[string]string{}
	for i := len(layers) - 1; i >= 0; i-- {
		layer := layers[i]
		if layer.Src == nil {
			continue
		}
		for k, v := range layer.Src {
			if err := applyKey(&cfg, k, v); err != nil {
				return Config{}, fmt.Errorf("config key %q: %w", k, err)
			}
			// Record the highest-precedence source (first seen when iterating
			// lowest-first means the last write wins here).
			seen[k] = layer.Name
		}
	}
	// Track provenance per key, defaults for the rest.
	cfg.Sources = map[string]string{}
	for k := range cfg.flatten() {
		if s, ok := seen[k]; ok {
			cfg.Sources[k] = s
		} else {
			cfg.Sources[k] = LayerDefaults
		}
	}
	return cfg, nil
}

// applyKey sets one dotted key on cfg. Unknown keys are an error (typo
// safety). Durations accept Go duration strings or plain seconds.
func applyKey(cfg *Config, key string, v any) error {
	switch key {
	case "model.provider":
		cfg.Provider = asString(v)
	case "model.model":
		cfg.ModelID = asString(v)
	case "model.baseUrl":
		cfg.BaseURL = asString(v)
	case "model.thinkingLevel":
		cfg.ThinkingLevel = asString(v)
	case "tools.allowed":
		cfg.AllowedTools = asStrings(v)
	case "tools.excluded":
		cfg.ExcludedTools = asStrings(v)
	case "timeouts.turn":
		d, err := asDuration(v)
		if err != nil {
			return err
		}
		cfg.TurnTimeout = d
	case "timeouts.tool":
		d, err := asDuration(v)
		if err != nil {
			return err
		}
		cfg.ToolTimeout = d
	case "timeouts.bash":
		d, err := asDuration(v)
		if err != nil {
			return err
		}
		cfg.BashTimeout = d
	case "retry.maxAttempts":
		cfg.RetryMaxAttempts = asInt(v)
	case "retry.backoff":
		d, err := asDuration(v)
		if err != nil {
			return err
		}
		cfg.RetryBackoff = d
	case "retry.maxDelay":
		d, err := asDuration(v)
		if err != nil {
			return err
		}
		cfg.RetryMaxDelay = d
	case "compaction.enabled":
		cfg.AutoCompactEnabled = asBool(v)
	case "compaction.window":
		cfg.CompactionWindow = asInt(v)
	case "compaction.reserve":
		cfg.CompactionReserve = asInt(v)
	case "compaction.keepRecent":
		cfg.KeepRecentTokens = asInt(v)
	case "app.url":
		cfg.AppURL = asString(v)
	case "app.title":
		cfg.AppTitle = asString(v)
	case "app.categories":
		cfg.AppCategories = asString(v)
	case "paths.agentDir":
		cfg.AgentDir = asString(v)
	case "paths.sessionDir":
		cfg.SessionDir = asString(v)
	case "paths.skillsDir":
		cfg.SkillsDir = asString(v)
	default:
		return fmt.Errorf("unknown setting")
	}
	return nil
}

// flatten returns the dotted-key representation of cfg's scalar fields.
func (c Config) flatten() map[string]any {
	return map[string]any{
		"model.provider":        c.Provider,
		"model.model":           c.ModelID,
		"model.baseUrl":         c.BaseURL,
		"model.thinkingLevel":   c.ThinkingLevel,
		"tools.allowed":         c.AllowedTools,
		"tools.excluded":        c.ExcludedTools,
		"timeouts.turn":         c.TurnTimeout.String(),
		"timeouts.tool":         c.ToolTimeout.String(),
		"timeouts.bash":         c.BashTimeout.String(),
		"retry.maxAttempts":     c.RetryMaxAttempts,
		"retry.backoff":         c.RetryBackoff.String(),
		"retry.maxDelay":        c.RetryMaxDelay.String(),
		"compaction.enabled":    c.AutoCompactEnabled,
		"compaction.window":     c.CompactionWindow,
		"compaction.reserve":    c.CompactionReserve,
		"compaction.keepRecent": c.KeepRecentTokens,
		"app.url":               c.AppURL,
		"app.title":             c.AppTitle,
		"app.categories":        c.AppCategories,
		"paths.agentDir":        c.AgentDir,
		"paths.sessionDir":      c.SessionDir,
		"paths.skillsDir":       c.SkillsDir,
	}
}

// --- typed accessors ------------------------------------------------------

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

func asStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = append(out, asString(e))
		}
		return out
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	default:
		return nil
	}
}

func asInt(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(t))
		return n
	default:
		return 0
	}
}

func asBool(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		b, _ := strconv.ParseBool(strings.TrimSpace(t))
		return b
	default:
		return false
	}
}

// asDuration accepts Go duration strings ("30s", "2m"), bare numbers
// (interpreted as seconds), or time.Duration values.
func asDuration(v any) (time.Duration, error) {
	switch t := v.(type) {
	case time.Duration:
		return t, nil
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, errors.New("empty duration")
		}
		if n, err := strconv.Atoi(s); err == nil {
			return time.Duration(n) * time.Second, nil
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return d, nil
	case int:
		return time.Duration(t) * time.Second, nil
	case int64:
		return time.Duration(t) * time.Second, nil
	case float64:
		return time.Duration(t * float64(time.Second)), nil
	default:
		return 0, fmt.Errorf("cannot interpret %T as duration", v)
	}
}

// --- path resolution ------------------------------------------------------

// ProjectDir returns the .fuji directory for a project, if it exists.
// It walks up from cwd to find the nearest .fuji directory (project
// config/skills discovery).
func ProjectDir(cwd string) string {
	dir := cwd
	for {
		candidate := filepath.Join(dir, DefaultAgentDirName)
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// ProjectConfigFile returns the path of the project config file, if any.
func ProjectConfigFile(cwd string) string {
	if pdir := ProjectDir(cwd); pdir != "" {
		for _, name := range []string{"fuji.toml", "settings.json", "fuji.json"} {
			p := filepath.Join(pdir, name)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	return ""
}

// UserConfigFile returns the user config file path, if any.
func UserConfigFile(agentDir string) string {
	for _, name := range []string{"fuji.toml", "settings.json", "fuji.json"} {
		p := filepath.Join(agentDir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// SessionsDir resolves the sessions root directory for a cwd, using the
// standard encoding: "--" + cwd (leading slash stripped; / \ : → -) + "--".
// SessionDir override (config) wins if set.
func SessionsDir(agentDir, sessionDirOverride, cwd string) string {
	if sessionDirOverride != "" {
		return sessionDirOverride
	}
	return filepath.Join(agentDir, "sessions", encodeCwd(cwd))
}

// encodeCwd encodes a cwd as a session directory name.
func encodeCwd(cwd string) string {
	cleaned := filepath.Clean(cwd)
	trimmed := strings.TrimLeft(cleaned, "/\\")
	repl := strings.NewReplacer("/", "-", "\\", "-", ":", "-")
	return "--" + repl.Replace(trimmed) + "--"
}

// SortedKeys returns layer keys sorted, for deterministic tests.
func SortedKeys(m Source) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// EnvPrefix is the prefix for fuji's own environment variables.
const EnvPrefix = "FUJI_"

// FromEnv builds a Source layer from FUJI_* environment variables.
// Mapping: FUJI_PROVIDER → model.provider, FUJI_MODEL → model.model,
// FUJI_BASE_URL → model.baseUrl, FUJI_THINKING_LEVEL → model.thinkingLevel,
// FUJI_TURN_TIMEOUT → timeouts.turn, FUJI_TOOL_TIMEOUT → timeouts.tool,
// FUJI_BASH_TIMEOUT → timeouts.bash, FUJI_ALLOWED_TOOLS → tools.allowed (comma-sep),
// FUJI_EXCLUDED_TOOLS → tools.excluded, FUJI_RETRY_MAX → retry.maxAttempts,
// FUJI_RETRY_BACKOFF → retry.backoff, FUJI_RETRY_MAX_DELAY → retry.maxDelay,
// FUJI_AUTO_COMPACT (0/1) → compaction.enabled, FUJI_COMPACT_WINDOW → compaction.window,
// FUJI_COMPACT_RESERVE → compaction.reserve, FUJI_KEEP_RECENT → compaction.keepRecent,
// FUJI_APP_URL → app.url, FUJI_APP_TITLE → app.title, FUJI_APP_CATEGORIES → app.categories,
// FUJI_AGENT_DIR → paths.agentDir, FUJI_SESSION_DIR → paths.sessionDir,
// FUJI_SKILLS_DIR → paths.skillsDir.
func FromEnv(getenv func(string) string) Source {
	src := Source{}
	set := func(envKey, cfgKey string) {
		if v := getenv(envKey); v != "" {
			src[cfgKey] = v
		}
	}
	set(EnvPrefix+"PROVIDER", "model.provider")
	set(EnvPrefix+"MODEL", "model.model")
	set(EnvPrefix+"BASE_URL", "model.baseUrl")
	set(EnvPrefix+"THINKING_LEVEL", "model.thinkingLevel")
	set(EnvPrefix+"TURN_TIMEOUT", "timeouts.turn")
	set(EnvPrefix+"TOOL_TIMEOUT", "timeouts.tool")
	set(EnvPrefix+"BASH_TIMEOUT", "timeouts.bash")
	if v := getenv(EnvPrefix + "ALLOWED_TOOLS"); v != "" {
		src["tools.allowed"] = strings.Split(v, ",")
	}
	if v := getenv(EnvPrefix + "EXCLUDED_TOOLS"); v != "" {
		src["tools.excluded"] = strings.Split(v, ",")
	}
	set(EnvPrefix+"RETRY_MAX", "retry.maxAttempts")
	set(EnvPrefix+"RETRY_BACKOFF", "retry.backoff")
	set(EnvPrefix+"RETRY_MAX_DELAY", "retry.maxDelay")
	if v := getenv(EnvPrefix + "AUTO_COMPACT"); v != "" {
		src["compaction.enabled"] = v
	}
	set(EnvPrefix+"COMPACT_WINDOW", "compaction.window")
	set(EnvPrefix+"COMPACT_RESERVE", "compaction.reserve")
	set(EnvPrefix+"KEEP_RECENT", "compaction.keepRecent")
	set(EnvPrefix+"APP_URL", "app.url")
	set(EnvPrefix+"APP_TITLE", "app.title")
	set(EnvPrefix+"APP_CATEGORIES", "app.categories")
	set(EnvPrefix+"AGENT_DIR", "paths.agentDir")
	set(EnvPrefix+"SESSION_DIR", "paths.sessionDir")
	set(EnvPrefix+"SKILLS_DIR", "paths.skillsDir")
	return src
}

// runtime.GOOS is referenced here so the package compiles on all platforms
// even though we only special-case paths on unix (kept for future Windows
// session-dir encoding work).
var _ = runtime.GOOS
