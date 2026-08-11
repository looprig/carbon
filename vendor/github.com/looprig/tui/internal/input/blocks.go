package input

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/looprig/core/content"
)

// maxAttachmentBytes caps the size of any single @path attachment (5 MiB).
const maxAttachmentBytes int64 = 5 << 20

// deniedSegments are path components that, if present, deny the whole path.
var deniedSegments = map[string]struct{}{
	".ssh": {}, ".aws": {}, ".gcloud": {}, ".gnupg": {}, ".kube": {},
}

// deniedBasenames are exact lower-cased basenames that are always denied.
var deniedBasenames = map[string]struct{}{
	".env": {}, ".npmrc": {}, ".netrc": {}, ".pypirc": {}, ".dockercfg": {},
	"id_rsa": {}, "id_dsa": {}, "id_ecdsa": {}, "id_ed25519": {},
}

// deniedExts are lower-cased extensions (with dot) that are always denied.
var deniedExts = map[string]struct{}{
	".env": {}, ".pem": {}, ".key": {}, ".p12": {},
	".pfx": {}, ".jks": {}, ".keystore": {},
}

// imageExts map a lower-cased extension to its image media type.
var imageExts = map[string]content.MediaType{
	".png":  content.MediaTypeImagePNG,
	".jpg":  content.MediaTypeImageJPEG,
	".jpeg": content.MediaTypeImageJPEG,
	".gif":  content.MediaTypeImageGIF,
	".webp": content.MediaTypeImageWebP,
}

// plaintextExts are lower-cased extensions read as UTF-8 text. .svg is here
// (XML), deliberately NOT an image, because providers reject image/svg+xml.
var plaintextExts = map[string]struct{}{
	".txt": {}, ".md": {}, ".go": {}, ".py": {}, ".js": {}, ".ts": {},
	".json": {}, ".yaml": {}, ".yml": {}, ".toml": {}, ".sh": {}, ".csv": {},
	".html": {}, ".xml": {}, ".rs": {}, ".java": {}, ".c": {}, ".cpp": {},
	".h": {}, ".svg": {},
}

// BuildBlocks parses input into content blocks: a leading prompt TextBlock
// (when non-empty) followed by one block per @path attachment, in order.
// allowImages gates image attachments; when false an image @path is rejected
// at the boundary rather than sent to a text-only model.
//
// Classification (denylist + extension/modality) happens strictly BEFORE any
// filesystem syscall, so a denied, unsupported, or text-only-model attachment
// is rejected without ever opening a file.
func BuildBlocks(input string, allowImages bool) ([]content.Block, error) {
	prompt, attachments := splitInput(input)

	if len(attachments) == 0 && prompt == "" {
		return nil, &EmptyInputError{}
	}

	blocks := make([]content.Block, 0, len(attachments)+1)
	if prompt != "" {
		blocks = append(blocks, &content.TextBlock{Text: prompt})
	}
	for _, path := range attachments {
		block, err := buildAttachment(path, allowImages)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

// splitInput separates @path attachments from the prompt text. An attachment is a
// whitespace-delimited token starting with '@' (len > 1). The REST of the text is kept
// VERBATIM — preserving the user's newlines and in-line spacing — rather than reflowed
// through Fields/Join (which collapsed every run of whitespace, including newlines, to a
// single space and silently flattened multi-line input). With no attachments the input
// is returned unchanged but for outer trimming; with attachments the @tokens are removed
// line-by-line so the surrounding newlines still survive.
func splitInput(input string) (prompt string, attachments []string) {
	for _, tok := range strings.Fields(input) {
		if len(tok) > 1 && tok[0] == '@' {
			attachments = append(attachments, tok[1:])
		}
	}
	if len(attachments) == 0 {
		return strings.TrimSpace(input), nil
	}
	var lines []string
	for _, line := range strings.Split(input, "\n") {
		var kept []string
		for _, tok := range strings.Fields(line) {
			if len(tok) > 1 && tok[0] == '@' {
				continue // already collected above
			}
			kept = append(kept, tok)
		}
		lines = append(lines, strings.Join(kept, " "))
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), attachments
}

// buildAttachment classifies path, then (only for an accepted classification)
// opens, stats, and reads the file into a content block.
func buildAttachment(path string, allowImages bool) (content.Block, error) {
	clean := filepath.Clean(path)
	if reason, denied := denyReason(clean); denied {
		return nil, &DeniedAttachmentError{Path: clean, Reason: reason}
	}

	ext := strings.ToLower(filepath.Ext(clean))
	mediaType, isImage := imageExts[ext]
	switch {
	case isImage:
		if !allowImages {
			return nil, &ImageUnsupportedError{Ext: ext}
		}
		data, err := readAttachment(clean)
		if err != nil {
			return nil, err
		}
		return &content.ImageBlock{MediaType: mediaType, Source: content.ImageSource{Data: data}}, nil
	case isPlaintextExt(ext) || ext == "":
		data, err := readAttachment(clean)
		if err != nil {
			return nil, err
		}
		// Extensionless files (Makefile, Dockerfile, LICENSE, .env-less configs, …) are
		// commonly plaintext. Accept them as text ONLY when the content is valid UTF-8,
		// so attaching a binary without an extension is rejected cleanly rather than
		// injecting garbage. Known plaintext extensions are trusted without the check.
		if ext == "" && !utf8.Valid(data) {
			return nil, &BinaryAttachmentError{Path: clean}
		}
		return &content.TextBlock{Text: formatAttachment(filepath.Base(clean), ext, string(data))}, nil
	default:
		return nil, &UnsupportedAttachmentError{Ext: ext}
	}
}

func isPlaintextExt(ext string) bool {
	_, ok := plaintextExts[ext]
	return ok
}

// formatAttachment formats an attached file's content for the message as markdown: a
// markdown file (.md/.markdown) is inlined as-is so it renders as markdown; any other
// text file is wrapped in a fenced code block (language inferred from the extension) so
// it renders as a syntax-aware code block. A "`name`" header labels it either way. The
// block text is BOTH what the model receives and what the committed user row renders
// (see renderUser), so the fences read well in both.
func formatAttachment(name, ext, body string) string {
	body = strings.TrimRight(body, "\n")
	if ext == ".md" || ext == ".markdown" {
		return "`" + name + "`\n\n" + body
	}
	return "`" + name + "`\n```" + strings.TrimPrefix(ext, ".") + "\n" + body + "\n```"
}

// denyReason reports whether clean matches the secret/credential denylist,
// inspecting lower-cased path segments, basename, and extension. It performs
// no I/O.
func denyReason(clean string) (string, bool) {
	for _, seg := range strings.Split(clean, string(os.PathSeparator)) {
		if _, ok := deniedSegments[strings.ToLower(seg)]; ok {
			return "denied path segment " + strings.ToLower(seg), true
		}
	}

	base := strings.ToLower(filepath.Base(clean))
	if _, ok := deniedBasenames[base]; ok {
		return "denied basename " + base, true
	}
	if strings.HasPrefix(base, ".env.") {
		return "denied basename pattern .env.*", true
	}

	ext := strings.ToLower(filepath.Ext(clean))
	if _, ok := deniedExts[ext]; ok {
		return "denied extension " + ext, true
	}
	return "", false
}

// readAttachment opens clean with a no-follow, non-blocking open (rejecting a
// symlinked/reparse-point final component, and — on platforms where it means
// anything — not blocking on a FIFO/named pipe until a writer connects),
// confirms it is a regular file via fd stat, enforces the size cap at stat
// time and again after a bounded read, and returns the bytes. See
// openNoFollow in attachment_open_unix.go / attachment_open_windows.go for
// the platform-specific mechanism.
func readAttachment(clean string) ([]byte, error) {
	// #nosec G304 -- user-selected local path, validated by denylist + classify + openNoFollow + fd stat
	f, err := openNoFollow(clean)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &AttachmentNotFoundError{Path: clean, Cause: err}
		}
		if errors.Is(err, errAttachmentSymlink) {
			return nil, &DeniedAttachmentError{Path: clean, Reason: "symlinked path component (no-follow)"}
		}
		return nil, &DeniedAttachmentError{Path: clean, Reason: "cannot open (symlink or permission)"}
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, &AttachmentReadError{Path: clean, Cause: err}
	}
	if !fi.Mode().IsRegular() {
		return nil, &DeniedAttachmentError{Path: clean, Reason: "not a regular file"}
	}
	if fi.Size() > maxAttachmentBytes {
		return nil, &AttachmentTooLargeError{Path: clean, Size: fi.Size(), Max: maxAttachmentBytes}
	}

	data, overCap, err := readCapped(f, maxAttachmentBytes)
	if err != nil {
		return nil, &AttachmentReadError{Path: clean, Cause: err}
	}
	if overCap {
		return nil, &AttachmentTooLargeError{Path: clean, Size: int64(len(data)), Max: maxAttachmentBytes}
	}
	return data, nil
}

// readCapped reads from r up to max bytes. It signals overCap when the source
// holds more than max bytes (e.g. a file that grew after its stat-time size
// check), so the caller can reject it. The returned data is read in full up to
// max+1 bytes; at the boundary (exactly max bytes) overCap is false and data is
// not truncated.
func readCapped(r io.Reader, max int64) (data []byte, overCap bool, err error) {
	data, err = io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, false, err
	}
	return data, int64(len(data)) > max, nil
}
