package input

import "errors"

// errAttachmentSymlink is wrapped into the error openNoFollow returns when
// clean's final path component is a symlink (POSIX) or reparse point
// (Windows) — readAttachment asked for a no-follow open and the target could
// not be opened without traversing one. readAttachment classifies this with
// errors.Is rather than a platform errno, since POSIX ELOOP has no Windows
// analogue. See attachment_open_unix.go and attachment_open_windows.go for
// the two platform mechanisms.
var errAttachmentSymlink = errors.New("input: refusing to open a symlinked/reparse-point attachment path")
