# EdgeOne trusted_proxy module for Caddy

This Caddy v2 module provides `http.ip_sources.edgeone`, an IP range source that periodically fetches Tencent Cloud EdgeOne egress (origin) IP CIDR lists and exposes them to Caddy's `trusted_proxies` feature.

By default, it fetches from the official endpoint `https://api.edgeone.ai/ips`. You can optionally filter by `version` (v4/v6) and `area` (global/mainland-china/overseas), or override with custom `urls`.

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

## Build with xcaddy

```
xcaddy build --with github.com/WeidiDeng/caddy-edgeone-ip
```

Then configure your Caddyfile as shown above.

## Notes and Recommendations

- Background refresh runs periodically. On fetch failure, it keeps the last successful snapshot and logs at error level.
- Ensure your configured URLs are the official EdgeOne origin IP list endpoints so that your origin firewall rules remain accurate.
- Logging levels: counts for initial/periodic loads are logged at debug level; operational problems are logged at error level. To see debug output during validation, enable `debug` in the global Caddyfile options.
