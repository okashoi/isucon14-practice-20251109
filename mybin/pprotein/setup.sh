#!/bin/sh -eu
#
# setup.sh
#   ISUCON競技サーバでpproteinが参照する各種ツールとpprotein-agentを
#   導入し、pproteinサーバからpprof/slowlog/httplogを収集できる状態を整えます。
# 使い方:
#   ./setup.sh install    # 依存ツール導入・バイナリ配置・systemd登録
#   ./setup.sh uninstall  # サービス停止とクリーンアップ
# 環境変数:
#   PPROTEIN_VERSION        (既定: 1.2.4)
#   PPROTEIN_ARCH           (既定: linux_amd64)
#   INSTALL_DIR             (既定: /home/isucon)
#   SERVICE_USER            (既定: root)
#   SERVICE_NAME            (既定: pprotein-agent)
#   BIND_PORT               (既定: 19000)
#   PPROTEIN_HTTPLOG        (既定: /var/log/nginx/access.log)
#   PPROTEIN_SLOWLOG        (既定: /var/log/mysql/mysql-slow.log)
#   PPROTEIN_GIT_REPOSITORY (既定: このリポジトリのルート)
# 参考: https://zenn.dev/team_soda/articles/20231206000000

COMMAND=${1:-install}
PPROTEIN_VERSION=${PPROTEIN_VERSION:-1.2.4}
PPROTEIN_ARCH=${PPROTEIN_ARCH:-linux_amd64}
INSTALL_DIR=${INSTALL_DIR:-/home/isucon}
SERVICE_USER=${SERVICE_USER:-root}
SERVICE_NAME=${SERVICE_NAME:-pprotein-agent}
BIND_PORT=${BIND_PORT:-19000}
PPROTEIN_HTTPLOG=${PPROTEIN_HTTPLOG:-/var/log/nginx/access.log}
PPROTEIN_SLOWLOG=${PPROTEIN_SLOWLOG:-/var/log/mysql/mysql-slow.log}
REPO_ROOT=${PPROTEIN_GIT_REPOSITORY:-$(cd "$(dirname "$0")/../.." && pwd)}
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
DOWNLOAD_URL="https://github.com/kaz/pprotein/releases/download/v${PPROTEIN_VERSION}/pprotein_${PPROTEIN_VERSION}_${PPROTEIN_ARCH}.tar.gz"

if id "${SERVICE_USER}" >/dev/null 2>&1; then
  SERVICE_GROUP=$(id -gn "${SERVICE_USER}")
  SERVICE_OWNER_AVAILABLE=1
else
  SERVICE_GROUP="${SERVICE_USER}"
  SERVICE_OWNER_AVAILABLE=
  echo "[WARN] service user ${SERVICE_USER} が見つかりません。所有権の調整をスキップします" >&2
fi

need_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "[ERROR] $1 が見つかりません" >&2
    exit 1
  fi
}

fetch() {
  url="$1"
  dest="$2"
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$url" -o "$dest"
  else
    need_cmd wget
    wget -q "$url" -O "$dest"
  fi
}

apt_install() {
  if command -v apt-get >/dev/null 2>&1; then
    if [ -z "${APT_UPDATED:-}" ]; then
      sudo apt-get update -y
      export APT_UPDATED=1
    fi
    sudo apt-get install -y "$@"
  else
    echo "[WARN] apt-get が見つからないため $* のインストールをスキップしました" >&2
  fi
}

install_graphviz_tools() {
  if command -v dot >/dev/null 2>&1 && command -v gv >/dev/null 2>&1; then
    return
  fi
  apt_install graphviz gv
}

download_release() {
  tmp=$(mktemp -d)
  (
    set -e
    trap 'rm -rf "$tmp"' EXIT
    archive="${tmp}/pprotein.tar.gz"
    fetch "${DOWNLOAD_URL}" "${archive}"
    tar -xzf "${archive}" -C "${tmp}"
    install_binary "${tmp}/pprotein-agent" "${INSTALL_DIR}/pprotein-agent"
    if [ -f "${tmp}/pprotein" ]; then
      install_binary "${tmp}/pprotein" "${INSTALL_DIR}/pprotein"
    fi
  )
}

ensure_permissions() {
  need_cmd setfacl
  if [ -f "${PPROTEIN_SLOWLOG}" ]; then
    sudo setfacl -m "u:${SERVICE_USER}:r" "${PPROTEIN_SLOWLOG}"
    slowlog_dir=$(dirname "${PPROTEIN_SLOWLOG}")
    sudo setfacl -m "u:${SERVICE_USER}:rx" "${slowlog_dir}"
  fi
  if [ -f "${PPROTEIN_HTTPLOG}" ]; then
    sudo setfacl -m "u:${SERVICE_USER}:r" "${PPROTEIN_HTTPLOG}"
    httplog_dir=$(dirname "${PPROTEIN_HTTPLOG}")
    sudo setfacl -m "u:${SERVICE_USER}:rx" "${httplog_dir}"
  fi
}

install_binary() {
  src="$1"
  dest="$2"
  if [ -n "${SERVICE_OWNER_AVAILABLE:-}" ]; then
    sudo install -m 0755 -o "${SERVICE_USER}" -g "${SERVICE_GROUP}" "${src}" "${dest}"
  else
    sudo install -m 0755 "${src}" "${dest}"
  fi
}

prepare_install_dir() {
  sudo mkdir -p "${INSTALL_DIR}"
  if [ -n "${SERVICE_OWNER_AVAILABLE:-}" ]; then
    sudo chown "${SERVICE_USER}:${SERVICE_GROUP}" "${INSTALL_DIR}"
  fi
  sudo chmod 0755 "${INSTALL_DIR}"
}

write_service() {
  sudo tee "${SERVICE_FILE}" >/dev/null <<EOF_SERVICE
[Unit]
Description=pprotein agent service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment=PORT=${BIND_PORT}
Environment=PPROTEIN_HTTPLOG=${PPROTEIN_HTTPLOG}
Environment=PPROTEIN_SLOWLOG=${PPROTEIN_SLOWLOG}
Environment=PPROTEIN_GIT_REPOSITORY=${REPO_ROOT}
ExecStart=${INSTALL_DIR}/pprotein-agent
WorkingDirectory=${INSTALL_DIR}
Restart=always
RestartSec=2s
User=${SERVICE_USER}

[Install]
WantedBy=multi-user.target
EOF_SERVICE
}

start_service() {
  sudo systemctl daemon-reload
  sudo systemctl enable "${SERVICE_NAME}"
  sudo systemctl restart "${SERVICE_NAME}"
}

stop_service() {
  if systemctl list-unit-files | grep -q "^${SERVICE_NAME}.service"; then
    sudo systemctl stop "${SERVICE_NAME}" || true
    sudo systemctl disable "${SERVICE_NAME}" || true
  fi
}

remove_files() {
  sudo rm -f "${SERVICE_FILE}"
  sudo systemctl daemon-reload
  sudo rm -f "${INSTALL_DIR}/pprotein-agent"
  sudo rm -f "${INSTALL_DIR}/pprotein"
}

install_dependencies() {
  need_cmd tar
  need_cmd sudo
  install_graphviz_tools
  if ! command -v setfacl >/dev/null 2>&1; then
    apt_install acl
  fi
}

case "${COMMAND}" in
  install)
    if ! command -v curl >/dev/null 2>&1 && ! command -v wget >/dev/null 2>&1; then
      echo "[ERROR] curl または wget を用意してください" >&2
      exit 1
    fi
    prepare_install_dir
    install_dependencies
    download_release
    ensure_permissions
    write_service
    start_service
    ;;
  uninstall)
    stop_service
    remove_files
    ;;
  *)
    echo "使い方: $0 {install|uninstall}" >&2
    exit 1
    ;;
 esac
