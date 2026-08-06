package fstools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Document reading: a docx/pdf/xlsx becomes Markdown in place.
//
// Before this, read_file refused every binary and told the model to go find a
// skill or write a python script. That refusal exists for a good reason —
// dumping zip/pdf bytes into the context was observed in production as 20-60KB
// of garbage — but it turned "read this document" into a scavenger hunt, and
// the paths it pointed at lose the most useful structure: `pdftotext` flattens
// a table into a column of loose cells, and pdfplumber's table detection
// misses borderless tables outright.
//
// anydoc (github.com/firecrawl/anydoc, MIT, pure Rust, no models and no
// network) converts the fourteen office/PDF formats to GitHub-Flavored
// Markdown, reconstructing headings, lists and tables. When it is on PATH the
// document is served as Markdown and the model just reads a file. When it is
// not — or when it fails, which is what a scanned PDF does — nothing changes:
// the old refusal comes back, now carrying the reason.
//
// The bytes go in through STDIN, never the path. read_file's sandbox already
// authorized this file handle; handing a path to a subprocess would re-resolve
// it outside that authorization, which is exactly the guarantee the sandbox
// exists to provide.

const (
	anydocBinary = "anydoc"

	// A conversion is local and typically single-digit milliseconds; the
	// timeout is a hang guard, not a budget.
	anydocTimeout = 90 * time.Second

	// Input cap. anydoc has its own decompression/nesting limits; this one
	// keeps a pathological file from being streamed into a subprocess at all.
	maxDocumentInputBytes = 64 << 20

	// Output cap. The Markdown is paginated by read_file afterwards, so this
	// only bounds what is held in memory.
	maxDocumentOutputBytes = 16 << 20
)

// documentExts are the extensions worth routing through the converter. Kept as
// an explicit list rather than "anything binary" so that an archive, an image
// or an unknown blob still gets the old refusal with its own tailored hint.
var documentExts = map[string]bool{
	".doc": true, ".docx": true, ".docm": true, ".odt": true, ".rtf": true,
	".ppt": true, ".pptx": true, ".pptm": true, ".pps": true, ".ppsx": true,
	".ppsm": true, ".pot": true, ".odp": true,
	".xls": true, ".xlsx": true, ".xlsm": true, ".xlsb": true, ".ods": true,
	".pdf": true, ".epub": true,
}

// errNoConverter means anydoc is absent — not a failure to convert, just the
// absence of the capability, which must read as "unchanged behavior".
var errNoConverter = errors.New("document converter not available")

// lookupAnydoc is resolved once: PATH does not change under a running agent,
// and read_file is a hot tool. A var so tests can substitute it.
var lookupAnydoc = sync.OnceValues(func() (string, error) {
	return exec.LookPath(anydocBinary)
})

// isConvertibleDocument reports whether the extension is one anydoc handles.
func isConvertibleDocument(path string) bool {
	return documentExts[strings.ToLower(filepath.Ext(path))]
}

// convertDocumentToMarkdown streams the document through anydoc and returns
// the Markdown. The format is detected from the content, so no extension is
// passed — which also means a mislabeled file still converts correctly.
func convertDocumentToMarkdown(ctx context.Context, r io.Reader) ([]byte, error) {
	bin, err := lookupAnydoc()
	if err != nil {
		return nil, errNoConverter
	}

	ctx, cancel := context.WithTimeout(ctx, anydocTimeout)
	defer cancel()

	// "-" reads the document from stdin.
	cmd := exec.CommandContext(ctx, bin, "-")
	cmd.Stdin = io.LimitReader(r, maxDocumentInputBytes)

	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("conversion timed out after %s", anydocTimeout)
		}
		// anydoc writes a one-line diagnosis to stderr and it is genuinely
		// useful to the model — a scanned PDF says "OCR is required", which is
		// what tells it to reach for the pdf skill instead of retrying.
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, fmt.Errorf("conversion failed: %w", err)
	}

	if out.Len() > maxDocumentOutputBytes {
		out.Truncate(maxDocumentOutputBytes)
	}
	if strings.TrimSpace(out.String()) == "" {
		return nil, errors.New("converted to an empty document")
	}
	return out.Bytes(), nil
}

// documentReadResult paginates the converted Markdown under the same
// offset/length contract read_file uses for bytes, so a 200-page PDF is walked
// with the same calls as a large text file. The offsets are into the MARKDOWN,
// not the original document — the header says so, otherwise a model paging
// through would compute its next offset against the file size on disk.
func documentReadResult(path string, md []byte, offset, length int64) *ToolResult {
	total := int64(len(md))
	if offset >= total {
		return NewToolResult("[END OF DOCUMENT - no content at this offset]")
	}
	end := min(offset+length, total)
	data := md[offset:end]

	header := fmt.Sprintf(
		"[document: %s | converted to markdown | total: %d bytes | read: bytes %d-%d]",
		filepath.Base(path), total, offset, end-1,
	)
	if end < total {
		header += fmt.Sprintf(
			"\n[TRUNCATED - document has more content. Call read_file again with offset=%d to continue.]",
			end,
		)
	} else {
		header += "\n[END OF DOCUMENT - no further content.]"
	}
	return NewToolResult(header + "\n\n" + string(data))
}

// documentMarkdown converts when the file is a document anydoc can handle.
// The three-way return separates "not a document / no converter" (leave the
// old behavior alone) from "is a document but failed" (say why).
func documentMarkdown(ctx context.Context, path string, r io.Reader) (md []byte, applicable bool, err error) {
	if !isConvertibleDocument(path) {
		return nil, false, nil
	}
	md, err = convertDocumentToMarkdown(ctx, r)
	if errors.Is(err, errNoConverter) {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	return md, true, nil
}
