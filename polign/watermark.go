package polign

import (
	"context"
	"errors"
	"net/http"

	"github.com/Polign/recall"
)

// Watermark returns the serving node's opaque visible-log revision. Servers
// without the endpoint fall back to uncached Recall reads.
//
// 501 belongs with 404 and 405: it is the conventional answer from a proxy or
// an older gateway in front of a server that predates the endpoint. A 200 whose
// body carries no watermark is the same case, and reporting it as unsupported
// keeps a server that answers the route without implementing it from failing
// every read.
func (b *Backend) Watermark(ctx context.Context, collection string) (string, error) {
	path, err := collectionPath(collection)
	if err != nil {
		return "", err
	}
	var out struct {
		Watermark string `json:"watermark"`
	}
	err = b.do(ctx, http.MethodGet, path+"/watermark", nil, &out)
	var status *StatusError
	if errors.As(err, &status) && (status.StatusCode == 404 || status.StatusCode == 405 || status.StatusCode == 501) {
		return "", recall.ErrWatermarkUnsupported
	}
	if err != nil {
		return "", err
	}
	if out.Watermark == "" {
		return "", recall.ErrWatermarkUnsupported
	}
	return out.Watermark, nil
}
