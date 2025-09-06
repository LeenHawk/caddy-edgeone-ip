# EdgeOne trusted_proxy module for Caddy

English | [简体中文](README.zh-CN.md)

This Caddy v2 module provides `http.ip_sources.edgeone`, an IP range source that periodically fetches Tencent Cloud EdgeOne egress (origin) IP CIDR lists and exposes them to Caddy's `trusted_proxies` feature.

By default, it fetches from the official endpoint `https://api.edgeone.ai/ips`. You can optionally filter by `version` (v4/v6) and `area` (global/mainland-china/overseas), or override with custom `urls`.

## Why This Module

- Zero‑config default: Talks to the official EdgeOne endpoint out of the box.
- Precise filtering: `version` (IPv4/IPv6) and `area` (global/mainland‑china/overseas).
- Always fresh: Background refresh with timeout + periodic schedule.
- Production hardening:
  - HTTP status checks, request timeouts
  - Exponential backoff + jitter with `Retry-After` support
  - Conditional requests (ETag / Last‑Modified → 304) to avoid unnecessary downloads
  - On‑disk persistence with sane defaults (Caddy app data dir) and TTL (7 days) for instant warm starts
- Safe by design: Concurrency‑safe reads; previous good snapshot retained on transient failures.
- Clean logs: Debug for counters, Error for failures.
- Verified: Automated tests cover text/JSON parsing, conditional 304s, retry/backoff, persistence + TTL; manual runs confirm real endpoint responses and module integration.

## Example Caddyfile

Put the following in global options under the corresponding server options:

Examples:

- Default (all IPv4+IPv6, global):
  ```
  trusted_proxies edgeone {
      interval 12h
      timeout 15s
  }
  ```

- Filter IPv6 and overseas only:
  ```
  trusted_proxies edgeone {
      version v6
      area overseas
  }
  ```

- Override with explicit URLs (rarely needed):
  ```
  trusted_proxies edgeone {
      urls https://api.edgeone.ai/ips?version=v4 https://api.edgeone.ai/ips?version=v6
  }
  ```

## Options

- `urls`: One or more HTTP(S) endpoints. Each must return a plain text body with one CIDR per line (EdgeOne official: `https://api.edgeone.ai/ips`). Blank lines or lines starting with `#` are ignored. If not set, uses the official endpoint.
- `version`: Optional. `v4` or `v6` to filter address family.
- `area`: Optional. `global`, `mainland-china`, or `overseas` to filter region.
- `interval`: How often the IP lists are retrieved (default `1h`).
- `timeout`: Maximum time to wait for each HTTP response.
- `max_retries`: Retries per refresh cycle on transient failures (default `3`).
- `backoff_initial`: Initial backoff wait (default `1s`).
- `backoff_max`: Max backoff wait cap (default `30s`).
- `jitter`: Random jitter fraction in range `0..1` (default `0.2`).
- `respect_retry_after`: Whether to honor `Retry-After` header for backoff (default `true`).
- `persist_path`: Enable on-disk persistence of the last successful snapshot and validators (ETag/Last-Modified). Defaults to a file under Caddy's app data dir (e.g., `${CADDY_APP_DATA_DIR}/edgeone-ip-cache.json`).
- `persist_ttl`: TTL for using the persisted snapshot at startup. If the snapshot is older than this TTL, ranges are not loaded from disk (validators may still be used). Default `168h` (7 days).

## Build with xcaddy

```
xcaddy build --with github.com/LeenHawk/caddy-edgeone-ip
```

Then configure your Caddyfile as shown above.

## Full Example (maximal)

This example shows most options together for reference. Trim it down to what you actually need.

```
{
    # Global options
    # debug                 # uncomment to see debug logs
    admin off
    servers :8080 {
        trusted_proxies edgeone {
            # Source filters
            version v6
            area overseas

            # Refresh cadence and request timeout
            interval 12h
            timeout 15s

            # Retry with exponential backoff + jitter
            max_retries 3
            backoff_initial 1s
            backoff_max 30s
            jitter 0.2
            respect_retry_after true

            # Persistence (defaults to Caddy's app data dir if not set)
            persist_path /var/lib/caddy/edgeone-ip-cache.json
            persist_ttl 168h   # 7 days
        }
    }
}

:8080 {
    # Your app/site routes
    respond /health 200 {
        body OK
    }
}
```

## Notes and Recommendations

- Background refresh runs periodically. On fetch failure, it keeps the last successful snapshot and logs at error level.
- Ensure your configured URLs are the official EdgeOne origin IP list endpoints so that your origin firewall rules remain accurate.
- Logging levels: counts for initial/periodic loads are logged at debug level; operational problems are logged at error level. To see debug output during validation, enable `debug` in the global Caddyfile options.
- Conditional requests: The module automatically uses `ETag` and `Last-Modified` (when provided) with `If-None-Match` / `If-Modified-Since` to avoid re-downloading unchanged lists (returns 304).
- Retries: Transient failures (e.g., 429/5xx) are retried within the same refresh cycle with exponential backoff and jitter; `Retry-After` is respected when present.
- Persistence: By default, the module persists the cache to Caddy's app data directory so restarts can immediately reuse the last good snapshot and conditional request validators. Override with `persist_path` if needed.
