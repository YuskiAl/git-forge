# git-forge

A standalone GitHub-compatible git forge in a single Node process: git smart
HTTP, REST v3, GraphQL v4 and MCP from its own in-memory store. No UI, no
outside services, no git binary — the standard library only.

## Run

```
node main.ts
```

The forge reads `MAKEFUN_ENV_URL` and `MAKEFUN_SHOP_TOKEN` from the
environment: it binds an ephemeral port, registers it with the trial
environment, and exits when the suite reports done. Without
`MAKEFUN_ENV_URL` it just serves on an ephemeral port (local development).

## Layout

- `main.ts` — entrypoint: bind, register, poll `/done`, exit.
- `src/app.ts` — HTTP routing, bearer auth, git smart-HTTP dispatch.
- `src/store.ts` — the in-memory store: repos, git objects, issues, PRs.
- `src/git.ts` — git objects, packfiles, pkt-lines, upload/receive-pack.
- `src/rest.ts` — REST v3: repos, branches, commits, contents, labels,
  issues, comments, pull requests, merges.
- `src/graphql.ts` — GraphQL v4: a parser plus an executor over the store.
- `src/mcp.ts` — MCP Streamable HTTP: `server/discover`, `initialize`,
  `tools/list`, `tools/call` (create/get repo, issues, pull requests).

## Determinism

Every timestamp in state comes from a deterministic sequence counter, and
request ids are sequential (`req-1`, `req-2`, ...). Two runs fed the same
requests in the same order produce the same state, byte for byte.

## License

MIT — see `LICENSE`.
