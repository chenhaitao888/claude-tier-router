package main

import (
	"claude-tier-router/internal/router"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

var conflictingEnv = map[string]string{
	"ANTHROPIC_API_KEY": "", "CLAUDE_CODE_OAUTH_TOKEN": "",
	"CLAUDE_CODE_USE_BEDROCK": "0", "CLAUDE_CODE_USE_VERTEX": "0", "CLAUDE_CODE_USE_FOUNDRY": "0",
	"CLAUDE_CODE_USE_MANTLE": "0", "CLAUDE_CODE_USE_ANTHROPIC_AWS": "0",
}

func routingEnv(c router.Config, address string) map[string]string {
	window := min(c.High.ContextTokens, c.Low.ContextTokens)
	return map[string]string{
		"ANTHROPIC_BASE_URL": address, "ANTHROPIC_AUTH_TOKEN": "local-router",
		"ANTHROPIC_MODEL": "router-auto", "ANTHROPIC_DEFAULT_OPUS_MODEL": "router-auto", "ANTHROPIC_DEFAULT_SONNET_MODEL": "router-auto", "ANTHROPIC_DEFAULT_HAIKU_MODEL": "router-auto",
		"CLAUDE_CODE_SUBAGENT_MODEL": "router-auto", "CLAUDE_CODE_GATEWAY_HINT_HEADERS": "1", "CLAUDE_CODE_AUTO_COMPACT_WINDOW": strconv.Itoa(window * 3 / 4),
	}
}

func launchEnv(existing []string, c router.Config, address string) []string {
	replace := routingEnv(c, address)
	env := []string{}
	for _, entry := range existing {
		key, _, _ := strings.Cut(entry, "=")
		_, conflict := conflictingEnv[key]
		if _, ok := replace[key]; !ok && !conflict {
			env = append(env, entry)
		}
	}
	for k, v := range replace {
		env = append(env, k+"="+v)
	}
	return env
}

// settings.json's env can override the process environment. Supply only the
// routing keys as CLI settings; normal hooks, permissions and plugins still load.
// Managed policy retains Claude Code's own higher precedence.
func launchArgs(existing []string, c router.Config, address string) ([]string, func(), error) {
	args := []string{}
	var custom string
	hasCustom := false
	for i := 0; i < len(existing); i++ {
		arg := existing[i]
		if arg == "--" {
			args = append(args, existing[i:]...)
			break
		}
		if arg == "--settings" || strings.HasPrefix(arg, "--settings=") {
			if hasCustom {
				return nil, nil, errors.New("pass at most one Claude --settings option")
			}
			hasCustom = true
			if arg == "--settings" {
				i++
				if i == len(existing) {
					return nil, nil, errors.New("Claude --settings requires a file or JSON object")
				}
				custom = existing[i]
			} else {
				custom = strings.TrimPrefix(arg, "--settings=")
			}
			if strings.TrimSpace(custom) == "" {
				return nil, nil, errors.New("Claude --settings requires a file or JSON object")
			}
		} else {
			args = append(args, arg)
		}
	}
	settings := map[string]json.RawMessage{}
	if hasCustom {
		raw := []byte(custom)
		if !strings.HasPrefix(strings.TrimSpace(custom), "{") {
			f, err := os.Open(custom)
			if err != nil {
				return nil, nil, errors.New("could not read Claude --settings file")
			}
			info, err := f.Stat()
			if err != nil || !info.Mode().IsRegular() {
				f.Close()
				return nil, nil, errors.New("Claude --settings requires a regular file")
			}
			raw, err = io.ReadAll(io.LimitReader(f, (2<<20)+1))
			f.Close()
			if err != nil {
				return nil, nil, errors.New("could not read Claude --settings file")
			}
		}
		if len(raw) > 2<<20 || json.Unmarshal(raw, &settings) != nil || settings == nil {
			return nil, nil, errors.New("Claude --settings must be one JSON object, at most 2 MiB")
		}
	}
	env := map[string]string{}
	if raw, ok := settings["env"]; ok {
		if json.Unmarshal(raw, &env) != nil || env == nil {
			return nil, nil, errors.New("Claude --settings env must be an object of strings")
		}
	}
	for key, value := range conflictingEnv {
		env[key] = value
	}
	for key, value := range routingEnv(c, address) {
		env[key] = value
	}
	settings["env"], _ = json.Marshal(env)
	settings["model"] = json.RawMessage(`"router-auto"`)
	settings["apiKeyHelper"] = json.RawMessage(`""`)
	raw, err := json.Marshal(settings)
	if err != nil {
		return nil, nil, err
	}
	// A private file avoids exposing any user-supplied --settings secrets in argv.
	dir, err := os.MkdirTemp("", "claude-tier-router-session-")
	if err != nil {
		return nil, nil, errors.New("could not create Claude session settings directory")
	}
	cleanup := func() { os.RemoveAll(dir) }
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		cleanup()
		return nil, nil, errors.New("could not write Claude session settings")
	}
	return append([]string{"--settings", path}, args...), cleanup, nil
}
