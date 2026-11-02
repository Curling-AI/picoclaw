package memory

import (
	"context"
	"errors"
	"os"
)

// Owner deletion uses both exact metadata and its external format/checkpoint
// floor. A metadata-only rollback must not turn a live Pending into an orphan.
// Unrooted files are deliberately outside this lifecycle operation.
func (s *JSONLStore) contentStorageForDeletion(
	ctx context.Context,
	key string,
	meta SessionMeta,
) ([]contentArtifactDescriptor, []*contentPreimage, error) {
	state := meta.ContentIdentity
	if state != nil && len(state.opaque) != 0 {
		return nil, nil, ErrContentIdentityUnavailable
	}
	var headers []contentStorageHeader
	if state != nil && state.storage != nil {
		if !validContentStorageHeader(state, key) {
			return nil, nil, ErrContentIdentityUnavailable
		}
		headers = append(headers, state.storage.Header)
	}
	witness, present, err := s.readContentDomain(key)
	if err != nil {
		if state != nil && state.storage != nil || witness.DiskVersion != 0 || witness.Storage != nil {
			return nil, nil, err
		}
		// The pre-disk2 owner deletion contract did not require order/content
		// proof. Do not add a new requirement to never-formatted sources.
		if !onlyContentIdentityUnavailable(err) {
			return nil, nil, err
		}
	}
	if err == nil && present && witness.Storage != nil {
		headers = append(headers, *witness.Storage)
	}
	filesByName := make(map[string]contentArtifactDescriptor)
	imagesByID := make(map[string]*contentPreimage)
	for _, header := range headers {
		root := &contentIdentityState{storage: &contentStorageState{Header: header}}
		for _, file := range contentStorageRoots(root) {
			// The floor is appended last and owns the current extent of an
			// appendable index also mentioned by older metadata.
			filesByName[file.name()] = file
		}
		for _, image := range []*contentPreimage{header.BeforeImage, header.RetiredImage} {
			if image != nil {
				imagesByID[image.ID] = image
			}
		}
	}
	files := make([]contentArtifactDescriptor, 0, len(filesByName))
	for _, file := range filesByName {
		if err = s.verifyContentArtifact(ctx, file); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
		files = append(files, file)
	}
	images := make([]*contentPreimage, 0, len(imagesByID))
	for _, image := range imagesByID {
		if _, err = s.readContentPreimage(ctx, key, image.DomainID, image.MutationID, image); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
		images = append(images, image)
	}
	return files, images, nil
}

func (s *JSONLStore) deleteContentStorage(files []contentArtifactDescriptor) error {
	for _, file := range files {
		if err := s.removeOwnedContentArtifact(file.name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *JSONLStore) removeOwnedContentArtifact(name string) error {
	if err := s.failContentStorage(name, "remove"); err != nil {
		return err
	}
	err := removeContentArtifact(s.dir, name, func() error {
		return s.failContentStorage(name, "remove-directory-sync")
	})
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// A previous unlink may have lost its directory-sync reply. Confirm the
	// current absence durably before retiring the last ownership descriptor.
	dir, openErr := os.Open(s.dir)
	if openErr != nil {
		return openErr
	}
	if err = s.failContentStorage(name, "remove-directory-sync"); err == nil {
		err = dir.Sync()
	}
	return errors.Join(err, dir.Close())
}
