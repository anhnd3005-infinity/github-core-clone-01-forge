# github-core-clone-01-forge

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/anhnd3005-infinity/github-core-clone-01-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/anhnd3005-infinity/github-core-clone-01-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

Pure stdlib static Go raw-TCP GitHub-compatible git forge (zero `net/http`, zero external dependencies) launched via Node 22 (`main.ts`). Serves Git Smart HTTP (`upload-pack` protocol v2 + v0 fallback, `receive-pack` v0), REST v3 (`/api/v3`), GraphQL v4 (`/api/graphql`), and Streamable HTTP MCP (`2026-07-28` + `2025-11-25`) over a unified lock-minimal deterministic store.

## Run it

```sh
node --experimental-strip-types main.ts
```

---

<sub>Entered on [make.fun](https://make.fun/?ref=87xkrk03&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
