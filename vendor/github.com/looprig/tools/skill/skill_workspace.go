package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"syscall"

	"github.com/looprig/harness/pkg/tool"
)

// maxSkillNameLen caps a workspace skill name. A skill name is a single path
// component the model supplies; 64 bytes is generous for a slug while bounding
// the (attacker-influenced) input the loader handles before any filesystem touch.
const maxSkillNameLen = 64

// workspaceSkillsDir is the fixed subdirectory under the workspace root that holds
// untrusted, project-local skills as <dir>/<name>/SKILL.md (design §7a). The
// leading dot keeps it distinct from the trusted compiled-in "skills/" embed and
// matches a product policy's `.skills/**` deny rule for generic file tools.
const workspaceSkillsDir = ".skills"

// skillFileName is the fixed document name within a skill directory.
const skillFileName = "SKILL.md"

// validateSkillName enforces the strict ASCII-slug name rule for an UNTRUSTED
// workspace skill name (design §7a): the only accepted names match
// ^[a-z0-9][a-z0-9_-]*$ and are at most maxSkillNameLen bytes. Everything else —
// empty, ".", "..", a separator, a control or non-ASCII byte, uppercase, an
// over-length string — is a containment violation. Validation runs BEFORE any
// path is built or filesystem touched, so a traversal payload never reaches the
// OS. A regexp would do, but an explicit byte scan keeps the rule auditable and
// avoids a package-level compiled pattern.
func validateSkillName(name string) error {
	reject := func(reason string) error {
		return &SkillContainmentError{Name: name, Reason: reason}
	}
	if name == "" {
		return reject("name is empty")
	}
	if len(name) > maxSkillNameLen {
		return reject("name exceeds the maximum length")
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		isLower := c >= 'a' && c <= 'z'
		isDigit := c >= '0' && c <= '9'
		isSep := c == '-' || c == '_'
		switch {
		case i == 0 && !(isLower || isDigit):
			// First byte must be alphanumeric — never a separator, dot, or anything
			// else. This alone rejects "", ".", "..", "-x", "_x", "/x", "\x".
			return reject("name must start with a lowercase letter or digit")
		case !(isLower || isDigit || isSep):
			// Any subsequent byte must be [a-z0-9_-]. This rejects separators '/'
			// '\\', '.', whitespace, control bytes, uppercase, and non-ASCII.
			return reject("name contains a disallowed character")
		}
	}
	return nil
}

// loadWorkspaceSkill takes a TOCTOU-safe snapshot of an UNTRUSTED workspace skill
// at <root>/.skills/<name>/SKILL.md and returns a tool.SkillArtifact carrying the
// parsed body plus the snapshot metadata (relative path, byte size, SHA-256). It
// is the loading mechanism only — authorization (which agent may load workspace
// skills) and the human Ask-gate are layered on in a later phase via the artifact.
//
// Security (design §7a):
//   - The model-supplied name is validated to a strict ASCII slug BEFORE any path
//     is built (validateSkillName) — a traversal payload never reaches the OS.
//   - The workspace root is opened ONCE as an os.Root; the skill path is then
//     resolved THROUGH that root, which confines EVERY component to the root and
//     refuses symlink escapes and ".." on intermediate dirs, not just the final
//     file.
//   - os.Root FOLLOWS in-root symlinks, so the final component is additionally
//     Lstat'd (no-follow): a symlink is rejected (a SKILL.md must be a real file,
//     never a link) and so is any non-regular target (dir/device/fifo) — the
//     latter BEFORE the open, so a FIFO at the path cannot block the read.
//   - The open uses O_NONBLOCK and the opened descriptor is re-Stat'd for a
//     regular file (defense-in-depth against a swap between Lstat and open); a
//     BOUNDED snapshot is then read from that same descriptor and hashed. The
//     bytes that are hashed are exactly the bytes read — no re-open.
//
// Errors: a bad name / traversal / symlink-escape / symlink-final / non-regular
// target → *SkillContainmentError; an oversize or malformed document →
// *MalformedSkillError; an absent file → *SkillNotFoundError. A root that cannot
// be opened is returned wrapped.
func loadWorkspaceSkill(root, name string) (tool.SkillArtifact, error) {
	return loadWorkspaceSkillMetadata(root, name, nil)
}

// loadWorkspaceSkillMetadata is the shared secure workspace read behind body
// loading and metadata discovery. When metaOut is non-nil, it receives the
// parsed frontmatter from the same bounded snapshot used to build the artifact.
// Keeping this seam private prevents callers from obtaining a body through the
// metadata-only discovery API while avoiding a second parser or filesystem path.
func loadWorkspaceSkillMetadata(root, name string, metaOut *SkillMeta) (tool.SkillArtifact, error) {
	if err := validateSkillName(name); err != nil {
		return tool.SkillArtifact{}, err
	}

	// Relative path inside the root, always forward-slash (os.Root path syntax).
	relPath := workspaceSkillsDir + "/" + name + "/" + skillFileName

	// Open the workspace root as an os.Root. Every subsequent open/stat through
	// this handle is confined to the root: any component (intermediate or final)
	// that references a location outside the root is refused.
	osRoot, err := os.OpenRoot(root)
	if err != nil {
		// The root itself is missing/unreadable — a configuration error, not a
		// containment violation and not a missing skill.
		return tool.SkillArtifact{}, &SkillNotFoundError{Name: name, Err: err}
	}
	defer func() { _ = osRoot.Close() }()
	return loadWorkspaceSkillAtRoot(osRoot, relPath, relPath, name, metaOut)
}

// loadWorkspaceSkillAtRoot reads one already-validated skill path through an
// anchored root. artifactPath is the workspace-relative path recorded on the
// body-loading artifact; openPath is relative to osRoot.
func loadWorkspaceSkillAtRoot(osRoot *os.Root, openPath, artifactPath, name string, metaOut *SkillMeta) (tool.SkillArtifact, error) {

	// Lstat the final component WITHOUT following it. os.Root follows in-root
	// symlinks, so a link whose target stays inside the root would otherwise be
	// opened; Lstat (no-follow) detects the link itself, and because it does not
	// follow, its mode is the real mode of the final component. An escaping
	// link/.. is already refused here by os.Root as a containment error. Rejecting
	// every non-regular target up front (before the Open below) is also what
	// prevents a FIFO at the skill path from BLOCKING the Open indefinitely (a
	// denial-of-service an untrusted workspace could otherwise mount).
	li, err := osRoot.Lstat(openPath)
	if err != nil {
		return tool.SkillArtifact{}, classifyFSError(name, err)
	}
	if li.Mode()&fs.ModeSymlink != 0 {
		return tool.SkillArtifact{}, &SkillContainmentError{
			Name:   name,
			Reason: "skill file is a symlink",
		}
	}
	if !li.Mode().IsRegular() {
		return tool.SkillArtifact{}, &SkillContainmentError{
			Name:   name,
			Reason: "skill file is not a regular file",
		}
	}

	// Open through the root (confined). O_NONBLOCK ensures that even if the final
	// component is swapped to a FIFO between the Lstat above and this open (a
	// TOCTOU race an untrusted workspace could attempt), the open returns
	// immediately instead of blocking; the descriptor's own Stat below then
	// re-validates a regular file, so a swapped-in non-regular target is rejected,
	// not read.
	f, err := osRoot.OpenFile(openPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return tool.SkillArtifact{}, classifyFSError(name, err)
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return tool.SkillArtifact{}, classifyFSError(name, err)
	}
	if !fi.Mode().IsRegular() {
		return tool.SkillArtifact{}, &SkillContainmentError{
			Name:   name,
			Reason: "skill file is not a regular file",
		}
	}

	// Read a BOUNDED snapshot from the open descriptor. Reading maxSkillBytes+1
	// lets us detect an oversize file without trusting the stat size.
	raw, err := io.ReadAll(io.LimitReader(f, maxSkillBytes+1))
	if err != nil {
		return tool.SkillArtifact{}, classifyFSError(name, err)
	}
	if len(raw) > maxSkillBytes {
		return tool.SkillArtifact{}, &MalformedSkillError{
			Name:   name,
			Reason: "document exceeds the maximum skill size",
		}
	}

	// Hash the exact bytes read (the snapshot), then parse them. The hash binds
	// what was approved to what executes — execution reuses Body, never a re-open.
	sum := sha256.Sum256(raw)

	meta, body, err := parseSkill(raw)
	if err != nil {
		// Stamp the now-known name onto the parser's name-less MalformedSkillError.
		var me *MalformedSkillError
		if errors.As(err, &me) && me.Name == "" {
			me.Name = name
		}
		return tool.SkillArtifact{}, err
	}
	if metaOut != nil {
		*metaOut = meta
	}

	return tool.SkillArtifact{
		Workspace: true,
		RelPath:   artifactPath,
		Size:      int64(len(raw)),
		SHA256:    hex.EncodeToString(sum[:]),
		Body:      body,
	}, nil
}

// classifyFSError maps a filesystem error from an os.Root operation to the right
// typed skill error. An os.Root containment refusal (symlink/".." escape) is NOT
// an fs.ErrNotExist, so it is reported as a *SkillContainmentError; a genuine
// missing path is a *SkillNotFoundError. This keeps the loader fail-secure: an
// escape attempt is never misreported as a benign "not found".
func classifyFSError(name string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return &SkillNotFoundError{Name: name, Err: err}
	}
	return &SkillContainmentError{Name: name, Reason: "skill path is not contained in the workspace root"}
}
