package memory

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"

	"github.com/google/uuid"
)

const (
	publicIndexHeaderBytes = 128
	publicIndexRecordBytes = 104
	publicIndexMagic       = "PUBIDX01"
)

type publicIndexHeader struct {
	Generation string `json:"generation"`
	Revision   uint64 `json:"revision"`
	Bytes      int64  `json:"bytes"`
	Count      int64  `json:"count"`
	Digest     string `json:"digest"`
}

type publicIndexRecord struct {
	Position   int64
	Generation uuid.UUID
	Offset     int64
	Length     int64
	Hash       [sha256.Size]byte
	Chain      [sha256.Size]byte
}

func (s *JSONLStore) publicIndexPath(key string) string {
	return s.publicHistoryPath(key) + ".index"
}

func (s *JSONLStore) publicIndexFiles(key string) []string {
	return []string{s.publicIndexPath(key), s.publicIndexPreparationPath(key), s.publicIndexBuildingPath(key)}
}

func publicIndexMeta(meta SessionMeta) publicIndexHeader {
	return publicIndexHeader{
		meta.PublicHistoryGeneration, meta.PublicHistoryRevision,
		meta.PublicHistoryBytes, meta.PublicHistoryCount, meta.PublicHistoryDigest,
	}
}

func encodePublicIndexHeader(header publicIndexHeader) ([]byte, error) {
	generation, err := uuid.Parse(header.Generation)
	digest, digestErr := hex.DecodeString(header.Digest)
	if err != nil || digestErr != nil || len(digest) != sha256.Size ||
		header.Bytes < 0 || header.Bytes > maxHistoryPosition ||
		header.Count < 0 ||
		header.Count > maxHistoryPosition ||
		header.Revision > uint64(maxHistoryPosition) {
		return nil, ErrPublicHistoryCoverage
	}
	data := make([]byte, publicIndexHeaderBytes)
	copy(data, publicIndexMagic)
	copy(data[8:24], generation[:])
	binary.BigEndian.PutUint64(data[24:32], header.Revision)
	binary.BigEndian.PutUint64(data[32:40], uint64(header.Bytes))
	binary.BigEndian.PutUint64(data[40:48], uint64(header.Count))
	copy(data[48:80], digest)
	checksum := sha256.Sum256(data[:80])
	copy(data[80:112], checksum[:])
	return data, nil
}

func decodePublicIndexHeader(data []byte) (publicIndexHeader, error) {
	if len(data) != publicIndexHeaderBytes || string(data[:8]) != publicIndexMagic {
		return publicIndexHeader{}, ErrPublicHistoryCoverage
	}
	checksum := sha256.Sum256(data[:80])
	if !bytes.Equal(data[80:112], checksum[:]) || !bytes.Equal(data[112:], make([]byte, 16)) {
		return publicIndexHeader{}, ErrPublicHistoryCoverage
	}
	generation, err := uuid.FromBytes(data[8:24])
	if err != nil {
		return publicIndexHeader{}, ErrPublicHistoryCoverage
	}
	byteCount, rowCount := binary.BigEndian.Uint64(data[32:40]), binary.BigEndian.Uint64(data[40:48])
	if byteCount > uint64(maxHistoryPosition) || rowCount > uint64(maxHistoryPosition) {
		return publicIndexHeader{}, ErrPublicHistoryCoverage
	}
	header := publicIndexHeader{
		Generation: generation.String(), Revision: binary.BigEndian.Uint64(data[24:32]),
		Bytes: int64(byteCount), Count: int64(rowCount),
		Digest: hex.EncodeToString(data[48:80]),
	}
	if _, err := encodePublicIndexHeader(header); err != nil {
		return publicIndexHeader{}, err
	}
	return header, nil
}

func encodePublicIndexRecord(record publicIndexRecord) ([]byte, error) {
	if record.Position < 0 || record.Position > maxHistoryPosition || record.Offset < 0 || record.Length < 0 {
		return nil, ErrPublicHistoryCoverage
	}
	data := make([]byte, publicIndexRecordBytes)
	binary.BigEndian.PutUint64(data[:8], uint64(record.Offset))
	binary.BigEndian.PutUint64(data[8:16], uint64(record.Length))
	copy(data[16:48], record.Hash[:])
	copy(data[48:80], record.Chain[:])
	binary.BigEndian.PutUint64(data[80:88], uint64(record.Position))
	copy(data[88:104], record.Generation[:])
	return data, nil
}

func decodePublicIndexRecord(data []byte) (publicIndexRecord, error) {
	if len(data) != publicIndexRecordBytes {
		return publicIndexRecord{}, ErrPublicHistoryCoverage
	}
	offset, length := binary.BigEndian.Uint64(data[:8]), binary.BigEndian.Uint64(data[8:16])
	position := binary.BigEndian.Uint64(data[80:88])
	if offset > uint64(maxHistoryPosition) || length > uint64(maxHistoryPosition) ||
		position > uint64(maxHistoryPosition) {
		return publicIndexRecord{}, ErrPublicHistoryCoverage
	}
	record := publicIndexRecord{Position: int64(position), Offset: int64(offset), Length: int64(length)}
	copy(record.Generation[:], data[88:104])
	copy(record.Hash[:], data[16:48])
	copy(record.Chain[:], data[48:80])
	return record, nil
}

func publicIndexRecordHash(record publicIndexRecord, line []byte) [sha256.Size]byte {
	var scope [24]byte
	copy(scope[:16], record.Generation[:])
	binary.BigEndian.PutUint64(scope[16:], uint64(record.Position))
	digest := sha256.New()
	_, _ = digest.Write(scope[:])
	_, _ = digest.Write(line)
	return [sha256.Size]byte(digest.Sum(nil))
}

func publicIndexRecords(data []byte, frontier publicIndexHeader) ([]byte, string, error) {
	generation, generationErr := uuid.Parse(frontier.Generation)
	if generationErr != nil {
		return nil, "", ErrPublicHistoryCoverage
	}
	offset, position, previous := frontier.Bytes, frontier.Count, frontier.Digest
	var records []byte
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			return nil, "", ErrPublicHistoryCoverage
		}
		line := data[:end]
		previous = appendPublicDigest(previous, line)
		chain, err := hex.DecodeString(previous)
		if err != nil {
			return nil, "", err
		}
		record := publicIndexRecord{Position: position, Generation: generation, Offset: offset, Length: int64(end + 1)}
		record.Hash = publicIndexRecordHash(record, line)
		copy(record.Chain[:], chain)
		encoded, err := encodePublicIndexRecord(record)
		if err != nil {
			return nil, "", err
		}
		records = append(records, encoded...)
		offset += record.Length
		position++
		data = data[end+1:]
	}
	return records, previous, nil
}

type publicIndexReader struct {
	file    publicHistoryReadFile
	header  publicIndexHeader
	bytes   int64
	records map[int64]publicIndexRecord
}

func (s *JSONLStore) openPublicIndex(key string, meta SessionMeta) (*publicIndexReader, error) {
	return openPublicIndex(os.DirFS(s.dir), sanitizeKey(key)+".jsonl.public.index", meta)
}

func openPublicIndex(files fs.FS, name string, meta SessionMeta) (*publicIndexReader, error) {
	file, err := openPublicHistoryReadFile(files, name)
	if err != nil {
		return nil, err
	}
	reader := &publicIndexReader{file: file, records: make(map[int64]publicIndexRecord)}
	data := make([]byte, publicIndexHeaderBytes)
	_, err = reader.readAt(data, 0)
	if err == nil {
		reader.header, err = decodePublicIndexHeader(data)
	}
	info, statErr := file.Stat()
	if err == nil && (statErr != nil || reader.header != publicIndexMeta(meta) ||
		info.Size() != publicIndexHeaderBytes+reader.header.Count*publicIndexRecordBytes) {
		err = ErrPublicHistoryCoverage
	}
	if err == nil && reader.header.Count > 0 {
		last, readErr := reader.record(reader.header.Count - 1)
		if readErr != nil || last.Offset+last.Length != reader.header.Bytes ||
			hex.EncodeToString(last.Chain[:]) != reader.header.Digest {
			err = ErrPublicHistoryCoverage
		}
	}
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return reader, nil
}

func (r *publicIndexReader) readAt(data []byte, offset int64) (int, error) {
	n, err := r.file.ReadAt(data, offset)
	r.bytes += int64(n)
	return n, err
}

func (r *publicIndexReader) record(position int64) (publicIndexRecord, error) {
	if position < 0 || position >= r.header.Count {
		return publicIndexRecord{}, ErrPublicHistoryCoverage
	}
	if record, ok := r.records[position]; ok {
		return record, nil
	}
	data := make([]byte, publicIndexRecordBytes)
	if _, err := r.readAt(data, publicIndexHeaderBytes+position*publicIndexRecordBytes); err != nil {
		return publicIndexRecord{}, errors.Join(ErrPublicHistoryCoverage, err)
	}
	record, err := decodePublicIndexRecord(data)
	if err != nil || record.Position != position || record.Generation.String() != r.header.Generation ||
		record.Offset < 0 || record.Length <= 1 || record.Offset > r.header.Bytes-record.Length {
		return publicIndexRecord{}, ErrPublicHistoryCoverage
	}
	r.records[position] = record
	return record, nil
}

func writePublicIndexBytes(file *os.File, data []byte, offset int64) error {
	n, err := file.WriteAt(data, offset)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

func closePublicFile(file io.Closer, result *error) {
	if err := file.Close(); !errors.Is(err, os.ErrClosed) {
		*result = errors.Join(*result, err)
	}
}
