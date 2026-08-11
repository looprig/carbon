package input

import (
	"fmt"
	"strconv"
)

// EmptyInputError — submitted input had no text and no attachments.
type EmptyInputError struct{}

func (e EmptyInputError) Error() string {
	return "tui: input is empty (no text and no attachments)"
}

// UnsupportedAttachmentError — @path extension is neither image nor plaintext.
type UnsupportedAttachmentError struct{ Ext string }

func (e UnsupportedAttachmentError) Error() string {
	return fmt.Sprintf("tui: unsupported attachment extension %q (expected image or plaintext)", e.Ext)
}

// BinaryAttachmentError — an extensionless @path whose content is not UTF-8 text.
type BinaryAttachmentError struct{ Path string }

func (e BinaryAttachmentError) Error() string {
	return fmt.Sprintf("tui: attachment %q is not UTF-8 text (binary file)", e.Path)
}

// ImageUnsupportedError — image @path while the active model is text-only.
type ImageUnsupportedError struct{ Ext string }

func (e ImageUnsupportedError) Error() string {
	return fmt.Sprintf("tui: image attachment %q not supported by text-only model", e.Ext)
}

// DeniedAttachmentError — @path matched the secret/credential denylist.
type DeniedAttachmentError struct{ Path, Reason string }

func (e DeniedAttachmentError) Error() string {
	return fmt.Sprintf("tui: attachment %q denied: %s", e.Path, e.Reason)
}

// AttachmentTooLargeError — @path exceeds the size cap.
type AttachmentTooLargeError struct {
	Path      string
	Size, Max int64
}

func (e AttachmentTooLargeError) Error() string {
	return fmt.Sprintf("tui: attachment %q is %s bytes, exceeds cap of %s bytes",
		e.Path, strconv.FormatInt(e.Size, 10), strconv.FormatInt(e.Max, 10))
}

// AttachmentNotFoundError — @path does not exist (Cause wraps the os error).
type AttachmentNotFoundError struct {
	Path  string
	Cause error
}

func (e AttachmentNotFoundError) Error() string {
	return fmt.Sprintf("tui: attachment %q not found: %v", e.Path, e.Cause)
}

func (e *AttachmentNotFoundError) Unwrap() error { return e.Cause }

// AttachmentReadError — reading @path failed (Cause wraps the os error).
type AttachmentReadError struct {
	Path  string
	Cause error
}

func (e AttachmentReadError) Error() string {
	return fmt.Sprintf("tui: read attachment %q failed: %v", e.Path, e.Cause)
}

func (e *AttachmentReadError) Unwrap() error { return e.Cause }
