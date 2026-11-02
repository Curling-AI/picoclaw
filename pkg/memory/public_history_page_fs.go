package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"strings"
)

type publicHistoryReadFile interface {
	fs.File
	io.ReaderAt
}

func openPublicHistoryReadFile(files fs.FS, name string) (publicHistoryReadFile, error) {
	file, err := files.Open(name)
	if err != nil {
		return nil, err
	}
	reader, ok := file.(publicHistoryReadFile)
	if !ok {
		return nil, errors.Join(ErrHistoryUnsupported, file.Close())
	}
	return reader, nil
}

// ReaderAt keeps shared-volume reads bounded without preparing or recovering source files.
func ReadPublicHistoryPageFS(ctx context.Context, files fs.FS, request PublicHistoryPageRequest) (_ PublicHistoryPage, resultErr error) {
	if err := ctx.Err(); err != nil {
		return PublicHistoryPage{}, err
	}
	if request.Cursor != "" {
		if err := ValidatePublicHistoryPageCursor(request.Cursor); err != nil {
			return PublicHistoryPage{}, err
		}
	}
	if strings.TrimSpace(request.Key) == "" || strings.TrimSpace(request.Key) != request.Key {
		return PublicHistoryPage{}, ErrHistoryCursor
	}
	name := sanitizeKey(request.Key)
	metadata, err := fs.ReadFile(files, name+".meta.json")
	if err != nil {
		return PublicHistoryPage{}, err
	}
	var meta SessionMeta
	if json.Unmarshal(metadata, &meta) != nil || meta.Key != request.Key ||
		meta.PublicHistoryVersion != PublicHistoryVersion || meta.PublicHistoryRevision == 0 ||
		meta.PublicHistoryPreparation != nil || !meta.PublicHistoryComplete {
		return PublicHistoryPage{}, ErrPublicHistoryCoverage
	}
	if err := checkPublicReadFiles(ctx, files, name, meta); err != nil {
		return PublicHistoryPage{}, err
	}
	index, err := openPublicIndex(files, name+".jsonl.public.index", meta)
	if errors.Is(err, fs.ErrNotExist) {
		return PublicHistoryPage{}, ErrPublicHistoryIndexPreparing
	}
	if err != nil {
		return PublicHistoryPage{}, err
	}
	defer closePublicFile(index.file, &resultErr)
	source, err := openPublicHistoryReadFile(files, name+".jsonl.public")
	if err != nil {
		return PublicHistoryPage{}, err
	}
	defer closePublicFile(source, &resultErr)
	info, err := source.Stat()
	if err != nil || info.Size() != meta.PublicHistoryBytes {
		return PublicHistoryPage{}, errors.Join(ErrPublicHistoryCoverage, err)
	}
	cursor, err := readPublicHistoryCursor(request, meta, index)
	if err != nil {
		return PublicHistoryPage{}, err
	}
	page, err := readPublicPage(ctx, request, cursor, source, index)
	if err != nil {
		return PublicHistoryPage{}, err
	}
	current, err := fs.ReadFile(files, name+".meta.json")
	if err != nil || !bytes.Equal(metadata, current) {
		return PublicHistoryPage{}, errors.Join(ErrPublicHistoryCoverage, err)
	}
	if err := checkPublicReadFiles(ctx, files, name, meta); err != nil {
		return PublicHistoryPage{}, err
	}
	return page, nil
}
