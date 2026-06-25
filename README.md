# GitDCY

Private Git-only multi-device workspace client.

GitDCY keeps source repos native on each device while avoiding manual `git fetch`
and `git pull` across many projects. It is intentionally strict:

- Git is the sync layer.
- Ignored files are ignored unless explicitly allowlisted for private WIP sync.
- Pulls are fast-forward only.
- Dirty work can be moved through private WIP refs on a `sync` remote.
- No auto-merge, auto-rebase, or force-push.

## Run From Source

GitDCY needs Rust, Cargo, and Git on every device.

### macOS

```bash
xcode-select --install
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
git clone https://github.com/<owner>/gitdcy.git
cd gitdcy
cargo run -p gitdcy-gui
```

### Windows 11

Install Git for Windows and Rustup. If Rustup asks for native build tools,
install Visual Studio Build Tools with the C++ desktop workload.

```powershell
winget install Git.Git Rustlang.Rustup
git clone https://github.com/<owner>/gitdcy.git
cd gitdcy
cargo run -p gitdcy-gui
```

### Linux Mint / Ubuntu

```bash
sudo apt update
sudo apt install -y build-essential git pkg-config libx11-dev libxcb1-dev libxkbcommon-dev libwayland-dev libxrandr-dev libxi-dev libgl1-mesa-dev
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh
git clone https://github.com/<owner>/gitdcy.git
cd gitdcy
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

## Sync Multiple Devices

GitDCY does not use a special pairing code. Device access is controlled by Git:
if a device can clone, fetch, and push to the configured Git remotes, GitDCY can
use those remotes for sync.

Set up each device like this:

1. Install GitDCY and Git.
2. Sign in to the Git hosts you use, usually with SSH keys, a credential helper,
   or the host's normal login flow.
3. Clone your repos into provider folders such as `~/Code/github`,
   `~/Code/gitlab`, or `~/Code/forgejo`.
4. Open the GUI and press **Refresh**.
5. For GitHub/GitLab repos, set a private **sync** remote for WIP refs. Forgejo
   repos can use Forgejo `origin` when that remote is private and reachable from
   your devices.
6. Press **Sync All**.

When a repo receives WIP from a device that has not been approved on this
machine, GitDCY stops before applying it. Select the repo and press **Trust
Incoming Device**, then sync again. The CLI equivalent is:

```bash
cargo run -p gitdcy-cli -- trust-device <repo> <device>
```

This approval is a local safety guard, not cryptographic MFA. The security
boundary is still the Git remote: remove a lost laptop's SSH key or token from
GitHub, GitLab, or Forgejo to revoke it.

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

## Local Config

GitDCY reads machine-local settings from the app config directory as
`local.yaml`. When run from this source checkout, it also reads
`.gitdcy.local.yaml`; that file is ignored by Git.

The GUI writes per-device settings, including the `.env` checkbox, to the app
config directory. Those settings are not committed to this repo.

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
through private WIP sync, select a repo in the GUI and enable **Sync .env through
private WIP**. The local config equivalent is:

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

Binary paths:

- macOS/Linux GUI: `target/release/gitdcy-gui`
- macOS/Linux CLI: `target/release/gitdcy`
- Windows GUI: `target\release\gitdcy-gui.exe`
- Windows CLI: `target\release\gitdcy.exe`
