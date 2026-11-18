use std::net::SocketAddr;
use std::time::Duration;

use tailscale::netstack::TcpListener;
use tokio::io::copy_bidirectional;
use tokio::net::TcpStream;
use tokio::sync::watch;

pub fn normalize_target(raw_target: &str, fallback_port: u16) -> String {
    let trimmed = raw_target.trim();
    if trimmed.is_empty() {
        return format!("127.0.0.1:{fallback_port}");
    }
    if let Ok(port_num) = trimmed.parse::<u16>() {
        return format!("127.0.0.1:{port_num}");
    }
    if trimmed.parse::<SocketAddr>().is_ok() {
        return trimmed.to_string();
    }
    if let Some((host, port_str)) = split_host_port(trimmed) {
        if !port_str.is_empty() {
            return format!("{host}:{port_str}");
        }
    }
    let host = trimmed.trim_start_matches('[').trim_end_matches(']');
    if host.contains(':') {
        format!("[{host}]:{fallback_port}")
    } else {
        format!("{host}:{fallback_port}")
    }
}

fn split_host_port(s: &str) -> Option<(&str, &str)> {
    if let Some(rest) = s.strip_prefix('[') {
        let end = rest.find(']')?;
        let host_with_brackets = &s[..end + 2];
        let after = rest[end + 1..].strip_prefix(':')?;
        return Some((host_with_brackets, after));
    }
    let colon = s.rfind(':')?;
    // Ignore raw IPv6 addresses without brackets (e.g. "::1").
    if s[..colon].contains(':') {
        return None;
    }
    Some((&s[..colon], &s[colon + 1..]))
}

pub async fn serve_listener(
    user_name: String,
    listen_port: u16,
    listener: TcpListener,
    target_addr: String,
    mut shutdown: watch::Receiver<bool>,
) {
    eprintln!(
        "[user:{user_name}] Forwarding: [Tailscale]:{listen_port} -> {target_addr}"
    );

    loop {
        tokio::select! {
            _ = shutdown.changed() => {
                if *shutdown.borrow() {
                    break;
                }
            }
            accept_res = listener.accept() => {
                match accept_res {
                    Ok(mut client_stream) => {
                        let user = user_name.clone();
                        let dst = target_addr.clone();
                        tokio::spawn(async move {
                            eprintln!("[user:{user}] [CONN] Accepted connection on :{listen_port} -> {dst}");
                            let connect_res = tokio::time::timeout(
                                Duration::from_secs(5),
                                TcpStream::connect(&dst),
                            )
                            .await;

                            let mut target_stream = match connect_res {
                                Ok(Ok(stream)) => stream,
                                Ok(Err(e)) => {
                                    eprintln!("[user:{user}] [ERROR] Failed to dial {dst}: {e}");
                                    return;
                                }
                                Err(_) => {
                                    eprintln!("[user:{user}] [ERROR] Dial timeout connecting to {dst}");
                                    return;
                                }
                            };

                            if let Err(e) = copy_bidirectional(&mut client_stream, &mut target_stream).await {
                                eprintln!("[user:{user}] [CONN] Closed with error: {e}");
                            } else {
                                eprintln!("[user:{user}] [CONN] Closed");
                            }
                        });
                    }
                    Err(e) => {
                        if *shutdown.borrow() {
                            break;
                        }
                        eprintln!("[user:{user_name}] [WARN] Accept error on port {listen_port}: {e}");
                        tokio::time::sleep(Duration::from_millis(100)).await;
                    }
                }
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_normalize_target() {
        assert_eq!(normalize_target("", 445), "127.0.0.1:445");
        assert_eq!(normalize_target("8080", 445), "127.0.0.1:8080");
        assert_eq!(normalize_target("192.168.1.10", 445), "192.168.1.10:445");
        assert_eq!(normalize_target("192.168.1.10:8443", 445), "192.168.1.10:8443");
        assert_eq!(normalize_target("[::1]:8080", 445), "[::1]:8080");
        assert_eq!(normalize_target("[::1]", 445), "[::1]:445");
        assert_eq!(normalize_target("::1", 445), "[::1]:445");
        assert_eq!(normalize_target(":9090", 445), ":9090");
    }
}
