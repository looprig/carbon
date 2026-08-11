package presentation

import (
	"encoding/json"
	"strconv"
	"strings"
)

const (
	toolNameReadFile   = "ReadFile"
	toolNameRead       = "Read"
	toolNameWriteFile  = "WriteFile"
	toolNameEditFile   = "EditFile"
	toolNameBash       = "Bash"
	toolNameFetch      = "Fetch"
	toolNameWebSearch  = "WebSearch"
	toolNameGlob       = "Glob"
	toolNameGrep       = "Grep"
	toolNameTaskCreate = "TaskCreate"
	toolNameTaskUpdate = "TaskUpdate"
	toolNameTaskGet    = "TaskGet"
	toolNameTaskList   = "TaskList"
	toolNameSkill      = "Skill"
)

type pathSummaryArgs struct {
	Path string `json:"path"`
}

type writeSummaryArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type bashSummaryArgs struct {
	Command string `json:"command"`
	Legacy  string `json:"cmd"`
}

type fetchSummaryArgs struct {
	Method string `json:"method"`
	URL    string `json:"url"`
}

type searchSummaryArgs struct {
	Query string `json:"query"`
}

type globSummaryArgs struct {
	Pattern string `json:"pattern"`
	Root    string `json:"root"`
}

type grepSummaryArgs struct {
	Pattern string `json:"pattern"`
	Legacy  string `json:"q"`
	Path    string `json:"path"`
}

type skillSummaryArgs struct {
	Name string `json:"name"`
}

// toolUseSummary reconstructs the redacted one-line display detail for a stored
// ToolUseBlock. This is the durable path used when ToolCallStarted.Summary is not
// available, notably for subagent nested cards. It never renders file contents,
// edit substrings, request headers, request bodies, or subagent task text.
func toolUseSummary(name string, input json.RawMessage) string {
	switch name {
	case toolNameReadFile, toolNameRead:
		return pathSummary(input)
	case toolNameWriteFile:
		return writeSummary(input)
	case toolNameEditFile:
		return pathSummary(input)
	case toolNameBash:
		return bashSummary(input)
	case toolNameFetch:
		return fetchSummary(input)
	case toolNameWebSearch:
		return querySummary(input)
	case toolNameGlob:
		return globSummary(input)
	case toolNameGrep:
		return grepSummary(input)
	case toolNameTaskCreate, toolNameTaskUpdate, toolNameTaskGet, toolNameTaskList:
		return ""
	case toolNameSkill:
		return skillSummary(input)
	default:
		return ""
	}
}

func pathSummary(input json.RawMessage) string {
	var args pathSummaryArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return ""
	}
	return strings.TrimSpace(args.Path)
}

func writeSummary(input json.RawMessage) string {
	var args writeSummaryArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return ""
	}
	path := strings.TrimSpace(args.Path)
	if path == "" {
		return ""
	}
	return path + " (" + strconv.Itoa(len(args.Content)) + " bytes)"
}

func bashSummary(input json.RawMessage) string {
	var args bashSummaryArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return ""
	}
	if args.Command != "" {
		return args.Command
	}
	return args.Legacy
}

func fetchSummary(input json.RawMessage) string {
	var args fetchSummaryArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return ""
	}
	method := strings.ToUpper(strings.TrimSpace(args.Method))
	url := strings.TrimSpace(args.URL)
	switch {
	case method != "" && url != "":
		return method + " " + url
	case url != "":
		return url
	default:
		return ""
	}
}

func querySummary(input json.RawMessage) string {
	var args searchSummaryArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return ""
	}
	return strings.TrimSpace(args.Query)
}

func globSummary(input json.RawMessage) string {
	var args globSummaryArgs
	if err := json.Unmarshal(input, &args); err != nil || strings.TrimSpace(args.Pattern) == "" {
		return ""
	}
	return summaryInPath(args.Pattern, args.Root)
}

func grepSummary(input json.RawMessage) string {
	var args grepSummaryArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return ""
	}
	pattern := args.Pattern
	if pattern == "" {
		pattern = args.Legacy
	}
	if strings.TrimSpace(pattern) == "" {
		return ""
	}
	return summaryInPath(pattern, args.Path)
}

func summaryInPath(value, path string) string {
	value = strings.TrimSpace(value)
	path = strings.TrimSpace(path)
	if path == "" {
		return value
	}
	return value + " in " + path
}

func skillSummary(input json.RawMessage) string {
	var args skillSummaryArgs
	if err := json.Unmarshal(input, &args); err != nil {
		return ""
	}
	return strings.TrimSpace(args.Name)
}
