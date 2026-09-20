// The `unix` constraint covers solaris and aix, which have no syscall.Flock, so
// it would build there and then fail to compile. Naming the platforms instead
// leaves lock_other.go to fail with an explanation, not a missing symbol.
//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package state

import (
	"errors"
	"os"
	"syscall"
)

// openLockOnly is O_NOFOLLOW where the platform has it. A symlink planted at
// the lock path in a directory another local user can write to would otherwise
// redirect the lock onto a file elsewhere.
const openLockOnly = syscall.O_NOFOLLOW

// lockFile takes an advisory lock without blocking: shared when the caller only
// intends to read, exclusive otherwise. A writer that cannot get it must stop
// rather than wait, because the other holder is a mirror pass over the same
// secrets and queueing behind it only moves the collision.
func lockFile(f *os.File, shared bool) error {
	how := syscall.LOCK_EX
	held := "another infisical-mirror run holds the state file"
	if shared {
		how = syscall.LOCK_SH
		held = "another infisical-mirror run is writing the state file"
	}
	err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errors.New(held)
	}
	return err
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// dirSyncTolerable reports whether a failed directory fsync is a property of
// the filesystem rather than a failed write. Some filesystems refuse fsync on a
// directory outright; the rename has already landed by then, so refusing to
// save would make the store unusable there for no gain.
func dirSyncTolerable(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)
}
