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

var ErrPublicHistorySessionNotFound = errors.New("public history session not found")

type publicHistoryReadFile interface {
	fs.File
	io.ReaderAt
}

func openPublicHistoryReadFile(files fs.FS, name string) (publicHistoryReadFile, error) {
	file, operationErr0 := files.Open(name)
	if operationErr0 != nil {
		return nil, operationErr0
	}
	reader, ok := file.(publicHistoryReadFile)
	if !ok {
		return nil, errors.Join(ErrHistoryUnsupported, file.Close())
	}
	return reader, nil
}

// ReadPublicHistoryPageFS uses ReaderAt without preparing or recovering source files.
func ReadPublicHistoryPageFS(
	ctx context.Context,
	files fs.FS,
	request PublicHistoryPageRequest,
) (_ PublicHistoryPage, resultErr error) {
	if operationErr1 := ctx.Err(); operationErr1 != nil {
		return PublicHistoryPage{}, operationErr1
	}
	if request.Cursor != "" {
		if operationErr2 := ValidatePublicHistoryPageCursor(request.Cursor); operationErr2 != nil {
			return PublicHistoryPage{}, operationErr2
		}
	}
	if strings.TrimSpace(request.Key) == "" || strings.TrimSpace(request.Key) != request.Key {
		return PublicHistoryPage{}, ErrHistoryCursor
	}
	name := sanitizeKey(request.Key)
	metadata, operationErr3 := fs.ReadFile(files, name+".meta.json")
	if errors.Is(operationErr3, fs.ErrNotExist) {
		return PublicHistoryPage{}, ErrPublicHistorySessionNotFound
	}
	if operationErr3 != nil {
		return PublicHistoryPage{}, operationErr3
	}
	var meta SessionMeta
	if json.Unmarshal(metadata, &meta) != nil || meta.Key != request.Key ||
		meta.PublicHistoryVersion != PublicHistoryVersion || meta.PublicHistoryRevision == 0 ||
		meta.PublicHistoryPreparation != nil || !meta.PublicHistoryComplete {
		return PublicHistoryPage{}, ErrPublicHistoryCoverage
	}
	if operationErr4 := checkPublicReadFiles(ctx, files, name, meta); operationErr4 != nil {
		return PublicHistoryPage{}, errors.Join(ErrPublicHistoryCoverage, operationErr4)
	}
	index, operationErr3 := openPublicIndex(files, name+".jsonl.public.index", meta)
	if errors.Is(operationErr3, fs.ErrNotExist) {
		return PublicHistoryPage{}, ErrPublicHistoryIndexPreparing
	}
	if operationErr3 != nil {
		return PublicHistoryPage{}, operationErr3
	}
	defer closePublicFile(index.file, &resultErr)
	source, operationErr3 := openPublicHistoryReadFile(files, name+".jsonl.public")
	if operationErr3 != nil {
		return PublicHistoryPage{}, errors.Join(ErrPublicHistoryCoverage, operationErr3)
	}
	defer closePublicFile(source, &resultErr)
	info, operationErr3 := source.Stat()
	if operationErr3 != nil || info.Size() != meta.PublicHistoryBytes {
		return PublicHistoryPage{}, errors.Join(ErrPublicHistoryCoverage, operationErr3)
	}
	cursor, operationErr3 := readPublicHistoryCursor(request, meta, index)
	if operationErr3 != nil {
		return PublicHistoryPage{}, operationErr3
	}
	page, operationErr3 := readPublicPage(ctx, request, cursor, source, index)
	if operationErr3 != nil {
		return PublicHistoryPage{}, operationErr3
	}
	current, operationErr3 := fs.ReadFile(files, name+".meta.json")
	if operationErr3 != nil || !bytes.Equal(metadata, current) {
		return PublicHistoryPage{}, errors.Join(ErrPublicHistoryCoverage, operationErr3)
	}
	if operationErr5 := checkPublicReadFiles(ctx, files, name, meta); operationErr5 != nil {
		return PublicHistoryPage{}, operationErr5
	}
	return page, nil
}
