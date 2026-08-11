package presentation

import (
	"strings"

	"github.com/looprig/core/uuid"
)

// AgentBanner is the static agent metadata shown as the startup info notice. Name and
// Description are threaded in at construction from the application composition root;
// the session identity comes from the current Agent when the notice is committed. The
// zero value renders a name-less banner; bannerText degrades gracefully when either
// static field is empty.
//
// It lives in its own file (not the shell) because it is shared state on the embedded
// sessionCore that BOTH the composition root and the presentation shell read — see
// sessioncore.go's banner field and Screen's use of bannerText.
type AgentBanner struct {
	Name        string
	Description string
}

// bannerText renders the startup notice from static agent metadata and the current
// session identity. Its first line is "<Name> — <Description>" when both are present,
// just the available field when one is empty, or a neutral fallback when both are
// empty. The second line always identifies the session with its full UUID.
func (b AgentBanner) bannerText(sessionID uuid.UUID) string {
	name, desc := strings.TrimSpace(b.Name), strings.TrimSpace(b.Description)
	var firstLine string
	switch {
	case name != "" && desc != "":
		firstLine = name + " — " + desc
	case name != "":
		firstLine = name
	case desc != "":
		firstLine = desc
	default:
		firstLine = "session ready"
	}
	return firstLine + "\nSession: #" + sessionID.String()
}
