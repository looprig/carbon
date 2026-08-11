package presentation

import (
	"strconv"
	"strings"
)

const structuredOutputToolName = "_looprig_final_output"

// toolActivity is the user-facing action performed by one concrete tool. Keeping both
// forms explicit handles irregular phrases such as "file search" / "file searches".
type toolActivity struct {
	singular string
	plural   string
}

var (
	unknownToolActivity   = toolActivity{singular: "tool used", plural: "tools used"}
	builtinToolActivities = map[string]toolActivity{
		"ReadFile":      {singular: "file read", plural: "files read"},
		"WriteFile":     {singular: "file written", plural: "files written"},
		"EditFile":      {singular: "file edited", plural: "files edited"},
		"Glob":          {singular: "file search", plural: "file searches"},
		"Grep":          {singular: "content search", plural: "content searches"},
		"Bash":          {singular: "command executed", plural: "commands executed"},
		"ProcessOutput": {singular: "process checked", plural: "processes checked"},
		"ProcessInput":  {singular: "process input sent", plural: "process inputs sent"},
		"ProcessStop":   {singular: "process stopped", plural: "processes stopped"},
		"WebSearch":     {singular: "web search", plural: "web searches"},
		"Fetch":         {singular: "page fetched", plural: "pages fetched"},
		"TaskCreate":    {singular: "task created", plural: "tasks created"},
		"TaskUpdate":    {singular: "task updated", plural: "tasks updated"},
		"TaskGet":       {singular: "task read", plural: "tasks read"},
		"TaskList":      {singular: "task list viewed", plural: "task lists viewed"},
		"AskUser":       {singular: "question asked", plural: "questions asked"},
		"Skill":         {singular: "skill loaded", plural: "skills loaded"},
		"StartAgent":    {singular: "agent started", plural: "agents started"},
		"MessageAgent":  {singular: "agent message sent", plural: "agent messages sent"},
		"ListAgents":    {singular: "agent check", plural: "agent checks"},
		"StopAgent":     {singular: "agent stopped", plural: "agents stopped"},
	}
)

type toolActivityGroup struct {
	activity toolActivity
	fixed    string
	count    int
	failed   bool
}

// toolRunSummary builds the collapsed label for a run of tool calls by grouping concrete
// names into user-facing activities in first-use order. Unknown tools share "tools used";
// synthetic subagent cards retain their named presentation. A group is marked " ✗" when
// any call in it failed, and the second return tints the whole node red when any group failed.
func toolRunSummary(calls []ToolCallView) (string, bool) {
	groups := make([]toolActivityGroup, 0, len(calls))
	groupIndexes := make(map[toolActivity]int, len(calls))
	anyFailed := false

	for _, call := range calls {
		if call.ToolName == structuredOutputToolName {
			continue
		}

		failed := toolCallFailed(call)
		if failed {
			anyFailed = true
		}
		if call.Agent != "" {
			groups = append(groups, toolActivityGroup{
				fixed:  call.ToolName + "(" + call.Agent + ")",
				count:  1,
				failed: failed,
			})
			continue
		}

		activity, known := builtinToolActivities[call.ToolName]
		if !known {
			activity = unknownToolActivity
		}
		if index, exists := groupIndexes[activity]; exists {
			groups[index].count++
			groups[index].failed = groups[index].failed || failed
			continue
		}
		groupIndexes[activity] = len(groups)
		groups = append(groups, toolActivityGroup{activity: activity, count: 1, failed: failed})
	}

	parts := make([]string, 0, len(groups))
	for _, group := range groups {
		part := group.fixed
		if part == "" {
			phrase := group.activity.plural
			if group.count == 1 {
				phrase = group.activity.singular
			}
			part = strconv.Itoa(group.count) + " " + phrase
		}
		if group.failed {
			part += " ✗"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", "), anyFailed
}

// toolCallFailed reports whether a committed call (tool or subagent) ended in failure.
func toolCallFailed(c ToolCallView) bool {
	if c.Agent != "" {
		return c.SubStatus == subFailed || c.SubStatus == subInterrupted
	}
	return c.Status == ToolError || c.Status == ToolCancelled
}
