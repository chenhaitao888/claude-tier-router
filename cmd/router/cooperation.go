package main

import (
	"claude-tier-router/internal/router"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

//go:embed prompts/coordinator.md
var coordinatorPrompt string

//go:embed prompts/executor.md
var executorPrompt string

//go:embed prompts/reviewer.md
var reviewerPrompt string

func readSessionFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot read Claude session option file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("Claude session option requires a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, (2<<20)+1))
	if err != nil || len(raw) > 2<<20 {
		return nil, errors.New("Claude session option file exceeds 2 MiB")
	}
	return raw, nil
}

func cooperativeLaunch(args []string, settings map[string]json.RawMessage, address, dir string, executorLimit int) ([]string, error) {
	agents := map[string]json.RawMessage{}
	var extra []string
	out := []string{}
	seenAgents := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			out = append(out, args[i:]...)
			break
		}
		key, value, equals := strings.Cut(arg, "=")
		if key != "--agents" && key != "--append-system-prompt" && key != "--append-system-prompt-file" && key != "--model" {
			out = append(out, arg)
			continue
		}
		if !equals {
			i++
			if i == len(args) {
				return nil, errors.New("Claude option requires a value: " + key)
			}
			value = args[i]
		}
		switch key {
		case "--model":
			if value != router.CoordinatorModel {
				return nil, errors.New("-cooperate manages the main model; omit Claude --model or use router-coordinator")
			}
		case "--agents":
			if seenAgents {
				return nil, errors.New("pass at most one Claude --agents option")
			}
			seenAgents = true
			raw := []byte(value)
			if !strings.HasPrefix(strings.TrimSpace(value), "{") {
				var err error
				raw, err = readSessionFile(value)
				if err != nil {
					return nil, err
				}
			}
			if len(raw) > 2<<20 || json.Unmarshal(raw, &agents) != nil || agents == nil {
				return nil, errors.New("Claude --agents must contain one JSON object")
			}
		case "--append-system-prompt":
			extra = append(extra, value)
		case "--append-system-prompt-file":
			raw, err := readSessionFile(value)
			if err != nil {
				return nil, err
			}
			extra = append(extra, string(raw))
		}
	}
	for name, definition := range map[string]any{
		router.ExecutorModel: map[string]any{"description": "Execute a bounded plan: batch code/evidence lookups for existing table names, event keys and constants with file/line references; collect specified data; format approved conclusions; or make an exact local code change. Return ambiguity and new design decisions to the parent.", "prompt": executorPrompt, "model": router.ExecutorModel, "disallowedTools": []string{"Agent", "Task"}},
		router.ReviewerModel: map[string]any{"description": "Independently review a completed delegated change or data report against the parent's acceptance criteria; analyze ambiguity and report required corrections.", "prompt": reviewerPrompt, "model": router.ReviewerModel, "tools": []string{"Read", "Grep", "Glob", "Bash"}, "disallowedTools": []string{"Agent", "Task", "Edit", "Write"}},
	} {
		if _, exists := agents[name]; exists {
			return nil, errors.New("reserved cooperation agent name in --agents: " + name)
		}
		agents[name], _ = json.Marshal(definition)
	}
	definitions, err := json.Marshal(agents)
	if err != nil {
		return nil, err
	}
	extra = append(extra, strings.ReplaceAll(coordinatorPrompt, "{{executor_request_limit}}", strconv.Itoa(executorLimit)))
	path := filepath.Join(dir, "cooperation.md")
	if err = os.WriteFile(path, []byte(strings.Join(extra, "\n\n")), 0600); err != nil {
		return nil, err
	}
	hooks := map[string][]json.RawMessage{}
	if raw, ok := settings["hooks"]; ok {
		if json.Unmarshal(raw, &hooks) != nil || hooks == nil {
			return nil, errors.New("Claude hooks must be an object of hook arrays")
		}
	}
	for _, event := range []string{"SubagentStart", "SubagentStop"} {
		group, _ := json.Marshal(map[string]any{"matcher": "^router-(executor|reviewer)$", "hooks": []any{map[string]any{"type": "http", "url": address + "/api/cooperation/events", "timeout": 5}}})
		hooks[event] = append(hooks[event], group)
	}
	settings["hooks"], _ = json.Marshal(hooks)
	return append([]string{"--agents", string(definitions), "--append-system-prompt-file", path}, out...), nil
}
