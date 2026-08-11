package tui

import "github.com/looprig/tui/internal/input"

// Attachment input errors remain available at the root for compatibility.
type (
	EmptyInputError            = input.EmptyInputError
	UnsupportedAttachmentError = input.UnsupportedAttachmentError
	BinaryAttachmentError      = input.BinaryAttachmentError
	ImageUnsupportedError      = input.ImageUnsupportedError
	DeniedAttachmentError      = input.DeniedAttachmentError
	AttachmentTooLargeError    = input.AttachmentTooLargeError
	AttachmentNotFoundError    = input.AttachmentNotFoundError
	AttachmentReadError        = input.AttachmentReadError
)
