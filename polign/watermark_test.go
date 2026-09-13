package polign_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/Polign/recall"
	"github.com/Polign/recall/polign"
)

func TestWatermarkCompatibilityAndAuthenticationErrors(t *testing.T) {
	for _, status := range []int{200, 404, 405, 401, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			b := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/collections/memories/watermark" || r.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("incorrect watermark request")
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"watermark":"revision"}`))
			})
			wm, err := b.Watermark(t.Context(), "memories")
			switch status {
			case 200:
				if err != nil || wm != "revision" {
					t.Fatalf("%q %v", wm, err)
				}
			case 404, 405:
				if !errors.Is(err, recall.ErrWatermarkUnsupported) {
					t.Fatal(err)
				}
			default:
				var e *polign.StatusError
				if !errors.As(err, &e) || e.StatusCode != status {
					t.Fatalf("error hidden as fallback: %v", err)
				}
			}
		})
	}
}
