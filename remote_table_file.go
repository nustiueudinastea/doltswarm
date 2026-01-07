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

// RemoteTableFile is a chunks.TableFile backed by a provider download API (not HTTP).
type RemoteTableFile struct {
	downloader  DownloaderClient
	chunkClient ChunkStoreClient
	info        TableFileInfo
}

func (rtf RemoteTableFile) LocationPrefix() string { return "" }

// LocationSuffix infers suffix from URL (e.g., .darc) to help downstream select the correct format.
func (rtf RemoteTableFile) LocationSuffix() string {
	if u, err := url.Parse(rtf.info.URL); err == nil {
		if strings.HasSuffix(u.Path, nbs.ArchiveFileSuffix) {
			return nbs.ArchiveFileSuffix
		}
	}
	return ""
}

func (rtf RemoteTableFile) FileID() string { return rtf.info.FileID }
func (rtf RemoteTableFile) NumChunks() int { return int(rtf.info.NumChunks) }
func (rtf RemoteTableFile) SplitOffset() uint64 {
	return rtf.info.SplitOffset
}

func (rtf RemoteTableFile) Open(ctx context.Context) (io.ReadCloser, uint64, error) {
	// Prefer the URL path base as download identifier (preserves suffix like ".darc").
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
