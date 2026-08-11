package catalog

import "context"

var errUnsupported = errorUnsupported{}

type errorUnsupported struct{}

func (errorUnsupported) Error() string { return "catalog: unsupported platform" }

type platform interface {
	root() string
	ensureData() (bool, error)
	close() error
	validateRoot() error
	validateLock() error
	lock(context.Context) (func(), error)
	read(string) ([]byte, error)
	writeTemp([]byte) (string, error)
	rename(string, string) error
	remove(string) (bool, error)
	syncDir() error
}
