# gtsa

基于 Tailscale `tsnet` 的无 TUN 多用户端口转发代理工具。

作为纯用户态嵌入式 Tailscale 节点运行，不创建 TUN 虚拟网卡，不接管宿主机路由或系统网络堆栈，仅获取 Tailscale IP 并将指定端口流量转发至本机或局域网目标服务（例如 NAS 的 SMB/HTTP/HTTPS 服务）。支持在单进程内配置多个不同用户的 Tailscale 认证（`users`），让多个不同用户的 Tailscale 网络同时访问同一台设备。

## 功能特性

- **纯用户态无 TUN**：基于 Tailscale 官方 `tsnet` 模块，无需 root 权限与虚拟网卡，仅获取 Tailscale IP 并转发 TCP 端口。
- **纯配置文件运行**：完全由 `config.json` 驱动，程序只读配置，不在运行时回写修改。
- **多用户并发接入**：在 `users` 中配置多个用户的 `auth_key` 或交互式登录，所有用户并发上线，互不阻塞。
- **统一固定 Hostname**：配置文件中全局指定固定 `hostname`，所有用户节点共用同一名称（因处于不同用户的 tailnet 中，不会冲突）。
- **State 目录隔离**：每个用户节点使用独立的状态子目录（`<state_dir>/<user.name>/`），凭据互不覆盖。
- **Portable 便携包**：可执行文件、`config.json` 与 `state/` 目录均自包含在同一目录中。

## 工程目录结构

```text
.
├── config.json             # 默认配置文件模板
├── src/
│   ├── main.go             # 入口、便携目录解析与多节点生命周期调度
│   ├── config.go           # config.json 解析、校验与 state 隔离路径生成
│   ├── node.go             # 单用户 tsnet 节点启动、认证与 Tailscale IP 获取
│   ├── forwarder.go        # TCP 端口监听与全端口转发代理实现
│   └── main_test.go        # 单元测试
├── Makefile                # 构建与发布入口（make test / make build / make dist）
├── AGENTS.md               # 工作区执行规范
└── dist/                   # 发行构建产物目录（含各平台二进制、config.json 与 SHA256SUMS）
```

## 便携包运行目录布局

```text
my-nas-gtsa/
├── gtsa (或 gtsa-linux-amd64)
├── config.json
└── state/
    ├── user1/
    │   └── tailscaled.state
    └── user2/
        └── tailscaled.state
```

## 配置文件说明（`config.json`）

```json
{
  "hostname": "nas",
  "state_dir": "state",
  "control_url": "",
  "verbose": false,
  "ports": "445,80",
  "target": "",
  "users": [
    {
      "name": "user1",
      "auth_key": "tskey-auth-xxxxxxxxxxxx"
    },
    {
      "name": "user2",
      "auth_key": "",
      "control_url": ""
    }
  ]
}
```

### 字段说明

- `hostname`：所有用户节点共用的 Tailscale 设备名（默认 `gtsa`）。
- `state_dir`：状态根目录，相对路径基于可执行文件所在目录解析（默认 `state`）。
- `control_url`：全局默认控制服务器地址（留空使用官方 Tailscale，可填写 Headscale 地址）。
- `verbose`：是否输出详细的 Tailscale 底层调试日志。
- `ports`：全局默认转发端口（必填，支持数字或逗号分隔字符串，范围 `1-65535`，如 `445`、`"445,80,443"`）。
- `target`：转发目标地址（可选，默认 `127.0.0.1`；留空或仅填主机 IP 时，各端口自动映射到 `<target>:<port>`）。
- `users`：多用户账号列表（至少 1 项）：
  - `name`：本地唯一标识（必填，仅允许 `a-zA-Z0-9._-` 且不能以 `.` 开头），用于日志前缀与 `state/<name>/` 目录隔离。
  - `auth_key`：预授权密钥；留空时启动后终端会打印带 `[user:<name>]` 前缀的网页授权链接。
  - `control_url`：可选，覆盖全局 `control_url`。
  - `ports` / `target`：可选，覆盖全局 `ports` / `target`。

## 运行方式

将可执行文件与 `config.json` 放在同一目录后直接启动：

```bash
./gtsa
```

或显式指定配置文件路径：

```bash
./gtsa -config /path/to/config.json
```

## 构建与测试

```bash
# 执行单元测试
make test

# 本地编译
make build

# 全量多平台发布打包（先清理 dist/，生成二进制、config.json、gtsa.service、compose.yml 与 SHA256SUMS）
make dist
```

### systemd 守护进程部署（Linux）

`make dist` 会在 `dist/` 目录生成通用 `gtsa.service` 模板（默认工作目录为 `/opt/gtsa`），部署时将对应架构二进制重命名为 `gtsa` 并与 `config.json` 一同放入 `/opt/gtsa` 后启用即可：

```bash
sudo mkdir -p /opt/gtsa
sudo cp gtsa-linux-amd64 /opt/gtsa/gtsa
sudo cp config.json /opt/gtsa/config.json
sudo cp gtsa.service /etc/systemd/system/gtsa.service
sudo systemctl daemon-reload
sudo systemctl enable --now gtsa
```
