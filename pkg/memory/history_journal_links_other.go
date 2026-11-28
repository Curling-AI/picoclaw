//go:build !unix

package memory

import "io/fs"

// historyJournalHasSecondLink does not read the link count outside unix.
//
// shortcut: a journal left with a second link is not repaired there; on
// Windows, whose artifact open also refuses a second link, its recovery keeps
// failing until the temporary file is removed by hand.
func historyJournalHasSecondLink(fs.FileInfo) bool { return false }
