package jupyter

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListKernels(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()

	ListKernels(rr, req)
	res := rr.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", res.StatusCode)
	}
}
