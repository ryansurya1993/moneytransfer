package loadtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func newRequest(key string) *request {
	return &request{key: key, body: transferBody{Amount: 1, Currency: "IDR"}}
}

func TestSendRecordsAnswers(t *testing.T) {
	var (
		mu   sync.Mutex
		seen = map[string]int{}
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body transferBody
		if r.URL.Path != "/transfer" || r.Header.Get("Content-Type") != "application/json" ||
			json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		key := r.Header.Get("Idempotency-Key")
		mu.Lock()
		seen[key]++
		n := seen[key]
		mu.Unlock()
		if n > 1 {
			w.Header().Set("Idempotent-Replayed", "true")
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	first, second := newRequest("k1"), newRequest("k2")
	shots := []shot{{req: first}, {req: second, twin: true}, {req: first, retry: true}}
	tr, err := send(t.Context(), srv.Client(), srv.URL, shots, 100, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(tr.results) != 4 || tr.skipped != 0 {
		t.Fatalf("got %d results and %d skipped, want 4 and 0", len(tr.results), tr.skipped)
	}
	replayed := map[string]int{}
	for _, r := range tr.results {
		if r.status != http.StatusAccepted {
			t.Errorf("key %s: status %d, want 202", r.key, r.status)
		}
		if r.replayed {
			replayed[r.key]++
		}
	}
	if replayed["k1"] != 1 || replayed["k2"] != 1 {
		t.Errorf("replays per key = %v, want one for k1 (retry) and one for k2 (twin)", replayed)
	}
}

func TestSendSkipsWhenAllSlotsAreBusy(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	// Real time on purpose: send paces with its own ticker. The four ticks take 40ms against a 200ms
	// hold, a wide margin (no failure in 7,000 stress runs); a fake clock would be test-only code in send.
	go func() {
		time.Sleep(200 * time.Millisecond)
		close(release)
	}()

	shots := make([]shot, 5)
	for i := range shots {
		shots[i] = shot{req: newRequest("k")}
	}
	// 5 shots 10ms apart, one slot held for 200ms: only the first is sent.
	tr, err := send(t.Context(), srv.Client(), srv.URL, shots, 100, 1)
	if err != nil {
		t.Fatal(err)
	}

	if len(tr.results) != 1 || tr.skipped != 4 {
		t.Fatalf("got %d results and %d skipped, want 1 and 4", len(tr.results), tr.skipped)
	}
	if tr.results[0].latency < 150*time.Millisecond {
		t.Errorf("latency %v, want the time the server held the request", tr.results[0].latency)
	}
}

func TestSendWithoutServer(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	tr, err := send(t.Context(), http.DefaultClient, url, []shot{{req: newRequest("k")}}, 100, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.results) != 1 || tr.results[0].status != 0 {
		t.Errorf("got %+v, want one result without an answer", tr.results)
	}
}
