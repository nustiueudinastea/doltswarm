package doltswarm

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"path"
	"strings"

	"github.com/dolthub/dolt/go/store/nbs"
)

// RemoteTableFile is an implementation of a TableFile that lives in a DoltChunkStore
type RemoteTableFile struct {
	downloader  DownloaderClient
	chunkClient ChunkStoreClient
	info        TableFileInfo
}

// LocationPrefix
func (rtf RemoteTableFile) LocationPrefix() string {
	return ""
}

// LocationSuffix infers suffix from URL (e.g., .darc) to help downstream
// openChunkSources choose the correct table file format.
func (rtf RemoteTableFile) LocationSuffix() string {
	if u, err := url.Parse(rtf.info.URL); err == nil {
		if strings.HasSuffix(u.Path, nbs.ArchiveFileSuffix) {
			return nbs.ArchiveFileSuffix
		}
	}
	return ""
}

// FileID gets the id of the file
func (rtf RemoteTableFile) FileID() string {
	return rtf.info.FileID
}

// NumChunks returns the number of chunks in a table file
func (rtf RemoteTableFile) NumChunks() int {
	return int(rtf.info.NumChunks)
}

// SplitOffset returns the byte offset from the beginning of the storage file where we transition from data to index.
func (rtf RemoteTableFile) SplitOffset() uint64 {
	return rtf.info.SplitOffset
}

// Open returns an io.ReadCloser which can be used to read the bytes of a table file.
func (rtf RemoteTableFile) Open(ctx context.Context) (io.ReadCloser, uint64, error) {
	// Prefer the URL path as the download identifier so we include any suffix
	// such as ".darc". Fallback to FileId for older manifests.
	id := rtf.info.FileID
	if u, err := url.Parse(rtf.info.URL); err == nil {
		if base := path.Base(u.Path); base != "" && base != "." && base != "/" {
			id = base
		}
	}

	r, size, err := rtf.downloader.DownloadFile(ctx, id)
	if err != nil {
		return nil, 0, fmt.Errorf("download file: %w", err)
	}

	return r, size, nil
}
