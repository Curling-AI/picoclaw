package memory

import (
	"context"
	"errors"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/sipeed/picoclaw/pkg/providers"
)

var nativeArchiveLinkTarget = regexp.MustCompile(`\]\(\s*<?([^\s)>]+)`)

const (
	nativePublicArchivePolicy = "picoclaw-public-history-v1"
	maxNativeArchiveRows      = 1000
	maxNativeArchiveBytes     = 8 << 20
)

type nativeArchiveProof struct {
	PolicyID   string                    `json:"policyId"`
	Frontier   publicIndexHeader         `json:"frontier"`
	Transition ContentTransition         `json:"transition"`
	Archived   []ContentRef              `json:"archived"`
	Excluded   []ContentArchiveExclusion `json:"excluded"`
}

// PrepareNativeHistoryArchive is a writer recovery step, never a getter or an
// application/read ACK. It releases only a fully proved native archive cut.
func (s *JSONLStore) PrepareNativeHistoryArchive(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lock := s.sessionLock(key)
	lock.Lock()
	defer lock.Unlock()
	if err := s.canonicalContentKey(ctx, key); err != nil {
		return err
	}
	return s.prepareNativeHistoryArchiveLocked(ctx, key)
}

func (s *JSONLStore) prepareNativeHistoryArchiveLocked(ctx context.Context, key string) error {
	meta, _, err := s.readContentMeta(key)
	if err != nil {
		return err
	}
	if meta.ContentIdentity == nil || meta.ContentIdentity.ArchivePolicy != nativePublicArchivePolicy ||
		meta.ContentIdentity.Pending == nil {
		return nil
	}
	image, _, err := s.contentMutationImage(ctx, key)
	if err != nil {
		return err
	}
	pending := image.Meta.ContentIdentity.Pending
	if pending == nil || pending.Mutation != nil {
		return nil // Explicit mutation application retains its separate consumer ACK.
	}
	before, err := s.contentCompactionSnapshot(ctx, key, image, pending, ContentBefore)
	if err != nil {
		return err
	}
	byLine := make(map[int][]byte, len(image.Rows))
	for _, row := range image.Rows {
		byLine[row.Line] = row.Raw
	}
	for index, locator := range pending.Before.Rows {
		before.Entries[index].Message.HistoryPublicMessage = decodedPublicMessage(byLine[locator.Line])
	}
	proof, err := s.verifyNativeArchiveAt(
		ctx,
		key,
		image.Meta,
		before,
		pending.Transition,
		pending.NativeArchiveFrontier,
	)
	if err != nil {
		return err
	}
	return s.commitContentArchiveAck(ctx, key, image, contentJSONDigest(proof))
}

func (s *JSONLStore) nativeArchiveMessages(
	before ContentFrame,
	transition ContentTransition,
) (map[ContentRef]providers.Message, []ContentArchiveExclusion, error) {
	if len(transition.Dropped) > len(before.Entries) || len(transition.Dropped) > maxNativeArchiveRows {
		return nil, nil, ErrHistoryCapacity
	}
	wanted := make(map[ContentRef]providers.Message, len(transition.Dropped))
	excluded := make([]ContentArchiveExclusion, 0)
	for index, ref := range transition.Dropped {
		entry := before.Entries[index]
		if entry.Ref == nil || *entry.Ref != ref {
			return nil, nil, ErrPublicHistoryCoverage
		}
		message := entry.Message
		if message.HistoryPublicMessage != nil {
			if s.historyPublicProjector() == nil || !validPublicProjection(message, *message.HistoryPublicMessage) {
				return nil, nil, ErrPublicHistoryCoverage
			}
			message = *message.HistoryPublicMessage
		}
		public, visible := publicHistoryMessage(message)
		if visible {
			wanted[ref] = public
			continue
		}
		reason := ContentArchiveUnsupportedRole
		if message.Role == "user" || message.Role == "assistant" || message.Role == "tool" {
			raw := strings.TrimSpace(message.Content)
			if !strings.HasPrefix(raw, "[System]") && !strings.HasPrefix(raw, "[SYSTEM]") &&
				!strings.HasPrefix(raw, "[System: ") {
				return nil, nil, ErrPublicHistoryCoverage
			}
			reason = ContentArchiveInternalSteering
		}
		excluded = append(excluded, ContentArchiveExclusion{Ref: ref, Reason: reason})
	}
	return wanted, excluded, nil
}

func (s *JSONLStore) verifyNativeArchive(
	ctx context.Context,
	key string,
	meta SessionMeta,
	before ContentFrame,
	transition ContentTransition,
) (nativeArchiveProof, error) {
	return s.verifyNativeArchiveAt(ctx, key, meta, before, transition, nil)
}

func (s *JSONLStore) verifyNativeArchiveAt(
	ctx context.Context, key string, meta SessionMeta, before ContentFrame, transition ContentTransition,
	frontier *publicIndexHeader,
) (_ nativeArchiveProof, resultErr error) {
	wanted, excluded, err := s.nativeArchiveMessages(before, transition)
	if err != nil {
		return nativeArchiveProof{}, err
	}
	files := os.DirFS(s.dir)
	name := sanitizeKey(key)
	if meta.PublicHistoryVersion != PublicHistoryVersion || !meta.PublicHistoryComplete ||
		meta.PublicHistoryPreparation != nil {
		return nativeArchiveProof{}, ErrPublicHistoryCoverage
	}
	if readErr := checkPublicReadFiles(ctx, files, name, &meta); readErr != nil {
		return nativeArchiveProof{}, readErr
	}
	if meta.publicRefUnavailable {
		return nativeArchiveProof{}, ErrPublicHistoryCoverage
	}
	index, err := openPublicIndex(files, name+".jsonl.public.index", meta)
	if err != nil {
		return nativeArchiveProof{}, err
	}
	defer closePublicFile(index.file, &resultErr)
	file, err := openPublicHistoryReadFile(files, name+".jsonl.public")
	if err != nil {
		return nativeArchiveProof{}, err
	}
	defer closePublicFile(file, &resultErr)
	info, err := file.Stat()
	if err != nil || info.Size() != meta.PublicHistoryBytes {
		return nativeArchiveProof{}, errors.Join(ErrPublicHistoryCoverage, err)
	}
	bound := index.header
	if frontier != nil {
		if err := validateNativeArchiveFrontier(index, *frontier); err != nil {
			return nativeArchiveProof{}, err
		}
		bound = *frontier
	}
	proof := nativeArchiveProof{
		PolicyID: nativePublicArchivePolicy, Frontier: bound, Transition: transition,
		Archived: make([]ContentRef, 0, len(wanted)), Excluded: excluded,
	}
	projector := s.historyPublicProjector()
	dropped := make(map[ContentRef]bool, len(transition.Dropped))
	for _, ref := range transition.Dropped {
		dropped[ref] = true
	}
	var bytesRead int64
	rows := 0
	entries := make([]PublicHistoryEntry, 0)
	roots := make(map[string]bool)
	// shortcut: non-local imports need the whole bounded archive, index anchors when larger imported archives must compact.
	for position := bound.Count - 1; position >= 0 && (len(wanted) != 0 || meta.PublicHistoryNonLocalImports); position-- {
		if err := ctx.Err(); err != nil {
			return nativeArchiveProof{}, err
		}
		if rows >= maxNativeArchiveRows {
			return nativeArchiveProof{}, ErrHistoryCapacity
		}
		record, err := index.record(position)
		if err != nil {
			return nativeArchiveProof{}, err
		}
		if record.Length > maxNativeArchiveBytes-bytesRead {
			return nativeArchiveProof{}, ErrHistoryCapacity
		}
		entry, size, err := readPublicPageEntry(file, index, position, bound.Bytes)
		if err != nil {
			return nativeArchiveProof{}, err
		}
		bytesRead += size
		rows++
		entries = append(entries, entry)
		if entry.AnchorRef != nil && dropped[*entry.AnchorRef] {
			if err := verifyNativePublicMessage(ctx, projector, entry.Message); err != nil {
				return nativeArchiveProof{}, err
			}
		}
		if entry.Ref == nil {
			continue
		}
		expected, present := wanted[*entry.Ref]
		if !present {
			continue
		}
		if entry.ImportID != "" || entry.Partial || contentJSONDigest(entry.Message) != contentJSONDigest(expected) {
			return nativeArchiveProof{}, ErrPublicHistoryCoverage
		}
		if err := verifyNativePublicMessage(ctx, projector, entry.Message); err != nil {
			return nativeArchiveProof{}, err
		}
		roots[entry.ID] = true
		proof.Archived = append(proof.Archived, *entry.Ref)
		delete(wanted, *entry.Ref)
	}
	if len(wanted) != 0 {
		return nativeArchiveProof{}, ErrPublicHistoryCoverage
	}
	if err := verifyNativeArchiveSupplements(ctx, projector, entries, roots); err != nil {
		return nativeArchiveProof{}, err
	}
	return proof, nil
}

func validateNativeArchiveFrontier(index *publicIndexReader, frontier publicIndexHeader) error {
	current := index.header
	if frontier.Generation != current.Generation || frontier.Revision > current.Revision ||
		frontier.Count < 0 || frontier.Count > current.Count || frontier.Bytes < 0 || frontier.Bytes > current.Bytes {
		return ErrPublicHistoryCoverage
	}
	if frontier.Count == 0 {
		if frontier.Bytes != 0 || frontier.Digest != emptyHistoryDigest {
			return ErrPublicHistoryCoverage
		}
		return nil
	}
	last, err := index.record(frontier.Count - 1)
	if err != nil || last.Offset+last.Length != frontier.Bytes || publicIndexRecordDigest(last) != frontier.Digest {
		return errors.Join(ErrPublicHistoryCoverage, err)
	}
	return nil
}

func verifyNativeArchiveSupplements(
	ctx context.Context,
	projector PublicHistoryProjector,
	entries []PublicHistoryEntry,
	roots map[string]bool,
) error {
	children := make(map[string][]string)
	byID := make(map[string]PublicHistoryEntry, len(entries))
	for _, entry := range entries {
		byID[entry.ID] = entry
		for _, parent := range []string{entry.AnchorID, entry.BoundaryID} {
			if parent != "" {
				children[parent] = append(children[parent], entry.ID)
			}
		}
	}
	queue := make([]string, 0, len(roots))
	for id := range roots {
		queue = append(queue, id)
	}
	for position := 0; position < len(queue); position++ {
		for _, id := range children[queue[position]] {
			if !roots[id] {
				roots[id] = true
				queue = append(queue, id)
				if err := verifyNativePublicMessage(ctx, projector, byID[id].Message); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func verifyNativePublicMessage(ctx context.Context, projector PublicHistoryProjector, message providers.Message) error {
	if projector != nil {
		return projector.Verify(ctx, message)
	}
	if len(message.Media) != 0 || len(message.Attachments) != 0 {
		return ErrPublicHistoryCoverage
	}
	for remaining := message.Content; ; {
		match := nativeArchiveLinkTarget.FindStringSubmatchIndex(remaining)
		if len(match) == 0 {
			break
		}
		target, err := url.Parse(remaining[match[2]:match[3]])
		if err != nil {
			return ErrPublicHistoryCoverage
		}
		if target.Scheme == "" && target.Path == "" && target.RawQuery == "" && target.Fragment != "" {
			remaining = remaining[match[1]:]
			continue
		}
		switch strings.ToLower(target.Scheme) {
		case "", "artifact", "sandbox", "media", "file":
			return ErrPublicHistoryCoverage
		}
		remaining = remaining[match[1]:]
	}
	return ctx.Err()
}

var _ HistoryNativeContentArchive = (*JSONLStore)(nil)
