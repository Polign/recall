package polign

import (
	"context"
	"errors"
	"net/http"

	"github.com/Polign/recall"
)

// Watermark returns the serving node's opaque visible-log revision. Older
// servers fall back to uncached Recall reads.
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
	if errors.As(err, &status) && (status.StatusCode == 404 || status.StatusCode == 405) {
		return "", recall.ErrWatermarkUnsupported
	}
	return out.Watermark, err
}
