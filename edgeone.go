package caddy_edgeone_ip

import (
    "bufio"
    "context"
    "errors"
    "fmt"
    "io"
    "io/ioutil"
    "encoding/json"
    "net/http"
    "net/netip"
    neturl "net/url"
    "math/rand"
    "os"
    "path/filepath"
    "strconv"
    "strings"
    "sync"
    "time"

    "github.com/caddyserver/caddy/v2"
    "github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
    "github.com/caddyserver/caddy/v2/modules/caddyhttp"
    "go.uber.org/zap"
)

const defaultEdgeOneBase = "https://api.edgeone.ai/ips"

func init() {
    caddy.RegisterModule(EdgeOneIPRange{})
}

// EdgeOneIPRange provides a range of IP address prefixes (CIDRs) retrieved from EdgeOne.
type EdgeOneIPRange struct {
    // Interval controls how often the IP lists are refreshed.
    Interval caddy.Duration `json:"interval,omitempty"`
    // Timeout controls the maximum duration for each HTTP fetch.
    Timeout caddy.Duration `json:"timeout,omitempty"`
    // URLs is a list of endpoints to fetch CIDR lists from. Each endpoint should
    // return one CIDR per line, or a text body containing CIDRs parseable by
    // caddyhttp.CIDRExpressionToPrefix. Empty or commented lines (#) are ignored.
    URLs []string `json:"urls,omitempty"`
    // Version filters the IP address type: "v4" or "v6" (optional).
    Version string `json:"version,omitempty"`
    // Area filters the region: "global", "mainland-china", or "overseas" (optional).
    Area string `json:"area,omitempty"`
    // Retry and backoff settings (optional; sensible defaults applied).
    MaxRetries        int            `json:"max_retries,omitempty"`
    BackoffInitial    caddy.Duration `json:"backoff_initial,omitempty"`
    BackoffMax        caddy.Duration `json:"backoff_max,omitempty"`
    Jitter            float64        `json:"jitter,omitempty"`
    RespectRetryAfter bool           `json:"respect_retry_after,omitempty"`

    // Persistence options: by default, the plugin persists cache to disk so that
    // ETag/Last-Modified and the last good snapshot survive restarts.
    // If empty, defaults to a file in Caddy's app data directory.
    PersistPath string `json:"persist_path,omitempty"`
    // Optional TTL for using persisted snapshot. If set, and the snapshot
    // is older than this duration at startup, ranges will not be loaded
    // from disk (validators may still be used). Default: disabled.
    PersistTTL caddy.Duration `json:"persist_ttl,omitempty"`

    ranges []netip.Prefix
    cache  map[string]sourceCache

    ctx  caddy.Context
    lock *sync.RWMutex
}

// CaddyModule returns the Caddy module information.
func (EdgeOneIPRange) CaddyModule() caddy.ModuleInfo {
    return caddy.ModuleInfo{
        ID:  "http.ip_sources.edgeone",
        New: func() caddy.Module { return new(EdgeOneIPRange) },
    }
}

// Provision implements caddy.Provisioner.
func (s *EdgeOneIPRange) Provision(ctx caddy.Context) error {
    s.ctx = ctx
    s.lock = new(sync.RWMutex)
    s.cache = make(map[string]sourceCache)

    // Defaults for retry/backoff when not provided
    if s.MaxRetries == 0 {
        s.MaxRetries = 3
    }
    if s.BackoffInitial == 0 {
        s.BackoffInitial = caddy.Duration(time.Second)
    }
    if s.BackoffMax == 0 {
        s.BackoffMax = caddy.Duration(30 * time.Second)
    }
    if s.Jitter == 0 {
        s.Jitter = 0.2
    }
    // default true if not explicitly set
    if !s.RespectRetryAfter {
        s.RespectRetryAfter = true
    }

    // Seed RNG for jitter
    rand.Seed(time.Now().UnixNano())

    // Default persist path if not provided
    if strings.TrimSpace(s.PersistPath) == "" {
        // Use a subdir under Caddy's app data dir
        base := caddy.AppDataDir()
        s.PersistPath = filepath.Join(base, "edgeone-ip-cache.json")
    }
    // Default TTL if not provided (use 7 days)
    if s.PersistTTL == 0 {
        s.PersistTTL = caddy.Duration(168 * time.Hour)
    }
    // Attempt to load persisted cache (best-effort)
    s.loadPersisted()

    // Start background refresh loop.
    go s.refreshLoop()
    return nil
}

// GetIPRanges implements caddyhttp.IPRangeSource.
func (s *EdgeOneIPRange) GetIPRanges(_ *http.Request) []netip.Prefix {
    s.lock.RLock()
    defer s.lock.RUnlock()
    return s.ranges
}

// UnmarshalCaddyfile implements caddyfile.Unmarshaler.
//
//  edgeone {
//     urls <url1> [<url2> ...]
//     interval <duration>
//     timeout <duration>
//  }
func (m *EdgeOneIPRange) UnmarshalCaddyfile(d *caddyfile.Dispenser) error {
    d.Next() // Skip module name.

    if d.NextArg() {
        return d.ArgErr()
    }

    for nesting := d.Nesting(); d.NextBlock(nesting); {
        switch d.Val() {
        case "interval":
            if !d.NextArg() {
                return d.ArgErr()
            }
            val, err := caddy.ParseDuration(d.Val())
            if err != nil {
                return err
            }
            m.Interval = caddy.Duration(val)
        case "timeout":
            if !d.NextArg() {
                return d.ArgErr()
            }
            val, err := caddy.ParseDuration(d.Val())
            if err != nil {
                return err
            }
            m.Timeout = caddy.Duration(val)
        case "urls":
            // Accept one or more URLs on the same line.
            for d.NextArg() {
                m.URLs = append(m.URLs, d.Val())
            }
        case "version":
            if !d.NextArg() {
                return d.ArgErr()
            }
            m.Version = d.Val()
        case "area":
            if !d.NextArg() {
                return d.ArgErr()
            }
            m.Area = d.Val()
        case "max_retries":
            if !d.NextArg() {
                return d.ArgErr()
            }
            v, err := strconv.Atoi(d.Val())
            if err != nil {
                return err
            }
            m.MaxRetries = v
        case "backoff_initial":
            if !d.NextArg() {
                return d.ArgErr()
            }
            val, err := caddy.ParseDuration(d.Val())
            if err != nil {
                return err
            }
            m.BackoffInitial = caddy.Duration(val)
        case "backoff_max":
            if !d.NextArg() {
                return d.ArgErr()
            }
            val, err := caddy.ParseDuration(d.Val())
            if err != nil {
                return err
            }
            m.BackoffMax = caddy.Duration(val)
        case "jitter":
            if !d.NextArg() {
                return d.ArgErr()
            }
            f, err := strconv.ParseFloat(d.Val(), 64)
            if err != nil {
                return err
            }
            m.Jitter = f
        case "respect_retry_after":
            if !d.NextArg() {
                return d.ArgErr()
            }
            val := strings.ToLower(d.Val())
            switch val {
            case "true", "on", "1", "yes":
                m.RespectRetryAfter = true
            case "false", "off", "0", "no":
                m.RespectRetryAfter = false
            default:
                return fmt.Errorf("invalid boolean value for respect_retry_after: %s", d.Val())
            }
        case "persist_path":
            if !d.NextArg() {
                return d.ArgErr()
            }
            m.PersistPath = d.Val()
        case "persist_ttl":
            if !d.NextArg() {
                return d.ArgErr()
            }
            val, err := caddy.ParseDuration(d.Val())
            if err != nil {
                return err
            }
            m.PersistTTL = caddy.Duration(val)
        default:
            return d.ArgErr()
        }
    }
    return nil
}

// getContext returns a cancelable context, with a timeout if configured.
func (s *EdgeOneIPRange) getContext() (context.Context, context.CancelFunc) {
    if s.Timeout > 0 {
        return context.WithTimeout(s.ctx, time.Duration(s.Timeout))
    }
    return context.WithCancel(s.ctx)
}

type sourceCache struct {
    ETag         string
    LastModified string
    Prefixes     []netip.Prefix
}

func (s *EdgeOneIPRange) fetch(url string, prev sourceCache) (sourceCache, bool, time.Duration, error) {
    ctx, cancel := s.getContext()
    defer cancel()

    req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
    if err != nil {
        return sourceCache{}, false, 0, err
    }
    if prev.ETag != "" {
        req.Header.Set("If-None-Match", prev.ETag)
    }
    if prev.LastModified != "" {
        req.Header.Set("If-Modified-Since", prev.LastModified)
    }

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return sourceCache{}, false, 0, err
    }
    defer resp.Body.Close()

    if resp.StatusCode == http.StatusNotModified {
        io.Copy(io.Discard, resp.Body)
        return prev, true, 0, nil
    }
    if resp.StatusCode < 200 || resp.StatusCode >= 300 {
        var ra time.Duration
        if s.RespectRetryAfter {
            if v := resp.Header.Get("Retry-After"); v != "" {
                if d, perr := parseRetryAfter(v); perr == nil {
                    ra = d
                }
            }
        }
        // Drain body to allow reuse of TCP connection
        io.Copy(io.Discard, resp.Body)
        return sourceCache{}, false, ra, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
    }

    // Read the body so we can support both plain text and JSON formats.
    body, err := ioutil.ReadAll(resp.Body)
    if err != nil {
        return sourceCache{}, false, 0, err
    }
    prefixes, perr := parseCIDRs(body)
    if perr != nil {
        return sourceCache{}, false, 0, perr
    }
    nc := sourceCache{
        ETag:         resp.Header.Get("ETag"),
        LastModified: resp.Header.Get("Last-Modified"),
        Prefixes:     prefixes,
    }
    return nc, false, 0, nil
}

func (s *EdgeOneIPRange) getPrefixes() ([]netip.Prefix, bool, error) {
    urls := s.resolveURLs()
    // Snapshot previous state for conditional requests
    s.lock.RLock()
    prevCache := make(map[string]sourceCache, len(s.cache))
    for k, v := range s.cache {
        prevCache[k] = v
    }
    prevRanges := make([]netip.Prefix, len(s.ranges))
    copy(prevRanges, s.ranges)
    s.lock.RUnlock()

    // Prepare working set
    resultCache := make(map[string]sourceCache, len(prevCache))
    for k, v := range prevCache {
        resultCache[k] = v
    }
    pending := make(map[string]struct{}, len(urls))
    for _, u := range urls {
        pending[u] = struct{}{}
        if _, ok := resultCache[u]; !ok {
            resultCache[u] = sourceCache{}
        }
    }

    updatedAny := false
    var lastErr error
    var retryAfterMax time.Duration

    doAttempt := func(attempt int) bool {
        lastErr = nil
        retryAfterMax = 0
        for u := range pending {
            prev := resultCache[u]
            nc, notMod, ra, err := s.fetch(u, prev)
            if err != nil {
                lastErr = err
                if ra > retryAfterMax {
                    retryAfterMax = ra
                }
                continue
            }
            if !notMod {
                resultCache[u] = nc
                updatedAny = true
            }
            delete(pending, u)
        }
        return len(pending) == 0
    }

    maxRetries := s.MaxRetries
    if maxRetries < 0 {
        maxRetries = 0
    }
    for attempt := 0; attempt <= maxRetries; attempt++ {
        if done := doAttempt(attempt); done {
            break
        }
        if attempt == maxRetries {
            break
        }
        // backoff with jitter and optional Retry-After
        pow := time.Duration(1) << attempt
        wait := time.Duration(s.BackoffInitial) * pow
        if wait > time.Duration(s.BackoffMax) {
            wait = time.Duration(s.BackoffMax)
        }
        if s.RespectRetryAfter && retryAfterMax > 0 && retryAfterMax > wait {
            wait = retryAfterMax
        }
        if s.Jitter > 0 {
            frac := (rand.Float64()*2 - 1) * s.Jitter // [-jitter, +jitter]
            jw := time.Duration(float64(wait) * (1 + frac))
            if jw > 0 {
                wait = jw
            }
        }
        if s.ctx.Logger() != nil {
            s.ctx.Logger().Named("edgeone").Debug("retrying fetch with backoff", zap.Int("attempt", attempt+1), zap.Duration("wait", wait))
        }
        time.Sleep(wait)
    }

    if len(pending) != 0 {
        if lastErr == nil {
            lastErr = fmt.Errorf("failed to fetch some URLs after retries")
        }
        return nil, false, lastErr
    }

    var full []netip.Prefix
    for _, u := range urls {
        if c, ok := resultCache[u]; ok {
            full = append(full, c.Prefixes...)
        }
    }
    s.lock.Lock()
    s.cache = resultCache
    s.lock.Unlock()

    if !updatedAny && len(full) != len(prevRanges) {
        updatedAny = true
    }

    // Persist snapshot if updated or if no persisted file exists
    if err := s.savePersisted(urls, resultCache, full); err != nil {
        if s.ctx.Logger() != nil {
            s.ctx.Logger().Named("edgeone").Debug("persist save failed", zap.Error(err))
        }
    }
    return full, updatedAny, nil
}

// resolveURLs returns the effective list of URLs from config.
func (s *EdgeOneIPRange) resolveURLs() []string {
    urls := s.URLs
    if len(urls) == 0 {
        u, _ := neturl.Parse(defaultEdgeOneBase)
        q := u.Query()
        if s.Version != "" {
            q.Set("version", s.Version)
        }
        if s.Area != "" {
            q.Set("area", s.Area)
        }
        u.RawQuery = q.Encode()
        urls = []string{u.String()}
    }
    return urls
}

// persist structures
type persistSource struct {
    ETag         string `json:"etag,omitempty"`
    LastModified string `json:"last_modified,omitempty"`
}

type persistFile struct {
    Format    int                          `json:"format"`
    Version   string                       `json:"version,omitempty"`
    Area      string                       `json:"area,omitempty"`
    URLs      []string                     `json:"urls,omitempty"`
    Cache     map[string]persistSource     `json:"cache"`
    Ranges    []string                     `json:"ranges,omitempty"`
    UpdatedAt time.Time                    `json:"updated_at"`
}

// loadPersisted attempts to read persisted cache and ranges into memory.
func (s *EdgeOneIPRange) loadPersisted() {
    path := strings.TrimSpace(s.PersistPath)
    if path == "" {
        return
    }
    f, err := os.Open(path)
    if err != nil {
        return
    }
    defer f.Close()
    dec := json.NewDecoder(f)
    var pf persistFile
    if err := dec.Decode(&pf); err != nil {
        return
    }
    // Validate that persisted URLs match current effective URLs to avoid cross-config reuse
    cur := s.resolveURLs()
    if len(cur) != len(pf.URLs) {
        return
    }
    for i := range cur {
        if cur[i] != pf.URLs[i] {
            return
        }
    }
    // Build caches and ranges
    cache := make(map[string]sourceCache, len(pf.Cache))
    for u, pc := range pf.Cache {
        cache[u] = sourceCache{ETag: pc.ETag, LastModified: pc.LastModified}
    }
    var ranges []netip.Prefix
    for _, sfx := range pf.Ranges {
        if pfx, err := caddyhttp.CIDRExpressionToPrefix(sfx); err == nil {
            ranges = append(ranges, pfx)
        }
    }
    // Commit under lock
    s.lock.Lock()
    s.cache = cache
    // Respect optional TTL for using persisted ranges at startup
    if len(ranges) > 0 {
        if s.PersistTTL > 0 {
            age := time.Since(pf.UpdatedAt)
            if age <= time.Duration(s.PersistTTL) {
                s.ranges = ranges
            }
        } else {
            s.ranges = ranges
        }
    }
    s.lock.Unlock()
    if s.ctx.Logger() != nil {
        fields := []zap.Field{zap.Int("ranges", len(ranges))}
        if s.PersistTTL > 0 {
            fields = append(fields, zap.Duration("persist_ttl", time.Duration(s.PersistTTL)))
        }
        s.ctx.Logger().Named("edgeone").Debug("loaded persisted cache", fields...)
    }
}

// savePersisted writes the current cache and ranges to disk atomically.
func (s *EdgeOneIPRange) savePersisted(urls []string, cache map[string]sourceCache, ranges []netip.Prefix) error {
    path := strings.TrimSpace(s.PersistPath)
    if path == "" {
        return nil
    }
    dir := filepath.Dir(path)
    if err := os.MkdirAll(dir, 0o755); err != nil {
        return err
    }
    pf := persistFile{
        Format:    1,
        Version:   s.Version,
        Area:      s.Area,
        URLs:      urls,
        Cache:     make(map[string]persistSource, len(cache)),
        Ranges:    make([]string, 0, len(ranges)),
        UpdatedAt: time.Now(),
    }
    for u, sc := range cache {
        pf.Cache[u] = persistSource{ETag: sc.ETag, LastModified: sc.LastModified}
    }
    for _, p := range ranges {
        pf.Ranges = append(pf.Ranges, p.String())
    }
    // Marshal and write to temp file, then rename
    data, err := json.MarshalIndent(&pf, "", "  ")
    if err != nil {
        return err
    }
    tmp := path + ".tmp"
    if err := os.WriteFile(tmp, data, 0o644); err != nil {
        return err
    }
    return os.Rename(tmp, path)
}

// parseCIDRs attempts to parse either plain-text (one CIDR per line) or a JSON
// structure containing CIDR strings anywhere in the tree.
func parseCIDRs(body []byte) ([]netip.Prefix, error) {
    // First, try plain text line-by-line.
    var prefixes []netip.Prefix
    scanner := bufio.NewScanner(strings.NewReader(string(body)))
    for scanner.Scan() {
        line := strings.TrimSpace(scanner.Text())
        if line == "" || strings.HasPrefix(line, "#") {
            continue
        }
        if pfx, err := caddyhttp.CIDRExpressionToPrefix(line); err == nil {
            prefixes = append(prefixes, pfx)
        }
    }
    if len(prefixes) > 0 {
        return prefixes, nil
    }

    // If no prefixes found, try JSON walking for any string values that parse as CIDR.
    var v interface{}
    if err := json.Unmarshal(body, &v); err != nil {
        // Not JSON; return error indicating no CIDRs parsed
        return nil, fmt.Errorf("no CIDRs found in response")
    }
    collect := func(s string) {
        if pfx, err := caddyhttp.CIDRExpressionToPrefix(s); err == nil {
            prefixes = append(prefixes, pfx)
        }
    }
    var walk func(any)
    walk = func(n any) {
        switch t := n.(type) {
        case string:
            collect(t)
        case []any:
            for _, it := range t {
                walk(it)
            }
        case map[string]any:
            for _, it := range t {
                walk(it)
            }
        }
    }
    walk(v)
    if len(prefixes) == 0 {
        return nil, fmt.Errorf("no CIDRs found in response")
    }
    return prefixes, nil
}

func (s *EdgeOneIPRange) refreshLoop() {
    if s.Interval == 0 {
        s.Interval = caddy.Duration(time.Hour)
    }

    ticker := time.NewTicker(time.Duration(s.Interval))
    defer ticker.Stop()

    // First-time update
    if full, updated, err := s.getPrefixes(); err != nil {
        if s.ctx.Logger() != nil {
            s.ctx.Logger().Named("edgeone").Error("initial refresh failed", zap.Error(err))
        }
    } else {
        s.lock.Lock()
        s.ranges = full
        s.lock.Unlock()
        if s.ctx.Logger() != nil {
            if updated {
                s.ctx.Logger().Named("edgeone").Debug("initial ranges loaded", zap.Int("count", len(full)))
            } else {
                s.ctx.Logger().Named("edgeone").Debug("initial ranges unchanged", zap.Int("count", len(full)))
            }
        }
    }

    for {
        select {
        case <-ticker.C:
            full, updated, err := s.getPrefixes()
            if err != nil {
                // Log but keep the previous ranges.
                if s.ctx.Logger() != nil {
                    s.ctx.Logger().Named("edgeone").Error("refresh failed", zap.Error(err))
                }
                continue
            }
            s.lock.Lock()
            s.ranges = full
            s.lock.Unlock()
            if s.ctx.Logger() != nil {
                if updated {
                    s.ctx.Logger().Named("edgeone").Debug("ranges updated", zap.Int("count", len(full)))
                } else {
                    s.ctx.Logger().Named("edgeone").Debug("ranges unchanged", zap.Int("count", len(full)))
                }
            }
        case <-s.ctx.Done():
            return
        }
    }
}

// Interface guards
var (
    _ caddy.Module            = (*EdgeOneIPRange)(nil)
    _ caddy.Provisioner       = (*EdgeOneIPRange)(nil)
    _ caddyfile.Unmarshaler   = (*EdgeOneIPRange)(nil)
    _ caddyhttp.IPRangeSource = (*EdgeOneIPRange)(nil)
    _ caddy.Validator         = (*EdgeOneIPRange)(nil)
)

// Validate implements caddy.Validator (optional), ensuring configuration sanity.
func (s *EdgeOneIPRange) Validate() error {
    // Allow empty URLs: we default to official base endpoint.
    // Validate optional filters if set.
    if s.Version != "" && s.Version != "v4" && s.Version != "v6" {
        return errors.New("edgeone: version must be 'v4' or 'v6'")
    }
    switch s.Area {
    case "", "global", "mainland-china", "overseas":
        // ok
    default:
        return errors.New("edgeone: area must be one of 'global', 'mainland-china', 'overseas'")
    }
    if s.MaxRetries < 0 {
        return errors.New("edgeone: max_retries cannot be negative")
    }
    if s.BackoffMax > 0 && s.BackoffInitial > 0 && time.Duration(s.BackoffMax) < time.Duration(s.BackoffInitial) {
        return errors.New("edgeone: backoff_max must be >= backoff_initial")
    }
    if s.Jitter < 0 || s.Jitter > 1 {
        return errors.New("edgeone: jitter must be between 0 and 1")
    }
    if s.PersistTTL < 0 {
        return errors.New("edgeone: persist_ttl cannot be negative")
    }
    return nil
}

// parseRetryAfter supports both seconds and HTTP-date per RFC 7231.
func parseRetryAfter(v string) (time.Duration, error) {
    if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
        if secs < 0 {
            secs = 0
        }
        return time.Duration(secs) * time.Second, nil
    }
    if t, err := http.ParseTime(v); err == nil {
        d := time.Until(t)
        if d < 0 {
            d = 0
        }
        return d, nil
    }
    return 0, fmt.Errorf("invalid Retry-After value: %s", v)
}
