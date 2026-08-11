//go:build !unix

package ttylog

import "errors"

// errUnsupported reports that file-descriptor redirection is not available on this
// platform (e.g. Windows, plan9, js).
var errUnsupported = errors.New("fd redirection unsupported on this platform")

func dupFD(int) (int, error) { return -1, errUnsupported }
func dup2FD(int, int) error  { return errUnsupported }
func closeFD(int) error      { return nil }
