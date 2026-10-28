//go:build !unix && !windows

package memory

import (
	"errors"
	"os"
	"path/filepath"
)

// A platform without a non-following regular-file API cannot opt into the
// physical rewrite capability. Ordinary content storage remains available.
func openContentArtifact(dir, name string, create bool) (*os.File, *os.File, error) {
	if !create {
		if _, err := os.Lstat(filepath.Join(dir, name)); errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
	}
	return nil, nil, ErrHistoryUnsupported
}

func removeContentArtifact(_, _ string, _ ...func() error) error { return ErrHistoryUnsupported }

func contentArtifactFileIdentity(_ *os.File) (contentFileIdentity, error) {
	return contentFileIdentity{}, ErrHistoryUnsupported
}

func removeContentArtifactIdentity(_, _ string, _ *contentFileIdentity, _ ...func() error) error {
	return ErrHistoryUnsupported
}

func openContentArtifactWritable(_, _ string) (*os.File, *os.File, error) {
	return nil, nil, ErrHistoryUnsupported
}

func replaceContentArtifact(_ *os.File, _, _ string) error { return ErrHistoryUnsupported }
