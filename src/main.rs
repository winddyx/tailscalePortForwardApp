mod config;
mod forwarder;
mod node;

use std::path::{Path, PathBuf};
use std::process;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};

use tokio::sync::watch;

use crate::config::load_config;
use crate::node::run_user_node;

#[tokio::main]
async fn main() {
    let config_arg = match parse_config_arg() {
        Ok(arg) => arg,
        Err(msg) => {
            eprintln!("[FATAL] {msg}");
            process::exit(2);
        }
    };

    let portable_dir = resolve_portable_dir();
    let config_path = match config_arg {
        Some(p) => {
            let pb = PathBuf::from(p);
            if pb.is_absolute() {
                pb
            } else {
                std::env::current_dir()
                    .unwrap_or_else(|_| portable_dir.clone())
                    .join(pb)
            }
        }
        None => portable_dir.join("config.json"),
    };

    let cfg = match load_config(&config_path, &portable_dir) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("[FATAL] invalid configuration: {e}");
            process::exit(1);
        }
    };

    if let Err(e) = std::fs::create_dir_all(&cfg.state_root) {
        eprintln!(
            "[FATAL] failed to create state root {}: {e}",
            cfg.state_root.display()
        );
        process::exit(1);
    }

    eprintln!("[INFO] Portable directory: {}", portable_dir.display());
    eprintln!("[INFO] Config file: {}", config_path.display());
    eprintln!("[INFO] State root: {}", cfg.state_root.display());
    eprintln!("[INFO] Hostname: {}", cfg.hostname);
    eprintln!("[INFO] Configured users: {}", cfg.users.len());

    let (shutdown_tx, shutdown_rx) = watch::channel(false);
    tokio::spawn(async move {
        wait_for_shutdown_signal().await;
        eprintln!("[INFO] Shutdown signal received, stopping all Tailscale nodes...");
        let _ = shutdown_tx.send(true);
    });

    let total_users = cfg.users.len();
    let err_count = Arc::new(AtomicUsize::new(0));
    let mut handles = Vec::with_capacity(total_users);

    for user in cfg.users {
        let hostname = cfg.hostname.clone();
        let rx = shutdown_rx.clone();
        let errs = Arc::clone(&err_count);
        let user_name = user.name.clone();

        let handle = tokio::spawn(async move {
            if let Err(e) = run_user_node(hostname, user, rx).await {
                errs.fetch_add(1, Ordering::Relaxed);
                eprintln!("[user:{user_name}] [ERROR] {e}");
            }
        });
        handles.push(handle);
    }

    for handle in handles {
        let _ = handle.await;
    }

    if !*shutdown_rx.borrow() && err_count.load(Ordering::Relaxed) == total_users {
        eprintln!("[FATAL] all {total_users} Tailscale node(s) failed");
        process::exit(1);
    }
    eprintln!("[INFO] All Tailscale nodes stopped");
}

fn parse_config_arg() -> Result<Option<String>, String> {
    let mut args = std::env::args().skip(1);
    let mut config_path = None;

    while let Some(arg) = args.next() {
        match arg.as_str() {
            "-config" | "--config" | "-c" => {
                let val = args
                    .next()
                    .ok_or_else(|| format!("missing path after {arg}"))?;
                config_path = Some(val);
            }
            "-h" | "--help" => {
                eprintln!("Usage: rust_tailscale_app [-config path/to/config.json]");
                process::exit(0);
            }
            other => {
                return Err(format!("unknown argument: {other}"));
            }
        }
    }

    Ok(config_path)
}

fn resolve_portable_dir() -> PathBuf {
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            if !is_dev_or_temp_dir(dir) {
                return dir.to_path_buf();
            }
        }
    }
    std::env::current_dir().unwrap_or_else(|_| PathBuf::from("."))
}

fn is_dev_or_temp_dir(path: &Path) -> bool {
    let s = path.to_string_lossy();
    s.contains("/target/") || s.ends_with("/target") || s.contains(".tmp") || s.starts_with("/tmp")
}

async fn wait_for_shutdown_signal() {
    #[cfg(unix)]
    {
        use tokio::signal::unix::{SignalKind, signal};
        let mut sigterm = signal(SignalKind::terminate()).ok();
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {}
            _ = async {
                if let Some(ref mut s) = sigterm {
                    s.recv().await;
                } else {
                    std::future::pending::<()>().await;
                }
            } => {}
        }
    }
    #[cfg(not(unix))]
    {
        let _ = tokio::signal::ctrl_c().await;
    }
}
