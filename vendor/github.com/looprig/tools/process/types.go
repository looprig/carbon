package process

// Identity groups the three immutable facts that exist for a supervised
// process from the moment it is admitted: its opaque capability Handle, its
// authority Owner (SessionID + LoopID), and its audit-only Origin (the
// creating tool execution). None of these fields ever change after
// admission; only a process's State (state.go) transitions over its
// lifetime.
type Identity struct {
	Handle Handle
	Owner  Owner
	Origin Origin
}
