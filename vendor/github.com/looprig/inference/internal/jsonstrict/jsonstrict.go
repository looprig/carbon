// Package jsonstrict provides a small, dialect-agnostic JSON scan used by
// every codec/*api server-decode path to reject a request body that smuggles
// a duplicate object member name. encoding/json silently takes the last
// occurrence of a duplicate key; each dialect's DecodeRequest instead rejects
// the request outright so a client cannot smuggle a semantically different
// value past a naive review of the first occurrence.
//
// This package was extracted after the identical scan had been independently
// copy-pasted into four sibling packages (anthropicapi, geminiapi,
// openairesponses, openaiapi) — a threshold each implementer was told, at the
// time, not to preemptively generalize away, since only a fourth copy proves
// the repetition durable enough to warrant a shared dependency. It is
// unexported from outside the module (see internal/usagenorm for the same
// convention) because the scan is an implementation detail of those four
// dialects' decoders, not a public contract of this module.
package jsonstrict

import (
	"bytes"
	"encoding/json"
	"io"
)

// DuplicateKeyError reports the first duplicate JSON object member name found.
type DuplicateKeyError struct{ Key string }

func (e *DuplicateKeyError) Error() string {
	return "jsonstrict: duplicate JSON object key " + e.Key
}

// MalformedError reports a JSON body that could not be fully walked (a syntax
// error or mismatched delimiter encountered while scanning).
type MalformedError struct{ Detail string }

func (e *MalformedError) Error() string {
	return "jsonstrict: malformed JSON body: " + e.Detail
}

// RejectDuplicateKeys reports the first duplicate object member name found
// anywhere in raw (at any nesting depth), as *DuplicateKeyError, or nil if
// raw has none. A JSON syntax error is reported as *MalformedError: this
// function's job is not to validate JSON, but it must never silently accept
// a body it cannot fully walk.
func RejectDuplicateKeys(raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	type frame struct {
		isObject     bool
		keys         map[string]struct{}
		expectingKey bool
	}
	var stack []frame

	finishValue := func() {
		if len(stack) == 0 {
			return
		}
		top := &stack[len(stack)-1]
		if top.isObject && !top.expectingKey {
			top.expectingKey = true
		}
	}

	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return &MalformedError{Detail: err.Error()}
		}

		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				finishValue()
				stack = append(stack, frame{isObject: true, keys: make(map[string]struct{}), expectingKey: true})
			case '[':
				finishValue()
				stack = append(stack, frame{})
			case '}', ']':
				if len(stack) == 0 {
					return &MalformedError{Detail: "mismatched JSON delimiter"}
				}
				stack = stack[:len(stack)-1]
				finishValue()
			}
		case string:
			if len(stack) > 0 {
				top := &stack[len(stack)-1]
				if top.isObject && top.expectingKey {
					if _, dup := top.keys[t]; dup {
						return &DuplicateKeyError{Key: t}
					}
					top.keys[t] = struct{}{}
					top.expectingKey = false
					continue
				}
			}
			finishValue()
		default:
			finishValue()
		}
	}
}
