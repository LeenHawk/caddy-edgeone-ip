package caddy_edgeone_ip

import (
    "context"
    "encoding/json"
    "net/http"
    "net/http/httptest"
    "os"
    "path/filepath"
    "sync"
    "sync/atomic"
    "testing"
    "time"

    "github.com/caddyserver/caddy/v2"
)

func newCtx() caddy.Context {
    return caddy.Context{Context: context.Background()}
}

func TestParseCIDRs_TextAndJSON(t *testing.T) {
    txt := "\n# comment\n192.0.2.0/24\n\n2001:db8::/32\n"
    pfx, err := parseCIDRs([]byte(txt))
    if err != nil || len(pfx) != 2 {
        t.Fatalf("text parsing failed: err=%v len=%d", err, len(pfx))
    }

    js := map[string]any{"a": []any{"10.0.0.0/8", map[string]any{"b": "2001:db8::/32"}}}
    b, _ := json.Marshal(js)
    pfx, err = parseCIDRs(b)
    if err != nil || len(pfx) != 2 {
        t.Fatalf("json parsing failed: err=%v len=%d", err, len(pfx))
    }
}

func TestFetchConditional304(t *testing.T) {
    var seen int32
    etag := "W/\"tag-1\""
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        atomic.AddInt32(&seen, 1)
        if inm := r.Header.Get("If-None-Match"); inm == etag {
            w.WriteHeader(http.StatusNotModified)
            return
        }
        w.Header().Set("ETag", etag)
        w.WriteHeader(200)
        w.Write([]byte("203.0.113.0/24\n"))
    }))
    defer srv.Close()

    s := &EdgeOneIPRange{Timeout: caddy.Duration(5 * time.Second)}
    s.ctx = newCtx()

    // First fetch: 200
    nc, notMod, _, err := s.fetch(srv.URL, sourceCache{})
    if err != nil || notMod {
        t.Fatalf("first fetch failed: err=%v notMod=%v", err, notMod)
    }
    if nc.ETag == "" || len(nc.Prefixes) != 1 {
        t.Fatalf("unexpected cache after first fetch: %+v", nc)
    }

    // Second fetch with If-None-Match: 304
    nc2, notMod, _, err := s.fetch(srv.URL, nc)
    if err != nil || !notMod {
        t.Fatalf("second fetch expected 304 path: err=%v notMod=%v", err, notMod)
    }
    if nc2.ETag != nc.ETag || len(nc2.Prefixes) != 1 {
        t.Fatalf("cache should be unchanged on 304")
    }
    if atomic.LoadInt32(&seen) < 2 {
        t.Fatalf("server should have been hit twice")
    }
}

func TestGetPrefixesWithRetryBackoff(t *testing.T) {
    var hits int32
    // First attempt: 503 with Retry-After: 0; Second attempt: 200
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        n := atomic.AddInt32(&hits, 1)
        if n == 1 {
            w.Header().Set("Retry-After", "0")
            http.Error(w, "busy", http.StatusServiceUnavailable)
            return
        }
        w.WriteHeader(200)
        w.Write([]byte("198.51.100.0/24\n"))
    }))
    defer srv.Close()

    s := &EdgeOneIPRange{
        Timeout:        caddy.Duration(2 * time.Second),
        MaxRetries:     1,
        BackoffInitial: caddy.Duration(1 * time.Millisecond),
        BackoffMax:     caddy.Duration(1 * time.Millisecond),
        URLs:           []string{srv.URL},
    }
    s.ctx = newCtx()
    s.lock = new(sync.RWMutex)
    s.cache = make(map[string]sourceCache)

    full, updated, err := s.getPrefixes()
    if err != nil {
        t.Fatalf("getPrefixes failed: %v", err)
    }
    if !updated || len(full) != 1 {
        t.Fatalf("expected updated= true and 1 prefix, got updated=%v len=%d", updated, len(full))
    }
}

func TestPersistenceLoadAndTTL(t *testing.T) {
    dir := t.TempDir()
    cacheFile := filepath.Join(dir, "edgeone-cache.json")

    // Server always returns fixed list with validators
    etag := "\"persist-tag\""
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("ETag", etag)
        w.WriteHeader(200)
        w.Write([]byte("100.64.0.0/10\n"))
    }))
    defer srv.Close()

    // First run: fetch and persist
    s := &EdgeOneIPRange{
        Timeout:     caddy.Duration(2 * time.Second),
        URLs:        []string{srv.URL},
        PersistPath: cacheFile,
        PersistTTL:  caddy.Duration(24 * time.Hour),
    }
    s.ctx = newCtx()
    s.lock = new(sync.RWMutex)
    s.cache = make(map[string]sourceCache)
    full, updated, err := s.getPrefixes()
    if err != nil || !updated || len(full) != 1 {
        t.Fatalf("initial fetch/persist failed: err=%v updated=%v len=%d", err, updated, len(full))
    }

    // Second instance: load persisted (within TTL)
    s2 := &EdgeOneIPRange{
        URLs:        []string{srv.URL},
        PersistPath: cacheFile,
        PersistTTL:  caddy.Duration(24 * time.Hour),
    }
    s2.ctx = newCtx()
    s2.lock = new(sync.RWMutex)
    s2.cache = make(map[string]sourceCache)
    s2.loadPersisted()
    if got := len(s2.GetIPRanges(nil)); got != 1 {
        t.Fatalf("expected to load 1 prefix from persisted cache, got %d", got)
    }

    // Third instance: craft an expired file and ensure ranges are not loaded
    // Write an old persist file manually
    pf := persistFile{
        Format:    1,
        URLs:      []string{srv.URL},
        Cache:     map[string]persistSource{srv.URL: {ETag: etag}},
        Ranges:    []string{"100.64.0.0/10"},
        UpdatedAt: time.Now().Add(-48 * time.Hour),
    }
    data, _ := json.Marshal(&pf)
    if err := os.WriteFile(cacheFile, data, 0o644); err != nil {
        t.Fatalf("write expired persist file: %v", err)
    }
    s3 := &EdgeOneIPRange{
        URLs:        []string{srv.URL},
        PersistPath: cacheFile,
        PersistTTL:  caddy.Duration(24 * time.Hour),
    }
    s3.ctx = newCtx()
    s3.lock = new(sync.RWMutex)
    s3.cache = make(map[string]sourceCache)
    s3.loadPersisted()
    if got := len(s3.GetIPRanges(nil)); got != 0 {
        t.Fatalf("expected 0 prefixes due to expired TTL, got %d", got)
    }
}
