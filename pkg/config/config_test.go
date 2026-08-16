package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolvePrecedenceTable(t *testing.T) {
	tests := []struct {
		name   string
		layers []Layer
		want   func(Config) bool
	}{
		{
			name: "flags beat env",
			layers: []Layer{
				{Name: LayerFlags, Src: Source{"model.provider": "openai"}},
				{Name: LayerEnv, Src: Source{"model.provider": "anthropic"}},
			},
			want: func(c Config) bool { return c.Provider == "openai" },
		},
		{
			name: "env beats project file",
			layers: []Layer{
				{Name: LayerEnv, Src: Source{"model.model": "env-model"}},
				{Name: LayerProject, Src: Source{"model.model": "project-model"}},
			},
			want: func(c Config) bool { return c.ModelID == "env-model" },
		},
		{
			name: "project beats user",
			layers: []Layer{
				{Name: LayerProject, Src: Source{"timeouts.turn": "60s"}},
				{Name: LayerUser, Src: Source{"timeouts.turn": "30s"}},
			},
			want: func(c Config) bool { return c.TurnTimeout == 60*time.Second },
		},
		{
			name: "user beats defaults",
			layers: []Layer{
				{Name: LayerUser, Src: Source{"model.thinkingLevel": "high"}},
			},
			want: func(c Config) bool { return c.ThinkingLevel == "high" },
		},
		{
			name: "defaults fill everything",
			layers: []Layer{
				{Name: LayerFlags, Src: Source{}},
			},
			want: func(c Config) bool {
				return c.Provider == DefaultProvider &&
					c.ThinkingLevel == DefaultThinkingLevel &&
					c.TurnTimeout == DefaultTurnTimeout &&
					c.RetryMaxAttempts == DefaultRetryMax &&
					c.AutoCompactEnabled
			},
		},
		{
			name: "slices taken wholesale from first layer",
			layers: []Layer{
				{Name: LayerFlags, Src: Source{"tools.allowed": []any{"read", "write"}}},
				{Name: LayerUser, Src: Source{"tools.allowed": []any{"bash"}}},
			},
			want: func(c Config) bool {
				return len(c.AllowedTools) == 2 && c.AllowedTools[0] == "read" && c.AllowedTools[1] == "write"
			},
		},
		{
			name: "bare seconds duration",
			layers: []Layer{
				{Name: LayerUser, Src: Source{"timeouts.bash": 90}},
			},
			want: func(c Config) bool { return c.BashTimeout == 90*time.Second },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Resolve(tt.layers...)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if !tt.want(cfg) {
				t.Fatalf("condition not met: %+v", cfg)
			}
		})
	}
}

func TestResolveUnknownKey(t *testing.T) {
	_, err := Resolve(Layer{Name: LayerUser, Src: Source{"model.typo": "x"}})
	if err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestProvenance(t *testing.T) {
	cfg, err := Resolve(
		Layer{Name: LayerFlags, Src: Source{"model.provider": "openai"}},
		Layer{Name: LayerUser, Src: Source{"model.thinkingLevel": "low"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sources["model.provider"] != LayerFlags {
		t.Errorf("provider source = %q, want %q", cfg.Sources["model.provider"], LayerFlags)
	}
	if cfg.Sources["model.thinkingLevel"] != LayerUser {
		t.Errorf("thinkingLevel source = %q, want %q", cfg.Sources["model.thinkingLevel"], LayerUser)
	}
	if cfg.Sources["timeouts.turn"] != LayerDefaults {
		t.Errorf("turn timeout source = %q, want defaults", cfg.Sources["timeouts.turn"])
	}
}

func TestFromFileTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fuji.toml")
	content := `
# fuji project config
[model]
provider = "anthropic"
model = "claude-sonnet-4-5"
baseUrl = "https://example.com"
thinkingLevel = "low"

[timeouts]
turn = "2m"
bash = 60
tool = "30s"

[tools]
excluded = ["git"]

[retry]
maxAttempts = 4
backoff = "1s"
maxDelay = "10s"

[compaction]
enabled = false
window = 100000
reserve = 5000
keepRecent = 20000

[paths]
sessionDir = "/tmp/sessions"
skillsDir = "/tmp/skills"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := FromFile(path)
	if err != nil {
		t.Fatalf("FromFile: %v", err)
	}
	cfg, err := Resolve(Layer{Name: LayerProject, Src: src})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Provider != "anthropic" || cfg.ModelID != "claude-sonnet-4-5" {
		t.Errorf("model = %s/%s", cfg.Provider, cfg.ModelID)
	}
	if cfg.BaseURL != "https://example.com" {
		t.Errorf("baseUrl = %q", cfg.BaseURL)
	}
	if cfg.TurnTimeout != 2*time.Minute || cfg.BashTimeout != 60*time.Second || cfg.ToolTimeout != 30*time.Second {
		t.Errorf("timeouts = %v/%v/%v", cfg.TurnTimeout, cfg.BashTimeout, cfg.ToolTimeout)
	}
	if len(cfg.ExcludedTools) != 1 || cfg.ExcludedTools[0] != "git" {
		t.Errorf("excluded = %v", cfg.ExcludedTools)
	}
	if cfg.RetryMaxAttempts != 4 || cfg.RetryBackoff != time.Second || cfg.RetryMaxDelay != 10*time.Second {
		t.Errorf("retry = %d/%v/%v", cfg.RetryMaxAttempts, cfg.RetryBackoff, cfg.RetryMaxDelay)
	}
	if cfg.AutoCompactEnabled {
		t.Error("auto-compact should be disabled")
	}
	if cfg.CompactionWindow != 100000 || cfg.CompactionReserve != 5000 || cfg.KeepRecentTokens != 20000 {
		t.Errorf("compaction = %d/%d/%d", cfg.CompactionWindow, cfg.CompactionReserve, cfg.KeepRecentTokens)
	}
	if cfg.SessionDir != "/tmp/sessions" || cfg.SkillsDir != "/tmp/skills" {
		t.Errorf("paths = %q/%q", cfg.SessionDir, cfg.SkillsDir)
	}
}

func TestFromFileJSONC(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.jsonc")
	content := `{
  // comment
  "model": {
    "provider": "openai",  // trailing comment
    "model": "gpt-5",
  },
  "compaction": { "enabled": true, },
}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := FromFile(path)
	if err != nil {
		t.Fatalf("FromFile: %v", err)
	}
	cfg, err := Resolve(Layer{Name: LayerProject, Src: src})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Provider != "openai" || cfg.ModelID != "gpt-5" {
		t.Errorf("model = %s/%s", cfg.Provider, cfg.ModelID)
	}
	if !cfg.AutoCompactEnabled {
		t.Error("auto-compact should be enabled")
	}
}

func TestFromEnv(t *testing.T) {
	env := map[string]string{
		"FUJI_PROVIDER":       "openai",
		"FUJI_MODEL":          "gpt-5",
		"FUJI_BASH_TIMEOUT":   "45s",
		"FUJI_ALLOWED_TOOLS":  "read,ls,find",
		"FUJI_AGENT_DIR":      "/custom/.fuji",
		"FUJI_APP_URL":        "https://myapp.com",
		"FUJI_APP_TITLE":      "My AI Assistant",
		"FUJI_APP_CATEGORIES": "cli-agent,cloud-agent",
	}
	getenv := func(k string) string { return env[k] }
	src := FromEnv(getenv)
	cfg, err := Resolve(Layer{Name: LayerEnv, Src: src})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "openai" || cfg.ModelID != "gpt-5" {
		t.Errorf("model = %s/%s", cfg.Provider, cfg.ModelID)
	}
	if cfg.BashTimeout != 45*time.Second {
		t.Errorf("bash timeout = %v", cfg.BashTimeout)
	}
	if len(cfg.AllowedTools) != 3 || cfg.AllowedTools[2] != "find" {
		t.Errorf("allowed = %v", cfg.AllowedTools)
	}
	if cfg.AgentDir != "/custom/.fuji" {
		t.Errorf("agentDir = %q", cfg.AgentDir)
	}
	if cfg.AppURL != "https://myapp.com" {
		t.Errorf("appURL = %q", cfg.AppURL)
	}
	if cfg.AppTitle != "My AI Assistant" {
		t.Errorf("appTitle = %q", cfg.AppTitle)
	}
	if cfg.AppCategories != "cli-agent,cloud-agent" {
		t.Errorf("appCategories = %q", cfg.AppCategories)
	}
}

func TestSessionsDirEncoding(t *testing.T) {
	got := SessionsDir("/home/u/.fuji", "", "/home/u/dev/my-project")
	// encoding: leading slash stripped, slashes become dashes, wrapped in
	// --…-- (session-manager.js getDefaultSessionDirPath).
	want := filepath.Join("/home/u/.fuji", "sessions", "--home-u-dev-my-project--")
	if got != want {
		t.Errorf("SessionsDir = %q, want %q", got, want)
	}
	// Leading slash is stripped: /Users/x → --Users-x--.
	if got := SessionsDir("/a", "", "/Users/x"); got != filepath.Join("/a", "sessions", "--Users-x--") {
		t.Errorf("SessionsDir = %q", got)
	}
	// Override wins.
	if got := SessionsDir("/a", "/override", "/b"); got != "/override" {
		t.Errorf("override = %q", got)
	}
}

func TestProjectDirWalkUp(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(filepath.Join(proj, ".fuji"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ProjectDir(proj); got != filepath.Join(proj, ".fuji") {
		t.Errorf("ProjectDir = %q", got)
	}
	// No .fuji anywhere: empty.
	empty := filepath.Join(root, "nope")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ProjectDir(empty); got != "" {
		t.Errorf("ProjectDir(empty) = %q, want empty", got)
	}
}
