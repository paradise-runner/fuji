package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"fuji/pkg/config"
)

// flagSet is a tiny flag parser (stdlib flag doesn't support the flag=value
// and repeated-string forms ergonomically; this keeps zero deps).
type flagSet struct {
	values map[string]string
	bools  map[string]bool
	rest   []string
}

func parseFlags(args []string) (*flagSet, error) {
	fs := &flagSet{values: map[string]string{}, bools: map[string]bool{}}
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--":
			fs.rest = append(fs.rest, args[i+1:]...)
			return fs, nil
		case strings.HasPrefix(a, "--"):
			name := a[2:]
			val := ""
			hasVal := false
			if eq := strings.Index(name, "="); eq >= 0 {
				val = name[eq+1:]
				name = name[:eq]
				hasVal = true
			}
			switch name {
			case "no-tools", "help":
				fs.bools[name] = true
			default:
				if !hasVal {
					if i+1 >= len(args) {
						return nil, fmt.Errorf("flag --%s requires a value", name)
					}
					val = args[i+1]
					i++
				}
				fs.values[name] = val
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			return nil, fmt.Errorf("unknown flag %s (use --flag)", a)
		default:
			fs.rest = append(fs.rest, a)
		}
		i++
	}
	return fs, nil
}

func (fs *flagSet) str(name string) (string, bool) {
	v, ok := fs.values[name]
	return v, ok
}

func (fs *flagSet) int(name string) (int, error) {
	if v, ok := fs.values[name]; ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, fmt.Errorf("flag --%s: invalid number %q", name, v)
		}
		return n, nil
	}
	return 0, nil
}

// resolveConfig merges flags > env > project file > user file > defaults.
func resolveConfig(fs *flagSet, cwd string) (config.Config, error) {
	userPath := config.UserConfigFile(agentDirDefault())
	projectPath := config.ProjectConfigFile(cwd)

	var layers []config.Layer
	// lowest precedence first: user file, project file, env, flags.
	if userPath != "" {
		src, err := config.FromFile(userPath)
		if err != nil {
			return config.Config{}, fmt.Errorf("user config %s: %w", userPath, err)
		}
		layers = append(layers, config.Layer{Name: config.LayerUser, Src: src})
	}
	if projectPath != "" {
		src, err := config.FromFile(projectPath)
		if err != nil {
			return config.Config{}, fmt.Errorf("project config %s: %w", projectPath, err)
		}
		layers = append(layers, config.Layer{Name: config.LayerProject, Src: src})
	}
	layers = append(layers, config.Layer{Name: config.LayerEnv, Src: config.FromEnv(os.Getenv)})

	flags := config.Source{}
	if v, ok := fs.str("provider"); ok {
		flags["model.provider"] = v
	}
	if v, ok := fs.str("model"); ok {
		flags["model.model"] = v
	}
	if v, ok := fs.str("base-url"); ok {
		flags["model.baseUrl"] = v
	}
	if v, ok := fs.str("thinking"); ok {
		flags["model.thinkingLevel"] = v
	}
	if v, ok := fs.str("timeout"); ok {
		flags["timeouts.turn"] = v
	}
	if v, ok := fs.str("tools"); ok {
		flags["tools.allowed"] = strings.Split(v, ",")
	}
	if v, ok := fs.str("session-dir"); ok {
		flags["paths.sessionDir"] = v
	}
	if v, ok := fs.str("skills"); ok {
		flags["paths.skillsDir"] = v
	}
	if v, ok := fs.str("skills"); ok {
		flags["paths.skillsDir"] = v
	}
	if v, ok := fs.str("agent-dir"); ok {
		flags["paths.agentDir"] = v
	}
	if v, ok := fs.str("app-url"); ok {
		flags["app.url"] = v
	}
	if v, ok := fs.str("app-title"); ok {
		flags["app.title"] = v
	}
	if v, ok := fs.str("app-categories"); ok {
		flags["app.categories"] = v
	}
	layers = append(layers, config.Layer{Name: config.LayerFlags, Src: flags})

	return config.Resolve(layers...)
}

func agentDirDefault() string {
	return config.Default().AgentDir
}

// installSignalHandlers wires SIGINT/SIGTERM → abort; a second signal forces
// exit 130 (core-spec §9). Returns the abort function for tests.
func installSignalHandlers(abort func()) func() {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		count := 0
		for range ch {
			count++
			if count >= 2 {
				os.Exit(ExitSigint)
			}
			abort()
		}
	}()
	return func() {
		signal.Stop(ch)
		close(ch)
	}
}
