# rustTailscaleApp

基于官方纯 Rust 实验库 [`tailscale` (`tailscale-rs`)](https://github.com/tailscale/tailscale-rs) 实现的无 TUN 多用户端口转发代理工具。

## 功能特性

- **纯 Rust 用户态实现**：使用 `tailscale 0.7`（内部基于 `smoltcp` 用户态协议栈），无需 TUN 网卡与 root 权限。
- **纯配置文件运行**：读取同目录 `config.json`（或通过 `-config` 指定路径），支持固定统一 `hostname`、逗号分隔 `ports`、默认目标 `127.0.0.1:<port>`。
- **多用户并发与 State 隔离**：`users` 中每个用户运行独立 `tailscale::Device`，私钥与节点状态隔离存储于 `<state_dir>/<user.name>/keys.json`。

## 已知限制（源自 `tailscale-rs` 0.7）

- `tailscale-rs` 目前处于实验阶段（构建需开启 `.cargo/config.toml` 中的 `TS_RS_EXPERIMENT=this_is_unstable_software`），官方标注尚未经过第三方安全审计；`0.7.1` 已内置基础 STUN / Disco / `PathDiscoverer` UDP 打洞直连与 DERP 中继回退，但穿透策略与稳定性仍在演进中。
- 需显式指定端口（如 `"445,80"`），不支持 Go 版 `tsnet` 的 `port: 0` 全端口通配转发。

## 构建与运行

```bash
cargo test --offline
cargo build --release --offline
./target/release/rust_tailscale_app
```
