package main

import (
	"fmt"
	"os"
	"strings"

	"fuji/internal/logging"
	"fuji/pkg/config"
	"fuji/pkg/messages"
	"fuji/pkg/modelrt"
	"fuji/pkg/resource"
	"fuji/pkg/session"
	"fuji/pkg/sessionmgr"
	"fuji/pkg/tools"
	"fuji/pkg/tools/all"

	// Register the provider transports.
	_ "fuji/pkg/modelrt/transport"
)

// runCmd implements `fuji run`.
func runCmd(args []string) int {
	fs, err := parseFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fuji: %v\n", err)
		return ExitConfigAuth
	}
	if fs.bools["help"] {
		printUsage(os.Stdout)
		return ExitOK
	}

	// Structured JSON logging to stderr (fleet observability).
	logger := logging.Default()

	// --prompt (required).
	promptArg, ok := fs.str("prompt")
	if !ok {
		fmt.Fprintln(os.Stderr, "fuji: --prompt is required")
		return ExitConfigAuth
	}
	promptText, err := resolvePrompt(promptArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fuji: %v\n", err)
		return ExitConfigAuth
	}

	cwd := "."
	if v, ok := fs.str("cwd"); ok {
		cwd = v
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		fmt.Fprintf(os.Stderr, "fuji: working directory does not exist: %s\n", cwd)
		return ExitConfigAuth
	}

	// Merge config.
	cfg, err := resolveConfig(fs, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fuji: config error: %v\n", err)
		return ExitConfigAuth
	}
	if cfg.Provider == "" || cfg.ModelID == "" {
		// Provider default model resolves at runtime; provider must be known.
		if cfg.Provider == "" {
			fmt.Fprintln(os.Stderr, "fuji: no provider configured (set FUJI_PROVIDER or --provider)")
			return ExitConfigAuth
		}
	}

	// Model runtime.
	runtime := modelrt.New(cfg, os.Getenv, nil)
	if err := runtime.CheckAuth(cfg.Provider); err != nil {
		fmt.Fprintf(os.Stderr, "fuji: %v\n", err)
		return ExitConfigAuth
	}

	// Resource discovery (P7): skills + prompt templates.
	var skillsDirs, templateDirs []string
	if v, ok := fs.str("skills"); ok && v != "" {
		skillsDirs = append(skillsDirs, v)
	}
	res := resource.Discover(resource.Options{
		SkillsDirs:    skillsDirs,
		TemplatesDirs: templateDirs,
		ProjectDir:    config.ProjectDir(cwd),
		UserDir:       cfg.AgentDir,
	})
	for _, d := range res.Diagnostics {
		fmt.Fprintf(os.Stderr, "fuji: warning: %s\n", d)
	}

	if v, ok := fs.str("log-level"); ok {
		logLevel, err := logging.ParseLevel(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fuji: %v\n", err)
			return ExitConfigAuth
		}
		logger.SetLevel(logLevel)
	}

	// Session manager: resume or create.
	var mgr *sessionmgr.Manager
	if path, ok := fs.str("session"); ok && path != "" {
		mgr, err = sessionmgr.Open(path, "", cwd)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fuji: cannot open session %s: %v\n", path, err)
			return ExitConfigAuth
		}
	} else {
		mgr = sessionmgr.Create(cwd, config.SessionsDir(cfg.AgentDir, cfg.SessionDir, cwd), sessionmgr.NewSessionOptions{})
	}
	// Degraded-mode hook: session persistence failures are logged, the loop
	// continues in-memory (core-spec §6).
	mgr.OnPersistError = func(err error) {
		logger.Error("session persistence failed (continuing in-memory)", "error", err.Error())
	}

	// Tools.
	reg := tools.NewRegistry()
	if !fs.bools["no-tools"] {
		res := all.BuildResolvers(cfg.AgentDir)
		all.RegisterAll(reg, cwd, res, cfg.BashTimeout)
		reg.SetAllowed(cfg.AllowedTools)
		reg.Exclude(cfg.ExcludedTools)
	}
	toolList := reg.List()

	sess, err := session.New(session.Config{
		Cwd:            cwd,
		AgentDir:       cfg.AgentDir,
		Config:         cfg,
		ModelRuntime:   runtime,
		SessionManager: mgr,
		Skills:         res.Skills,
		Templates:      res.Templates,
		Tools:          toolList,
		AllowedTools:   cfg.AllowedTools,
		ExcludedTools:  cfg.ExcludedTools,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "fuji: %v\n", err)
		return ExitConfigAuth
	}
	defer sess.Dispose()

	// Minimal progress on stderr: stream assistant text as it arrives.
	var lastAssistant string
	streamAssistantText := func(m *messages.AgentMessage) {
		if m == nil || m.Role != "assistant" {
			return
		}
		text := m.Content.TextOf()
		if len(text) > len(lastAssistant) {
			fmt.Fprint(os.Stderr, text[len(lastAssistant):])
			lastAssistant = text
		}
	}
	logger.Info("session started", "session", sess.SessionID(), "file", sess.SessionFile(), "model", sess.Model().ID, "provider", sess.Model().ProviderID)
	sess.Subscribe(func(e session.Event) {
		switch e.Type {
		case session.EvMessageStart, session.EvMessageUpdate:
			// The first delta arrives as EvMessageStart; a single-delta
			// response may never emit EvMessageUpdate, so both must stream.
			streamAssistantText(e.Message)
		case session.EvMessageEnd:
			if e.Message != nil && e.Message.Role == "assistant" {
				// Defensive: print any text that never streamed.
				streamAssistantText(e.Message)
				fmt.Fprintln(os.Stderr)
				logger.Info("assistant message", "stopReason", e.Message.StopReason, "provider", e.Message.Provider, "model", e.Message.Model)
			}
		case session.EvToolExecutionStart:
			fmt.Fprintf(os.Stderr, "\n[tool: %s]\n", e.ToolName)
			logger.Info("tool start", "tool", e.ToolName)
		case session.EvToolExecutionEnd:
			logger.Info("tool end", "tool", e.ToolName, "isError", e.IsError)
		case session.EvAgentEnd:
			logger.Info("agent end", "messages", len(e.Messages))
		case session.EvAutoRetryStart:
			fmt.Fprintf(os.Stderr, "\n[retrying (%d/%d): %s]\n", e.Attempt, e.MaxAttempts, e.ErrorMessage)
			logger.Warn("retry", "attempt", e.Attempt, "maxAttempts", e.MaxAttempts, "error", e.ErrorMessage)
		case session.EvCompactionStart:
			logger.Info("compaction", "reason", e.CompactionReason)
		}
	})

	// SIGINT → Abort; second SIGINT → 130.
	uninstall := installSignalHandlers(func() { _ = sess.Abort() })
	defer uninstall()

	fmt.Fprintf(os.Stderr, "fuji: session %s\n", sess.SessionFile())

	if err := sess.Prompt(promptText, session.PromptOptions{}); err != nil {
		fmt.Fprintf(os.Stderr, "fuji: %v\n", err)
		return ExitConfigAuth
	}

	// Abort detection: if the last assistant message was aborted → 3.
	msgs := sess.Messages()
	if len(msgs) > 0 {
		last := msgs[len(msgs)-1]
		if last.Role == "assistant" && last.StopReason == "aborted" {
			return ExitAborted
		}
		if last.Role == "assistant" && last.StopReason == "error" {
			fmt.Fprintf(os.Stderr, "fuji: %s\n", last.ErrorMessage)
			return ExitRuntime
		}
	}
	return ExitOK
}

// resolvePrompt handles "@file" prompt arguments.
func resolvePrompt(arg string) (string, error) {
	if strings.HasPrefix(arg, "@") {
		data, err := os.ReadFile(strings.TrimPrefix(arg, "@"))
		if err != nil {
			return "", fmt.Errorf("cannot read prompt file: %v", err)
		}
		return string(data), nil
	}
	return arg, nil
}
