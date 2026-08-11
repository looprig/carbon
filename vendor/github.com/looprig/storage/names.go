package storage

import "strconv"

// maxNameLen is the inclusive upper bound, in bytes, on a storage name.
const maxNameLen = 512

// InvalidNameError reports a name that violates the storage grammar: one or
// more segments joined by single '/', each segment matching
// [a-z0-9][a-z0-9_.-]*, no leading/trailing '/', at most 512 bytes total.
type InvalidNameError struct {
	Name string
	Rule string
}

func (e *InvalidNameError) Error() string {
	return "storage: invalid name " + strconv.Quote(e.Name) + ": " + e.Rule
}

// ValidateName returns a non-nil *InvalidNameError if name violates the storage
// name grammar, or nil if it is valid. The grammar is canonical by construction:
// empty, ".", and ".." segments are unrepresentable, so no two valid names alias
// one backend location.
//
// A valid name is 1..512 bytes of one or more segments joined by single '/',
// with no leading, trailing, or doubled '/'. Each segment starts with a byte in
// [a-z0-9] and continues with bytes in [a-z0-9_.-].
func ValidateName(name string) error {
	if len(name) == 0 {
		return &InvalidNameError{Name: name, Rule: "empty"}
	}
	if len(name) > maxNameLen {
		return &InvalidNameError{Name: name, Rule: "too long"}
	}

	// Walk the segments delimited by '/'. i == len(name) closes the final
	// segment, so a trailing '/' surfaces as a closing empty segment.
	start := 0
	for i := 0; i <= len(name); i++ {
		if i < len(name) && name[i] != '/' {
			continue
		}
		if err := validateSegment(name, name[start:i]); err != nil {
			return err
		}
		start = i + 1
	}
	return nil
}

// validateSegment checks a single '/'-delimited segment against the grammar,
// attributing any violation to the whole name.
func validateSegment(name, seg string) error {
	if len(seg) == 0 {
		return &InvalidNameError{Name: name, Rule: "empty segment"}
	}
	if !isNameStart(seg[0]) {
		return &InvalidNameError{Name: name, Rule: "segment must start with [a-z0-9]"}
	}
	for j := 1; j < len(seg); j++ {
		if !isNameByte(seg[j]) {
			return &InvalidNameError{Name: name, Rule: "illegal byte in segment"}
		}
	}
	return nil
}

// isNameStart reports whether b is a legal first byte of a segment: [a-z0-9].
func isNameStart(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9')
}

// isNameByte reports whether b is a legal non-leading byte of a segment:
// [a-z0-9_.-].
func isNameByte(b byte) bool {
	return isNameStart(b) || b == '_' || b == '.' || b == '-'
}
