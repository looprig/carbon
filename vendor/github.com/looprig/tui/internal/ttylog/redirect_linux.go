//go:build linux

package ttylog

import "syscall"

// dupFD duplicates fd, returning a new descriptor referring to the same file.
func dupFD(fd int) (int, error) { return syscall.Dup(fd) }

// dup2FD points newfd at the same file as oldfd. Linux uses dup3 because the dup2
// syscall is unavailable on some architectures (e.g. arm64).
func dup2FD(oldfd, newfd int) error { return syscall.Dup3(oldfd, newfd, 0) }

// closeFD closes a descriptor.
func closeFD(fd int) error { return syscall.Close(fd) }
