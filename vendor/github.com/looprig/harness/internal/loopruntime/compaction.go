package loopruntime

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"strings"

	"github.com/looprig/core/content"
	"github.com/looprig/harness/pkg/loop"
)

var compactionSummarySections = [...]string{"goal", "constraints", "decisions", "state", "open_items"}

// CompactionOutcome carries exactly one typed adapter result or failure while
// the hustle controller still owns finalization.
type CompactionOutcome struct {
	Value *loop.CompactionOutput
	Err   error
}

// CompactionOutcomeError reports a malformed adapter/finalizer handoff.
type CompactionOutcomeError struct{}

func (*CompactionOutcomeError) Error() string { return "loopruntime: invalid compaction outcome" }

// Validate enforces the exactly-one result contract.
func (o CompactionOutcome) Validate() error {
	if (o.Value == nil) == (o.Err == nil) {
		return &CompactionOutcomeError{}
	}
	return nil
}

// Compactor is the only hustle capability visible to the loop actor. It cannot
// select arbitrary definitions or run a generic hustle.
type Compactor interface {
	CompactAndFinalize(context.Context, loop.CompactionInput, func(context.Context, CompactionOutcome) error) error
}

// ParseCompactionSummaryXML validates and wraps the exact replacement grammar.
// It preserves the original escaped XML bytes in the returned user text block.
func ParseCompactionSummaryXML(raw []byte) (*content.UserMessage, error) {
	if bytes.Contains(raw, []byte("<![CDATA[")) {
		return nil, invalidCompactionXML(loop.InvalidSummaryXMLStructure, nil)
	}
	parser := compactionXMLParser{decoder: xml.NewDecoder(bytes.NewReader(raw))}
	if err := parser.parse(); err != nil {
		return nil, err
	}
	return &content.UserMessage{Message: content.Message{
		Role:   content.RoleUser,
		Blocks: []content.Block{&content.TextBlock{Text: string(raw)}},
	}}, nil
}

type compactionXMLParser struct {
	decoder     *xml.Decoder
	seenRoot    bool
	closedRoot  bool
	depth       int
	section     int
	sectionText [len(compactionSummarySections)]strings.Builder
}

func (p *compactionXMLParser) parse() error {
	for {
		token, err := p.decoder.Token()
		if err == io.EOF {
			return p.finish()
		}
		if err != nil {
			return invalidCompactionXML(loop.InvalidSummaryXMLSyntax, err)
		}
		if err := p.accept(token); err != nil {
			return err
		}
	}
}

func (p *compactionXMLParser) accept(token xml.Token) error {
	switch typed := token.(type) {
	case xml.StartElement:
		return p.start(typed)
	case xml.EndElement:
		return p.end()
	case xml.CharData:
		return p.characters(typed)
	case xml.Comment, xml.Directive, xml.ProcInst:
		return invalidCompactionXML(loop.InvalidSummaryXMLStructure, nil)
	default:
		return invalidCompactionXML(loop.InvalidSummaryXMLStructure, nil)
	}
}

func (p *compactionXMLParser) start(element xml.StartElement) error {
	if p.closedRoot {
		return invalidCompactionXML(loop.InvalidSummaryXMLStructure, nil)
	}
	if !p.seenRoot {
		if element.Name.Local != "conversation_summary" {
			return invalidCompactionXML(loop.InvalidSummaryXMLRoot, nil)
		}
		if element.Name.Space != "" || len(element.Attr) != 0 {
			return invalidCompactionXML(loop.InvalidSummaryXMLStructure, nil)
		}
		p.seenRoot = true
		p.depth = 1
		return nil
	}
	if p.depth != 1 || p.section >= len(compactionSummarySections) ||
		element.Name.Local != compactionSummarySections[p.section] || element.Name.Space != "" || len(element.Attr) != 0 {
		return invalidCompactionXML(loop.InvalidSummaryXMLStructure, nil)
	}
	p.depth = 2
	return nil
}

func (p *compactionXMLParser) end() error {
	switch p.depth {
	case 2:
		p.depth = 1
		p.section++
		return nil
	case 1:
		if p.section != len(compactionSummarySections) {
			return invalidCompactionXML(loop.InvalidSummaryXMLStructure, nil)
		}
		p.depth = 0
		p.closedRoot = true
		return nil
	default:
		return invalidCompactionXML(loop.InvalidSummaryXMLSyntax, nil)
	}
}

func (p *compactionXMLParser) characters(data xml.CharData) error {
	if !p.seenRoot || p.closedRoot {
		return invalidCompactionXML(loop.InvalidSummaryXMLStructure, nil)
	}
	switch p.depth {
	case 1:
		if strings.TrimSpace(string(data)) != "" {
			return invalidCompactionXML(loop.InvalidSummaryXMLStructure, nil)
		}
	case 2:
		_, _ = p.sectionText[p.section].Write(data)
	default:
		return invalidCompactionXML(loop.InvalidSummaryXMLStructure, nil)
	}
	return nil
}

func (p *compactionXMLParser) finish() error {
	if !p.seenRoot {
		return invalidCompactionXML(loop.InvalidSummaryXMLRoot, nil)
	}
	if !p.closedRoot || p.depth != 0 || p.section != len(compactionSummarySections) {
		return invalidCompactionXML(loop.InvalidSummaryXMLSyntax, nil)
	}
	if strings.TrimSpace(p.sectionText[0].String()) == "" || strings.TrimSpace(p.sectionText[3].String()) == "" {
		return invalidCompactionXML(loop.InvalidSummaryXMLContent, nil)
	}
	return nil
}

func invalidCompactionXML(reason loop.InvalidSummaryReason, cause error) error {
	return &loop.InvalidSummaryError{Reason: reason, Cause: cause}
}
