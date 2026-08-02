package planner

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestPlannerIdentityAndRole(t *testing.T) {
	t.Parallel()

	if Name != "planner" {
		t.Fatalf("Name = %q, want planner", Name)
	}
	if strings.TrimSpace(Description) == "" {
		t.Fatal("Description is empty")
	}
	var probe struct {
		XMLName xml.Name `xml:"role"`
		Name    string   `xml:"name,attr"`
	}
	if err := xml.Unmarshal([]byte(Role), &probe); err != nil {
		t.Fatalf("Role is not well-formed XML: %v", err)
	}
	if probe.Name != "planner" {
		t.Fatalf("role name = %q, want planner", probe.Name)
	}
	for _, want := range []string{"investigate", "decomposition", "read-only", "delegate"} {
		if !strings.Contains(Role, want) {
			t.Errorf("Role missing %q", want)
		}
	}
}
