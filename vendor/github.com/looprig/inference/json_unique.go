package inference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

type duplicateMemberKind uint8

const (
	duplicateMemberNone duplicateMemberKind = iota
	duplicateMemberGeneric
	duplicateMemberSchemaKeyword
	duplicateMemberSchemaProperty
)

type jsonContainerFrame struct {
	delimiter    json.Delim
	memberKind   duplicateMemberKind
	keys         map[string]struct{}
	expectingKey bool
	pendingKey   string
}

// findDuplicateObjectMember scans valid JSON without materializing its values.
// JSON object names are decoded before comparison, so escaped equivalents are
// duplicates. Callers bound raw before calling this function.
func findDuplicateObjectMember(raw []byte, schema bool) (duplicateMemberKind, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	frames := make([]jsonContainerFrame, 0, 8)

	for {
		token, err := decoder.Token()
		if err != nil {
			if err == io.EOF {
				if len(frames) != 0 {
					return duplicateMemberNone, false, fmt.Errorf("incomplete JSON token stream")
				}
				return duplicateMemberNone, false, nil
			}
			return duplicateMemberNone, false, fmt.Errorf("decode JSON token: %w", err)
		}

		// json.Decoder.Token returns a serialization-boundary dynamic value.
		// Narrow it immediately and retain only concrete parser state.
		switch typed := token.(type) {
		case json.Delim:
			switch typed {
			case '{':
				memberKind := duplicateMemberGeneric
				if schema {
					memberKind = duplicateMemberSchemaKeyword
					if len(frames) > 0 {
						parent := &frames[len(frames)-1]
						if parent.delimiter == '{' && parent.memberKind == duplicateMemberSchemaKeyword && !parent.expectingKey && parent.pendingKey == "properties" {
							memberKind = duplicateMemberSchemaProperty
						}
					}
				}
				finishJSONValue(frames)
				frames = append(frames, jsonContainerFrame{
					delimiter:    typed,
					memberKind:   memberKind,
					keys:         make(map[string]struct{}),
					expectingKey: true,
				})
			case '[':
				finishJSONValue(frames)
				frames = append(frames, jsonContainerFrame{delimiter: typed})
			case '}', ']':
				if len(frames) == 0 || frames[len(frames)-1].delimiter+2 != typed {
					return duplicateMemberNone, false, fmt.Errorf("mismatched JSON delimiter")
				}
				frames = frames[:len(frames)-1]
			default:
				return duplicateMemberNone, false, fmt.Errorf("unsupported JSON delimiter")
			}
		case string:
			if len(frames) > 0 {
				frame := &frames[len(frames)-1]
				if frame.delimiter == '{' && frame.expectingKey {
					if _, exists := frame.keys[typed]; exists {
						return frame.memberKind, true, nil
					}
					frame.keys[typed] = struct{}{}
					frame.pendingKey = typed
					frame.expectingKey = false
					continue
				}
			}
			finishJSONValue(frames)
		case json.Number, float64, bool, nil:
			finishJSONValue(frames)
		default:
			return duplicateMemberNone, false, fmt.Errorf("unsupported JSON token type")
		}
	}
}

func finishJSONValue(frames []jsonContainerFrame) {
	if len(frames) == 0 {
		return
	}
	frame := &frames[len(frames)-1]
	if frame.delimiter == '{' && !frame.expectingKey {
		frame.expectingKey = true
		frame.pendingKey = ""
	}
}
