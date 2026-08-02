package builder

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestBuilderIdentityAndRole(t *testing.T) {
	t.Parallel()

	if Name != "builder" {
		t.Fatalf("Name = %q, want builder", Name)
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
	if probe.Name != "builder" {
		t.Fatalf("role name = %q, want builder", probe.Name)
	}
	for _, want := range []string{"implement", "root cause", "workspace", "delegate"} {
		if !strings.Contains(Role, want) {
			t.Errorf("Role missing %q", want)
		}
	}
}
