package reviewer

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestReviewerIdentityAndRole(t *testing.T) {
	t.Parallel()

	if Name != "reviewer" {
		t.Fatalf("Name = %q, want reviewer", Name)
	}
	var probe struct {
		XMLName xml.Name `xml:"role"`
		Name    string   `xml:"name,attr"`
	}
	if err := xml.Unmarshal([]byte(Role), &probe); err != nil {
		t.Fatalf("Role is not well-formed XML: %v", err)
	}
	if probe.Name != "reviewer" {
		t.Fatalf("role name = %q, want reviewer", probe.Name)
	}
	for _, want := range []string{"correctness", "security", "read-only", "delegate"} {
		if !strings.Contains(Role, want) {
			t.Errorf("Role missing %q", want)
		}
	}
}
