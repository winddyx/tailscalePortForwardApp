# pfgots

基于 Tailscale `tsnet` 的无 TUN 端口转发 CLI 工具。

作为独立的嵌入式 Tailscale 节点运行在用户态，无需 root 权限，不创建 TUN 虚拟网卡设备，不影响宿主机的网络堆栈。通过 Tailscale 网络分配的 IP，将该 IP 上指定端口的流量直接转发到本机的目标端口。

## 功能特性

- **纯用户态无 TUN**：使用 Tailscale 官方 `tsnet` 模块，无需配置虚拟网卡设备。
- **Portable 便携包模式**：可执行文件所在目录即为便携根目录（自包含配置与凭证，支持整包拷贝移动）。
- **交互式认证**：首次运行自动打印类似 `tailscale up` 的网页授权登录链接，授权后凭据自动持久化。
- **配置持久化**：支持自动读取与同步便携包内的 `config.json`，方便无参或快捷启动。
- **目标可配置**：默认转发至本机 `127.0.0.1:<port>`，亦支持自定义目标地址。

## 工程目录结构

```text
.
├── pfgots                  # 编译生成的 CLI 二进制文件
├── src/                    # 源码目录
│   ├── main.go             # 核心实现
│   └── main_test.go        # 单元测试
├── Makefile                # 构建与发布入口（支持 make dist 与 make test）
├── AGENTS.md               # 工作区执行规范
└── dist/                   # 发行构建产物目录
```

## 便携包（Portable）运行目录布局

便携包运行时（例如参考 `.protable-test/` 布局）：

```text
my-portable-dir/
├── pfgots (或 pfgots-darwin-arm64)   # 可执行文件
├── config.json                       # 配置文件（自动保存或手动指定）
└── tailscaled/                       # Tailscale 机器凭据与状态目录
    ├── tailscaled.state              # 节点状态与私钥凭证
    └── tailscaled.log.conf           # 日志配置
```

## 使用方法

### 1. 基础端口转发（如 SMB 445）

```bash
./pfgots -p 445
```

首次运行终端将输出类似如下信息：

```text
[INFO] Portable directory: /path/to/my-portable-dir
[INFO] Tailscale state directory: /path/to/my-portable-dir/tailscaled
[INFO] Starting Tailscale node 'pfgots-445'...
[TAILSCALE] LocalBackend state is NeedsLogin; running StartLoginInteractive...

================================================================================
To authenticate this device, please visit:
https://login.tailscale.com/a/xxxxxx
================================================================================
```

在浏览器打开链接完成 Tailscale 账号授权后，节点自动上线：

```text
==================================================
Tailscale Node: pfgots-445.tailscale.net
Tailscale IPv4: 100.x.y.z
Tailscale IPv6: fd7a:115c:xxxx::xxxx
Forwarding:     [Tailscale]:445 -> 127.0.0.1:445
==================================================
[INFO] Listening on Tailscale port 445, ready for connections
```

此后再次启动时，程序将直接复用 `tailscaled/` 中的认证凭证，无需重复授权。

### 2. 命令行参数

```text
Usage: pfgots -p <port> [options]

Options:
  -p, -port <int>
        指定转发端口（例如 445，必填或在 config.json 中配置）
  -target <string>
        本地转发目标地址（默认 127.0.0.1:<port>）
  -name, -hostname <string>
        Tailscale 节点名称（默认 pfgots-<port>）
  -dir <string>
        Portable 目录路径（默认可执行文件所在目录）
  -authkey <string>
        Tailscale 预授权密钥（可选，留空则提示交互式登录链接）
  -control-url <string>
        自定义控制服务器地址（可选，用于 Headscale 私有化部署）
  -verbose
        输出详细的 Tailscale 内部底层日志
```

### 3. 便携包配置文件（config.json）

运行一次后或手动创建同级目录下的 `config.json`：

```json
{
  "port": 445,
  "target": "127.0.0.1:445",
  "hostname": "pfgots-445"
}
```

配置后可直接执行 `./pfgots` 启动。命令行参数优先级高于配置文件。

## 构建与测试

```bash
# 执行单元测试
make test

# 本地编译
make build

# 多平台发布打包并生成校验和
make dist
```
