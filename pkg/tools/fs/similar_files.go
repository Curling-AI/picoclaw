package fstools

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"unicode"
)

const maxSimilarFiles = 10

// similarFilesHint names the files beside a missing path that look like it, so
// the model stops guessing: pdftoppm numbers its pages -1, -01 or -001 depending
// on how many there are. Empty when err is another failure or nothing is alike.
func similarFilesHint(sysFs fileSystem, path string, err error) string {
	if !errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	entries, readErr := sysFs.ReadDir(filepath.Dir(path))
	if readErr != nil {
		return ""
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	similar := similarFileNames(filepath.Base(path), names)
	if len(similar) == 0 {
		return ""
	}
	return fmt.Sprintf("\nSimilar files in that directory: %s", strings.Join(similar, ", "))
}

// similarFileNames keeps the names that start like target, once its extension and
// the number or separator at its end are gone.
func similarFileNames(target string, names []string) []string {
	stem := strings.TrimSuffix(target, filepath.Ext(target))
	stem = strings.ToLower(strings.TrimRightFunc(stem, func(r rune) bool {
		return unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' || r == ' '
	}))
	if stem == "" {
		return nil
	}
	var similar []string
	for _, name := range names {
		if strings.HasPrefix(strings.ToLower(name), stem) {
			similar = append(similar, name)
			if len(similar) == maxSimilarFiles {
				break
			}
		}
	}
	return similar
}
