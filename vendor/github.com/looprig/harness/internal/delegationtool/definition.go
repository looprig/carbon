package delegationtool

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/looprig/harness/pkg/loop"
	"github.com/looprig/harness/pkg/tool"
	inferencemodel "github.com/looprig/inference/model"
)

var agentToolNames = []string{"ListAgents", "MessageAgent", "StartAgent", "StopAgent"}

const startAgentDescPrefix = "Start a new in-session child agent and optionally wait for its response."

// Definition binds the harness-owned agent collaboration tools to one parent Loop.
func Definition(style loop.DelegationStyle, catalog []AgentCatalogEntry, runtimeCatalog ...loop.RuntimeCatalog) tool.Definition {
	config := newAgentToolConfig(style, catalog, runtimeCatalog...)
	return tool.NewBundleDefinition("AgentTools", agentToolNames, tool.RequiresDelegateController, func(_ context.Context, bindings tool.Bindings) ([]tool.InvokableTool, error) {
		return []tool.InvokableTool{
			newListAgents(bindings.Delegate, config),
			newMessageAgent(bindings.Delegate, config),
			newStartAgent(bindings.Delegate, config),
			newStopAgent(bindings.Delegate, config),
		}, nil
	})
}

const (
	maxAvailableAgentRows         = 24
	maxAvailableAgentRuntimeRows  = 64
	maxAvailableAgentRowBytes     = 768
	maxStartAgentDescriptionBytes = 32 << 10
	availableAgentElisionMarker   = "<elided additional agent capabilities>"
)

func buildStartAgentSchema(style loop.DelegationStyle, catalog []AgentCatalogEntry, runtimeCatalog loop.RuntimeCatalog) string {
	properties := map[string]any{
		"agent_type":        map[string]any{"type": "string"},
		"name":              map[string]any{"type": "string"},
		"instructions":      map[string]any{"type": "string"},
		"wait_for_response": map[string]any{"type": "boolean", "default": true},
		"timeout_seconds":   map[string]any{"type": "integer", "minimum": 0, "maximum": maxTimeoutSeconds},
		"model":             map[string]any{"type": "string"},
		"effort":            map[string]any{"type": "string"},
	}
	selectors := availableRuntimeSelectors(catalog, runtimeCatalog)
	if selectors.Harness {
		properties["agent_harness"] = map[string]any{"type": "string"}
	}
	if selectors.Source {
		properties["agent_source"] = map[string]any{"type": "string"}
	}
	if agentModeSelectable(catalog) {
		properties["agent_mode"] = map[string]any{"type": "string"}
	}
	if style == loop.DelegationSyncOnly {
		properties["wait_for_response"] = map[string]any{"type": "boolean", "const": true, "default": true}
	}
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
		"required":             []string{"agent_type", "instructions"},
		"oneOf":                startRoleVariants(catalog, runtimeCatalog),
	}
	encoded, _ := json.Marshal(schema)
	return string(encoded)
}

func buildMessageAgentSchema(style loop.DelegationStyle) string {
	wait := map[string]any{"type": "boolean", "default": true}
	if style == loop.DelegationSyncOnly {
		wait["const"] = true
	}
	return marshalAgentSchema(map[string]any{
		"agent_id":          map[string]any{"type": "string"},
		"message":           map[string]any{"type": "string"},
		"wait_for_response": wait,
		"timeout_seconds":   map[string]any{"type": "integer", "minimum": 0, "maximum": maxTimeoutSeconds},
	}, []string{"agent_id", "message"})
}

func buildListAgentsSchema() string {
	return marshalAgentSchema(map[string]any{"agent_id": map[string]any{"type": "string"}}, nil)
}

func buildStopAgentSchema() string {
	return marshalAgentSchema(map[string]any{"agent_id": map[string]any{"type": "string"}}, []string{"agent_id"})
}

func marshalAgentSchema(properties map[string]any, required []string) string {
	schema := map[string]any{"type": "object", "additionalProperties": false, "properties": properties}
	if len(required) > 0 {
		schema["required"] = required
	}
	encoded, _ := json.Marshal(schema)
	return string(encoded)
}

func agentModeSelectable(catalog []AgentCatalogEntry) bool {
	for _, role := range catalog {
		if len(selectableAgentModes(role.Modes)) >= 2 {
			return true
		}
	}
	return false
}

func selectableAgentModes(modes []loop.ModeName) []string {
	distinct := make(map[string]struct{}, len(modes))
	for _, mode := range modes {
		if mode != "" {
			distinct[string(mode)] = struct{}{}
		}
	}
	result := make([]string, 0, len(distinct))
	for mode := range distinct {
		result = append(result, mode)
	}
	sort.Strings(result)
	return result
}

func requiredProperties(names []string) []any {
	result := make([]any, len(names))
	for i, name := range names {
		result[i] = map[string]any{"required": []string{name}}
	}
	return result
}

func startRoleVariants(catalog []AgentCatalogEntry, runtimeCatalog loop.RuntimeCatalog) []any {
	catalog = orderedAgentCatalog(catalog)
	variants := make([]any, 0, len(catalog))
	for _, role := range catalog {
		entries := runtimeCatalog.EntriesFor(role.Name)
		if len(entries) == 0 {
			if runtimeCatalog.HasEntries() {
				continue
			}
			variants = append(variants, startRoleVariant(role, nil, false, false))
			continue
		}
		if !runtimeHarnessSelectable(entries) {
			defaultEntry := runtimeDefaultEntry(entries)
			variants = append(variants, startRoleChoiceForHarness(role, runtimeEntriesForHarness(entries, defaultEntry.AgentHarness), false, true))
			continue
		}
		defaultEntry := runtimeDefaultEntry(entries)
		harnesses := runtimeHarnessNames(entries)
		harnessBranches := []any{startRoleChoiceForHarness(role, runtimeEntriesForHarness(entries, defaultEntry.AgentHarness), false, true)}
		for _, harness := range harnesses {
			harnessEntries := runtimeEntriesForHarness(entries, harness)
			harnessBranches = append(harnessBranches, startRoleChoiceForHarness(role, harnessEntries, true, harness == defaultEntry.AgentHarness))
		}
		variants = append(variants, map[string]any{
			"type": "object",
			"properties": map[string]any{
				"agent_harness": map[string]any{"type": "string", "enum": harnesses},
			},
			"oneOf": harnessBranches,
		})
	}
	return variants
}

func startRoleVariant(role AgentCatalogEntry, entry *loop.RuntimeCatalogEntry, explicitHarness, defaultBranch bool) map[string]any {
	return startRoleVariantWithSelectors(role, entry, explicitHarness, defaultBranch, false, false, false)
}

func startRoleChoiceForHarness(role AgentCatalogEntry, entries []loop.RuntimeCatalogEntry, explicitHarness, defaultSource bool) any {
	if len(entries) == 0 {
		return startRoleVariant(role, nil, explicitHarness, !explicitHarness && defaultSource)
	}
	if runtimeSourceSelectableForEntries(entries) {
		return startRoleSourceVariants(role, entries, explicitHarness, defaultSource)
	}
	entry := runtimeDefaultEntry(entries)
	return startRoleVariantWithSelectors(role, &entry, explicitHarness, !explicitHarness && defaultSource, false, false, false)
}

func startRoleSourceVariants(role AgentCatalogEntry, entries []loop.RuntimeCatalogEntry, explicitHarness, defaultSource bool) map[string]any {
	branches := make([]any, 0, len(entries)+1)
	if defaultSource || explicitHarness {
		entry := runtimeDefaultEntry(entries)
		if filtered, ok := runtimeEntryForSource([]loop.RuntimeCatalogEntry{entry}, entry.Source); ok {
			entry = filtered
		}
		branches = append(branches, startRoleVariantWithSelectors(role, &entry, explicitHarness, !explicitHarness, false, true, true))
	}
	for _, source := range runtimeSourcesForEntries(entries) {
		entry, ok := runtimeEntryForSource(entries, source)
		if !ok {
			continue
		}
		branches = append(branches, startRoleVariantWithSelectors(role, &entry, explicitHarness, false, true, true, false))
	}
	return map[string]any{"oneOf": branches}
}

func startRoleVariantWithSelectors(role AgentCatalogEntry, entry *loop.RuntimeCatalogEntry, explicitHarness, defaultHarness, explicitSource, sourceSelectable, defaultSource bool) map[string]any {
	properties := map[string]any{
		"agent_type":        map[string]any{"const": string(role.Name)},
		"name":              map[string]any{"type": "string"},
		"instructions":      map[string]any{"type": "string"},
		"wait_for_response": map[string]any{"type": "boolean"},
		"timeout_seconds":   map[string]any{"type": "integer", "minimum": 0, "maximum": maxTimeoutSeconds},
	}
	modes := selectableAgentModes(role.Modes)
	if len(modes) >= 2 {
		properties["agent_mode"] = map[string]any{"type": "string", "enum": modes}
	}
	if entry != nil {
		if explicitHarness {
			properties["agent_harness"] = map[string]any{"const": string(entry.AgentHarness)}
		}
		if explicitSource {
			properties["agent_source"] = map[string]any{"const": string(entry.Source)}
		}
	}
	branch := map[string]any{"type": "object", "additionalProperties": false, "properties": properties}
	required := make([]string, 0, 2)
	notRequired := make([]string, 0, 2)
	if entry != nil {
		if runtimeModelEffortsVary(entry.Models) {
			properties["model"] = map[string]any{"type": "string"}
			properties["effort"] = map[string]any{"type": "string"}
			branch["oneOf"] = modelEffortVariants(entry.Models, entry.DefaultModel)
		} else {
			addModelAndEffort(properties, *entry)
		}
	}
	if explicitHarness {
		required = append(required, "agent_harness")
	} else if defaultHarness {
		notRequired = append(notRequired, "agent_harness")
	}
	if explicitSource {
		required = append(required, "agent_source")
	} else if sourceSelectable && defaultSource {
		notRequired = append(notRequired, "agent_source")
	}
	if len(notRequired) > 0 {
		branch["not"] = map[string]any{"anyOf": requiredProperties(notRequired)}
	}
	if len(required) > 0 {
		branch["required"] = required
	}
	return branch
}

func runtimeModelEffortsVary(models []loop.RuntimeModelOption) bool {
	if len(models) < 2 {
		return false
	}
	want := admittedEfforts(models[:1])
	for _, model := range models[1:] {
		if !equalStringSlices(want, admittedEfforts([]loop.RuntimeModelOption{model})) {
			return true
		}
	}
	return false
}

func equalStringSlices(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func modelEffortVariants(models []loop.RuntimeModelOption, defaultModel loop.ModelAlias) []any {
	defaultEfforts := admittedEfforts(models[:1])
	for _, model := range models {
		if model.Alias == defaultModel {
			defaultEfforts = admittedEfforts([]loop.RuntimeModelOption{model})
			break
		}
	}
	variants := []any{map[string]any{
		"not": map[string]any{"required": []string{"model"}},
		"properties": map[string]any{
			"effort": map[string]any{"type": "string", "enum": defaultEfforts},
		},
	}}
	for _, model := range models {
		variants = append(variants, map[string]any{
			"properties": map[string]any{
				"model":  map[string]any{"const": string(model.Alias)},
				"effort": map[string]any{"type": "string", "enum": admittedEfforts([]loop.RuntimeModelOption{model})},
			},
			"required": []string{"model"},
		})
	}
	return variants
}

type runtimeSelectorAvailability struct {
	Harness bool
	Source  bool
	Model   bool
	Effort  bool
}

func runtimeHarnessSelectable(entries []loop.RuntimeCatalogEntry) bool {
	return len(runtimeHarnessNames(entries)) > 1
}

func runtimeHarnessNames(entries []loop.RuntimeCatalogEntry) []loop.AgentHarnessName {
	seen := make(map[loop.AgentHarnessName]struct{}, len(entries))
	for _, entry := range entries {
		seen[entry.AgentHarness] = struct{}{}
	}
	result := make([]loop.AgentHarnessName, 0, len(seen))
	for harness := range seen {
		result = append(result, harness)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func runtimeEntriesForHarness(entries []loop.RuntimeCatalogEntry, harness loop.AgentHarnessName) []loop.RuntimeCatalogEntry {
	result := make([]loop.RuntimeCatalogEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.AgentHarness == harness {
			result = append(result, entry)
		}
	}
	return result
}

func runtimeEntrySources(entry loop.RuntimeCatalogEntry) []loop.RuntimeSourceName {
	seen := map[loop.RuntimeSourceName]struct{}{entry.Source: {}}
	for _, option := range entry.Models {
		source := option.Source
		if source == "" {
			source = entry.Source
		}
		seen[source] = struct{}{}
	}
	result := make([]loop.RuntimeSourceName, 0, len(seen))
	for source := range seen {
		if source != "" {
			result = append(result, source)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func runtimeSourcesForEntries(entries []loop.RuntimeCatalogEntry) []loop.RuntimeSourceName {
	seen := make(map[loop.RuntimeSourceName]struct{})
	for _, entry := range entries {
		for _, source := range runtimeEntrySources(entry) {
			seen[source] = struct{}{}
		}
	}
	result := make([]loop.RuntimeSourceName, 0, len(seen))
	for source := range seen {
		result = append(result, source)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func runtimeSourceSelectableForEntries(entries []loop.RuntimeCatalogEntry) bool {
	return len(runtimeSourcesForEntries(entries)) > 1
}

func runtimeSourceSelectable(entries []loop.RuntimeCatalogEntry) bool {
	for _, harness := range runtimeHarnessNames(entries) {
		if runtimeSourceSelectableForEntries(runtimeEntriesForHarness(entries, harness)) {
			return true
		}
	}
	return false
}

func runtimeEntryForSource(entries []loop.RuntimeCatalogEntry, source loop.RuntimeSourceName) (loop.RuntimeCatalogEntry, bool) {
	for _, entry := range entries {
		if entry.Source == source {
			if filtered, ok := runtimeEntryViewForSource(entry, source); ok {
				return filtered, true
			}
		}
	}
	for _, entry := range entries {
		for _, candidate := range runtimeEntrySources(entry) {
			if candidate == source {
				if filtered, ok := runtimeEntryViewForSource(entry, source); ok {
					return filtered, true
				}
			}
		}
	}
	return loop.RuntimeCatalogEntry{}, false
}

// runtimeEntryViewForSource returns the catalog entry as seen by one source
// branch. A model option's source override is authoritative, so a mixed entry
// must not leak options from another source into this branch. The returned
// default model is also made source-local when the entry-level default belongs
// to a different source.
func runtimeEntryViewForSource(entry loop.RuntimeCatalogEntry, source loop.RuntimeSourceName) (loop.RuntimeCatalogEntry, bool) {
	if source == "" {
		return loop.RuntimeCatalogEntry{}, false
	}
	if len(entry.Models) == 0 {
		if entry.Source != source {
			return loop.RuntimeCatalogEntry{}, false
		}
		entry.Source = source
		return entry, true
	}

	filtered := make([]loop.RuntimeModelOption, 0, len(entry.Models))
	for _, option := range entry.Models {
		optionSource := option.Source
		if optionSource == "" {
			optionSource = entry.Source
		}
		if optionSource == source {
			filtered = append(filtered, option)
		}
	}
	if len(filtered) == 0 {
		return loop.RuntimeCatalogEntry{}, false
	}

	entry.Source = source
	entry.Models = filtered
	defaultBelongsToSource := false
	for _, option := range filtered {
		if option.Alias == entry.DefaultModel {
			defaultBelongsToSource = true
			break
		}
	}
	if !defaultBelongsToSource {
		entry.DefaultModel = filtered[0].Alias
	}
	return entry, true
}

func runtimeModelSelectable(entry loop.RuntimeCatalogEntry) bool {
	return len(entry.Models) > 1
}

func runtimeEffortSelectable(entry loop.RuntimeCatalogEntry) bool {
	return len(admittedEfforts(entry.Models)) > 1
}

func runtimeDefaultEntry(entries []loop.RuntimeCatalogEntry) loop.RuntimeCatalogEntry {
	if len(entries) == 0 {
		return loop.RuntimeCatalogEntry{}
	}
	for _, entry := range entries {
		if entry.Default {
			return entry
		}
	}
	return entries[0]
}

func runtimeAdvertisedSelectors(entries []loop.RuntimeCatalogEntry, selected loop.RuntimeCatalogEntry) runtimeSelectorAvailability {
	explicitRuntime := selected.SelectionKind != loop.RuntimeSelectionHarnessManaged && len(selected.Models) > 0
	return runtimeSelectorAvailability{
		Harness: runtimeHarnessSelectable(entries),
		Source:  runtimeSourceSelectableForEntries(runtimeEntriesForHarness(entries, selected.AgentHarness)),
		Model:   explicitRuntime,
		Effort:  explicitRuntime,
	}
}

func availableRuntimeSelectors(catalog []AgentCatalogEntry, runtimeCatalog loop.RuntimeCatalog) runtimeSelectorAvailability {
	available := runtimeSelectorAvailability{}
	for _, role := range catalog {
		entries := runtimeCatalog.EntriesFor(role.Name)
		if len(entries) == 0 {
			continue
		}
		if runtimeHarnessSelectable(entries) {
			available.Harness = true
		}
		if runtimeSourceSelectable(entries) {
			available.Source = true
		}
		for _, entry := range entries {
			if runtimeModelSelectable(entry) {
				available.Model = true
			}
			if runtimeEffortSelectable(entry) {
				available.Effort = true
			}
		}
	}
	return available
}

func addModelAndEffort(properties map[string]any, entry loop.RuntimeCatalogEntry) {
	if len(entry.Models) > 0 {
		models := make([]string, len(entry.Models))
		for i, model := range entry.Models {
			models[i] = string(model.Alias)
		}
		properties["model"] = map[string]any{"type": "string", "enum": models}
	}
	efforts := admittedEfforts(entry.Models)
	if len(efforts) > 0 {
		properties["effort"] = map[string]any{"type": "string", "enum": efforts}
	}
}

func admittedEfforts(models []loop.RuntimeModelOption) []string {
	seen := make(map[string]struct{})
	efforts := make([]string, 0)
	for _, option := range models {
		for _, effort := range option.Efforts {
			value := string(effort)
			if effort == "" {
				value = "none"
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			efforts = append(efforts, value)
		}
	}
	sort.SliceStable(efforts, func(i, j int) bool { return effortOrder(efforts[i]) < effortOrder(efforts[j]) })
	return efforts
}

func effortOrder(effort string) int {
	switch effort {
	case "none":
		return 0
	case "low":
		return 1
	case "medium":
		return 2
	case "high":
		return 3
	case "max":
		return 4
	default:
		return 5
	}
}

type availableAgentRuntimeRow struct {
	defaultRuntime     bool
	harness            string
	source             string
	model              string
	efforts            []string
	harnessDescription string
	modelDescription   string
}

type availableAgentSection struct {
	agentLine   string
	defaultLine string
	runtimeRows []string
}

func buildStartAgentDescription(catalog []AgentCatalogEntry, runtimeCatalog loop.RuntimeCatalog) string {
	if len(catalog) == 0 {
		return startAgentDescPrefix
	}

	ordered := orderedAgentCatalog(catalog)
	sections := make([]availableAgentSection, 0, min(len(ordered), maxAvailableAgentRows))
	elided := false
	for _, agent := range ordered {
		entries := runtimeCatalog.EntriesFor(agent.Name)
		if len(entries) == 0 && runtimeCatalog.HasEntries() {
			continue
		}
		if len(sections) == maxAvailableAgentRows {
			elided = true
			break
		}

		agentLine := "- " + string(agent.Name)
		if agent.Description != "" {
			agentLine += ": " + agent.Description
		}
		defaultLine, runtimeRows := availableAgentRuntimeLines(agent, entries)
		if len(agentLine) > maxAvailableAgentRowBytes || len(defaultLine) > maxAvailableAgentRowBytes {
			elided = true
			continue
		}
		sections = append(sections, availableAgentSection{agentLine: agentLine, defaultLine: defaultLine, runtimeRows: runtimeRows})
	}

	runtimeRows := len(sections)
	for sectionIndex := range sections {
		kept := sections[sectionIndex].runtimeRows[:0]
		for _, row := range sections[sectionIndex].runtimeRows {
			if len(row) > maxAvailableAgentRowBytes || runtimeRows == maxAvailableAgentRuntimeRows {
				elided = true
				continue
			}
			kept = append(kept, row)
			runtimeRows++
		}
		sections[sectionIndex].runtimeRows = kept
	}

	description := renderStartAgentDescription(sections, elided)
	for len(description) > maxStartAgentDescriptionBytes {
		removed := false
		for i := len(sections) - 1; i >= 0; i-- {
			if len(sections[i].runtimeRows) == 0 {
				continue
			}
			sections[i].runtimeRows = sections[i].runtimeRows[:len(sections[i].runtimeRows)-1]
			removed = true
			break
		}
		if !removed {
			if len(sections) == 0 {
				return startAgentDescPrefix
			}
			sections = sections[:len(sections)-1]
		}
		elided = true
		description = renderStartAgentDescription(sections, elided)
	}
	return description
}

func availableAgentRuntimeLines(agent AgentCatalogEntry, entries []loop.RuntimeCatalogEntry) (string, []string) {
	if len(entries) == 0 {
		return "- agent_type=" + string(agent.Name) + " default: harness=native model=default effort=default",
			[]string{"  - harness=native model=default efforts=[default]"}
	}

	defaultEntry := runtimeDefaultEntry(entries)
	defaultLine := "- agent_type=" + string(agent.Name) + " default: harness=" + string(defaultEntry.AgentHarness)
	if defaultEntry.Source != "" {
		defaultLine += " source=" + string(defaultEntry.Source)
	}
	if defaultEntry.SelectionKind != loop.RuntimeSelectionHarnessManaged {
		defaultModel := runtimeModelOption(defaultEntry, defaultEntry.DefaultModel)
		defaultSource := defaultModel.Source
		if defaultSource == "" {
			defaultSource = defaultEntry.Source
		}
		if defaultSource != "" && defaultSource != defaultEntry.Source {
			defaultLine = "- agent_type=" + string(agent.Name) + " default: harness=" + string(defaultEntry.AgentHarness) + " source=" + string(defaultSource)
		}
		defaultLine += " model=" + string(defaultEntry.DefaultModel) + " effort=" + renderedEffort(defaultModel.DefaultEffort)
	}

	rows := make([]availableAgentRuntimeRow, 0)
	for _, entry := range entries {
		if entry.SelectionKind == loop.RuntimeSelectionHarnessManaged {
			rows = append(rows, availableAgentRuntimeRow{
				defaultRuntime:     entry.Default,
				harness:            string(entry.AgentHarness),
				source:             string(entry.Source),
				harnessDescription: entry.Description,
			})
			continue
		}
		for _, source := range runtimeEntrySources(entry) {
			view, ok := runtimeEntryViewForSource(entry, source)
			if !ok {
				continue
			}
			for _, model := range view.Models {
				efforts := admittedEfforts([]loop.RuntimeModelOption{model})
				if len(efforts) == 0 {
					efforts = []string{renderedEffort(model.DefaultEffort)}
				}
				rows = append(rows, availableAgentRuntimeRow{
					defaultRuntime:     entry.Default,
					harness:            string(entry.AgentHarness),
					source:             string(source),
					model:              string(model.Alias),
					efforts:            efforts,
					harnessDescription: entry.Description,
					modelDescription:   model.Description,
				})
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		left, right := rows[i], rows[j]
		if left.defaultRuntime != right.defaultRuntime {
			return left.defaultRuntime
		}
		if left.harness != right.harness {
			return left.harness < right.harness
		}
		if left.source != right.source {
			return left.source < right.source
		}
		return left.model < right.model
	})

	lines := make([]string, 0, len(rows))
	previousRuntime := ""
	for _, row := range rows {
		line := "  - harness=" + row.harness
		if row.source != "" {
			line += " source=" + row.source
		}
		if row.model != "" {
			line += " model=" + row.model + " efforts=[" + strings.Join(row.efforts, ",") + "]"
		}
		runtimeKey := row.harness + "\x00" + row.source
		if row.harnessDescription != "" && runtimeKey != previousRuntime {
			line += " harness_description=" + strconv.Quote(row.harnessDescription)
		}
		if row.modelDescription != "" {
			line += " model_description=" + strconv.Quote(row.modelDescription)
		}
		lines = append(lines, line)
		previousRuntime = runtimeKey
	}
	return defaultLine, lines
}

func runtimeModelOption(entry loop.RuntimeCatalogEntry, alias loop.ModelAlias) loop.RuntimeModelOption {
	for _, option := range entry.Models {
		if option.Alias == alias {
			return option
		}
	}
	return loop.RuntimeModelOption{}
}

func renderedEffort(effort inferencemodel.Effort) string {
	if effort == "" {
		return "none"
	}
	return string(effort)
}

func renderStartAgentDescription(sections []availableAgentSection, elided bool) string {
	var b strings.Builder
	b.WriteString(startAgentDescPrefix)
	b.WriteString("\n<available_agents>\n")
	for _, section := range sections {
		b.WriteString(section.agentLine)
		b.WriteByte('\n')
	}
	b.WriteString("</available_agents>\n\n<available_agent_runtimes>\n")
	for _, section := range sections {
		b.WriteString(section.defaultLine)
		b.WriteByte('\n')
		for _, row := range section.runtimeRows {
			b.WriteString(row)
			b.WriteByte('\n')
		}
	}
	if elided {
		b.WriteString("- ")
		b.WriteString(availableAgentElisionMarker)
		b.WriteByte('\n')
	}
	b.WriteString("</available_agent_runtimes>")
	return b.String()
}

func orderedAgentCatalog(catalog []AgentCatalogEntry) []AgentCatalogEntry {
	ordered := cloneAgentCatalog(catalog)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Name < ordered[j].Name })
	return ordered
}
