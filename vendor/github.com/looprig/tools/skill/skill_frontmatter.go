package skill

import "strings"

// maxSkillBytes is the hard ceiling on a SKILL.md document. A skill is a curated
// markdown file injected into an agent's context; 64 KiB is generous for a
// checklist-style prompt while bounding the work the parser does on any single
// (potentially attacker-influenced) document. Anything larger is rejected before
// parsing as a *MalformedSkillError.
const maxSkillBytes = 64 * 1024

// fence is the YAML-style frontmatter delimiter line. A well-formed SKILL.md
// opens with a fence line, carries flat key: value frontmatter, closes with a
// second fence line, and is followed by the markdown body.
const fence = "---"

// SkillMeta is the flat, typed view of a SKILL.md frontmatter block. Only the
// fields the loader needs are surfaced; every other frontmatter key is parsed
// (to detect duplicates / malformed lines) but otherwise ignored. The
// frontmatter is treated as inert data — values are trimmed but never executed,
// interpolated, or otherwise interpreted.
type SkillMeta struct {
	Name        string
	Description string
}

// parseSkill splits a SKILL.md document into its frontmatter (a flat key: value
// block fenced by `---` lines) and its markdown body. It is fail-secure: any
// ambiguity — oversize input, a missing opening fence, an unterminated fence, a
// frontmatter line that is not `key: value`, or a duplicate recognised key —
// returns a *MalformedSkillError and zero values for meta and body. It never
// returns a partial parse.
//
// Frontmatter parsing reads only the recognised keys (name, description). Blank
// lines and `#`-prefixed comment lines inside the frontmatter are skipped.
// Unknown keys are tolerated and ignored, but every key is still checked for the
// duplicate-key rule so an unknown duplicate of a recognised key cannot smuggle
// a second value past us. Both LF and CRLF line endings are accepted; values are
// trimmed of surrounding whitespace. Nothing in the document is interpreted.
func parseSkill(raw []byte) (meta SkillMeta, body string, err error) {
	if len(raw) > maxSkillBytes {
		return SkillMeta{}, "", &MalformedSkillError{
			Reason: "document exceeds the maximum skill size",
		}
	}

	// Normalize CRLF -> LF so the line scanner sees one newline convention. This
	// is a copy of bounded (<= maxSkillBytes) data.
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")

	// Skip leading blank lines, then require the opening fence as its own line.
	rest := strings.TrimLeft(text, "\n")
	if rest != fence && !strings.HasPrefix(rest, fence+"\n") {
		return SkillMeta{}, "", &MalformedSkillError{
			Reason: "missing opening frontmatter fence",
		}
	}
	// Drop the opening fence line.
	if i := strings.IndexByte(rest, '\n'); i >= 0 {
		rest = rest[i+1:]
	} else {
		// rest == fence with no trailing newline => no closing fence can follow.
		return SkillMeta{}, "", &MalformedSkillError{
			Reason: "missing closing frontmatter fence",
		}
	}

	// Find the closing fence line.
	frontmatter, after, ok := splitAtClosingFence(rest)
	if !ok {
		return SkillMeta{}, "", &MalformedSkillError{
			Reason: "missing closing frontmatter fence",
		}
	}

	meta, err = parseFrontmatterLines(frontmatter)
	if err != nil {
		return SkillMeta{}, "", err
	}

	return meta, after, nil
}

// splitAtClosingFence scans rest line-by-line for a line equal to the fence
// delimiter. It returns the frontmatter content before the fence, the body after
// it, and ok=false if no closing fence is found.
func splitAtClosingFence(rest string) (frontmatter, body string, ok bool) {
	offset := 0
	for offset <= len(rest) {
		nl := strings.IndexByte(rest[offset:], '\n')
		var line string
		var lineEnd int // index in rest just past this line's newline (or len for last line)
		if nl < 0 {
			line = rest[offset:]
			lineEnd = len(rest)
		} else {
			line = rest[offset : offset+nl]
			lineEnd = offset + nl + 1
		}
		if line == fence {
			return rest[:offset], rest[lineEnd:], true
		}
		if nl < 0 {
			break
		}
		offset = lineEnd
	}
	return "", "", false
}

// parseFrontmatterLines parses a flat key: value block. Blank lines and
// `#`-comment lines are skipped. Every other line must be `key: value`; a line
// without a colon is malformed. A recognised key (name, description) appearing
// twice is a duplicate and is malformed. Unknown keys are validated for shape
// then ignored.
func parseFrontmatterLines(frontmatter string) (SkillMeta, error) {
	var meta SkillMeta
	seenName := false
	seenDesc := false

	for _, rawLine := range strings.Split(frontmatter, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			return SkillMeta{}, &MalformedSkillError{
				Reason: "frontmatter line is not key: value",
			}
		}
		key := strings.TrimSpace(line[:colon])
		value := strings.TrimSpace(line[colon+1:])

		switch key {
		case "name":
			if seenName {
				return SkillMeta{}, &MalformedSkillError{Reason: "duplicate key name"}
			}
			seenName = true
			meta.Name = value
		case "description":
			if seenDesc {
				return SkillMeta{}, &MalformedSkillError{Reason: "duplicate key description"}
			}
			seenDesc = true
			meta.Description = value
		default:
			// Unknown key: shape already validated (has a colon); ignored.
		}
	}

	return meta, nil
}
