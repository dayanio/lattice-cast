# LatticeCast

One-sentence casting for the [Lattice](https://github.com/dayanio/lattice) mesh:
say "把 NAS 里的那部电影投到卧室电视" and it plays.

- `cmd/lattice-cast` — cast-agent (Go): discovery / adapter / media resolver / MCP tools
- `android/` — renderer APK (Kotlin + ExoPlayer) for Android TV / boxes
- `docs/protocol.md` — the LatticeCast wire protocol (authoritative)

Status: v1 in development. Design spec: lattice repo `docs/superpowers/specs/2026-09-18-latticecast-design.md`.

## Security notes

- The MCP endpoint (`mcp_listen`, default `:7800`) requires Bearer Token auth.
- Renderer calls carry a per-renderer Bearer Token over the LAN.
- The media HTTP server (default `0.0.0.0:7810`) is intentionally **unauthenticated in v1** — any LAN device can fetch media by `media_id`; home trusted network only (shared Token planned for v2).
- All traffic stays on your LAN; nothing goes through any third-party server.
