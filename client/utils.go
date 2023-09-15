package client

import (
	"io"

	"github.com/nustiueudinastea/doltswarm/proto"
)

const (
	grpcRetries            = 5
	MaxFetchSize           = 128 * 1024 * 1024
	HedgeDownloadSizeLimit = 4 * 1024 * 1024
)

func batchItr(elemCount, batchSize int, cb func(start, end int) (stop bool)) {
	for st, end := 0, batchSize; st < elemCount; st, end = end, end+batchSize {
		if end > elemCount {
			end = elemCount
		}

		stop := cb(st, end)

		if stop {
			break
		}
	}
}

func copyFileChunksFromResponse(w *io.PipeWriter, res proto.Downloader_DownloadFileClient) {
	message := new(proto.DownloadFileResponse)
	var err error
	for {
		err = res.RecvMsg(message)
		if err == io.EOF {
			_ = w.Close()
			break
		}
		if err != nil {
			_ = w.CloseWithError(err)
			break
		}
		if len(message.GetChunk()) > 0 {
			_, err = w.Write(message.Chunk)
			if err != nil {
				_ = res.CloseSend()
				break
			}
		}
		message.Chunk = message.Chunk[:0]
	}
}
