// Package locktest holds the test seam of the session lock's Check. The fsync
// that fails after a lost NFS lease cannot be made to fail on a local disk, so
// the tests of pkg/memory and of its sessionlock replace it here. The package
// is internal: only pkg/memory and its subpackages can import it, and the
// fork exports no test hook.
package locktest

import "os"

// Sync, when set, runs in place of the fsync of every Check.
var Sync func(*os.File) error
