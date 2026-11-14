package loadtest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProviderPayouts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stats" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"payouts":[{"key":"a","status":"SUCCEEDED","requests":2},{"key":"b","status":"FAILED","failure_code":"INVALID_ACCOUNT","requests":1}]}`))
	}))
	defer srv.Close()

	got, err := providerPayouts(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["a"] != "SUCCEEDED" || got["b"] != "FAILED" {
		t.Errorf("payouts = %v, want a SUCCEEDED, b FAILED", got)
	}

	if _, err := providerPayouts(context.Background(), srv.Client(), srv.URL+"/missing"); err == nil {
		t.Error("a 404 from the provider gave no error")
	}
}
