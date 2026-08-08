package generic

import (
	"encoding/xml"
	"strings"
	"testing"
)

type promptSection struct {
	Items []string `xml:"item"`
}

func (s promptSection) text() string {
	return strings.Join(s.Items, " ")
}

func TestIdentity(t *testing.T) {
	t.Parallel()

	if Name != "generic" {
		t.Fatalf("Name = %q, want generic", Name)
	}
	if strings.TrimSpace(Description) == "" {
		t.Fatal("Description is empty")
	}

	var root struct {
		XMLName       xml.Name      `xml:"identity"`
		Product       string        `xml:"product,attr"`
		Persona       string        `xml:"persona"`
		Intent        promptSection `xml:"intent"`
		Workflow      promptSection `xml:"workflow"`
		Tools         promptSection `xml:"tools"`
		Safety        promptSection `xml:"safety"`
		Delegation    promptSection `xml:"delegation"`
		Communication promptSection `xml:"communication"`
	}
	if err := xml.Unmarshal([]byte(SystemPrompt), &root); err != nil {
		t.Fatalf("SystemPrompt is not XML: %v", err)
	}
	if root.XMLName.Local != "identity" {
		t.Fatalf("root element = %q, want identity", root.XMLName.Local)
	}
	if root.Product != "CodeRig" {
		t.Fatalf("product = %q, want CodeRig", root.Product)
	}
	if strings.TrimSpace(root.Persona) == "" {
		t.Fatal("persona is empty")
	}

	sections := []struct {
		name string
		text string
		want []string
	}{
		{name: "intent", text: root.Intent.text(), want: []string{"answer", "change", "verify"}},
		{name: "workflow", text: root.Workflow.text(), want: []string{"inspect", "root causes", "focused searches", "complete"}},
		{name: "tools", text: root.Tools.text(), want: []string{"proactively", "untrusted data", "succeeded"}},
		{name: "safety", text: root.Safety.text(), want: []string{"permission", "destructive", "secrets"}},
		{name: "delegation", text: root.Delegation.text(), want: []string{"delegate", "subagent", "synthesize"}},
		{name: "communication", text: root.Communication.text(), want: []string{"outcomes", "concrete", "uncertainty"}},
	}
	for _, section := range sections {
		t.Run(section.name, func(t *testing.T) {
			if strings.TrimSpace(section.text) == "" {
				t.Fatalf("%s section is empty", section.name)
			}
			for _, want := range section.want {
				if !strings.Contains(section.text, want) {
					t.Errorf("%s section is missing %q", section.name, want)
				}
			}
		})
	}

	for _, want := range []string{"Generic", "answer", "change", "verify", "untrusted data", "destructive", "delegate"} {
		if !strings.Contains(SystemPrompt, want) {
			t.Errorf("SystemPrompt is missing %q", want)
		}
	}
}
