package presentation

import "github.com/looprig/tui/styles"

// animState is the live-surface animation state advanced once per blink tick while
// a turn is Running. It is meaningful ONLY for the live tail rendered in View(); the
// committed scrollback path never consults it (already-printed entries are frozen).
// blink toggles the streaming assistant dot between its lit and dimmed glyph; frame
// indexes the running tool-card spinner; ticking guards against double-starting the
// tick loop (a second TurnStarted while a tick is already in flight must not spawn a
// parallel loop). The zero value is the idle, not-yet-ticking state.
type animState struct {
	blink   bool
	frame   uint
	ticking bool
}

// advance moves the animation one step: it flips blink and increments frame
// (wrapping is handled at index time by spinnerGlyph's modulo). It does NOT touch
// ticking — start/stop of the loop is the caller's (Screen's) concern.
func (a animState) advance() animState {
	a.blink = !a.blink
	a.frame++
	return a
}

// reset returns the idle animation state: blink off, frame zeroed, not ticking. It
// is applied when a turn ends so the next live render carries no lingering animation
// and a fresh turn starts a clean tick loop.
func (a animState) reset() animState { return animState{} }

// spinnerFrames are the running-tool spinner cells, cycled one per blink tick. The
// braille dots rotate a single filled segment around the cell, reading as a smooth
// "working" spinner at the ~450ms blink cadence.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerGlyph returns the running-tool spinner cell for frame, wrapping modulo the
// frame count so any frame value is in range (the counter grows unbounded over a long
// turn). It is retired from the live path (a running node now pulses via liveRunningNode);
// it survives only for the tests that pin its absence from rail output.
func spinnerGlyph(frame uint) string {
	return spinnerFrames[frame%uint(len(spinnerFrames))]
}

// workingWords are the retired live "doing work" synonyms that once headlined an
// empty-text tool step. The unified rail renders such a step's tool nodes directly (no
// working-word umbrella — see renderLiveAssistant), so these no longer render; the set
// survives ONLY as the sentinel words render tests assert are ABSENT from live output.
var workingWords = []string{"Working", "Crunching", "Churning", "Toiling", "Cooking", "Whirring"}

// workingWord returns the working-word for frame, wrapping modulo the word count. It is
// retired from the live path (a card-only step now shows its tool nodes directly); it
// survives only for the tests that pin its absence.
func workingWord(frame uint) string {
	return workingWords[frame%uint(len(workingWords))]
}

// liveRunningNode is the pulsing "◍" node glyph for a still-running tool call in the live
// tail: it alternates the lit (DotColor) and white phases on the blink tick, matching the
// status line's working pulse. 2 columns wide like the other node glyphs so rail alignment
// holds. The pulse is a colour change only, so an ANSI-stripped assertion sees a stable "◍".
func liveRunningNode(blink bool) string {
	style := styles.StatusWorkingStyle
	if blink {
		style = styles.StatusWorkingAltStyle
	}
	return style.Render("◍") + " "
}

// liveDotLit / liveDotDim are the two glyphs the live (streaming) assistant bullet
// alternates between on each blink tick: the normal lit colored "● " (styles.LitDot,
// the same DotColor-foregrounded bullet the committed path renders) and a dimmed hollow
// "◦ ". Both keep the dotWidth (2 columns) so narration alignment is unchanged.
const liveDotDim = "◦ "

var liveDotLit = styles.LitDot

// liveDot returns the live assistant bullet for the current blink phase: the dimmed
// hollow dot when blink is true, the lit dot otherwise. Only the LIVE streaming dot
// blinks; a committed assistant "●" renders the static styles.Dot via renderMD.
func liveDot(blink bool) string {
	if blink {
		return liveDotDim
	}
	return liveDotLit
}
