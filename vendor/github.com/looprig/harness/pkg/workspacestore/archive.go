package workspacestore

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// gzipOSUnknown is the gzip header OS byte meaning "unknown" (RFC 1952 §2.3.1).
// It is written into every archive's gzip header so the wrapper does not leak
// the producing operating system and stays byte-identical across hosts.
const gzipOSUnknown = 255

// walkedEntry is one node discovered while walking the tree: its absolute path,
// its forward-slash path relative to the archive root (the tar entry name, no
// trailing slash), and the lstat-based FileInfo (symlinks describe the link
// itself, never its target).
type walkedEntry struct {
	abs  string
	name string
	info fs.FileInfo
}

// writeArchive writes the tree rooted at root to w as a byte-deterministic
// tar.gz: entries are emitted in sorted (byte-order) name order with every
// nondeterministic header field normalized, so the same logical tree always
// produces the same bytes — and thus the same sha256, the foundation of
// content-addressing. Regular files contribute their contents; directories and
// symlinks contribute headers only (symlinks are stored as symlinks, never
// followed). An unsupported node type (socket, fifo, device) fails closed with
// *ArchiveEntryError rather than being silently skipped.
func writeArchive(w io.Writer, root string) error {
	gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
	if err != nil {
		return err
	}
	// Zero the gzip header: no mtime, name, comment, or producing-OS leak, so
	// the compression wrapper is itself deterministic.
	gz.Header = gzip.Header{OS: gzipOSUnknown}

	tw := tar.NewWriter(gz)
	if err := writeTree(tw, root); err != nil {
		// Best-effort teardown; the walk error is the meaningful one.
		_ = tw.Close()
		_ = gz.Close()
		return err
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return err
	}
	return gz.Close()
}

// writeTree collects, sorts, and emits every entry of the tree rooted at root.
func writeTree(tw *tar.Writer, root string) error {
	entries, err := collectEntries(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := writeEntry(tw, e); err != nil {
			return err
		}
	}
	return nil
}

// collectEntries walks the tree rooted at root and returns its entries (the root
// itself excluded) sorted by their forward-slash relative name in byte order.
// Sorting decouples the archive from the platform's directory-read order, so
// traversal order can never affect the output. WalkDir does not descend into
// symlinked directories, so an in-tree symlink is a single leaf entry.
func collectEntries(root string) ([]walkedEntry, error) {
	var entries []walkedEntry
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil // the root itself is not an archive entry
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		entries = append(entries, walkedEntry{abs: p, name: filepath.ToSlash(rel), info: info})
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	return entries, nil
}

// writeEntry emits one entry, dispatching on its lstat node type: directories
// and symlinks get a normalized header only; regular files also get their
// contents; anything else (socket, fifo, device) is rejected fail-closed.
func writeEntry(tw *tar.Writer, e walkedEntry) error {
	mode := e.info.Mode()
	switch {
	case mode.IsDir():
		return writeHeaderOnly(tw, e, "", e.name+"/")
	case mode&fs.ModeSymlink != 0:
		target, err := os.Readlink(e.abs)
		if err != nil {
			return err
		}
		return writeHeaderOnly(tw, e, target, e.name)
	case mode.IsRegular():
		return writeRegular(tw, e)
	default:
		return &ArchiveEntryError{Name: e.name, Reason: "unsupported file type: " + typeName(mode)}
	}
}

// writeHeaderOnly emits a normalized, contentless header. link is the symlink
// target for symlink entries (empty otherwise); name is the tar entry name
// (with a trailing slash for directories, as tar expects).
func writeHeaderOnly(tw *tar.Writer, e walkedEntry, link, name string) error {
	hdr, err := tar.FileInfoHeader(e.info, link)
	if err != nil {
		return err
	}
	normalizeHeader(hdr, name)
	return tw.WriteHeader(hdr)
}

// writeRegular emits a regular file's normalized header followed by its bytes,
// verifying the copied length matches the declared size — a file mutated
// mid-archive is an error, never a silently truncated entry.
func writeRegular(tw *tar.Writer, e walkedEntry) error {
	hdr, err := tar.FileInfoHeader(e.info, "")
	if err != nil {
		return err
	}
	normalizeHeader(hdr, e.name)
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}

	// e.abs is a filepath.WalkDir-provided path rooted at the archive root; by
	// construction it stays within root and cannot be attacker-influenced here.
	f, err := os.Open(e.abs) // #nosec G304 -- walk-derived path under root, not external input
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(tw, f)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if n != hdr.Size {
		return &fileChangedError{Name: e.name, Declared: hdr.Size, Copied: n}
	}
	return nil
}

// normalizeHeader rewrites every nondeterministic field of a FileInfoHeader to a
// fixed value so identical trees produce identical bytes. Mode bits are
// deliberately preserved (the executable bit is semantically meaningful).
func normalizeHeader(hdr *tar.Header, name string) {
	hdr.Name = name
	hdr.ModTime = time.Unix(0, 0)
	hdr.AccessTime = time.Time{}
	hdr.ChangeTime = time.Time{}
	hdr.Uid = 0
	hdr.Gid = 0
	hdr.Uname = ""
	hdr.Gname = ""
	hdr.Format = tar.FormatPAX
}

// typeName names an unsupported (irregular) node type for an ArchiveEntryError
// reason. All returned values are static, log-safe descriptors.
func typeName(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSocket != 0:
		return "socket"
	case mode&fs.ModeNamedPipe != 0:
		return "named pipe"
	case mode&fs.ModeDevice != 0:
		if mode&fs.ModeCharDevice != 0 {
			return "character device"
		}
		return "block device"
	default:
		return "irregular (mode " + mode.String() + ")"
	}
}

// fileChangedError reports that a regular file's size changed between building
// its tar header (Declared bytes) and copying its contents (Copied bytes) — a
// concurrent mutation mid-archive. writeArchive fails closed rather than emit a
// truncated or corrupt entry. Name is the entry name; all fields are log-safe.
type fileChangedError struct {
	Name     string
	Declared int64
	Copied   int64
}

func (e *fileChangedError) Error() string {
	return "workspacestore: file " + strconv.Quote(e.Name) +
		" changed size during archive: declared " + strconv.FormatInt(e.Declared, 10) +
		" bytes, copied " + strconv.FormatInt(e.Copied, 10)
}
