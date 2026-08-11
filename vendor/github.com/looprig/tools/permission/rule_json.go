package permission

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// rule_json.go is the strict schema-version-2 codec. Decoding disallows
// unknown fields at every level and decodes the match object with the exact
// shape of the record's enforcement class, so a wildcard or family command
// record structurally cannot carry a filesystem or network delta.

// fileV2 is the on-disk document: {"version":2,"normalization_version":1,...}.
type fileV2 struct {
	Version              int          `json:"version"`
	NormalizationVersion int          `json:"normalization_version"`
	Rules                []ruleRecord `json:"rules"`
}

// ruleRecord is one durable rule with its class-shaped match payload.
type ruleRecord struct {
	Effect           string          `json:"effect"`
	Capability       string          `json:"capability"`
	EnforcementClass string          `json:"enforcement_class"`
	Match            json.RawMessage `json:"match"`
}

// Per-class match payloads. Field sets are exact: strict decoding rejects
// anything extra.
type (
	matchExactCommand struct {
		Command string `json:"command"`
	}
	matchWildcard struct{}
	matchFamily   struct {
		Tokens            []string `json:"tokens"`
		TrailingArguments bool     `json:"trailing_arguments"`
	}
	matchNetworkTarget struct {
		Transport string `json:"transport,omitempty"`
		Host      string `json:"host"`
		Port      int    `json:"port,omitempty"`
	}
	matchBroadEgress struct {
		Command string `json:"command"`
		Target  string `json:"target"`
	}
	matchPath struct {
		Path string `json:"path"`
	}
	matchTree struct {
		Root string `json:"root"`
	}
	matchHostBound struct {
		Command string `json:"command"`
	}
)

// FileErrorReason classifies a fatal permission-file failure.
type FileErrorReason string

// Fatal file-failure reasons. Hardening reasons are produced by the store;
// schema reasons by this codec.
const (
	FileMalformed          FileErrorReason = "malformed"
	FileVersionUnsupported FileErrorReason = "version_unsupported"
	FileRuleInvalid        FileErrorReason = "rule_invalid"
	FileNotRegular         FileErrorReason = "not_regular"
	FileSymlink            FileErrorReason = "symlink"
	FileOwnerUnexpected    FileErrorReason = "owner_unexpected"
	FileModeUnexpected     FileErrorReason = "mode_unexpected"
	FileLinkCount          FileErrorReason = "link_count_unexpected"
	FileTooLarge           FileErrorReason = "too_large"
	FileMissing            FileErrorReason = "missing"
	FileIO                 FileErrorReason = "io"
	FileLock               FileErrorReason = "lock"
	FileReadOnly           FileErrorReason = "read_only"
	FileCandidateInvalid   FileErrorReason = "candidate_invalid"
)

// FileError is the typed fatal failure for loading or writing one
// permission file. Non-fatal rule diagnostics are reported separately as
// Diagnostic values, never as FileError.
type FileError struct {
	Path   string
	Reason FileErrorReason
	Err    error
}

func (e *FileError) Error() string {
	msg := "permission: file " + strconv.Quote(e.Path) + ": " + string(e.Reason)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

func (e *FileError) Unwrap() error { return e.Err }

// decodeFile parses one schema-version-2 permission file and validates every
// rule. Path context is attached by the caller.
func decodeFile(data []byte) ([]Rule, error) {
	var document fileV2
	if err := strictUnmarshal(data, &document); err != nil {
		return nil, &FileError{Reason: FileMalformed, Err: err}
	}
	if document.Version != SchemaVersion {
		return nil, &FileError{Reason: FileVersionUnsupported, Err: fmt.Errorf("schema version %d is not supported (want %d)", document.Version, SchemaVersion)}
	}
	if document.NormalizationVersion != NormalizationVersion {
		return nil, &FileError{Reason: FileVersionUnsupported, Err: fmt.Errorf("normalization version %d is not supported (want %d)", document.NormalizationVersion, NormalizationVersion)}
	}

	rules := make([]Rule, 0, len(document.Rules))
	for index, record := range document.Rules {
		rule, err := decodeRule(index, record)
		if err != nil {
			return nil, &FileError{Reason: FileRuleInvalid, Err: err}
		}
		rules = append(rules, rule)
	}
	return rules, nil
}

// decodeRule converts one record into a validated Rule.
func decodeRule(index int, record ruleRecord) (Rule, error) {
	rule := Rule{
		Effect:     Effect(record.Effect),
		Capability: record.Capability,
		Class:      record.EnforcementClass,
	}
	classes, ok := classesByCapability[rule.Capability]
	if !ok {
		return Rule{}, &RuleError{Index: index, Reason: "unknown capability " + strconv.Quote(rule.Capability)}
	}
	if !classes[rule.Class] {
		return Rule{}, &RuleError{Index: index, Reason: "enforcement class " + strconv.Quote(rule.Class) + " is not valid for capability " + strconv.Quote(rule.Capability)}
	}
	if len(record.Match) == 0 {
		return Rule{}, &RuleError{Index: index, Reason: "missing match object"}
	}

	var err error
	switch rule.Class {
	case ClassCommandInvoke:
		var match matchExactCommand
		if err = strictUnmarshal(record.Match, &match); err == nil {
			rule.Command = match.Command
		}
	case ClassCommandInvokeWildcard:
		var match matchWildcard
		err = strictUnmarshal(record.Match, &match)
	case ClassCommandInvokeFamily:
		var match matchFamily
		if err = strictUnmarshal(record.Match, &match); err == nil {
			rule.Tokens = match.Tokens
			rule.TrailingArguments = match.TrailingArguments
		}
	case ClassNetworkTarget:
		var match matchNetworkTarget
		if err = strictUnmarshal(record.Match, &match); err == nil {
			rule.Transport, rule.Host, rule.Port = match.Transport, match.Host, match.Port
		}
	case ClassNetworkBroad:
		var match matchBroadEgress
		if err = strictUnmarshal(record.Match, &match); err == nil {
			rule.Command, rule.Target = match.Command, match.Target
		}
	case ClassFilesystemPathRead, ClassFilesystemPathWrite:
		var match matchPath
		if err = strictUnmarshal(record.Match, &match); err == nil {
			rule.Path = match.Path
		}
	case ClassFilesystemTreeRead, ClassFilesystemTreeWrite:
		var match matchTree
		if err = strictUnmarshal(record.Match, &match); err == nil {
			rule.Root = match.Root
		}
	case ClassFilesystemHostRead, ClassFilesystemHostWrite:
		var match matchHostBound
		if err = strictUnmarshal(record.Match, &match); err == nil {
			rule.Command = match.Command
		}
	}
	if err != nil {
		return Rule{}, &RuleError{Index: index, Reason: "match does not fit enforcement class " + rule.Class + ": " + err.Error()}
	}
	if err := rule.validate(index); err != nil {
		return Rule{}, err
	}
	return rule, nil
}

// encodeFile serializes rules as one schema-version-2 document, refusing any
// rule the loader would reject.
func encodeFile(rules []Rule) ([]byte, error) {
	document := fileV2{Version: SchemaVersion, NormalizationVersion: NormalizationVersion, Rules: make([]ruleRecord, 0, len(rules))}
	for index, rule := range rules {
		if err := rule.validate(index); err != nil {
			return nil, &FileError{Reason: FileRuleInvalid, Err: err}
		}
		match, err := encodeMatch(index, rule)
		if err != nil {
			return nil, err
		}
		document.Rules = append(document.Rules, ruleRecord{
			Effect:           string(rule.Effect),
			Capability:       rule.Capability,
			EnforcementClass: rule.Class,
			Match:            match,
		})
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, &FileError{Reason: FileMalformed, Err: err}
	}
	return append(encoded, '\n'), nil
}

// encodeMatch renders the class-shaped match payload and rejects any stray
// field from another class so a command-only record cannot smuggle a
// capability delta into the file.
func encodeMatch(index int, rule Rule) (json.RawMessage, error) {
	if rule.carriesForeignFields() {
		return nil, &FileError{Reason: FileRuleInvalid, Err: &RuleError{Index: index, Reason: "rule carries fields outside its enforcement class " + rule.Class}}
	}
	var payload any
	switch rule.Class {
	case ClassCommandInvoke:
		payload = matchExactCommand{Command: rule.Command}
	case ClassCommandInvokeWildcard:
		payload = matchWildcard{}
	case ClassCommandInvokeFamily:
		payload = matchFamily{Tokens: rule.Tokens, TrailingArguments: rule.TrailingArguments}
	case ClassNetworkTarget:
		payload = matchNetworkTarget{Transport: rule.Transport, Host: rule.Host, Port: rule.Port}
	case ClassNetworkBroad:
		payload = matchBroadEgress{Command: rule.Command, Target: rule.Target}
	case ClassFilesystemPathRead, ClassFilesystemPathWrite:
		payload = matchPath{Path: rule.Path}
	case ClassFilesystemTreeRead, ClassFilesystemTreeWrite:
		payload = matchTree{Root: rule.Root}
	case ClassFilesystemHostRead, ClassFilesystemHostWrite:
		payload = matchHostBound{Command: rule.Command}
	default:
		return nil, &FileError{Reason: FileRuleInvalid, Err: &RuleError{Index: index, Reason: "unknown enforcement class"}}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, &FileError{Reason: FileMalformed, Err: err}
	}
	return encoded, nil
}

// carriesForeignFields reports whether the rule populates any field that does
// not belong to its enforcement class.
func (r Rule) carriesForeignFields() bool {
	remainder := r
	remainder.Effect, remainder.Capability, remainder.Class = "", "", ""
	switch r.Class {
	case ClassCommandInvoke, ClassFilesystemHostRead, ClassFilesystemHostWrite:
		remainder.Command = ""
	case ClassCommandInvokeWildcard:
		// No fields belong to the wildcard class.
	case ClassCommandInvokeFamily:
		remainder.Tokens, remainder.TrailingArguments = nil, false
	case ClassNetworkTarget:
		remainder.Transport, remainder.Host, remainder.Port = "", "", 0
	case ClassNetworkBroad:
		remainder.Command, remainder.Target = "", ""
	case ClassFilesystemPathRead, ClassFilesystemPathWrite:
		remainder.Path = ""
	case ClassFilesystemTreeRead, ClassFilesystemTreeWrite:
		remainder.Root = ""
	}
	return remainder.Command != "" || remainder.Tokens != nil || remainder.TrailingArguments ||
		remainder.Transport != "" || remainder.Host != "" || remainder.Port != 0 ||
		remainder.Target != "" || remainder.Path != "" || remainder.Root != ""
}

// strictUnmarshal decodes JSON rejecting unknown fields and trailing data.
func strictUnmarshal(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("trailing data after document")
	}
	return nil
}
