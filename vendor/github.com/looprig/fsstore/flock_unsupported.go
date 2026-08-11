//go:build !unix

package fsstore

import "os"

// lockFileEx fails closed on a platform without advisory file locking: fsstore
// cannot guarantee two processes will not corrupt a shared file, so it refuses
// to proceed rather than degrade to a silent no-op that would permit corruption.
func lockFileEx(f *os.File) error {
	return &FlockError{Op: "lock", Path: f.Name(), Cause: errFlockUnsupported}
}

// tryLockFileEx fails closed on a platform without advisory file locking, mirroring
// lockFileEx: it cannot prove exclusivity, so it reports (false, *FlockError)
// rather than pretend the lock was granted (which would let two holders believe
// they each own the same lease). The Leaser surfaces this as a hard error, never
// as a *LeaseHeldError.
func tryLockFileEx(f *os.File) (acquired bool, err error) {
	return false, &FlockError{Op: "trylock", Path: f.Name(), Cause: errFlockUnsupported}
}

// unlockFile mirrors lockFileEx: with no advisory locking to release, it reports
// the same fail-closed error so no caller mistakes a no-op for success.
func unlockFile(f *os.File) error {
	return &FlockError{Op: "unlock", Path: f.Name(), Cause: errFlockUnsupported}
}
