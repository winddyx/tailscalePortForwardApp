use std::collections::HashSet;
use std::fs;
use std::path::{Path, PathBuf};

use serde::Deserialize;

use crate::forwarder::normalize_target;

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ForwardRule {
    pub port: u16,
    pub target: String,
}

#[derive(Debug, Clone, Default, Deserialize)]
#[serde(untagged)]
pub enum PortSpec {
    #[default]
    Empty,
    Number(u16),
    Text(String),
}

impl PortSpec {
    fn as_raw_str(&self) -> Option<String> {
        match self {
            PortSpec::Empty => None,
            PortSpec::Number(n) => Some(n.to_string()),
            PortSpec::Text(s) => {
                let trimmed = s.trim();
                if trimmed.is_empty() {
                    None
                } else {
                    Some(trimmed.to_string())
                }
            }
        }
    }
}

#[derive(Debug, Clone, Deserialize)]
pub struct UserConfig {
    pub name: String,
    #[serde(default)]
    pub auth_key: String,
    #[serde(default)]
    pub control_url: String,
    #[serde(default)]
    pub ports: PortSpec,
    #[serde(default)]
    pub target: String,
}

#[derive(Debug, Clone, Deserialize)]
pub struct RawConfig {
    #[serde(default)]
    pub hostname: String,
    #[serde(default)]
    pub state_dir: String,
    #[serde(default)]
    pub control_url: String,
    #[serde(default)]
    pub ports: PortSpec,
    #[serde(default)]
    pub target: String,
    #[serde(default)]
    pub users: Vec<UserConfig>,
}

#[derive(Debug, Clone)]
pub struct ResolvedUser {
    pub name: String,
    pub auth_key: Option<String>,
    pub control_url: Option<String>,
    pub state_dir: PathBuf,
    pub key_file: PathBuf,
    pub forwards: Vec<ForwardRule>,
}

#[derive(Debug, Clone)]
pub struct ResolvedConfig {
    pub hostname: String,
    pub state_root: PathBuf,
    pub users: Vec<ResolvedUser>,
}

pub fn load_config(path: &Path, portable_dir: &Path) -> Result<ResolvedConfig, String> {
    let data = fs::read_to_string(path)
        .map_err(|e| format!("read config file {}: {e}", path.display()))?;
    let raw: RawConfig = serde_json::from_str(&data)
        .map_err(|e| format!("parse config file {}: {e}", path.display()))?;
    resolve_config(raw, portable_dir)
}

pub fn resolve_config(raw: RawConfig, portable_dir: &Path) -> Result<ResolvedConfig, String> {
    let hostname = {
        let trimmed = raw.hostname.trim();
        if trimmed.is_empty() {
            "gtsa".to_string()
        } else {
            trimmed.to_string()
        }
    };

    let state_root = {
        let trimmed = raw.state_dir.trim();
        if trimmed.is_empty() {
            portable_dir.join("state")
        } else {
            let p = PathBuf::from(trimmed);
            if p.is_absolute() {
                p
            } else {
                portable_dir.join(p)
            }
        }
    };

    let global_forwards = match raw.ports.as_raw_str() {
        Some(spec) => {
            let ports = parse_ports(&spec).map_err(|e| format!("global ports: {e}"))?;
            Some(build_forward_rules(&ports, &raw.target))
        }
        None => None,
    };

    if raw.users.is_empty() {
        return Err("users list cannot be empty".to_string());
    }

    let mut seen_users = HashSet::with_capacity(raw.users.len());
    let mut resolved_users = Vec::with_capacity(raw.users.len());

    for (idx, u) in raw.users.iter().enumerate() {
        let name = u.name.trim().to_string();
        validate_user_name(&name).map_err(|e| format!("users[{idx}].name: {e}"))?;

        let lower_name = name.to_lowercase();
        if !seen_users.insert(lower_name) {
            return Err(format!("duplicate user name {name:?} (case-insensitive)"));
        }

        let forwards = match u.ports.as_raw_str() {
            Some(spec) => {
                let ports =
                    parse_ports(&spec).map_err(|e| format!("users[{idx}] ({name}) ports: {e}"))?;
                let target = if u.target.trim().is_empty() {
                    &raw.target
                } else {
                    &u.target
                };
                build_forward_rules(&ports, target)
            }
            None if !u.target.trim().is_empty() => {
                let base = global_forwards
                    .as_ref()
                    .ok_or_else(|| format!("users[{idx}] ({name}): no ports configured"))?;
                let ports: Vec<u16> = base.iter().map(|f| f.port).collect();
                build_forward_rules(&ports, &u.target)
            }
            None => global_forwards.clone().ok_or_else(|| {
                format!(
                    "users[{idx}] ({name}): no ports configured (specify ports like \"445,80\")"
                )
            })?,
        };

        let control_url = {
            let u_url = u.control_url.trim();
            if !u_url.is_empty() {
                Some(u_url.to_string())
            } else {
                let g_url = raw.control_url.trim();
                if g_url.is_empty() {
                    None
                } else {
                    Some(g_url.to_string())
                }
            }
        };

        let auth_key = {
            let trimmed = u.auth_key.trim();
            if trimmed.is_empty() {
                None
            } else {
                Some(trimmed.to_string())
            }
        };

        let user_state_dir = state_root.join(&name);
        let key_file = user_state_dir.join("keys.json");

        resolved_users.push(ResolvedUser {
            name,
            auth_key,
            control_url,
            state_dir: user_state_dir,
            key_file,
            forwards,
        });
    }

    Ok(ResolvedConfig {
        hostname,
        state_root,
        users: resolved_users,
    })
}

fn validate_user_name(name: &str) -> Result<(), String> {
    if name.is_empty() {
        return Err("must not be empty".to_string());
    }
    if name == "." || name == ".." || name.starts_with('.') {
        return Err(format!("invalid name {name:?}: must not start with '.'"));
    }
    for ch in name.chars() {
        let valid = ch.is_ascii_alphanumeric() || ch == '-' || ch == '_' || ch == '.';
        if !valid {
            return Err(format!(
                "invalid character {ch:?} in {name:?} (allowed: a-z, A-Z, 0-9, '-', '_', '.')"
            ));
        }
    }
    Ok(())
}

pub fn parse_ports(spec: &str) -> Result<Vec<u16>, String> {
    let trimmed = spec.trim();
    if trimmed.is_empty() {
        return Err("ports must not be empty".to_string());
    }

    let mut ports = Vec::new();
    let mut seen = HashSet::new();

    for part in trimmed.split(',') {
        let token = part.trim();
        if token.is_empty() {
            return Err(format!("empty port entry in {spec:?}"));
        }
        let port: u16 = token
            .parse()
            .map_err(|_| format!("port {token:?} must be an integer between 1 and 65535"))?;
        if port == 0 {
            return Err(
                "port 0 (wildcard) is not supported by tailscale-rs; specify explicit ports like \"445,80\""
                    .to_string(),
            );
        }
        if seen.insert(port) {
            ports.push(port);
        }
    }

    Ok(ports)
}

fn build_forward_rules(ports: &[u16], raw_target: &str) -> Vec<ForwardRule> {
    ports
        .iter()
        .map(|&port| ForwardRule {
            port,
            target: normalize_target(raw_target, port),
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_multi_user_config_and_state_isolation() {
        let dir = Path::new("/tmp/gtsa-rust-test");
        let raw_json = r#"{
            "hostname": "my-nas",
            "state_dir": "custom-state",
            "control_url": "https://headscale.example.com",
            "ports": "445, 80, 443",
            "users": [
                {"name": "alice", "auth_key": "tskey-auth-alice"},
                {"name": "bob", "ports": 8080, "target": "192.168.1.10"}
            ]
        }"#;
        let raw: RawConfig = serde_json::from_str(raw_json).unwrap();
        let resolved = resolve_config(raw, dir).unwrap();

        assert_eq!(resolved.hostname, "my-nas");
        assert_eq!(resolved.state_root, dir.join("custom-state"));
        assert_eq!(resolved.users.len(), 2);

        let alice = &resolved.users[0];
        assert_eq!(alice.state_dir, dir.join("custom-state").join("alice"));
        assert_eq!(alice.key_file, dir.join("custom-state").join("alice").join("keys.json"));
        assert_eq!(alice.forwards.len(), 3);
        assert_eq!(alice.forwards[0].target, "127.0.0.1:445");

        let bob = &resolved.users[1];
        assert_eq!(bob.state_dir, dir.join("custom-state").join("bob"));
        assert_eq!(bob.forwards.len(), 1);
        assert_eq!(bob.forwards[0].port, 8080);
        assert_eq!(bob.forwards[0].target, "192.168.1.10:8080");
    }

    #[test]
    fn test_validation_errors() {
        let dir = Path::new("/tmp/gtsa-rust-test");
        let bad_cases = [
            r#"{"hostname":"nas","ports":"445","users":[]}"#,
            r#"{"hostname":"nas","ports":"445","users":[{"name":"Alice"},{"name":"alice"}]}"#,
            r#"{"hostname":"nas","ports":"445","users":[{"name":"../evil"}]}"#,
            r#"{"hostname":"nas","users":[{"name":"u1"}]}"#,
            r#"{"hostname":"nas","ports":"0","users":[{"name":"u1"}]}"#,
            r#"{"hostname":"nas","ports":"445,","users":[{"name":"u1"}]}"#,
        ];

        for case in bad_cases {
            let raw: RawConfig = serde_json::from_str(case).unwrap();
            assert!(resolve_config(raw, dir).is_err(), "expected error for {case}");
        }
    }
}
