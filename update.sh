#!/usr/bin/env bash
set -euo pipefail

# 可调配置（均可通过环境变量覆盖）
REPO="${REPO:-winddyx/tailscalePortForwardApp}"
APP_DIR="${1:-${APP_DIR:-$(cd "$(dirname "$0")" && pwd)}}"
BIN_NAME="${BIN_NAME:-}"
MODE="${MODE:-auto}"                  # auto | systemd | compose | none
SERVICE_NAME="${SERVICE_NAME:-tspfa}"
GITHUB_TOKEN="${GITHUB_TOKEN:-}"
GITHUB_PROXY="${GITHUB_PROXY:-}"      # 可选下载代理前缀，例如 https://gh-proxy.com/
FORCE="${FORCE:-0}"                   # 设为 1 强制重新下载并替换

export PATH="/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:${PATH:-}"

log()  { printf '[INFO] %s\n' "$*"; }
warn() { printf '[WARN] %s\n' "$*" >&2; }
die()  { printf '[ERROR] %s\n' "$*" >&2; exit 1; }

need_cmd() {
  command -v "$1" >/dev/null 2>&1
}

# 校验 MODE 配置
case "$MODE" in
  auto|systemd|compose|none) ;;
  *) die "未知的 MODE 配置: ${MODE}（可选值: auto, systemd, compose, none）" ;;
esac

# 检测系统与架构
OS="$(uname -s)"
if [ "$OS" != "Linux" ]; then
  die "本脚本仅支持 Linux（当前系统: $OS）"
fi

ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in
  x86_64|amd64)   ARCH="amd64" ;;
  aarch64|arm64)  ARCH="arm64" ;;
  *) die "不支持的 CPU 架构: $ARCH_RAW" ;;
esac

ASSET_NAME="tspfa-linux-${ARCH}"

# 检查目标目录与写权限
[ -d "$APP_DIR" ] || die "目标目录不存在: $APP_DIR"
APP_DIR="$(cd "$APP_DIR" && pwd)"
[ -w "$APP_DIR" ] || die "对目标目录无写权限: ${APP_DIR}，请使用 sudo 运行"

# 确定本地目标二进制文件名
if [ -n "$BIN_NAME" ]; then
  TARGET_BIN="${APP_DIR}/${BIN_NAME}"
elif [ -f "${APP_DIR}/tspfa" ]; then
  TARGET_BIN="${APP_DIR}/tspfa"
elif [ -f "${APP_DIR}/${ASSET_NAME}" ]; then
  TARGET_BIN="${APP_DIR}/${ASSET_NAME}"
else
  TARGET_BIN="${APP_DIR}/tspfa"
fi

VERSION_FILE="${APP_DIR}/.version"

# 计算文件 SHA256
file_sha256() {
  if need_cmd sha256sum; then
    sha256sum "$1" | awk '{print $1}'
  elif need_cmd shasum; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    die "缺少 sha256sum 或 shasum 命令"
  fi
}

# HTTP 请求（输出到 stdout）
http_get_stdout() {
  local url="$1"
  if need_cmd curl; then
    if [ -n "$GITHUB_TOKEN" ]; then
      curl -fsSL -H "Accept: application/vnd.github+json" -H "Authorization: Bearer ${GITHUB_TOKEN}" "$url"
    else
      curl -fsSL -H "Accept: application/vnd.github+json" "$url"
    fi
  elif need_cmd wget; then
    if [ -n "$GITHUB_TOKEN" ]; then
      wget -qO- --header="Accept: application/vnd.github+json" --header="Authorization: Bearer ${GITHUB_TOKEN}" "$url"
    else
      wget -qO- --header="Accept: application/vnd.github+json" "$url"
    fi
  else
    die "缺少 curl 或 wget 命令"
  fi
}

# HTTP 下载到指定文件（不携带 GITHUB_TOKEN，避免泄露给第三方代理或被 302 对象存储拒绝）
http_download() {
  local url="$1"
  local out="$2"
  if need_cmd curl; then
    curl -fL --progress-bar -o "$out" "$url"
  elif need_cmd wget; then
    wget -q -O "$out" "$url"
  else
    die "缺少 curl 或 wget 命令"
  fi
}

# 拼接下载链接（支持可选代理前缀）
asset_url() {
  local tag="$1"
  local file="$2"
  local raw="https://github.com/${REPO}/releases/download/${tag}/${file}"
  if [ -n "$GITHUB_PROXY" ]; then
    printf '%s%s' "${GITHUB_PROXY%/}/" "$raw"
  else
    printf '%s' "$raw"
  fi
}

# 自动识别运行模式（compose 优先于 systemd）
detect_mode() {
  if [ "$MODE" != "auto" ]; then
    printf '%s' "$MODE"
    return
  fi

  if [ -f "${APP_DIR}/compose.yml" ] || [ -f "${APP_DIR}/compose.yaml" ] || \
     [ -f "${APP_DIR}/docker-compose.yml" ] || [ -f "${APP_DIR}/docker-compose.yaml" ]; then
    printf 'compose'
    return
  fi

  if need_cmd systemctl; then
    if [ -f "/etc/systemd/system/${SERVICE_NAME}.service" ] || \
       [ -f "/lib/systemd/system/${SERVICE_NAME}.service" ] || \
       [ -f "/usr/lib/systemd/system/${SERVICE_NAME}.service" ]; then
      printf 'systemd'
      return
    fi
  fi

  printf 'none'
}

# 提权封装
run_priv() {
  if [ "$(id -u)" -eq 0 ]; then
    "$@"
  elif need_cmd sudo; then
    sudo "$@"
  else
    die "需要 root 权限执行: $*"
  fi
}

# 1. 查询最新 GitHub Release 版本
API_URL="https://api.github.com/repos/${REPO}/releases/latest"
log "正在检查最新版本: ${REPO} (${ASSET_NAME})"
RELEASE_JSON="$(http_get_stdout "$API_URL")" || die "获取 GitHub Release 信息失败"

LATEST_TAG="$(printf '%s\n' "$RELEASE_JSON" | grep -m1 '"tag_name"' | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"
[ -n "$LATEST_TAG" ] || die "无法从 GitHub API 响应中解析 tag_name"

CURRENT_TAG=""
if [ -f "$VERSION_FILE" ]; then
  CURRENT_TAG="$(tr -d '[:space:]' < "$VERSION_FILE")"
fi

log "当前记录版本: ${CURRENT_TAG:-未知}"
log "远端最新版本: ${LATEST_TAG}"

if [ "$FORCE" != "1" ] && [ -n "$CURRENT_TAG" ] && [ "$CURRENT_TAG" = "$LATEST_TAG" ] && [ -f "$TARGET_BIN" ]; then
  log "已是最新版本 (${LATEST_TAG})，无需更新。"
  exit 0
fi

# 2. 创建同目录临时工作区，保证退出清理与同分区原子替换
TMP_DIR="$(mktemp -d "${APP_DIR}/.update.tmp.XXXXXX")"
cleanup() {
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT

# 3. 下载 SHA256SUMS 并比对本地二进制
SUMS_FILE="${TMP_DIR}/SHA256SUMS"
log "正在获取校验文件 SHA256SUMS..."
http_download "$(asset_url "$LATEST_TAG" "SHA256SUMS")" "$SUMS_FILE" || die "下载 SHA256SUMS 失败"

EXPECTED_SHA="$(awk -v name="$ASSET_NAME" '$2 == name || $2 == ("*" name) {print $1; exit}' "$SUMS_FILE")"
[ -n "$EXPECTED_SHA" ] || die "SHA256SUMS 中未找到 ${ASSET_NAME} 的校验值"

if [ "$FORCE" != "1" ] && [ -f "$TARGET_BIN" ]; then
  LOCAL_SHA="$(file_sha256 "$TARGET_BIN")"
  if [ "$LOCAL_SHA" = "$EXPECTED_SHA" ]; then
    printf '%s\n' "$LATEST_TAG" > "$VERSION_FILE"
    log "本地文件校验和与最新版本 (${LATEST_TAG}) 一致，无需更新。"
    exit 0
  fi
fi

# 4. 下载并校验新版本二进制
NEW_BIN="${TMP_DIR}/${ASSET_NAME}"
log "正在下载 ${ASSET_NAME} (${LATEST_TAG})..."
http_download "$(asset_url "$LATEST_TAG" "$ASSET_NAME")" "$NEW_BIN" || die "下载 ${ASSET_NAME} 失败"

ACTUAL_SHA="$(file_sha256 "$NEW_BIN")"
if [ "$ACTUAL_SHA" != "$EXPECTED_SHA" ]; then
  die "SHA256 校验失败: 期望 ${EXPECTED_SHA}，实际 ${ACTUAL_SHA}"
fi
log "SHA256 校验通过: ${ACTUAL_SHA}"

# 5. 备份并原子替换本地二进制
chmod 0755 "$NEW_BIN"
if [ -f "$TARGET_BIN" ]; then
  cp -p "$TARGET_BIN" "${TARGET_BIN}.bak"
  log "已备份旧版本至: ${TARGET_BIN}.bak"
fi
mv -f "$NEW_BIN" "$TARGET_BIN"
printf '%s\n' "$LATEST_TAG" > "$VERSION_FILE"
log "已替换本地文件: ${TARGET_BIN} -> ${LATEST_TAG}"

# 6. 按运行模式重启服务
RUN_MODE="$(detect_mode)"
log "检测到运行模式: ${RUN_MODE}"

case "$RUN_MODE" in
  systemd)
    log "正在重启 systemd 服务: ${SERVICE_NAME}"
    run_priv systemctl restart "${SERVICE_NAME}"
    if run_priv systemctl is-active --quiet "${SERVICE_NAME}"; then
      log "服务 ${SERVICE_NAME} 重启成功并处于 active 状态。"
    else
      warn "服务 ${SERVICE_NAME} 未处于 active 状态，请执行 systemctl status ${SERVICE_NAME} 检查。"
      exit 1
    fi
    ;;
  compose)
    log "正在重启 Docker Compose 项目: ${APP_DIR}"
    cd "$APP_DIR"
    if docker info >/dev/null 2>&1; then
      if docker compose version >/dev/null 2>&1; then
        COMPOSE_CMD=(docker compose)
      elif need_cmd docker-compose; then
        COMPOSE_CMD=(docker-compose)
      else
        die "未找到 docker compose 或 docker-compose 命令"
      fi
    elif run_priv docker info >/dev/null 2>&1; then
      if run_priv docker compose version >/dev/null 2>&1; then
        COMPOSE_CMD=(run_priv docker compose)
      elif run_priv docker-compose version >/dev/null 2>&1; then
        COMPOSE_CMD=(run_priv docker-compose)
      else
        die "未找到 docker compose 或 docker-compose 命令"
      fi
    else
      die "无法连接 Docker 守护进程，请确认 Docker 已启动并具备权限"
    fi

    if [ -n "$("${COMPOSE_CMD[@]}" ps -q 2>/dev/null || true)" ]; then
      "${COMPOSE_CMD[@]}" restart
    else
      "${COMPOSE_CMD[@]}" up -d
    fi
    log "Docker Compose 项目重启完成。"
    ;;
  none)
    log "未检测到 systemd 服务或 Compose 配置文件，跳过自动重启，请手动重启进程。"
    ;;
  *)
    die "未知的 MODE 配置: ${RUN_MODE}（可选值: auto, systemd, compose, none）"
    ;;
esac
