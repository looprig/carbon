package workspacestore

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// dirWorkPerm is the mode new directories are created with during extraction,
// before their archived mode is restored in a final pass. It is owner
// writable/searchable so children can always be written into a directory even
// when its archived mode is restrictive (e.g. a read-only 0o555 tree, as the Go
// module cache uses). A deferred chmod applies the real mode once every entry
// beneath the directory has been written.
const dirWorkPerm fs.FileMode = 0o700

// maxEntryNameLen bounds the byte length of an archive entry name. It exceeds
// every real filesystem's path limit (Linux PATH_MAX is 4096, darwin 1024), so
// it never rejects a name produced by walking a genuine tree, while it fails a
// hostile name closed before any syscall — the cheapest layer to reject a
// pathologically deep path whose per-component directory creation would
// otherwise amplify one entry into unbounded filesystem work.
const maxEntryNameLen = 4096

// limits bounds a single extraction against decompression bombs: at most
// maxEntries entries read from the tar stream and at most maxBytes cumulative
// bytes written to disk. Both are enforced from observed counts, never from a
// header's self-declared size.
type limits struct {
	maxEntries int64
	maxBytes   int64
}

// pendingDirMode records a directory whose archived permission bits must be
// restored after every entry beneath it has been written — deferred so a
// restrictive mode never blocks writing the directory's children mid-extraction.
type pendingDirMode struct {
	name string
	mode fs.FileMode
}

// extractArchive restores the gzip-compressed tar read from r into dest — the
// trust boundary where an untrusted archive becomes files on disk. The caller
// guarantees dest exists and is empty. Every entry name is validated (no
// absolute paths, no "..", no NUL) and every write is confined by an *os.Root
// sandbox that refuses to traverse or escape via symlinks, so a hostile entry
// name or a planted symlink can never redirect a write outside dest. Entry and
// byte limits guard decompression bombs; device, fifo, and hardlink entries are
// rejected. On any failure the partial output under dest is removed best-effort
// and the raw typed error is returned (Materialize wraps it in *MaterializeError).
func extractArchive(ctx context.Context, r io.Reader, dest string, lim limits) error {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()

	if err := extractEntries(ctx, root, r, lim); err != nil {
		removeContents(root)
		return err
	}
	return nil
}

// extractEntries reads the gzip tar stream and materializes each entry through
// root, enforcing the entry-count and cumulative-byte limits as it goes. It
// stops and returns the first error — a hostile entry, a tripped limit, a
// cancelled context, or an I/O fault — leaving cleanup to the caller.
func extractEntries(ctx context.Context, root *os.Root, r io.Reader, lim limits) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	var pendingDirs []pendingDirMode
	var seen, written int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		seen++
		if seen > lim.maxEntries {
			return &ArchiveLimitError{Limit: ArchiveLimitEntries, Cap: lim.maxEntries, Observed: seen}
		}
		n, err := extractEntry(ctx, root, hdr, tr, lim, written, &pendingDirs)
		if err != nil {
			return err
		}
		written += n
	}
	// Every entry is written; only now restore restrictive directory modes.
	return restoreDirModes(root, pendingDirs)
}

// restoreDirModes applies each recorded directory's archived permission bits.
// It processes the deepest paths first (longest name first): a directory is a
// strict prefix of its children, so a child's name is always longer, and this
// order guarantees every child is chmod'd — and reached through still-writable
// ancestors — before its parent is locked down to a possibly no-search mode.
func restoreDirModes(root *os.Root, dirs []pendingDirMode) error {
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].name) > len(dirs[j].name) })
	for _, d := range dirs {
		if err := root.Chmod(d.name, d.mode); err != nil {
			return err
		}
	}
	return nil
}

// extractEntry validates one entry's name, confirms no ancestor component is a
// symlink, and dispatches on the entry type. It returns the number of content
// bytes written (nonzero only for regular files) so the caller can maintain the
// cumulative-byte total. ctx bounds the per-entry content copy.
func extractEntry(ctx context.Context, root *os.Root, hdr *tar.Header, tr io.Reader, lim limits, written int64, dirs *[]pendingDirMode) (int64, error) {
	if err := validateEntryName(hdr.Name); err != nil {
		return 0, err
	}
	name := filepath.FromSlash(path.Clean(hdr.Name))
	if err := rejectSymlinkAncestor(root, hdr.Name, name); err != nil {
		return 0, err
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		return 0, extractDir(root, name, hdr, dirs)
	case tar.TypeReg:
		// tar.Reader normalizes the deprecated old-style regular-file flag
		// (TypeRegA) to TypeReg on read, so this one case covers every regular
		// file; any other flag falls through to the fail-closed default.
		return extractRegular(ctx, root, name, hdr, tr, lim, written)
	case tar.TypeSymlink:
		return 0, extractSymlink(root, name, hdr)
	default:
		return 0, &ArchiveEntryError{Name: hdr.Name, Reason: "unsupported entry type: " + entryTypeName(hdr.Typeflag)}
	}
}

// validateEntryName enforces the entry-name grammar every archive entry must
// satisfy: non-empty, no NUL byte, not absolute, no ".." segment, and not a name
// that cleans to the destination root itself. A violation yields *ArchiveEntryError
// with the offending name and the rule broken. This is the function the fuzz
// target exercises directly; the os.Root sandbox is the independent second layer.
func validateEntryName(name string) error {
	if name == "" {
		return &ArchiveEntryError{Name: name, Reason: "empty entry name"}
	}
	if len(name) > maxEntryNameLen {
		return &ArchiveEntryError{Name: name[:64] + "...(truncated)", Reason: "entry name exceeds " + strconv.Itoa(maxEntryNameLen) + " bytes"}
	}
	if strings.IndexByte(name, 0) >= 0 {
		return &ArchiveEntryError{Name: name, Reason: "entry name contains a NUL byte"}
	}
	if path.IsAbs(name) {
		return &ArchiveEntryError{Name: name, Reason: "absolute entry name"}
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return &ArchiveEntryError{Name: name, Reason: `entry name contains a ".." segment`}
		}
	}
	cleaned := path.Clean(name)
	if cleaned == "." {
		return &ArchiveEntryError{Name: name, Reason: "entry name resolves to the destination root"}
	}
	// Unreachable given the ".." rejection and absolute check above (Clean cannot
	// synthesize a "..", "../" prefix, or leading "/" from a name lacking them) —
	// kept as defense in depth so a future grammar change fails closed here too.
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || path.IsAbs(cleaned) {
		return &ArchiveEntryError{Name: name, Reason: "entry name escapes the destination"}
	}
	return nil
}

// rejectSymlinkAncestor verifies no parent path component of this entry is a
// symlink, so a symlink planted by an earlier entry can never be traversed to
// redirect this entry's write outside dest. It descends component by component
// through nested *os.Root sandboxes, Lstat'ing each single component without
// following it: a symlink component is reported as a typed *ArchiveEntryError at
// any depth. A not-yet-created ancestor is fine — MkdirAll will make it a real
// directory. os.Root remains the independent hard backstop; this check exists so
// the boundary reports a hostile entry as *ArchiveEntryError rather than a raw
// sandbox error. origName is the raw entry name carried in the error.
func rejectSymlinkAncestor(root *os.Root, origName, name string) error {
	dir := filepath.Dir(name)
	if dir == "." || dir == string(filepath.Separator) {
		return nil
	}
	cur := root
	closeCur := func() {
		if cur != root {
			_ = cur.Close()
		}
	}
	segs := strings.Split(dir, string(filepath.Separator))
	for i, seg := range segs {
		info, err := cur.Lstat(seg)
		if err != nil {
			closeCur()
			if errors.Is(err, fs.ErrNotExist) {
				return nil // this ancestor does not exist yet; nothing to traverse
			}
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			closeCur()
			return &ArchiveEntryError{Name: origName, Reason: "a parent path component is a symlink"}
		}
		if i == len(segs)-1 {
			break // last component needs only the Lstat above, not a descent
		}
		next, err := cur.OpenRoot(seg)
		closeCur()
		if err != nil {
			return err
		}
		cur = next
	}
	closeCur()
	return nil
}

// extractDir creates the directory named name (with any missing ancestors) at
// the working mode and records its archived permission bits for restoration in
// the final pass. Deferring the chmod keeps the directory writable while its
// children are written, so a restrictive archived mode (e.g. 0o555) round-trips.
func extractDir(root *os.Root, name string, hdr *tar.Header, dirs *[]pendingDirMode) error {
	if err := root.MkdirAll(name, dirWorkPerm); err != nil {
		return err
	}
	*dirs = append(*dirs, pendingDirMode{name: name, mode: hdr.FileInfo().Mode().Perm()})
	return nil
}

// extractSymlink creates name as a symlink to the archived target. The target is
// written verbatim (never resolved): a hostile absolute or escaping target is
// stored as-is but is inert, because every subsequent write is name-validated and
// refuses to traverse a symlink component (see rejectSymlinkAncestor and os.Root).
// A name already taken by a prior entry fails closed as a typed duplicate.
func extractSymlink(root *os.Root, name string, hdr *tar.Header) error {
	if err := ensureParent(root, name); err != nil {
		return err
	}
	return asDuplicateEntry(hdr.Name, root.Symlink(hdr.Linkname, name))
}

// asDuplicateEntry translates an "already exists" error from creating an entry
// into a typed *ArchiveEntryError — a malformed or hostile archive may repeat a
// name, and the boundary reports that as a rejected entry rather than a raw os
// error. Any other error (including nil) is returned unchanged.
func asDuplicateEntry(name string, err error) error {
	if errors.Is(err, fs.ErrExist) {
		return &ArchiveEntryError{Name: name, Reason: "duplicate entry name"}
	}
	return err
}

// extractRegular writes a regular file's contents, capping the copy at the
// remaining byte budget so a bomb cannot inflate past lim.maxBytes. The copy is
// bounded by ctx, so a cancelled context or deadline aborts a large entry
// promptly rather than only between entries. The bytes-written count is returned
// so callers can advance the running total; on a byte-limit breach it is folded
// into the *ArchiveLimitError's Observed (written+n) here. The file mode is
// restricted to permission bits (setuid/setgid/sticky are never restored from an
// untrusted archive) and applied after the write to defeat the umask.
func extractRegular(ctx context.Context, root *os.Root, name string, hdr *tar.Header, tr io.Reader, lim limits, written int64) (int64, error) {
	if err := ensureParent(root, name); err != nil {
		return 0, err
	}
	perm := hdr.FileInfo().Mode().Perm()
	// name is validated (no absolute, no "..") and confined to the os.Root
	// sandbox, which refuses any symlink traversal or escape; O_EXCL fails closed
	// on a duplicate/overwrite entry (reported as a typed duplicate).
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) // #nosec G304 -- validated name inside os.Root sandbox
	if err != nil {
		return 0, asDuplicateEntry(hdr.Name, err)
	}
	n, exceeded, copyErr := copyCapped(f, &ctxReader{ctx: ctx, r: tr}, lim.maxBytes-written)
	closeErr := f.Close()
	if copyErr != nil {
		return n, copyErr
	}
	if exceeded {
		return n, &ArchiveLimitError{Limit: ArchiveLimitBytes, Cap: lim.maxBytes, Observed: written + n}
	}
	if closeErr != nil {
		return n, closeErr
	}
	return n, root.Chmod(name, perm)
}

// ctxReader bounds a copy by a context: each Read returns ctx.Err() before
// delegating, so a cancelled context or passed deadline aborts a long copy
// between the underlying reader's chunks (io.Copy's ~32 KiB reads) rather than
// only at entry boundaries. It matters most once the source is a streaming
// network Get, where one entry could otherwise block unbounded.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// copyCapped copies from src to dst, reading at most remaining+1 bytes. It
// reports the bytes copied and whether the source held more than the budget
// allowed (n > remaining): reading the one extra byte is what proves the overflow
// without trusting any declared size. A negative remaining is treated as zero.
func copyCapped(dst io.Writer, src io.Reader, remaining int64) (int64, bool, error) {
	if remaining < 0 {
		remaining = 0
	}
	lr := &io.LimitedReader{R: src, N: remaining + 1}
	n, err := io.Copy(dst, lr)
	if err != nil {
		return n, false, err
	}
	return n, n > remaining, nil
}

// ensureParent creates the parent directory chain for name if name is nested.
// Directories are created at the working mode; an explicit directory entry (if
// present in the archive) restores the exact archived mode in the final pass.
func ensureParent(root *os.Root, name string) error {
	dir := filepath.Dir(name)
	if dir == "." || dir == string(filepath.Separator) {
		return nil
	}
	return root.MkdirAll(dir, dirWorkPerm)
}

// removeContents best-effort empties the root's directory without removing the
// directory itself (which the caller created). It first makes every directory
// owner-writable so RemoveAll can unlink the children of a directory whose
// (possibly already-restored) archived mode is read-only, then removes each
// top-level entry. Every operation goes through the *os.Root sandbox and the
// walk never follows symlinks, so a symlink planted during a partial extraction
// is unlinked, never traversed. Errors are ignored: cleanup is best-effort and
// the original extraction error is the meaningful one.
func removeContents(root *os.Root) {
	fsys := root.FS()
	// Re-enable write/search on every directory. WalkDir visits a directory
	// before reading its children, so chmod'ing it here also re-opens descent
	// into an otherwise no-search directory.
	_ = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p != "." && d.IsDir() {
			_ = root.Chmod(p, dirWorkPerm)
		}
		return nil
	})
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return
	}
	for _, e := range entries {
		_ = root.RemoveAll(e.Name())
	}
}

// entryTypeName names a rejected tar entry type for an *ArchiveEntryError reason.
// All returned values are static or a numeric flag, so the reason stays log-safe.
func entryTypeName(flag byte) string {
	switch flag {
	case tar.TypeLink:
		return "hard link"
	case tar.TypeChar:
		return "character device"
	case tar.TypeBlock:
		return "block device"
	case tar.TypeFifo:
		return "fifo"
	default:
		return "typeflag " + strconv.Itoa(int(flag))
	}
}
