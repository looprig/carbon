//go:build unix && !linux

package ttylog

import "syscall"

// dupFD duplicates fd, returning a new descriptor referring to the same file.
func dupFD(fd int) (int, error) { return syscall.Dup(fd) }

// dup2FD points newfd at the same file as oldfd. Darwin and the BSDs do not expose
// dup3 in the syscall package, so they use the classic dup2.
func dup2FD(oldfd, newfd int) error { return syscall.Dup2(oldfd, newfd) }

// closeFD closes a descriptor.
func closeFD(fd int) error { return syscall.Close(fd) }
