package doltswarm

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	remotesapi "github.com/dolthub/dolt/go/gen/proto/dolt/services/remotesapi/v1alpha1"
	"github.com/nustiueudinastea/doltswarm/proto"
)

// RemoteTableFile is an implementation of a TableFile that lives in a DoltChunkStore
type RemoteTableFile struct {
	client *DBClient
	info   *remotesapi.TableFileInfo
}

// LocationPrefix
func (rtf RemoteTableFile) LocationPrefix() string {
	return ""
}

// LocationSuffix
func (rtf RemoteTableFile) LocationSuffix() string {
	return ""
}

// FileID gets the id of the file
func (rtf RemoteTableFile) FileID() string {
	return rtf.info.FileId
}

// NumChunks returns the number of chunks in a table file
func (rtf RemoteTableFile) NumChunks() int {
	return int(rtf.info.NumChunks)
}

// Open returns an io.ReadCloser which can be used to read the bytes of a table file.
func (rtf RemoteTableFile) Open(ctx context.Context) (io.ReadCloser, uint64, error) {
	if rtf.info.RefreshAfter != nil && rtf.info.RefreshAfter.AsTime().After(time.Now()) {
		resp, err := rtf.client.RefreshTableFileUrl(ctx, rtf.info.RefreshRequest)
		if err == nil {
			rtf.info.Url = resp.Url
			rtf.info.RefreshAfter = resp.RefreshAfter
		}
	}

	response, err := rtf.client.DownloadFile(
		ctx,
		&proto.DownloadFileRequest{Id: rtf.info.FileId},
	)
	if err != nil {
		return nil, 0, fmt.Errorf("client.LoadFile: %w", err)
	}

	md, err := response.Header()
	if err != nil {
		return nil, 0, fmt.Errorf("response.Header: %w", err)
	}

	var size uint64
	if sizes := md.Get("file-size"); len(sizes) > 0 {
		size, err = strconv.ParseUint(sizes[0], 10, 64)
		if err != nil {
			return nil, 0, fmt.Errorf("response.Header: file size header not valid: %w", err)
		}
	} else {
		size = 0
	}

	r, w := io.Pipe()
	go copyFileChunksFromResponse(w, response)

	return r, size, nil
}
