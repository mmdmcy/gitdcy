use anyhow::{anyhow, bail, Context, Result};
use std::io::Read;
use std::process::{Command, Output, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::thread;
use std::time::{Duration, Instant};

#[cfg(unix)]
use std::os::unix::process::CommandExt;

pub(crate) fn run_bounded_command(
    mut command: Command,
    limit: usize,
    timeout: Duration,
) -> Result<Output> {
    #[cfg(not(unix))]
    {
        let _ = (limit, timeout);
        return command.output().context("run bounded command");
    }

    #[cfg(unix)]
    {
        command.process_group(0);
        #[cfg(target_os = "linux")]
        unsafe {
            command.pre_exec(|| {
                if libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGKILL) != 0 {
                    return Err(std::io::Error::last_os_error());
                }
                if libc::getppid() == 1 {
                    return Err(std::io::Error::from_raw_os_error(libc::ESRCH));
                }
                Ok(())
            });
        }
        command.stdout(Stdio::piped()).stderr(Stdio::piped());
        let mut child = command.spawn().context("start bounded command")?;
        let stdout = child
            .stdout
            .take()
            .ok_or_else(|| anyhow!("bounded command stdout was not captured"))?;
        let stderr = child
            .stderr
            .take()
            .ok_or_else(|| anyhow!("bounded command stderr was not captured"))?;
        set_nonblocking(&stdout).context("configure bounded command stdout")?;
        set_nonblocking(&stderr).context("configure bounded command stderr")?;
        let stop_readers = Arc::new(AtomicBool::new(false));
        let stdout_reader = {
            let stop = Arc::clone(&stop_readers);
            thread::spawn(move || read_bounded_until_stopped(stdout, limit, &stop))
        };
        let stderr_reader = {
            let stop = Arc::clone(&stop_readers);
            thread::spawn(move || read_bounded_until_stopped(stderr, limit, &stop))
        };

        let started = Instant::now();
        let status = loop {
            if let Some(status) = child.try_wait().context("poll bounded command")? {
                terminate_process_group(&mut child)?;
                break status;
            }
            if started.elapsed() >= timeout {
                terminate_process_group(&mut child)?;
                break child.wait().context("wait for timed-out bounded command")?;
            }
            thread::sleep(Duration::from_millis(10));
        };

        stop_readers.store(true, Ordering::Release);
        let (stdout, stdout_truncated) = stdout_reader
            .join()
            .map_err(|_| anyhow!("bounded stdout reader panicked"))?
            .context("read bounded stdout")?;
        let (stderr, stderr_truncated) = stderr_reader
            .join()
            .map_err(|_| anyhow!("bounded stderr reader panicked"))?
            .context("read bounded stderr")?;
        if stdout_truncated || stderr_truncated {
            bail!("bounded command exceeded its output safety limit");
        }
        Ok(Output {
            status,
            stdout,
            stderr,
        })
    }
}

#[cfg(unix)]
fn terminate_process_group(child: &mut std::process::Child) -> Result<()> {
    let process_group = -(child.id() as i32);
    let result = unsafe { libc::kill(process_group, libc::SIGKILL) };
    if result != 0 {
        let error = std::io::Error::last_os_error();
        if error.raw_os_error() != Some(libc::ESRCH) {
            return Err(error).context("terminate bounded command process group");
        }
    }
    Ok(())
}

#[cfg(unix)]
fn set_nonblocking<T: std::os::fd::AsRawFd>(pipe: &T) -> std::io::Result<()> {
    let descriptor = pipe.as_raw_fd();
    let flags = unsafe { libc::fcntl(descriptor, libc::F_GETFL) };
    if flags < 0 {
        return Err(std::io::Error::last_os_error());
    }
    if unsafe { libc::fcntl(descriptor, libc::F_SETFL, flags | libc::O_NONBLOCK) } < 0 {
        return Err(std::io::Error::last_os_error());
    }
    Ok(())
}

#[cfg(unix)]
fn read_bounded_until_stopped<R: Read>(
    mut reader: R,
    limit: usize,
    stop: &AtomicBool,
) -> std::io::Result<(Vec<u8>, bool)> {
    let mut bytes = Vec::with_capacity(limit.min(64 * 1024));
    let mut buffer = [0_u8; 8192];
    let mut truncated = false;
    loop {
        match reader.read(&mut buffer) {
            Ok(0) => break,
            Ok(count) => {
                let stopping = stop.load(Ordering::Acquire);
                let remaining = limit.saturating_sub(bytes.len());
                let retained = remaining.min(count);
                bytes.extend_from_slice(&buffer[..retained]);
                truncated |= retained < count;
                if stopping && truncated {
                    break;
                }
            }
            Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                if stop.load(Ordering::Acquire) {
                    break;
                }
                thread::sleep(Duration::from_millis(10));
            }
            Err(error) => return Err(error),
        }
    }
    Ok((bytes, truncated))
}

#[cfg(all(test, unix))]
mod tests {
    use super::run_bounded_command;
    use std::process::Command;
    use std::time::{Duration, Instant};

    #[test]
    fn escaped_descendant_cannot_hold_command_output_open() {
        let started = Instant::now();
        let mut command = Command::new("sh");
        command.args(["-c", "setsid sleep 1 & exit 0"]);
        let output = run_bounded_command(command, 1024, Duration::from_secs(1)).unwrap();
        assert!(output.status.success());
        assert!(started.elapsed() < Duration::from_millis(500));
    }

    #[test]
    fn escaped_continuous_writer_cannot_hold_command_output_open() {
        let started = Instant::now();
        let mut command = Command::new("sh");
        command.args(["-c", "setsid sh -c 'yes output' & exit 0"]);
        if let Ok(output) = run_bounded_command(command, 1024, Duration::from_secs(1)) {
            assert!(output.status.success());
        }
        assert!(started.elapsed() < Duration::from_secs(2));
    }

    #[test]
    fn completed_command_drains_valid_output_above_64_kib() {
        let mut command = Command::new("sh");
        command.args(["-c", "dd if=/dev/zero bs=131072 count=1 2>/dev/null"]);
        let output = run_bounded_command(command, 256 * 1024, Duration::from_secs(1)).unwrap();
        assert_eq!(output.stdout.len(), 131_072);
    }
}
