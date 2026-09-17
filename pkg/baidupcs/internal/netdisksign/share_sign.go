package netdisksign

import (
	"crypto/md5"
	"encoding/hex"
	"strconv"

	"github.com/cocomhub/sproxy/pkg/baidupcs/internal/pcsutil/cachepool"
	"github.com/cocomhub/sproxy/pkg/baidupcs/internal/pcsutil/converter"
)

func ShareSURLInfoSign(shareID int64) []byte {
	s := strconv.FormatInt(shareID, 10)
	m := md5.New()
	m.Write(converter.ToBytes(s))
	m.Write([]byte("_sharesurlinfo!@#"))
	res := m.Sum(nil)
	resHex := cachepool.RawMallocByteSlice(32)
	hex.Encode(resHex, res)
	return resHex
}
