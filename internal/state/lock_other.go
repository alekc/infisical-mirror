//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package state

import (
	"errors"
	"os"
)

// openLockOnly adds nothing where O_NOFOLLOW is unavailable. Nothing reaches
// the lock file here anyway: lockFile below refuses first.
const openLockOnly = 0

// The file backend refuses to run where it cannot take the lock, rather than
// running unlocked: two concurrent passes each write back a document missing
// the other's work, surfacing later as unsynced secrets far from the cause.
// Release builds target Linux and macOS, so this guards an unintended port.
func lockFile(*os.File, bool) error {
	return errors.New("the file state backend needs a unix-like OS for advisory locking")
}

func unlockFile(*os.File) error { return nil }

// dirSyncTolerable is unreachable here: lockFile refuses before any write.
func dirSyncTolerable(error) bool { return true }
