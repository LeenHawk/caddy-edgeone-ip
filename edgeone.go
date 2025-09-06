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

    ranges []netip.Prefix

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

func (s *EdgeOneIPRange) fetch(url string) ([]netip.Prefix, error) {
    ctx, cancel := s.getContext()
    defer cancel()

    req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
    if err != nil {
        return nil, err
    }

    resp, err := http.DefaultClient.Do(req)
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    if resp.StatusCode < 200 || resp.StatusCode >= 300 {
        // Drain body to allow reuse of TCP connection
        io.Copy(io.Discard, resp.Body)
        return nil, fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
    }

    // Read the body so we can support both plain text and JSON formats.
    body, err := ioutil.ReadAll(resp.Body)
    if err != nil {
        return nil, err
    }
    return parseCIDRs(body)
}

func (s *EdgeOneIPRange) getPrefixes() ([]netip.Prefix, error) {
    urls := s.URLs
    if len(urls) == 0 {
        // Build a default URL from base + optional query params.
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
    var full []netip.Prefix
    for _, u := range urls {
        pfx, err := s.fetch(u)
        if err != nil {
            return nil, err
        }
        full = append(full, pfx...)
    }
    return full, nil
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
    if full, err := s.getPrefixes(); err != nil {
        if s.ctx.Logger() != nil {
            s.ctx.Logger().Named("edgeone").Error("initial refresh failed", zap.Error(err))
        }
    } else {
        s.lock.Lock()
        s.ranges = full
        s.lock.Unlock()
        if s.ctx.Logger() != nil {
            s.ctx.Logger().Named("edgeone").Debug("initial ranges loaded", zap.Int("count", len(full)))
        }
    }

    for {
        select {
        case <-ticker.C:
            full, err := s.getPrefixes()
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
                s.ctx.Logger().Named("edgeone").Debug("ranges updated", zap.Int("count", len(full)))
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
    return nil
}
