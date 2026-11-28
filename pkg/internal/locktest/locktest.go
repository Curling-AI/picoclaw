// Package locktest holds the test seam of the session lock's Check. The fsync
// that fails after a lost NFS lease cannot be made to fail on a local disk, so
// the tests of every writer of sessions/ (the store and its sessionlock, the
// file tools, the fallback session manager) replace it here. The package is
// internal: only packages under pkg/ can import it, and the fork exports no
// test hook.
package locktest

import "os"

// Sync, when set, runs in place of the fsync of every Check.
var Sync func(*os.File) error
