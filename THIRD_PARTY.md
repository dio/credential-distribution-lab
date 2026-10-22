# Third-party provenance

The original reference, model, tests and launcher in this repository are distributed
under Apache-2.0. Dependencies and external fixtures retain their upstream licenses.

## Real MCP fixture

`run.sh` downloads public Envoy AI Gateway source from
[envoyproxy/ai-gateway](https://github.com/envoyproxy/ai-gateway) at commit
`994579660ae2a9f2feef3cb77766e155746858b4`. The archive SHA-256 is
`f1677ea0f55c78ece09a443720501048229eeb741b35ef4cfa67e948ae0f7f5e`.
The upstream source is Apache-2.0 licensed. The build stages our thin launcher
`fixture/mcp-main.go.txt` inside that source so it can use upstream internal packages.
Upstream source and its license remain in the temporary build tree; they are not
vendored into this repository. We publish neither the downloaded source nor a
fixture binary as part of this research repository.

## Other dependencies

`go.mod` and `go.sum` identify the Go dependencies, including Envoy control-plane
protos, gRPC and go-sqlite3. Their module distributions carry their respective
licenses. SQLite itself is provided through go-sqlite3; consult both distributions
before redistributing a built artifact.

CI extracts Envoy from the official image pinned in `ci/versions.env`. Its image
and binary remain governed by upstream notices. GitHub Actions are pinned to
commits in their official repositories. No upstream project's endorsement is implied.
