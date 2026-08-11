package loop

// Engine selects the loop backend. The zero value is native.
type Engine uint8

const (
	EngineNative Engine = iota
	EngineForeignClaude
	EngineForeignCodex
	EngineAdapter
)
