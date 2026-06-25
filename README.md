# GitDCY

Private Git-only multi-device workspace client.

GitDCY keeps source repos native on each device while avoiding manual `git fetch`
and `git pull` across many projects. It is intentionally strict:

- Git is the sync layer.
- Ignored files are ignored unless explicitly allowlisted for private WIP sync.
- Pulls are fast-forward only.
- Dirty work can be moved through private WIP refs on a `sync` remote.
- No auto-merge, auto-rebase, or force-push.

## Run

```bash
cargo run -p gitdcy-gui
```

Useful CLI checks:

```bash
cargo run -p gitdcy-cli -- doctor
cargo run -p gitdcy-cli -- status
cargo run -p gitdcy-cli -- sync --all
```

The default workspace root is `~/Code`. On first run, if no manifest exists, the
app scans configured roots and discovers Git repositories.

## Daily Use

1. Open the GUI.
2. Press **Refresh** to inspect all repos.
3. Press **Sync All** to fetch every remote, fast-forward clean branches, push
   dirty WIP snapshots, and apply incoming WIP when safe.
4. Use **Commit All Non-Ignored Changes** and **Push** when work is ready for the
   normal branch history.

Dirty sync uses private Git refs under `refs/gitdcy/wip/*`. GitHub and GitLab
repos need a private `sync` remote for this. Forgejo repos can use their Forgejo
`origin`.

## Configure Private WIP Sync

For GitHub/GitLab repos, create or choose a private Git mirror and set it as
the `sync` remote in the GUI.

Configure machine-specific paths and remote templates in a local ignored file:

```bash
cp .gitdcy.local.example.yaml .gitdcy.local.yaml
```

The CLI equivalent for setting one repo's private sync remote is:

```bash
cargo run -p gitdcy-cli -- set-sync-remote <repo> ssh://git@example.internal/owner/<repo>.git
```

Provider-folder repos can also get default `origin` URLs from local ignored
templates. For example, a repo under `~/Code/github/my-app` can suggest
`https://github.com/YOUR_GITHUB_USERNAME/my-app.git`.

```bash
cargo run -p gitdcy-cli -- set-origin-remote my-app
```

Ignored local files such as `.env` are not included by default. To move one
through private WIP sync, opt in per repo from the ignored local config:

```yaml
local_sync_files:
  github/my-app:
    - .env
```

This does not change normal commits. `Commit All Non-Ignored Changes` still
uses Git's normal tracked, untracked, and ignored-file rules.

## Build Binaries

```bash
cargo build --release --workspace
```

The GUI binary is `target/release/gitdcy-gui`; the CLI binary is
`target/release/gitdcy`.
