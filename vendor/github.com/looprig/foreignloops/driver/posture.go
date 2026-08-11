package driver

// Posture is a neutral, secret-free access posture translated by each harness
// driver. PostureReadOnly permits reads, searches, and non-mutating command
// execution only; mutation and network access are denied by configuration and
// the permission handler. PostureWorkspaceWrite permits workspace file
// mutation and commands inside the session workspace.
type Posture string

const (
	PostureReadOnly       Posture = "read-only"
	PostureWorkspaceWrite Posture = "workspace-write"
)

// Valid reports whether p is one of the defined access postures.
func (p Posture) Valid() bool {
	return p == PostureReadOnly || p == PostureWorkspaceWrite
}
