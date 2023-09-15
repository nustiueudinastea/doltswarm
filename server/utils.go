package server

import (
	"strconv"
	"sync/atomic"

	remotesapi "github.com/dolthub/dolt/go/gen/proto/dolt/services/remotesapi/v1alpha1"
	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/sirupsen/logrus"
)

var requestId int32

func incReqId() int {
	return int(atomic.AddInt32(&requestId, 1))
}

func getReqLogger(lgr *logrus.Entry, method string) *logrus.Entry {
	lgr = lgr.WithFields(logrus.Fields{
		"method":      method,
		"request_num": strconv.Itoa(incReqId()),
	})
	lgr.Info("starting request")
	return lgr
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
