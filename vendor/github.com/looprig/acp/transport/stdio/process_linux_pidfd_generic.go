//go:build linux && !android && !(mips || mipsle || mips64 || mips64le)

package stdio

// The syscall package is frozen and does not expose SYS_PIDFD_OPEN on every
// architecture supported by Go.
const sysPIDFDOpen = 434
