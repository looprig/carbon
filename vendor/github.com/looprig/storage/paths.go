package storage

// PathReporter is an optional capability implemented by providers that persist
// data on the local filesystem. StoragePaths returns the provider's canonical
// local roots in a caller-owned slice. Remote and in-memory providers need not
// implement it.
type PathReporter interface {
	StoragePaths() []string
}
