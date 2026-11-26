//go:build unix

package memory

import (
	"io/fs"
	"syscall"
)

// historyJournalHasSecondLink reports whether the journal os.Stat described
// still has another name, as its temporary file when its writer died between
// the link and the temporary file's removal.
func historyJournalHasSecondLink(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink > 1
}
