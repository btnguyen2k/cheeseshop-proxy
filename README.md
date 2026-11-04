# cheeseshop

[![Actions Status](https://github.com/btnguyen2k/cheeseshop-proxy/actions/workflows/ci.yaml/badge.svg)](https://github.com/btnguyen2k/cheeseshop-proxy/actions)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg?style=flat-square)](LICENSE.md)

**cheeseshop** is a standalone, lightweight, read-only proxy for public PyPI,
implemented in Go. Expose only approved Python projects through the Simple
Repository API, with an on-demand bounded disk cache. The repository and Docker
image are named `cheeseshop-proxy`; the application and executable are named
`cheeseshop`.

The initial implementation supports **allow rules only**. Block rules, uploads,
private package hosting, search, and full/offline mirroring are not implemented.

## Quick start

These examples use POSIX shell syntax. The package index is
`http://localhost:8080/simple/`.

### Build and run from source

Requires Go 1.26 or newer:

```sh
git clone https://github.com/btnguyen2k/cheeseshop-proxy.git
cd cheeseshop-proxy
export CHEESESHOP_LISTEN="127.0.0.1:8080"
export CHEESESHOP_ALLOW="requests,urllib3,idna,charset-normalizer,certifi"
go build -o ./cheeseshop ./cmd/cheeseshop
./cheeseshop -config ./config.yaml
```

### Run with Docker

Build the image locally until the first cheeseshop image has been published:

```sh
docker build -t cheeseshop:local .
docker run --rm -p 8080:8080 \
  --env "CHEESESHOP_ALLOW=requests,urllib3,idna,charset-normalizer,certifi" \
  --env "CHEESESHOP_PUBLIC_BASE_URL=http://localhost:8080" \
  --mount "type=volume,source=cheeseshop-cache,target=/data" \
  cheeseshop:local
```

The publishing workflow targets `btnguyen2k/cheeseshop-proxy`. The image includes
`config.yaml`; environment variables override it. The volume preserves cached
content and the local URL-signing key between container runs.

## Use with pip

For local development:

```sh
python -m pip install \
  --index-url http://localhost:8080/simple/ \
  --trusted-host localhost \
  requests
```

The trusted-host exception is for local HTTP only. Shared deployments should use
HTTPS without this exception:

```sh
python -m pip install --index-url https://packages.example.com/simple/ requests
export PIP_INDEX_URL="https://packages.example.com/simple/"
```

Alternatively, include the index in a requirements file:

```text
--index-url https://packages.example.com/simple/
requests
```

`python -m pip config --user set global.index-url https://packages.example.com/simple/`
sets a user-wide default. Project, environment, and command-line settings can
override it. `PIP_INDEX_URL` configures pip; `CHEESESHOP_*` configures the server.

Remove other configured indexes, including `extra-index-url`, if the proxy must
be the only package source. Adding the proxy as an extra index does not enforce
its allowlist. Direct URL/VCS requirements and already-cached or installed client
packages can bypass the proxy; network/client policy is needed to prevent that.

## Allow rules

```yaml
allow:
  - requests
  - company-*
```

- Exact names match one normalized project. Case is ignored, and any run of
  `.`, `_`, and `-` becomes a single `-`: `Friendly_Bard`, `friendly.bard`, and
  `friendly--bard` all identify `friendly-bard`.
- A terminal `*` matches a literal normalized prefix. `company-*` matches
  `company-core` and `company.core`, but not `company` or `companyextra`.
  `partial*` also matches `partial` itself.
- Standalone `"*"` explicitly allows every valid project. Quote it in YAML.
- Empty or omitted `allow` exposes no packages. Invalid names, regexes, and
  non-terminal wildcards are rejected.
- Allowing a project permits all its versions. Runtime dependencies and
  source-build dependencies require their own matching rules.

Excluded projects return **404, not 403**, including previously cached files and
previously issued download links. Rule changes require a restart. Prefixes
restrict names, not publisher identity. There is no `block` setting in this MVP.

## Protocol and caching

`/simple/` lists permitted projects; `/simple/<normalized-name>/` lists their
distributions. The server negotiates Simple HTML/JSON, preserves Requires-Python,
yanked reasons and hashes, and proxies wheels, source archives, and advertised
PEP 658/714 core metadata. Local file URLs remain project-bound across restarts;
downloads never redirect installers to the upstream CDN.

Metadata is fetched as Simple JSON from `pypi.org`; artifacts are restricted to
`files.pythonhosted.org`. The server advertises at most Simple API 1.1 and does
not advertise unsupported provenance/signature resources. Python installers
select versions/platforms and resolve dependencies; cheeseshop does not modify
archives or dependency declarations.

Project metadata is revalidated on its TTL, including yanked-state changes.
Immutable downloads remain cached until evicted. Concurrent misses share a
fetch. Expired metadata does not silently hide upstream failures.

The root catalog is fetched only when a nonempty allowlist's `/simple/` listing
is requested, then streamed from the disk cache and filtered. It is not a full
package mirror. Large indexes and wheels must fit the configured object/cache
limits; increase both limits together if necessary.

## Configuration

Use [`config.yaml`](config.yaml) for native runs. The image includes the same file
at `/config/config.yaml`, which can be replaced with a read-only bind mount.

| Setting | Default or included value | Environment variable |
| --- | --- | --- |
| `listen` | Included `0.0.0.0:8080`; built-in default `127.0.0.1:8080` | `CHEESESHOP_LISTEN` |
| `public_base_url` | Required; included `http://localhost:8080` | `CHEESESHOP_PUBLIC_BASE_URL` |
| `allow` | `[]`; no packages exposed | `CHEESESHOP_ALLOW` |
| `cache.dir` | `.cache`, relative to the working directory | `CHEESESHOP_CACHE_DIR` |
| `cache.max_bytes` | `1073741824` (1 GiB) | `CHEESESHOP_CACHE_MAX_BYTES` |
| `cache.max_object_bytes` | `134217728` (128 MiB) | `CHEESESHOP_CACHE_MAX_OBJECT_BYTES` |
| `cache.metadata_ttl` | `5m` | `CHEESESHOP_CACHE_METADATA_TTL` |
| `upstream.timeout` | `2m`, including concurrency waits and downloads | `CHEESESHOP_UPSTREAM_TIMEOUT` |
| `upstream.max_concurrent` | `8`, including active response bodies | `CHEESESHOP_UPSTREAM_MAX_CONCURRENT` |

`public_base_url` may include an ingress path prefix, for example
`https://packages.example.com/python`, but must not include `/simple/`.
Generated index and file URLs preserve that prefix.

Precedence is **built-in defaults < YAML < environment**. The YAML file must exist
and parse successfully. Unset variables preserve YAML/default values; explicitly
empty values override them and are validated.

`CHEESESHOP_ALLOW="requests,company-*"` replaces the YAML list. Surrounding spaces
are trimmed; an empty value clears it. Empty entries and JSON/YAML array syntax
in this variable are invalid. Unset the variable to restore the YAML setting.

Byte counts are positive base-10 integers without unit suffixes.
`cache.max_bytes` must exceed `cache.max_object_bytes` by at least 64 KiB.
Durations use positive Go values such as `30s`, `5m`, or `2h`.
Invalid configuration fails startup.

## Deployment and operations

- For shared access, provide HTTPS and access control at a reverse proxy.
- Persist `/data` in Docker. Its cache must be writable by UID 65532; the default
  cache location is `/data/.cache`. Use one process per cache directory.
- `/healthz` reports liveness without upstream access. `/readyz` checks cache
  writability and performs a lightweight upstream HEAD request.
- JSON logs go to stderr; termination triggers graceful shutdown.

## Development

Run `go build ./...` and `go test ./...`. Real pip integration uses generated
local wheels/source distributions, with no public-PyPI fallback:

```sh
CHEESESHOP_PIP_TEST=1 go test ./internal/proxy -run TestPipInstall -count=1
```

Python with pip is required for this opt-in suite, and CI enables it explicitly.
Python is not required in the server container.

## License

MIT licensed; see [LICENSE.md](LICENSE.md).

Report bugs and propose changes through this repository's
[issues](https://github.com/btnguyen2k/cheeseshop-proxy/issues) and
[pull requests](https://github.com/btnguyen2k/cheeseshop-proxy/pulls).
