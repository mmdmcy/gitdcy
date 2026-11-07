//! `gitdcy dashboard`: every repository, the selected one's details and its
//! findings in the terminal. Drawn with plain ANSI escape codes on the
//! alternate screen, the keyboard read in raw mode (termios on Unix, the
//! console API on Windows), so no terminal library is compiled in. It draws
//! what the ratatui version drew: the same panes and sizes (ratatui's layout
//! solver's results, worked out below), colours and keys.

use anyhow::{Context, Result};
use gitdcy_core::{load_or_discover_manifest, status_all, sync_repo, RepoStatus};
use std::collections::VecDeque;
use std::io::{self, Write};
use std::time::Duration;

pub fn run() -> Result<()> {
    let mut tty = term::Tty::raw().context("enable raw mode")?;
    let mut out = io::stdout();
    // The alternate screen, so the shell's own screen comes back on quit.
    out.write_all(b"\x1b[?1049h")
        .and_then(|()| out.flush())
        .context("enter alternate screen")?;
    let result = tty
        .size()
        .context("create terminal")
        .and_then(|_| run_loop(&mut tty, &mut out));
    tty.restore().ok();
    out.write_all(b"\x1b[?1049l\x1b[?25h")
        .and_then(|()| out.flush())
        .ok();
    result
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Key {
    Quit,
    Refresh,
    Down,
    Up,
    Sync,
    SyncAll,
}

fn run_loop(tty: &mut term::Tty, out: &mut impl Write) -> Result<()> {
    let mut manifest = load_or_discover_manifest()?;
    let mut statuses = status_all(&manifest);
    let mut selected = 0usize;
    let mut log =
        "q quit | r refresh | j/k or arrows move | s sync selected | S sync all safe".to_string();
    let mut shown: Option<Screen> = None;
    let mut keys = VecDeque::new();

    loop {
        if selected >= statuses.len() {
            selected = statuses.len().saturating_sub(1);
        }
        let (width, height) = tty.size()?;
        let screen = draw(&view(&statuses, selected, &log), width, height);
        if shown.as_ref() != Some(&screen) {
            let resized = shown.as_ref().map(|s| (s.width, s.height)) != Some((width, height));
            out.write_all(&screen.ansi(resized))?;
            out.flush()?;
            shown = Some(screen);
        }

        if keys.is_empty() {
            keys.extend(tty.keys(Duration::from_millis(250))?);
        }
        let Some(key) = keys.pop_front() else {
            continue;
        };
        match key {
            Key::Quit => return Ok(()),
            Key::Refresh => {
                manifest = load_or_discover_manifest()?;
                statuses = status_all(&manifest);
                log = "refreshed".to_string();
            }
            Key::Down if selected + 1 < statuses.len() => {
                selected += 1;
            }
            Key::Down => {}
            Key::Up => {
                selected = selected.saturating_sub(1);
            }
            Key::Sync => {
                if let Some(status) = statuses.get(selected) {
                    if status.safety.has_fatal_findings() {
                        log = format!("sync blocked for {}: safety findings", status.entry.id);
                    } else {
                        let report = sync_repo(&status.entry);
                        log = if let Some(blocked) = report.blocked {
                            format!("sync blocked for {}: {blocked}", status.entry.id)
                        } else {
                            format!("synced {}", status.entry.id)
                        };
                        statuses = status_all(&manifest);
                    }
                }
            }
            Key::SyncAll => {
                let mut synced = 0usize;
                let mut blocked = 0usize;
                for status in &statuses {
                    if status.safety.has_fatal_findings() {
                        blocked += 1;
                        continue;
                    }
                    let report = sync_repo(&status.entry);
                    if report.blocked.is_some() {
                        blocked += 1;
                    } else {
                        synced += 1;
                    }
                }
                statuses = status_all(&manifest);
                log = format!("sync all safe: {synced} synced, {blocked} blocked/skipped");
            }
        }
    }
}

/// The keys in what the terminal sent, as the old reader took them: a
/// letter also with Ctrl or Alt held (Ctrl+S syncs, as it did), the arrows
/// in either cursor mode and with modifiers; everything else is ignored.
fn parse_keys(bytes: &[u8]) -> Vec<Key> {
    let letter = |byte: u8| match byte {
        b'q' | 0x11 => Some(Key::Quit),
        b'r' | 0x12 => Some(Key::Refresh),
        b'j' | 0x0a => Some(Key::Down),
        b'k' | 0x0b => Some(Key::Up),
        b's' | 0x13 => Some(Key::Sync),
        b'S' => Some(Key::SyncAll),
        _ => None,
    };
    let arrow = |byte: u8| match byte {
        b'A' => Some(Key::Up),
        b'B' => Some(Key::Down),
        _ => None,
    };
    let mut keys = Vec::new();
    let mut i = 0;
    while i < bytes.len() {
        if bytes[i] != 0x1b {
            keys.extend(letter(bytes[i]));
            i += 1;
            continue;
        }
        match bytes.get(i + 1) {
            // CSI: parameters, then one final byte; ESC [ A is Up.
            Some(b'[') => {
                let mut end = i + 2;
                while end < bytes.len() && !(0x40..=0x7e).contains(&bytes[end]) {
                    end += 1;
                }
                if let Some(&last) = bytes.get(end) {
                    keys.extend(arrow(last));
                }
                i = end + 1;
            }
            // SS3, the application cursor mode: ESC O A is Up.
            Some(b'O') if bytes.get(i + 2).is_some() => {
                keys.extend(arrow(bytes[i + 2]));
                i += 3;
            }
            // Alt and a key.
            Some(&next) => {
                keys.extend(letter(next));
                i += 2;
            }
            None => i += 1,
        }
    }
    keys
}

/// What one frame shows.
pub(crate) struct View {
    pub(crate) rows: Vec<[String; 4]>,
    pub(crate) selected: Option<usize>,
    /// The selected repository's lines, each a run of (text, bold).
    pub(crate) detail: Vec<Vec<(String, bool)>>,
    pub(crate) findings: Vec<String>,
    pub(crate) log: String,
}

fn view(statuses: &[RepoStatus], selected: usize, log: &str) -> View {
    View {
        rows: statuses
            .iter()
            .map(|status| {
                [
                    status.entry.id.clone(),
                    status.branch.as_deref().unwrap_or("-").to_string(),
                    status.short_state(),
                    status.safety.short_state(),
                ]
            })
            .collect(),
        selected: (!statuses.is_empty()).then_some(selected),
        detail: statuses
            .get(selected)
            .map(status_detail_lines)
            .unwrap_or_else(|| vec![vec![("No repositories discovered.".to_string(), false)]]),
        findings: statuses
            .get(selected)
            .map(finding_items)
            .unwrap_or_default(),
        log: log.to_string(),
    }
}

fn status_detail_lines(status: &RepoStatus) -> Vec<Vec<(String, bool)>> {
    let field = |name: &str, value: String| vec![(format!("{name}: "), true), (value, false)];
    let mut lines = vec![
        field("id", status.entry.id.clone()),
        field("path", status.path.display().to_string()),
        field(
            "branch",
            status.branch.clone().unwrap_or_else(|| "-".to_string()),
        ),
        field(
            "tracking",
            status
                .tracking_branch
                .clone()
                .unwrap_or_else(|| "-".to_string()),
        ),
        field("visibility", status.safety.visibility.label().to_string()),
        field("state", status.short_state()),
        field("identity", status.identity.short_state()),
    ];
    let plain = |text: String| vec![(text, false)];
    if status.identity.state_label() != "identity-ok"
        && status.identity.state_label() != "identity-unchecked"
    {
        lines.push(plain(format!("identity note: {}", status.identity.message)));
    }
    if status.remotes.is_empty() {
        lines.push(plain("remotes: -".to_string()));
    } else {
        lines.push(plain("remotes:".to_string()));
        for (name, url) in &status.remotes {
            lines.push(plain(format!("  {name}: {url}")));
        }
    }
    lines
}

fn finding_items(status: &RepoStatus) -> Vec<String> {
    let mut items = Vec::new();
    if status.identity.state_label() != "identity-ok"
        && status.identity.state_label() != "identity-unchecked"
    {
        items.push(format!("[identity] {}", status.identity.message));
    }
    items.extend(status.safety.findings.iter().map(|finding| {
        let path = finding.path.as_deref().unwrap_or("-");
        format!("[{}] {path}: {}", finding.severity.label(), finding.reason)
    }));
    if items.is_empty() {
        items.push("No safety or identity findings.".to_string());
    }
    items
}

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq)]
pub(crate) struct Style {
    pub(crate) yellow: bool,
    pub(crate) bold: bool,
    pub(crate) reversed: bool,
}

const BOLD: Style = Style {
    yellow: false,
    bold: true,
    reversed: false,
};

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct Cell {
    /// What the cell shows; empty when a wide character to its left covers it.
    pub(crate) symbol: String,
    pub(crate) style: Style,
}

#[derive(Debug, Clone, Copy)]
struct Area {
    x: u16,
    y: u16,
    width: u16,
    height: u16,
}

impl Area {
    /// Inside a border all around.
    fn inner(self) -> Area {
        Area {
            x: self.x.saturating_add(1).min(self.x + self.width),
            y: self.y.saturating_add(1).min(self.y + self.height),
            width: self.width.saturating_sub(2),
            height: self.height.saturating_sub(2),
        }
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub(crate) struct Screen {
    pub(crate) width: u16,
    pub(crate) height: u16,
    pub(crate) cells: Vec<Cell>,
}

impl Screen {
    fn new(width: u16, height: u16) -> Screen {
        let blank = Cell {
            symbol: " ".to_string(),
            style: Style::default(),
        };
        Screen {
            width,
            height,
            cells: vec![blank; usize::from(width) * usize::from(height)],
        }
    }

    fn cell(&mut self, x: u16, y: u16) -> Option<&mut Cell> {
        if x < self.width && y < self.height {
            let index = usize::from(y) * usize::from(self.width) + usize::from(x);
            self.cells.get_mut(index)
        } else {
            None
        }
    }

    fn put(&mut self, x: u16, y: u16, symbol: &str) {
        if let Some(cell) = self.cell(x, y) {
            cell.symbol = symbol.to_string();
        }
    }

    fn style(&mut self, area: Area, style: Style) {
        for y in area.y..area.y.saturating_add(area.height) {
            for x in area.x..area.x.saturating_add(area.width) {
                if let Some(cell) = self.cell(x, y) {
                    cell.style.yellow |= style.yellow;
                    cell.style.bold |= style.bold;
                    cell.style.reversed |= style.reversed;
                }
            }
        }
    }

    /// Writes `text` from (x, y), at most `width` columns, and returns the
    /// columns used. Control characters are dropped, as ratatui dropped
    /// them: a repository name or a message can never move the cursor or
    /// change the terminal.
    fn text(&mut self, x: u16, y: u16, width: u16, text: &str, style: Style) -> u16 {
        let mut used = 0u16;
        let mut last: Option<u16> = None;
        for c in text.chars().filter(|c| !c.is_control()) {
            let w = char_width(c);
            if w == 0 {
                if let Some(at) = last {
                    if let Some(cell) = self.cell(at, y) {
                        cell.symbol.push(c);
                    }
                }
                continue;
            }
            if used + w > width {
                break;
            }
            let at = x + used;
            if let Some(cell) = self.cell(at, y) {
                cell.symbol = c.to_string();
            }
            self.style(
                Area {
                    x: at,
                    y,
                    width: 1,
                    height: 1,
                },
                style,
            );
            if w == 2 {
                if let Some(cell) = self.cell(at + 1, y) {
                    cell.symbol.clear();
                }
            }
            last = Some(at);
            used += w;
        }
        used
    }

    /// One line of runs, left-aligned, cut at the area's width.
    fn line(&mut self, area: Area, y: u16, runs: &[(String, bool)]) {
        let mut x = area.x;
        for (text, bold) in runs {
            let left = area.width - (x - area.x);
            if left == 0 {
                break;
            }
            let style = if *bold { BOLD } else { Style::default() };
            x += self.text(x, y, left, text, style);
        }
    }

    /// A border all around with `title` on its top edge; returns the inside.
    fn block(&mut self, area: Area, title: &str) -> Area {
        if area.width == 0 || area.height == 0 {
            return area.inner();
        }
        let (left, top) = (area.x, area.y);
        let right = area.x + area.width - 1;
        let bottom = area.y + area.height - 1;
        for y in top..=bottom {
            self.put(left, y, "│");
        }
        for x in left..=right {
            self.put(x, top, "─");
        }
        for y in top..=bottom {
            self.put(right, y, "│");
        }
        for x in left..=right {
            self.put(x, bottom, "─");
        }
        self.put(right, bottom, "┘");
        self.put(right, top, "┐");
        self.put(left, bottom, "└");
        self.put(left, top, "┌");
        self.text(
            left + 1,
            top,
            area.width.saturating_sub(2),
            title,
            Style::default(),
        );
        area.inner()
    }

    /// The frame as escape codes: every row from its first column, a style
    /// change only where the style changes. `clear` wipes the screen first,
    /// after a resize.
    fn ansi(&self, clear: bool) -> Vec<u8> {
        let mut out = String::from("\x1b[?25l");
        if clear {
            out.push_str("\x1b[2J");
        }
        for y in 0..self.height {
            out.push_str(&format!("\x1b[{};1H", y + 1));
            let mut style = Style::default();
            out.push_str("\x1b[0m");
            for x in 0..self.width {
                let cell = &self.cells[usize::from(y) * usize::from(self.width) + usize::from(x)];
                if cell.style != style {
                    style = cell.style;
                    out.push_str("\x1b[0");
                    if style.bold {
                        out.push_str(";1");
                    }
                    if style.reversed {
                        out.push_str(";7");
                    }
                    if style.yellow {
                        out.push_str(";33");
                    }
                    out.push('m');
                }
                out.push_str(&cell.symbol);
            }
        }
        out.push_str("\x1b[0m");
        out.into_bytes()
    }
}

/// Columns a character takes in a terminal: 0 for combining marks and other
/// zero-width characters, 2 for East Asian wide characters and emoji, else 1.
fn char_width(c: char) -> u16 {
    match u32::from(c) {
        0x0300..=0x036f
        | 0x0483..=0x0489
        | 0x0591..=0x05bd
        | 0x0610..=0x061a
        | 0x064b..=0x065f
        | 0x200b..=0x200f
        | 0x20d0..=0x20ff
        | 0xfe00..=0xfe0f
        | 0xfe20..=0xfe2f
        | 0xe0100..=0xe01ef => 0,
        0x1100..=0x115f
        | 0x2e80..=0x303e
        | 0x3041..=0x33ff
        | 0x3400..=0x4dbf
        | 0x4e00..=0x9fff
        | 0xa000..=0xa4cf
        | 0xac00..=0xd7a3
        | 0xf900..=0xfaff
        | 0xfe30..=0xfe4f
        | 0xff00..=0xff60
        | 0xffe0..=0xffe6
        | 0x1f300..=0x1f64f
        | 0x1f900..=0x1f9ff
        | 0x20000..=0x2fffd
        | 0x30000..=0x3fffd => 2,
        _ => 1,
    }
}

/// The table's four columns (x, width) inside `width` cells: 32, 18, 30 and
/// 20 per cent with one cell between them, as ratatui's solver placed them.
/// The three spacing cells come off the columns a hundredth of a cell at a
/// time, each time off the column whose shrinking evens out its neighbours
/// most (the solver's weak equal-size rule); then each edge is rounded.
fn columns(width: u16) -> [(u16, u16); 4] {
    let w = i64::from(width);
    let mut size = [32 * w, 18 * w, 30 * w, 20 * w];
    let gain = |size: &[i64; 4], i: usize| -> i64 {
        [i.checked_sub(1), Some(i + 1)]
            .into_iter()
            .flatten()
            .filter(|&j| j < 4)
            .map(|j| if size[i] > size[j] { 1 } else { -1 })
            .sum()
    };
    for _ in 0..300 {
        let best = (0..4)
            .filter(|&i| size[i] > 0)
            .max_by_key(|&i| (gain(&size, i), std::cmp::Reverse(i)));
        match best {
            Some(i) => size[i] -= 1,
            None => break,
        }
    }
    let round = |hundredths: i64| ((hundredths + 50) / 100) as u16;
    let mut start = 0;
    size.map(|size| {
        let (x, end) = (round(start), round(start + size));
        start += size + 100;
        (x, end.saturating_sub(x))
    })
}

/// The frame for a terminal `width` by `height`.
pub(crate) fn draw(view: &View, width: u16, height: u16) -> Screen {
    let mut screen = Screen::new(width, height);

    // Rows: the panes take at least 12, the findings 9 and the footer 2;
    // short of room the findings and footer share what is left after 12.
    let top = height.min(12.max(height.saturating_sub(11)));
    let rest = height - top;
    let footer = (rest / 2).min(2);
    let findings = rest - footer;
    // Columns: 58 and 42 per cent, rounded half up.
    let left = ((u32::from(width) * 58 + 50) / 100) as u16;

    let table = screen.block(
        Area {
            x: 0,
            y: 0,
            width: left,
            height: top,
        },
        "GitDCY Dashboard",
    );
    if table.width > 0 && table.height > 0 {
        let cols = columns(table.width);
        let cells = |screen: &mut Screen, y: u16, row: &[String], style: Style| {
            for ((x, w), text) in cols.iter().zip(row) {
                let first = text.lines().next().unwrap_or("");
                screen.text(table.x + x, y, *w, first, style);
            }
        };
        let header = ["repo", "branch", "state", "safety"].map(String::from);
        let yellow_bold = Style {
            yellow: true,
            bold: true,
            reversed: false,
        };
        let header_row = Area { height: 1, ..table };
        screen.style(header_row, yellow_bold);
        cells(&mut screen, table.y, &header, yellow_bold);

        let visible = usize::from(table.height - 1);
        let first = match view.selected {
            Some(selected) if selected >= visible => selected + 1 - visible,
            _ => 0,
        };
        for (n, row) in view.rows.iter().skip(first).take(visible).enumerate() {
            let y = table.y + 1 + n as u16;
            cells(&mut screen, y, row, Style::default());
            if view.selected == Some(first + n) {
                let reversed = Style {
                    reversed: true,
                    ..Style::default()
                };
                screen.style(
                    Area {
                        y,
                        height: 1,
                        ..table
                    },
                    reversed,
                );
            }
        }
    }

    let detail = screen.block(
        Area {
            x: left,
            y: 0,
            width: width - left,
            height: top,
        },
        "Selected Repo",
    );
    for (n, runs) in view
        .detail
        .iter()
        .take(usize::from(detail.height))
        .enumerate()
    {
        screen.line(detail, detail.y + n as u16, runs);
    }

    let list = screen.block(
        Area {
            x: 0,
            y: top,
            width,
            height: findings,
        },
        "Findings",
    );
    let mut y = 0u16;
    for item in &view.findings {
        let lines: Vec<&str> = if item.is_empty() {
            vec![""]
        } else {
            item.lines().collect()
        };
        if usize::from(y) + lines.len() > usize::from(list.height) {
            break;
        }
        for line in lines {
            screen.line(list, list.y + y, &[(line.to_string(), false)]);
            y += 1;
        }
    }

    let log = screen.block(
        Area {
            x: 0,
            y: top + findings,
            width,
            height: footer,
        },
        "",
    );
    if log.height > 0 {
        let first = view.log.lines().next().unwrap_or("");
        screen.line(log, log.y, &[(first.to_string(), false)]);
    }
    screen
}

#[cfg(unix)]
mod term {
    use super::{parse_keys, Key};
    use std::io;
    use std::time::Duration;

    /// The terminal in raw mode: stdin when it is one, else /dev/tty.
    pub(super) struct Tty {
        fd: libc::c_int,
        owned: bool,
        saved: Option<libc::termios>,
    }

    impl Tty {
        pub(super) fn raw() -> io::Result<Tty> {
            let (fd, owned) = if unsafe { libc::isatty(libc::STDIN_FILENO) } == 1 {
                (libc::STDIN_FILENO, false)
            } else {
                let fd = unsafe { libc::open(c"/dev/tty".as_ptr(), libc::O_RDWR) };
                if fd < 0 {
                    return Err(io::Error::last_os_error());
                }
                (fd, true)
            };
            let mut tty = Tty {
                fd,
                owned,
                saved: None,
            };
            let mut saved = unsafe { std::mem::zeroed::<libc::termios>() };
            if unsafe { libc::tcgetattr(fd, &mut saved) } != 0 {
                return Err(io::Error::last_os_error());
            }
            let mut raw = saved;
            unsafe { libc::cfmakeraw(&mut raw) };
            if unsafe { libc::tcsetattr(fd, libc::TCSANOW, &raw) } != 0 {
                return Err(io::Error::last_os_error());
            }
            tty.saved = Some(saved);
            Ok(tty)
        }

        pub(super) fn restore(&mut self) -> io::Result<()> {
            if let Some(saved) = self.saved.take() {
                if unsafe { libc::tcsetattr(self.fd, libc::TCSANOW, &saved) } != 0 {
                    return Err(io::Error::last_os_error());
                }
            }
            Ok(())
        }

        pub(super) fn size(&self) -> io::Result<(u16, u16)> {
            let mut size = unsafe { std::mem::zeroed::<libc::winsize>() };
            if unsafe { libc::ioctl(self.fd, libc::TIOCGWINSZ, &mut size) } != 0 {
                return Err(io::Error::last_os_error());
            }
            Ok((size.ws_col, size.ws_row))
        }

        /// The keys typed within `timeout`, none when nothing was.
        pub(super) fn keys(&mut self, timeout: Duration) -> io::Result<Vec<Key>> {
            let mut poll = libc::pollfd {
                fd: self.fd,
                events: libc::POLLIN,
                revents: 0,
            };
            let ready = unsafe { libc::poll(&mut poll, 1, timeout.as_millis() as libc::c_int) };
            if ready < 0 {
                let error = io::Error::last_os_error();
                return match error.kind() {
                    io::ErrorKind::Interrupted => Ok(Vec::new()),
                    _ => Err(error),
                };
            }
            if ready == 0 {
                return Ok(Vec::new());
            }
            let mut buffer = [0u8; 1024];
            let read = unsafe { libc::read(self.fd, buffer.as_mut_ptr().cast(), buffer.len()) };
            if read < 0 {
                let error = io::Error::last_os_error();
                return match error.kind() {
                    io::ErrorKind::Interrupted | io::ErrorKind::WouldBlock => Ok(Vec::new()),
                    _ => Err(error),
                };
            }
            Ok(parse_keys(&buffer[..read as usize]))
        }
    }

    impl Drop for Tty {
        fn drop(&mut self) {
            self.restore().ok();
            if self.owned {
                unsafe { libc::close(self.fd) };
            }
        }
    }
}

#[cfg(windows)]
mod term {
    //! The Windows console through kernel32, which every Windows program
    //! links: key presses read as console input records, the screen drawn
    //! with the console's own escape-code support (Windows 10 and later).
    use super::{parse_keys, Key};
    use std::ffi::c_void;
    use std::io;
    use std::time::Duration;

    type Handle = *mut c_void;
    const STD_INPUT_HANDLE: u32 = -10i32 as u32;
    const STD_OUTPUT_HANDLE: u32 = -11i32 as u32;
    const ENABLE_PROCESSED_INPUT: u32 = 0x0001;
    const ENABLE_LINE_INPUT: u32 = 0x0002;
    const ENABLE_ECHO_INPUT: u32 = 0x0004;
    const ENABLE_VIRTUAL_TERMINAL_PROCESSING: u32 = 0x0004;
    const KEY_EVENT: u16 = 0x0001;
    const VK_UP: u16 = 0x26;
    const VK_DOWN: u16 = 0x28;
    const WAIT_OBJECT_0: u32 = 0;

    /// INPUT_RECORD with its KEY_EVENT_RECORD; other events fit in it too.
    #[repr(C)]
    #[derive(Clone, Copy, Default)]
    struct InputRecord {
        event_type: u16,
        _padding: u16,
        key_down: i32,
        repeat_count: u16,
        virtual_key_code: u16,
        virtual_scan_code: u16,
        unicode_char: u16,
        control_key_state: u32,
    }

    #[repr(C)]
    #[derive(Default)]
    struct ScreenBufferInfo {
        size: [i16; 2],
        cursor_position: [i16; 2],
        attributes: u16,
        window: [i16; 4],
        maximum_window_size: [i16; 2],
    }

    #[link(name = "kernel32")]
    extern "system" {
        fn GetStdHandle(std_handle: u32) -> Handle;
        fn GetConsoleMode(console: Handle, mode: *mut u32) -> i32;
        fn SetConsoleMode(console: Handle, mode: u32) -> i32;
        fn WaitForSingleObject(handle: Handle, milliseconds: u32) -> u32;
        fn GetNumberOfConsoleInputEvents(console: Handle, events: *mut u32) -> i32;
        fn ReadConsoleInputW(
            console: Handle,
            buffer: *mut InputRecord,
            length: u32,
            read: *mut u32,
        ) -> i32;
        fn GetConsoleScreenBufferInfo(console: Handle, info: *mut ScreenBufferInfo) -> i32;
    }

    pub(super) struct Tty {
        input: Handle,
        output: Handle,
        saved: Option<(u32, u32)>,
    }

    fn check(ok: i32) -> io::Result<()> {
        if ok == 0 {
            Err(io::Error::last_os_error())
        } else {
            Ok(())
        }
    }

    impl Tty {
        pub(super) fn raw() -> io::Result<Tty> {
            let input = unsafe { GetStdHandle(STD_INPUT_HANDLE) };
            let output = unsafe { GetStdHandle(STD_OUTPUT_HANDLE) };
            let (mut input_mode, mut output_mode) = (0u32, 0u32);
            check(unsafe { GetConsoleMode(input, &mut input_mode) })?;
            check(unsafe { GetConsoleMode(output, &mut output_mode) })?;
            let raw =
                input_mode & !(ENABLE_LINE_INPUT | ENABLE_ECHO_INPUT | ENABLE_PROCESSED_INPUT);
            check(unsafe { SetConsoleMode(input, raw) })?;
            let mut tty = Tty {
                input,
                output,
                saved: Some((input_mode, output_mode)),
            };
            if let Err(error) = check(unsafe {
                SetConsoleMode(output, output_mode | ENABLE_VIRTUAL_TERMINAL_PROCESSING)
            }) {
                tty.restore().ok();
                return Err(error);
            }
            Ok(tty)
        }

        pub(super) fn restore(&mut self) -> io::Result<()> {
            if let Some((input_mode, output_mode)) = self.saved.take() {
                check(unsafe { SetConsoleMode(self.input, input_mode) })?;
                check(unsafe { SetConsoleMode(self.output, output_mode) })?;
            }
            Ok(())
        }

        pub(super) fn size(&self) -> io::Result<(u16, u16)> {
            let mut info = ScreenBufferInfo::default();
            check(unsafe { GetConsoleScreenBufferInfo(self.output, &mut info) })?;
            let [left, top, right, bottom] = info.window;
            Ok(((right - left + 1) as u16, (bottom - top + 1) as u16))
        }

        /// The keys pressed within `timeout` (presses only, not releases).
        pub(super) fn keys(&mut self, timeout: Duration) -> io::Result<Vec<Key>> {
            if unsafe { WaitForSingleObject(self.input, timeout.as_millis() as u32) }
                != WAIT_OBJECT_0
            {
                return Ok(Vec::new());
            }
            let mut pending = 0u32;
            check(unsafe { GetNumberOfConsoleInputEvents(self.input, &mut pending) })?;
            if pending == 0 {
                return Ok(Vec::new());
            }
            let mut records = [InputRecord::default(); 64];
            let mut read = 0u32;
            check(unsafe {
                ReadConsoleInputW(
                    self.input,
                    records.as_mut_ptr(),
                    records.len() as u32,
                    &mut read,
                )
            })?;
            let mut bytes = Vec::new();
            for record in &records[..read as usize] {
                if record.event_type != KEY_EVENT || record.key_down == 0 {
                    continue;
                }
                match record.virtual_key_code {
                    VK_UP => bytes.extend_from_slice(b"\x1b[A"),
                    VK_DOWN => bytes.extend_from_slice(b"\x1b[B"),
                    _ => match u8::try_from(record.unicode_char) {
                        Ok(byte) if byte != 0x1b => bytes.push(byte),
                        _ => {}
                    },
                }
            }
            Ok(parse_keys(&bytes))
        }
    }

    impl Drop for Tty {
        fn drop(&mut self) {
            self.restore().ok();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn keys_are_read_as_the_old_reader_read_them() {
        use Key::*;
        assert_eq!(
            parse_keys(b"qrjksS"),
            [Quit, Refresh, Down, Up, Sync, SyncAll]
        );
        // Ctrl and a letter, Alt and a letter.
        assert_eq!(parse_keys(b"\x11\x13\x0a\x0b"), [Quit, Sync, Down, Up]);
        assert_eq!(parse_keys(b"\x1bq\x1bS"), [Quit, SyncAll]);
        // Arrows in both cursor modes and with a modifier; other keys ignored.
        assert_eq!(
            parse_keys(b"\x1b[A\x1bOB\x1b[1;5A\x1b[C\x1b[3~x\x1b"),
            [Up, Down, Up]
        );
    }

    #[test]
    fn columns_are_where_ratatuis_solver_put_them() {
        // Taken from ratatui 0.30's layout for these widths.
        assert_eq!(columns(20), [(0, 5), (6, 4), (11, 4), (16, 4)]);
        assert_eq!(columns(29), [(0, 9), (10, 5), (16, 6), (23, 6)]);
        assert_eq!(columns(91), [(0, 29), (30, 17), (48, 24), (73, 18)]);
        assert_eq!(columns(399), [(0, 128), (129, 72), (202, 116), (319, 80)]);
        assert_eq!(columns(4), [(0, 0), (1, 1), (3, 0), (4, 0)]);
        assert!(columns(0).iter().all(|&(_, w)| w == 0));
    }

    fn row(screen: &Screen, y: u16) -> String {
        let start = usize::from(y) * usize::from(screen.width);
        screen.cells[start..start + usize::from(screen.width)]
            .iter()
            .map(|cell| cell.symbol.as_str())
            .collect()
    }

    #[test]
    fn a_frame_has_the_panes_and_drops_control_characters() {
        let view = View {
            rows: vec![
                ["one".into(), "main".into(), "clean".into(), "ok".into()],
                ["two\x1b[2J".into(), "-".into(), "dirty".into(), "ok".into()],
            ],
            selected: Some(1),
            detail: vec![vec![("id: ".into(), true), ("two".into(), false)]],
            findings: vec!["No safety or identity findings.".into()],
            log: "refreshed".into(),
        };
        // The frame ratatui 0.30 drew for this view (compared cell by cell
        // with it at every size up to 130 by 40 when it was replaced).
        let screen = draw(&view, 60, 24);
        let expected = [
            "┌GitDCY Dashboard─────────────────┐┌Selected Repo──────────┐",
            "│repo        branch state  safety ││id: two                │",
            "│one         main   clean  ok     ││                       │",
            "│two[2J      -      dirty  ok     ││                       │",
        ];
        for (y, line) in expected.iter().enumerate() {
            assert_eq!(row(&screen, y as u16), *line);
        }
        assert_eq!(
            row(&screen, 12),
            format!("└{}┘└{}┘", "─".repeat(33), "─".repeat(23))
        );
        assert_eq!(row(&screen, 13), format!("┌Findings{}┐", "─".repeat(50)));
        assert_eq!(
            row(&screen, 14),
            format!("│No safety or identity findings.{}│", " ".repeat(27))
        );
        // The footer's two rows are its border: the log line never showed.
        assert_eq!(row(&screen, 22), format!("┌{}┐", "─".repeat(58)));
        assert_eq!(row(&screen, 23), format!("└{}┘", "─".repeat(58)));
        let at = |x: u16, y: u16| &screen.cells[usize::from(y) * 60 + usize::from(x)];
        assert!(at(1, 1).style.yellow && at(1, 1).style.bold);
        assert!(at(1, 3).style.reversed && !at(1, 2).style.reversed);
        assert!(at(36, 1).style.bold && !at(40, 1).style.bold);
        assert!(!screen.ansi(false).windows(4).any(|w| w == b"\x1b[2J"));
    }
}
