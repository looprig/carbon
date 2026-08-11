package styles

import "charm.land/lipgloss/v2"

// CardBorderColor tints the gate card's left ▌ rail — and the pressable accelerator
// keys in its footer — a soft brand blue (the same calm accent as MarkdownHeadingColor).
// A pending permission/AskUser gate is an action-required FOREGROUND affordance, so it
// reads as a blue-tinted panel card (the user-message card treatment in blue), distinct
// from the faint, borderless tool cards and the neutral-gray composer, and stays legible
// on a dark terminal.
var CardBorderColor = lipgloss.Color(MarkdownHeadingColor)

// CardPanelBg is the fill painted behind a gate card — a near-neutral dark gray carrying only
// a whisper of blue (the blue tint dialed ~90% out of the earlier #1b2233), so a pending gate
// reads as the same padded card family as the neutral user-message panel (PanelBg) with just
// enough cool cast to pair with the blue rail. Dark enough that the bold title, body text and
// dim hints stay readable on a dark terminal.
var CardPanelBg = lipgloss.Color("#242527")

// CardSelectedBg highlights the selected choice row — a brighter blue than CardPanelBg so
// the current selection reads as a filled bar standing proud of the panel (the opencode
// dialog look), rather than only the ▸ cursor.
var CardSelectedBg = lipgloss.Color("#2c3a5a")

// TraySelectedBg is the completion tray's settled selection fill. It is lighter and more
// blue than the gate-card selection while remaining dark enough for faint path/description
// text. Screen animates toward this endpoint; standalone component views render it directly.
var TraySelectedBg = lipgloss.Color("#3A526B")

// CardRailStyle colors the gate card's left ▌ rail the brand blue, matching the user card's
// gray rail but tinted — the continuous accent edge down the padded blue panel.
var CardRailStyle = lipgloss.NewStyle().Foreground(CardBorderColor)

// WorkflowActivityStyle is the shared TUI blue used for durable workflow markers. It
// intentionally reuses CardBorderColor so notifications and action cards have one semantic
// blue token rather than drifting shades.
var WorkflowActivityStyle = lipgloss.NewStyle().Foreground(CardBorderColor)

// CardTitleStyle renders the bold card title row — "Approve Bash?", the AskUser
// question, or the "answer" label — so the required action reads at a glance above the
// card body.
var CardTitleStyle = lipgloss.NewStyle().Bold(true)

// CardKeyStyle renders the bracketed accelerator of a footer key hint ("[y]", "[o]")
// in the bold brand accent, so the pressable key stands out from its muted label.
var CardKeyStyle = lipgloss.NewStyle().Bold(true).Foreground(CardBorderColor)

// CardHintStyle renders a card's muted secondary text — a key hint's label ("once"),
// the choice key legend, the free-text submit hint, and the "(+N more pending)"
// queue-depth note — faint so it recedes beneath the title and body.
var CardHintStyle = lipgloss.NewStyle().Faint(true)

// CardSelectedStyle highlights the selected choice row: bold with the brighter blue fill
// (CardSelectedBg) spanning the row, so the current selection reads as a filled bar rather
// than only the ▸ cursor. The caller sets the row Width so the fill spans the card body.
var CardSelectedStyle = lipgloss.NewStyle().Bold(true).Background(CardSelectedBg)
