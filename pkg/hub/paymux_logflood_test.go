package hub

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Ten thousand bad webhook posts from one source leave a handful of rows and a
// small logs file, not ten thousand rows.
func TestAFloodOfBadWebhookPostsDoesNotFloodTheLog(t *testing.T) {
	dir := t.TempDir()
	r := startApp(t, testConfig(t, "sqlite://"+dir+"/paymux.db", dir))
	waitReady(t, r)

	const total = 10_000
	var wg sync.WaitGroup
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 16}}
	jobs := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				req, _ := http.NewRequest("POST", "http://"+r.app.Addr()+"/hooks/pa_nobody", strings.NewReader(`{"event":"x"}`))
				req.Header.Set("Content-Type", "application/json")
				resp, err := client.Do(req)
				if err == nil {
					resp.Body.Close()
				}
			}
		}()
	}
	for range total {
		jobs <- struct{}{}
	}
	close(jobs)
	wg.Wait()

	httpRows := 0
	for _, row := range logRows(t, r) {
		if row.Kind == "http" {
			httpRows++
		}
	}
	if httpRows == 0 || httpRows > 40 {
		t.Errorf("%d http rows for %d bad posts from one source", httpRows, total)
	}
	_, rec := r.app.logStore()
	if got := rec.Stats().Suppressed; got < total-100 {
		t.Errorf("only %d of the posts were suppressed", got)
	}
	st, err := os.Stat(filepath.Join(dir, "logs.db"))
	if err != nil || st.Size() > 1<<20 {
		t.Errorf("logs.db: %v %v", st, err)
	}
	shutdown(t, r)
}
