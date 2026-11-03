# cheeseshop

A lightweight, read-only public PyPI proxy written in Go. Expose approved
Python projects through the Simple HTML/JSON API with a bounded persistent cache.
The application is **cheeseshop**; the image repository is **cheeseshop-proxy**.

This MVP supports **allow rules only**. Uploads, private hosting, search, block
rules, and full/offline mirroring are not implemented.

## Run

After an image version has been published, replace `<tag>` with that version:

```sh
docker run --rm -p 8080:8080 \
  --env "CHEESESHOP_ALLOW=requests,urllib3,idna,charset-normalizer,certifi" \
  --env "CHEESESHOP_PUBLIC_BASE_URL=http://localhost:8080" \
  --mount "type=volume,source=cheeseshop-cache,target=/data" \
  btnguyen2k/cheeseshop-proxy:<tag>
```

Before the first release, build the image from the
[source repository](https://github.com/btnguyen2k/cheeseshop-proxy).
The index is available at `http://localhost:8080/simple/`.

For local HTTP development:

```sh
python -m pip install --index-url http://localhost:8080/simple/ \
  --trusted-host localhost requests
```

Use HTTPS without trusted-host exceptions for shared deployments. Remove other
indexes and `extra-index-url` settings if the proxy must enforce the approved
subset. Direct URLs, VCS requirements, and existing client caches can bypass it.

## Allow rules

Use `CHEESESHOP_ALLOW="requests,company-*"` or configure `allow` in YAML.
Names ignore case and normalize runs of `.`, `_`, and `-` to `-`.
Exact names match one project; terminal `*` matches a literal prefix;
standalone `"*"` allows all valid projects.

The default empty list exposes no packages. Exclusions return **404**, even for
cached artifacts and previously issued file links. Dependencies, including
source-build requirements, need their own rules. Prefixes are not publisher
identity checks. Restart the container after changing rules.

## Configuration and storage

The image includes `/config/config.yaml`. Mount a replacement read-only or
override its settings with environment variables. Precedence is built-in
defaults < YAML < environment. Explicitly empty variables override YAML values.

| Setting | Environment variable |
| --- | --- |
| Bind address | `CHEESESHOP_LISTEN` |
| External base URL, without `/simple/` | `CHEESESHOP_PUBLIC_BASE_URL` |
| Comma-separated allow rules | `CHEESESHOP_ALLOW` |
| Cache directory | `CHEESESHOP_CACHE_DIR` |
| Cache byte budget | `CHEESESHOP_CACHE_MAX_BYTES` |
| Maximum index/artifact bytes | `CHEESESHOP_CACHE_MAX_OBJECT_BYTES` |
| Metadata revalidation TTL | `CHEESESHOP_CACHE_METADATA_TTL` |
| Upstream deadline | `CHEESESHOP_UPSTREAM_TIMEOUT` |
| Upstream concurrency | `CHEESESHOP_UPSTREAM_MAX_CONCURRENT` |

The default cache is `/data/.cache`, with a 1 GiB budget, 128 MiB object limit,
and 5-minute metadata TTL. Increase both byte limits for larger wheels/indexes;
the budget must leave at least 64 KiB beyond the object limit. Byte counts are
base-10 integers; durations use Go syntax such as `30s` or `5m`.

Persist `/data`, including its URL-signing key. Bind-mounted storage must be
writable by UID 65532. Use one process per cache directory. This is an on-demand
cache, not an offline mirror.

`/healthz` provides liveness; `/readyz` checks cache writability and lightweight
upstream access. Logs are JSON on stderr. Provide HTTPS and authentication at
your reverse proxy; set the external base URL, including any path prefix.

Full usage, protocol details, and configuration examples are in the
[README](https://github.com/btnguyen2k/cheeseshop-proxy#readme).
MIT licensed; see
[LICENSE.md](https://github.com/btnguyen2k/cheeseshop-proxy/blob/main/LICENSE.md).
