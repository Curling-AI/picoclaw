package memory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/google/uuid"

	"github.com/sipeed/picoclaw/pkg/providers"
	"github.com/sipeed/picoclaw/pkg/providers/messageutil"
)

// A preimage names exact source-owned bytes. It is never discovered by glob,
// adopted by its filename or used as an order witness. The descriptor remains
// bounded retirement provenance in Last after its payload is collected.
type contentPreimage struct {
	Version    uint8  `json:"version"`
	Key        string `json:"key"`
	DomainID   string `json:"domain"`
	MutationID string `json:"mutation"`
	ID         string `json:"id"`
	Bytes      int64  `json:"bytes"`
	Digest     string `json:"digest"`
}

func (image contentPreimage) valid(key, domain, mutation string) bool {
	id, err := uuid.Parse(image.ID)
	return image.Version == 1 && image.Key == key && image.DomainID == domain && image.MutationID == mutation &&
		err == nil && id.String() == image.ID && uuid.Validate(domain) == nil && uuid.Validate(mutation) == nil &&
		image.Bytes >= 0 && validHistoryDigest(image.Digest)
}

func contentPreimageName(key, id string) string {
	return sanitizeKey(key) + ".jsonl.content-preimage." + id
}

func closeContentArtifact(file, dir *os.File) error {
	return errors.Join(file.Close(), dir.Close())
}

func (s *JSONLStore) createContentPreimage(
	ctx context.Context,
	image contentPreimage,
	data []byte,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !image.valid(image.Key, image.DomainID, image.MutationID) || int64(len(data)) != image.Bytes ||
		contentDigest(data) != image.Digest {
		return ErrContentIdentityUnavailable
	}
	file, dir, err := openContentArtifact(s.dir, contentPreimageName(image.Key, image.ID), true)
	if err != nil {
		return err
	}
	defer closeContentArtifact(file, dir)
	written, err := file.Write(data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = file.Sync()
	}
	if err == nil {
		err = dir.Sync()
	}
	// An interrupted create has no committed provenance. Preserve it rather
	// than deleting a path that another process may have replaced.
	if err != nil {
		return err
	}
	return ctx.Err()
}

func (s *JSONLStore) readContentPreimage(
	ctx context.Context,
	key, domain, mutation string,
	image *contentPreimage,
) ([]byte, error) {
	if image == nil || !image.valid(key, domain, mutation) {
		return nil, ErrContentIdentityUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, dir, err := openContentArtifact(s.dir, contentPreimageName(key, image.ID), false)
	if err != nil {
		return nil, err
	}
	defer closeContentArtifact(file, dir)
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() != image.Bytes {
		return nil, ErrContentIdentityUnavailable
	}
	// The descriptor bounds the read; an unowned suffix is never adopted.
	data, err := io.ReadAll(io.LimitReader(file, image.Bytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != image.Bytes || contentDigest(data) != image.Digest {
		return nil, ErrContentIdentityUnavailable
	}
	return data, ctx.Err()
}

// The caller holds the source lock, has committed the exact application ACK,
// and has settled its journal. Missing payload already retired is harmless;
// Changed bytes, symbolic links and hard links are preserved with an explicit failure.
func (s *JSONLStore) retireContentPreimage(ctx context.Context, key string, image *contentPreimage) error {
	if image == nil {
		return nil
	}
	if err := s.checkContentJournalSettled(key); err != nil {
		return err
	}
	_, err := s.readContentPreimage(ctx, key, image.DomainID, image.MutationID, image)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	name := contentPreimageName(key, image.ID)
	if err = s.removeOwnedContentArtifact(name); err != nil {
		return fmt.Errorf("memory: retire content preimage: %w", err)
	}
	return nil
}

func (s *JSONLStore) retireAppliedContentImages(ctx context.Context, key string, images ...*contentPreimage) {
	seen := make(map[string]bool, len(images))
	for _, image := range images {
		if image == nil || seen[image.ID] {
			continue
		}
		seen[image.ID] = true
		if err := s.retireContentPreimage(ctx, key, image); err != nil {
			log.Printf("memory: content application committed; preimage %s retirement pending: %v", image.ID, err)
		}
	}
}

// Explicit owner deletion and automatic retention both inspect exact rooted
// descriptors before removing the listing/body. No unreferenced filename is
// permission to remove a pre-journal orphan or another source's bytes.
func (s *JSONLStore) contentImagesForDeletion(
	ctx context.Context,
	key string,
	meta SessionMeta,
) ([]*contentPreimage, error) {
	state := meta.ContentIdentity
	if state == nil {
		return nil, nil
	}
	if len(state.opaque) != 0 {
		return nil, ErrContentIdentityUnavailable
	}
	images := make([]*contentPreimage, 0, 2)
	if state.Pending != nil && state.Pending.Mutation != nil {
		image := state.Pending.Mutation.Before.Image
		if image == nil || !image.valid(key, state.DomainID, state.Pending.Mutation.Transition.MutationID) {
			return nil, ErrContentIdentityUnavailable
		}
		images = append(images, image)
	}
	if state.Last != nil && state.Last.Mutation != nil && state.Last.Mutation.Retired != nil {
		image := state.Last.Mutation.Retired
		if !image.valid(key, state.DomainID, state.Last.Mutation.Transition.MutationID) {
			return nil, ErrContentIdentityUnavailable
		}
		images = append(images, image)
	}
	for _, image := range images {
		if _, err := s.readContentPreimage(ctx, key, image.DomainID, image.MutationID, image); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	return images, nil
}

func (s *JSONLStore) deleteContentImages(key string, images []*contentPreimage) error {
	for _, image := range images {
		if err := s.removeOwnedContentArtifact(contentPreimageName(key, image.ID)); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func contentPreimageMatchesImage(key string, image contentImage, data []byte) bool {
	return int64(len(data)) == image.Bytes &&
		contentJSONDigest([]any{key, image.MetaBytes, contentDigest(data), image.Bytes}) == image.Observation
}

func contentImageFromPreimage(ctx context.Context, data []byte) (contentImage, error) {
	// Reuse the same newline/invalid/thought rules without writing a temporary
	// transcript or assigning any private history origin to frozen content.
	return scanContentPreimage(ctx, bytes.NewReader(data))
}

func scanContentPreimage(ctx context.Context, reader io.Reader) (contentImage, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLineSize)
	scanner.Split(scanCommittedHistoryLine)
	image := contentImage{Rows: []contentRawRow{}, DeliveryIDCounts: make(map[string]int)}
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return contentImage{}, err
		}
		raw := bytes.TrimRight(scanner.Bytes(), "\r\n")
		if len(raw) == 0 {
			continue
		}
		image.Count++
		if id := physicalDeliveryIDFromRow(raw); id != "" {
			image.DeliveryIDCounts[id]++
		}
		var message providers.Message
		if json.Unmarshal(raw, &message) != nil || messageutil.IsTransientAssistantThoughtMessage(message) {
			continue
		}
		image.Rows = append(
			image.Rows,
			contentRawRow{Line: image.Count, Raw: bytes.Clone(raw), Message: ordinaryContentMessage(message)},
		)
	}
	if err := scanner.Err(); err != nil {
		return contentImage{}, err
	}
	return image, ctx.Err()
}
