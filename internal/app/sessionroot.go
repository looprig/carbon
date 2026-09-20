package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	sessionwire "github.com/looprig/core/sessionwire/v1"
)

const sessionWorkspacesDir = "session-workspaces"
const sessionWorkspaceDigestDomain = "looprig/carbon/session-workspace-root/v1"

var ErrNoSessionIdentity = errors.New("carbon: a session workspace needs a session identity")
var ErrNoDataRoot = errors.New("carbon: a session workspace needs an absolute data root")

type SessionWorkspaceRootError struct {
	TenantID  sessionwire.TenantID
	SessionID sessionwire.SessionID
	Cause     error
}

func (e *SessionWorkspaceRootError) Error() string {
	return fmt.Sprintf("carbon: workspace root for tenant %q session %q: %v", e.TenantID, e.SessionID, e.Cause)
}
func (e *SessionWorkspaceRootError) Unwrap() error { return e.Cause }

func sessionWorkspaceRoot(dataRoot string, tenant sessionwire.TenantID, session sessionwire.SessionID) (string, error) {
	if session == "" {
		return "", &SessionWorkspaceRootError{tenant, session, ErrNoSessionIdentity}
	}
	if !filepath.IsAbs(dataRoot) {
		return "", &SessionWorkspaceRootError{tenant, session, ErrNoDataRoot}
	}
	sum := sha256.Sum256([]byte(sessionWorkspaceDigestDomain + "\x00" + string(tenant) + "\x00" + string(session)))
	return filepath.Join(filepath.Clean(dataRoot), sessionWorkspacesDir, hex.EncodeToString(sum[:])), nil
}

func materializeSessionWorkspaceRoot(dataRoot string, tenant sessionwire.TenantID, session sessionwire.SessionID) (string, error) {
	root, err := sessionWorkspaceRoot(dataRoot, tenant, session)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", &SessionWorkspaceRootError{tenant, session, err}
	}
	return root, nil
}
