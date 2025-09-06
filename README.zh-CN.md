# Caddy 的 EdgeOne trusted_proxy 模块

[English](README.md) | 简体中文

这个 Caddy v2 模块提供 `http.ip_sources.edgeone`，它会定期从腾讯云 EdgeOne 官方接口拉取回源（egress/origin）IP 的 CIDR 列表，并将其提供给 Caddy 的 `trusted_proxies` 功能使用。

默认从官方接口 `https://api.edgeone.ai/ips` 拉取。你可以用 `version`（v4/v6）和 `area`（global/mainland-china/overseas）做筛选，或用自定义 `urls` 覆盖。

## 为什么选择本模块

- 开箱即用：默认直连 EdgeOne 官方接口，无需额外配置。
- 精准筛选：按 `version`（IPv4/IPv6）与 `area`（global/mainland-china/overseas）。
- 持续更新：后台定时刷新，支持请求超时控制。
- 生产可用的稳健性：
  - HTTP 状态检查与超时控制
  - 指数退避 + 抖动，支持 `Retry-After`
  - 条件请求（ETag / Last-Modified → 304），避免重复下载
  - 磁盘持久化（默认写入 Caddy 应用数据目录）+ TTL（默认 7 天），重启即热启动
- 安全可靠：并发读写安全；失败时保留上次成功快照。
- 日志友好：计数类为 debug，异常为 error，生产环境更干净。
- 验证充分：自动化用例覆盖文本/JSON 解析、条件请求 304、退避重试、持久化 + TTL；并有真实接口与 Caddy 集成实测。

## Caddyfile 示例

将如下配置放到全局 options 的对应 `servers` 下。

示例：

- 默认（IPv4+IPv6，全球）：
  ```
  trusted_proxies edgeone {
      interval 12h
      timeout 15s
  }
  ```

- 仅 IPv6 且海外：
  ```
  trusted_proxies edgeone {
      version v6
      area overseas
  }
  ```

- 指定 URL（一般不需要）：
  ```
  trusted_proxies edgeone {
      urls https://api.edgeone.ai/ips?version=v4 https://api.edgeone.ai/ips?version=v6
  }
  ```

## 配置项

- `urls`：一个或多个 HTTP(S) 地址。每个都应返回“每行一个 CIDR”的纯文本（EdgeOne 官方：`https://api.edgeone.ai/ips`）。忽略空行及以 `#` 开头的行；未设置则使用官方地址。
- `version`：可选。`v4` 或 `v6`，按地址族筛选。
- `area`：可选。`global`、`mainland-china`、`overseas`，按区域筛选。
- `interval`：刷新周期，默认 `1h`。
- `timeout`：每次 HTTP 请求的超时。
- `max_retries`：单次刷新周期内的重试次数（默认 `3`）。
- `backoff_initial`：初始退避时间（默认 `1s`）。
- `backoff_max`：最大退避封顶（默认 `30s`）。
- `jitter`：随机抖动系数 `0..1`（默认 `0.2`，即 ±20%）。
- `respect_retry_after`：是否遵循响应头 `Retry-After` 进行退避（默认 `true`）。
- `persist_path`：将最近一次成功快照与校验字段（ETag/Last-Modified）落盘。默认写入 Caddy 应用数据目录（如 `${CADDY_APP_DATA_DIR}/edgeone-ip-cache.json`）。
- `persist_ttl`：在启动时使用持久化快照的最长允许时间。如果快照早于该 TTL，则启动时不会从磁盘加载（但仍会使用校验字段进行条件请求）。默认 `168h`（7 天）。

## 使用 xcaddy 构建

```
xcaddy build --with github.com/WeidiDeng/caddy-edgeone-ip
```

然后按上面的示例配置你的 Caddyfile。

## 最大范例

以下示例把主要选项都示范了一遍，实际使用时请按需精简。

```
{
    # 全局参数
    # debug                 # 排障时可开启调试日志
    admin off
    servers :8080 {
        trusted_proxies edgeone {
            # 源与筛选
            version v6
            area overseas

            # 刷新与请求超时
            interval 12h
            timeout 15s

            # 指数退避重试 + 抖动
            max_retries 3
            backoff_initial 1s
            backoff_max 30s
            jitter 0.2
            respect_retry_after true

            # 持久化（若不设置，默认写入 Caddy 应用数据目录）
            persist_path /var/lib/caddy/edgeone-ip-cache.json
            persist_ttl 168h   # 7 天
        }
    }
}

:8080 {
    # 业务路由示例
    respond /health 200 {
        body OK
    }
}
```

## 说明与建议

- 后台定期刷新。拉取失败会保留上次成功快照，并输出 error 日志。
- 请确保使用 EdgeOne 官方回源 IP 列表地址，以保证源站白名单的准确性。
- 日志级别：启动/周期刷新成功的“数量”在 debug 级别；运行失败在 error 级别。若要查看调试信息，请在全局 options 开启 `debug`。
- 条件请求：自动使用 `ETag` 与 `Last-Modified`（若服务端提供），并通过 `If-None-Match` / `If-Modified-Since` 避免重复下载（返回 304）。
- 重试：对于 429/5xx 等暂时性失败，单次刷新周期内使用指数退避 + 抖动重试；若含 `Retry-After` 则优先使用。
- 持久化：默认将快照持久化到 Caddy 应用数据目录，重启后能立即复用快照并继续走条件请求。如需自定义路径可用 `persist_path` 覆盖。
