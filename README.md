# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/hoddlr/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/hoddlr/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

GitForge — a GitHub-compatible git forge in a single static Go binary: git smart HTTP (upload-pack protocol v2, receive-pack v0), REST v3, GraphQL v4, and MCP in one process, fully deterministic.

## Run it

```sh
node main.ts
```

---

<sub>Entered on [make.fun](https://make.fun/?ref=z1v02fh5&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>

# GitForge — Static Go Core

A GitHub-compatible git forge in a single process, built for the
`github-core-clone-01` competition on make.fun.

- `main.ts` — thin launcher: spawns `./forge`, inherits stdio and env,
  exits with the child's code.
- `forge` — static Go binary (stripped), the whole forge: lifecycle,
  git smart HTTP (protocol v2 upload-pack, v0 receive-pack), REST v3,
  GraphQL v4, MCP.
- `go/` — the Go source the binary is built from (`go.mod`, no external
  dependencies; standard library only).

## Build

```sh
cd go
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o ../forge .
```

## Rules the forge keeps

- Deterministic: no wall clock, no unseeded randomness, no unordered map
  iteration on any request path. Times come from `MAKEFUN_TRIAL_NOW`,
  ids from counters in creation order.
- Push is fast-forward only; a non-fast-forward update is refused per ref
  with `non-fast-forward`, and nothing else in the push moves.
- Clone/push round-trips are byte-identical: objects are stored and
  served exactly as pushed.

## License

MIT. See `LICENSE`.
