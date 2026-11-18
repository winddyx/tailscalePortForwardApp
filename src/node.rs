use std::net::SocketAddr;
use std::sync::Mutex;
use std::time::Duration;

use tailscale::{AuthState, Config, Device};
use tokio::sync::watch;

use crate::config::ResolvedUser;
use crate::forwarder::serve_listener;

static BANNER_LOCK: Mutex<()> = Mutex::new(());

pub async fn run_user_node(
    hostname: String,
    user: ResolvedUser,
    mut shutdown: watch::Receiver<bool>,
) -> Result<(), String> {
    tokio::fs::create_dir_all(&user.state_dir)
        .await
        .map_err(|e| format!("create state dir {}: {e}", user.state_dir.display()))?;

    eprintln!(
        "[user:{}] State directory: {}",
        user.name,
        user.state_dir.display()
    );
    eprintln!(
        "[user:{}] Starting Tailscale node '{}'...",
        user.name, hostname
    );

    let mut ts_cfg = Config::default_with_key_file(&user.key_file)
        .await
        .map_err(|e| format!("load key file {}: {e}", user.key_file.display()))?;
    ts_cfg.requested_hostname = Some(hostname.clone());

    if let Some(ref ctrl_url) = user.control_url {
        ts_cfg.control_server_url = url::Url::parse(ctrl_url)
            .map_err(|e| format!("invalid control_url {ctrl_url:?}: {e}"))?;
    }

    let dev = Device::new(&ts_cfg, user.auth_key.clone())
        .await
        .map_err(|e| format!("Device::new failed: {e}"))?;

    if !wait_for_auth(&user.name, &dev, &mut shutdown).await? {
        eprintln!("[user:{}] Shutdown before authentication completed", user.name);
        let _ = dev.shutdown(Some(Duration::from_secs(5))).await;
        return Ok(());
    }

    let ipv4 = dev
        .ipv4_addr()
        .await
        .map_err(|e| format!("failed to get Tailscale IPv4 address: {e}"))?;
    let ipv6 = dev.ipv6_addr().await.ok();

    match ipv6 {
        Some(v6) => eprintln!(
            "[user:{}] Online: node={} ipv4={} ipv6={}",
            user.name, hostname, ipv4, v6
        ),
        None => eprintln!(
            "[user:{}] Online: node={} ipv4={}",
            user.name, hostname, ipv4
        ),
    }

    let mut listener_handles = Vec::with_capacity(user.forwards.len());
    for rule in &user.forwards {
        let listen_addr = SocketAddr::from((ipv4, rule.port));
        let listener = dev
            .tcp_listen(listen_addr)
            .await
            .map_err(|e| format!("tcp_listen({listen_addr}) failed: {e}"))?;

        let handle = tokio::spawn(serve_listener(
            user.name.clone(),
            rule.port,
            listener,
            rule.target.clone(),
            shutdown.clone(),
        ));
        listener_handles.push(handle);
    }

    while !*shutdown.borrow() {
        if shutdown.changed().await.is_err() {
            break;
        }
    }

    for handle in listener_handles {
        let _ = handle.await;
    }

    eprintln!("[user:{}] Shutting down node...", user.name);
    let _ = dev.shutdown(Some(Duration::from_secs(5))).await;
    Ok(())
}

async fn wait_for_auth(
    user_name: &str,
    dev: &Device,
    shutdown: &mut watch::Receiver<bool>,
) -> Result<bool, String> {
    let mut last_url: Option<String> = None;

    loop {
        if *shutdown.borrow() {
            return Ok(false);
        }

        let auth_res = tokio::select! {
            _ = shutdown.changed() => {
                if *shutdown.borrow() {
                    return Ok(false);
                }
                continue;
            }
            res = dev.is_authorized() => res,
        };

        match auth_res.map_err(|e| format!("is_authorized failed: {e}"))? {
            AuthState::Authorized => return Ok(true),
            AuthState::NotAuthorized(url) => {
                let url_str = url.to_string();
                if last_url.as_deref() != Some(&url_str) {
                    print_auth_banner(user_name, &url_str);
                    last_url = Some(url_str);
                }
            }
        }

        tokio::select! {
            _ = shutdown.changed() => {
                if *shutdown.borrow() {
                    return Ok(false);
                }
            }
            _ = tokio::time::sleep(Duration::from_secs(2)) => {}
        }
    }
}

fn print_auth_banner(user_name: &str, auth_url: &str) {
    let _guard = BANNER_LOCK.lock().unwrap_or_else(|e| e.into_inner());
    println!();
    println!("================================================================================");
    println!("[user:{user_name}] To authenticate this device, please visit:");
    println!("{auth_url}");
    println!("================================================================================");
    println!();
}
