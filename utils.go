package doltswarm

import (
	"errors"
	"math/rand"
	"os"

	"io"

	"github.com/nustiueudinastea/doltswarm/proto"

	remotesapi "github.com/dolthub/dolt/go/gen/proto/dolt/services/remotesapi/v1alpha1"
	"github.com/dolthub/dolt/go/store/chunks"
)

const (
	grpcRetries            = 5
	MaxFetchSize           = 128 * 1024 * 1024
	HedgeDownloadSizeLimit = 4 * 1024 * 1024
)

func ensureDir(dirName string) error {
	err := os.Mkdir(dirName, os.ModePerm)
	if err == nil {
		return nil
	}
	if os.IsExist(err) {
		info, err := os.Stat(dirName)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("path exists but is not a directory")
		}
		return nil
	}
	return err
}

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

func buildTableFileInfo(tableList []chunks.TableFile) []*remotesapi.TableFileInfo {
	tableFileInfo := make([]*remotesapi.TableFileInfo, 0)
	for _, t := range tableList {
		tableFileInfo = append(tableFileInfo, &remotesapi.TableFileInfo{
			FileId:    t.FileID(),
			NumChunks: uint32(t.NumChunks()),
			Url:       t.FileID(),
		})
	}
	return tableFileInfo
}

var letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

func randSeq(n int) string {
	b := make([]rune, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}
