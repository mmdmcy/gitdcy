//! GitDCY's error: a message and the errors that caused it, with the
//! `Context` trait and the `bail!` and `err!` macros. It prints as the anyhow
//! crate printed its errors, which it replaced: `{}` the message, `{:#}` the
//! whole chain joined with ": ", and `{:?}` (what `main` shows on failure)
//! the message, then "Caused by:" and each cause, then the stack backtrace
//! when RUST_BACKTRACE or RUST_LIB_BACKTRACE asks for one.

use std::backtrace::{Backtrace, BacktraceStatus};
use std::error::Error as StdError;
use std::fmt::{self, Debug, Display, Write};

pub type Result<T, E = Error> = std::result::Result<T, E>;

pub struct Error {
    error: Box<dyn StdError + Send + Sync + 'static>,
    backtrace: Backtrace,
}

impl Error {
    /// An error that is only a message.
    pub fn msg(message: impl Display) -> Error {
        Error {
            error: Box::new(Message(message.to_string())),
            backtrace: Backtrace::capture(),
        }
    }

    fn wrap(self, context: impl Display) -> Error {
        Error {
            error: Box::new(Wrapped {
                context: context.to_string(),
                source: self.error,
            }),
            backtrace: self.backtrace,
        }
    }

    /// This error and every error that caused it, outermost first.
    fn chain(&self) -> impl Iterator<Item = &(dyn StdError + 'static)> {
        let first: &(dyn StdError + 'static) = &*self.error;
        std::iter::successors(Some(first), |&error| error.source())
    }
}

// Not `impl std::error::Error for Error`: that would clash with this
// conversion, which lets `?` take any error.
impl<E: StdError + Send + Sync + 'static> From<E> for Error {
    fn from(error: E) -> Error {
        Error {
            error: Box::new(error),
            backtrace: Backtrace::capture(),
        }
    }
}

impl Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        write!(f, "{}", self.error)?;
        if f.alternate() {
            for cause in self.chain().skip(1) {
                write!(f, ": {cause}")?;
            }
        }
        Ok(())
    }
}

impl Debug for Error {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        if f.alternate() {
            return Debug::fmt(&self.error, f);
        }
        write!(f, "{}", self.error)?;
        let causes: Vec<_> = self.chain().skip(1).collect();
        if !causes.is_empty() {
            f.write_str("\n\nCaused by:")?;
            let numbered = causes.len() > 1;
            for (n, cause) in causes.iter().enumerate() {
                let (first, rest) = if numbered {
                    (format!("{n: >5}: "), "       ")
                } else {
                    ("    ".to_string(), "    ")
                };
                write!(f, "\n{first}")?;
                for (i, line) in cause.to_string().split('\n').enumerate() {
                    if i > 0 {
                        f.write_char('\n')?;
                        f.write_str(rest)?;
                    }
                    f.write_str(line)?;
                }
            }
        }
        if let BacktraceStatus::Captured = self.backtrace.status() {
            let mut backtrace = self.backtrace.to_string();
            f.write_str("\n\n")?;
            if backtrace.starts_with("stack backtrace:") {
                backtrace.replace_range(0..1, "S");
            } else {
                f.write_str("Stack backtrace:\n")?;
            }
            backtrace.truncate(backtrace.trim_end().len());
            f.write_str(&backtrace)?;
        }
        Ok(())
    }
}

struct Message(String);

impl Display for Message {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        f.write_str(&self.0)
    }
}

impl Debug for Message {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        Debug::fmt(&self.0, f)
    }
}

impl StdError for Message {}

struct Wrapped {
    context: String,
    source: Box<dyn StdError + Send + Sync + 'static>,
}

impl Display for Wrapped {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        f.write_str(&self.context)
    }
}

impl Debug for Wrapped {
    fn fmt(&self, f: &mut fmt::Formatter) -> fmt::Result {
        f.debug_struct("Error")
            .field("context", &self.context)
            .field("source", &self.source)
            .finish()
    }
}

impl StdError for Wrapped {
    fn source(&self) -> Option<&(dyn StdError + 'static)> {
        Some(&*self.source)
    }
}

/// What went on when an error happened, put in front of it; on an `Option`,
/// the error that `None` becomes.
pub trait Context<T> {
    fn context<C: Display>(self, context: C) -> Result<T>;
    fn with_context<C: Display, F: FnOnce() -> C>(self, context: F) -> Result<T>;
}

impl<T, E: Into<Error>> Context<T> for std::result::Result<T, E> {
    fn context<C: Display>(self, context: C) -> Result<T> {
        self.map_err(|error| error.into().wrap(context))
    }

    fn with_context<C: Display, F: FnOnce() -> C>(self, context: F) -> Result<T> {
        self.map_err(|error| error.into().wrap(context()))
    }
}

impl<T> Context<T> for Option<T> {
    fn context<C: Display>(self, context: C) -> Result<T> {
        self.ok_or_else(|| Error::msg(context))
    }

    fn with_context<C: Display, F: FnOnce() -> C>(self, context: F) -> Result<T> {
        self.ok_or_else(|| Error::msg(context()))
    }
}

/// An error from a message, written like `format!`.
#[macro_export]
macro_rules! err {
    ($($arg:tt)*) => {
        $crate::error::Error::msg(::std::format!($($arg)*))
    };
}

/// Returns an error from a message, written like `format!`.
#[macro_export]
macro_rules! bail {
    ($($arg:tt)*) => {
        return ::std::result::Result::Err($crate::err!($($arg)*))
    };
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::io;

    fn io_error() -> Result<()> {
        Err(io::Error::new(io::ErrorKind::NotFound, "no such file"))?;
        Ok(())
    }

    #[test]
    fn errors_print_as_anyhow_printed_them() {
        let one = io_error().context("read rules.toml").unwrap_err();
        assert_eq!(one.to_string(), "read rules.toml");
        assert_eq!(format!("{one:#}"), "read rules.toml: no such file");
        let debug = format!("{one:?}");
        assert!(debug.starts_with("read rules.toml\n\nCaused by:\n    no such file"));

        let two = Err::<(), _>(one)
            .with_context(|| format!("check {}", "repo"))
            .unwrap_err();
        assert_eq!(
            format!("{two:#}"),
            "check repo: read rules.toml: no such file"
        );
        let debug = format!("{two:?}");
        assert!(debug
            .starts_with("check repo\n\nCaused by:\n    0: read rules.toml\n    1: no such file"));

        let lines = Err::<(), _>(crate::err!("first\nsecond"))
            .context("outer")
            .unwrap_err();
        assert!(format!("{lines:?}").starts_with("outer\n\nCaused by:\n    first\n    second"));

        let none: Result<u8> = None.context("pass --all or a repo id/name");
        assert_eq!(
            format!("{:#}", none.unwrap_err()),
            "pass --all or a repo id/name"
        );
    }

    #[test]
    fn bail_returns_the_message() {
        fn refuse(n: u8) -> Result<u8> {
            if n > 1 {
                crate::bail!("checks blocked {n} repos");
            }
            Ok(n)
        }
        assert_eq!(refuse(1).unwrap(), 1);
        assert_eq!(refuse(3).unwrap_err().to_string(), "checks blocked 3 repos");
    }
}
