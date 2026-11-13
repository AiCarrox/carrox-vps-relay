#!/usr/bin/env bash
# install.sh — one-shot bring-up helper for vps-relay on a host that already has
# Nginx 1.28.x and Docker installed.
#
# What it does (idempotent):
#   1. Verifies host prerequisites (nginx 1.28 + required modules, docker)
#   2. Ensures /usr/local/nginx/conf/nginx.conf contains `connection_upgrade` map
#      (fallback: auto-patch only if missing; keeps a timestamped .bak)
#   3. Creates /etc/vps-relay/ directory
#   4. Optionally adds a panel vhost when `--domain` is provided
#   5. Builds the image and runs `docker compose up -d`
#   6. Outputs deployment info (admin key is auto-generated on first start)
#
# Re-run safe.
set -euo pipefail

log() { printf '[install] %s\n' "$*"; }

PANEL_DOMAIN=""
PANEL_SCHEME=http

while [ "$#" -gt 0 ]; do
  case "$1" in
    --domain)
      if [ "$#" -lt 2 ] || [ -z "${2:-}" ]; then
        echo "ERROR: --domain requires a value." >&2
        exit 1
      fi
      PANEL_DOMAIN="$2"
      shift 2
      ;;
    *)
      echo "ERROR: unknown argument: $1" >&2
      echo "usage: bash deploy/install.sh [--domain panel.example.com]" >&2
      exit 1
      ;;
  esac
done

HERE="$(cd "$(dirname "$0")/.." && pwd)"
NGX_CONF=/usr/local/nginx/conf/nginx.conf

require_nginx_feature() {
  local name="$1"
  local pattern="$2"
  local output="$3"
  if ! printf '%s\n' "$output" | grep -Eq -- "$pattern"; then
    echo "ERROR: nginx is missing required feature: $name" >&2
    exit 1
  fi
}

ensure_connection_upgrade_map() {
  if grep -qE 'connection_upgrade' "$NGX_CONF"; then
    log "connection_upgrade map already present"
    return 0
  fi

  log "patching nginx.conf with connection_upgrade map (fallback)"
  local backup="${NGX_CONF}.bak.$(date +%Y%m%d%H%M%S)"
  cp "$NGX_CONF" "$backup"
  python3 "$HERE/deploy/patch-nginx-map.py" "$NGX_CONF"

  if /usr/local/nginx/sbin/nginx -p /usr/local/nginx -c conf/nginx.conf -t >/dev/null 2>&1; then
    /usr/local/nginx/sbin/nginx -s reload
  else
    cp "$backup" "$NGX_CONF"
    echo "ERROR: nginx -t failed after fallback patch attempt; original restored from $backup." >&2
    exit 1
  fi
}

# Managed sites resolve their upstream at request time (variable proxy_pass),
# which needs an http-level `resolver`. Add one only if none is configured.
ensure_resolver() {
  if grep -qE '^[[:space:]]*resolver[[:space:]]' "$NGX_CONF"; then
    log "resolver already present"
    return 0
  fi

  log "patching nginx.conf with resolver (fallback)"
  local backup="${NGX_CONF}.bak.$(date +%Y%m%d%H%M%S)-resolver"
  cp "$NGX_CONF" "$backup"
  python3 "$HERE/deploy/patch-nginx-resolver.py" "$NGX_CONF"

  if /usr/local/nginx/sbin/nginx -p /usr/local/nginx -c conf/nginx.conf -t >/dev/null 2>&1; then
    /usr/local/nginx/sbin/nginx -s reload
  else
    cp "$backup" "$NGX_CONF"
    echo "ERROR: nginx -t failed after resolver patch attempt; original restored from $backup." >&2
    exit 1
  fi
}

# The panel's HTTPS vhost and every managed site are served by nginx on 443.
# If some other process (e.g. xray/Reality) already holds it, nginx cannot bind
# and nothing would work — stop before touching anything. Nginx itself holding
# 443 is fine, which includes a host already set up with a 443 SNI split.
check_port_443() {
  if ! command -v ss >/dev/null 2>&1; then
    log "WARN: 'ss' not found — skipping the port 443 check"
    return 0
  fi
  local listeners others
  listeners=$(ss -H -ltnp 'sport = :443' 2>/dev/null || true)
  [ -n "$listeners" ] || return 0
  others=$(printf '%s\n' "$listeners" | grep -oE '\("[^"]+"' | tr -d '("' | sort -u | grep -vx 'nginx' || true)
  [ -n "$others" ] || return 0

  echo "ERROR: 443 端口已被非 Nginx 进程占用: $(printf '%s' "$others" | tr '\n' ' ')" >&2
  echo "  vps-relay 的面板 HTTPS 和所有站点都需要由 Nginx 监听 443。" >&2
  echo "  不要直接停掉该服务；常见做法是在宿主机做 443 SNI 分流，让 Nginx 接管 443" >&2
  echo "  并按 SNI 转发给它（见 README「与其他服务共用 443」），完成后重跑本脚本。" >&2
  echo "  当前监听情况：" >&2
  printf '%s\n' "$listeners" | sed 's/^/    /' >&2
  exit 1
}

# --- 1. Prereq checks ------------------------------------------------------
check_port_443

if [ ! -x /usr/local/nginx/sbin/nginx ]; then
  echo "ERROR: /usr/local/nginx/sbin/nginx not found — install nginx 1.28.x first." >&2
  exit 1
fi
NGX_VER=$(/usr/local/nginx/sbin/nginx -v 2>&1 | sed 's|.*nginx/||')
log "nginx version: $NGX_VER"

NGX_BUILD_INFO=$(/usr/local/nginx/sbin/nginx -V 2>&1)
require_nginx_feature "http_ssl" '--with-http_ssl_module' "$NGX_BUILD_INFO"
require_nginx_feature "http_v2" '--with-http_v2_module' "$NGX_BUILD_INFO"
require_nginx_feature "http_v3" '--with-http_v3_module' "$NGX_BUILD_INFO"
require_nginx_feature "stream" '--with-stream([[:space:]]|$)' "$NGX_BUILD_INFO"
require_nginx_feature "http_realip" '--with-http_realip_module' "$NGX_BUILD_INFO"

if ! command -v docker >/dev/null 2>&1; then
  echo "ERROR: docker not found." >&2
  exit 1
fi
if ! docker info >/dev/null 2>&1; then
  echo "ERROR: docker daemon not running." >&2
  exit 1
fi
if ! docker compose version >/dev/null 2>&1; then
  echo "ERROR: docker compose not available." >&2
  exit 1
fi

# --- 2. nginx.conf fallback patch ------------------------------------------
ensure_connection_upgrade_map
ensure_resolver

# --- 3. /etc/vps-relay/ ----------------------------------------------------
install -d -m 0750 /etc/vps-relay
install -d -m 0755 /usr/local/nginx/conf/ssl
install -d -m 0755 /usr/local/nginx/conf/conf.d

# --- 4. Optional panel vhost ------------------------------------------------
if [ -n "$PANEL_DOMAIN" ]; then
  PANEL_CONF=/usr/local/nginx/conf/conf.d/relay-panel.conf

  # Conflict check: any *other* top-level conf (what nginx actually includes)
  # naming this exact domain in a server_name directive. The loop simply does
  # not run on an empty conf.d, and an exact-token match keeps
  # my-relay.example.com from colliding with relay.example.com.
  PANEL_DOMAIN_RE="server_name[[:space:]]([^;]*[[:space:]])?${PANEL_DOMAIN//./\\.}[[:space:]]*(;|[[:space:]])"
  for f in /usr/local/nginx/conf/conf.d/*.conf; do
    [ -f "$f" ] || continue
    [ "$f" = "$PANEL_CONF" ] && continue
    if grep -qE "$PANEL_DOMAIN_RE" "$f"; then
      echo "ERROR: panel domain already used by another nginx conf: $PANEL_DOMAIN ($f)" >&2
      exit 1
    fi
  done

  # Keep HTTPS on re-runs: once the panel certificate exists (issued by the
  # panel itself after the admin saves a Cloudflare token), render the TLS vhost.
  PANEL_TMPL="$HERE/templates/panel.conf.tmpl"
  PANEL_SSL_DIR="/usr/local/nginx/conf/ssl/$PANEL_DOMAIN"
  if [ -s "$PANEL_SSL_DIR/fullchain.pem" ] && [ -s "$PANEL_SSL_DIR/key.pem" ]; then
    PANEL_TMPL="$HERE/templates/panel-ssl.conf.tmpl"
    PANEL_SCHEME=https
  fi

  # Render → swap in → nginx -t; on failure put the previous conf back.
  PANEL_BAK="${PANEL_CONF}.bak.install"
  rm -f "$PANEL_BAK"
  if [ -f "$PANEL_CONF" ]; then
    cp -p "$PANEL_CONF" "$PANEL_BAK"
  fi
  # Hosts prepared by the carrox-vps-base nginx scripts keep the HTTPS listen
  # directives in a snippet (direct :443, or 127.0.0.1:8443 behind the 443 SNI
  # split). Use it when present so the panel follows the host's mode.
  HTTPS_LISTEN_SNIPPET=/usr/local/nginx/conf/snippets/https_listen.conf
  if [ -f "$HTTPS_LISTEN_SNIPPET" ]; then
    PANEL_HTTPS_LISTEN="include $HTTPS_LISTEN_SNIPPET;"
  else
    PANEL_HTTPS_LISTEN=$'listen 443 ssl;\n    listen [::]:443 ssl;'
  fi
  bash "$HERE/deploy/render-panel-conf.sh" "$PANEL_DOMAIN" "$PANEL_TMPL" "$PANEL_HTTPS_LISTEN" > "${PANEL_CONF}.new"
  mv "${PANEL_CONF}.new" "$PANEL_CONF"
  if NGX_TEST_OUT=$(/usr/local/nginx/sbin/nginx -p /usr/local/nginx -c conf/nginx.conf -t 2>&1); then
    /usr/local/nginx/sbin/nginx -s reload
    rm -f "$PANEL_BAK"
  else
    if [ -f "$PANEL_BAK" ]; then
      mv "$PANEL_BAK" "$PANEL_CONF"
    else
      rm -f "$PANEL_CONF"
    fi
    echo "ERROR: nginx -t failed for panel conf; previous state restored." >&2
    printf '%s\n' "$NGX_TEST_OUT" >&2
    exit 1
  fi

  # Tells the container which domain is the panel, so it can issue the panel
  # certificate and switch this vhost to HTTPS after the first CF token is saved.
  printf '%s\n' "$PANEL_DOMAIN" > /etc/vps-relay/panel-domain
fi

# --- 5. Build + up ---------------------------------------------------------
cd "$HERE"

log "building image vps-relay:latest"
docker build -t vps-relay:latest .

log "starting docker compose"
docker compose up -d

sleep 3

# --- 6. Output deployment info ---------------------------------------------
for i in $(seq 1 10); do
  if curl -sf http://127.0.0.1:8787/api/healthz >/dev/null 2>&1; then
    break
  fi
  sleep 1
done

# The key is logged on the line *after* the announcement.
ADMIN_KEY=$(docker logs vps-relay 2>&1 | grep -A1 '已自动生成管理员密钥' | tail -n 1 | awk '{print $NF}' || true)
EDGE_IP=$(curl -sf --max-time 5 https://ifconfig.me 2>/dev/null || echo '<未检测到>')

echo ""
echo "============================================"
echo "  vps-relay 部署完成"
echo "============================================"
echo ""
if [ -n "$PANEL_DOMAIN" ]; then
  echo "  面板地址:   $PANEL_SCHEME://$PANEL_DOMAIN"
else
  echo "  面板地址:   http://127.0.0.1:8787"
fi
echo "  边缘 IP:    $EDGE_IP"
echo "  容器状态:   $(docker inspect -f '{{.State.Status}}' vps-relay 2>/dev/null || echo unknown)"
echo "  版本:       $(curl -sf http://127.0.0.1:8787/api/healthz | grep -oP '"version":"\K[^"]+' || echo unknown)"
echo ""

if [ -n "$ADMIN_KEY" ]; then
  echo "  管理员密钥: $ADMIN_KEY"
  echo ""
  echo "  ⚠  请立即登录面板，配置 Cloudflare API Token。"
  echo "  ⚠  可通过面板「密钥管理」为其他用户创建密钥。"
else
  echo "  管理员密钥已在之前生成，请查看 /etc/vps-relay/keys.json"
  echo "  或运行: docker logs vps-relay 2>&1 | grep 密钥"
fi

# This script cannot tell whether the provider blocks inbound 80 (a local curl
# only hits loopback), so always show the fallback that does not depend on it.
print_tunnel_hint() {
  local conn="${SSH_CONNECTION:-}"
  local ssh_port="${conn##* }"
  [ -n "$ssh_port" ] || ssh_port=22
  echo "  $1"
  echo "  在你自己的电脑上执行（SSH 隧道）："
  echo "    ssh -p $ssh_port -L 8787:127.0.0.1:8787 root@$EDGE_IP"
  echo "  保持该窗口不关，然后用浏览器打开 http://127.0.0.1:8787"
}

echo ""
if [ -n "$PANEL_DOMAIN" ]; then
  if [ "$PANEL_SCHEME" != "https" ]; then
    echo "  首次配置（仅此一次需要网页）："
    echo "    用管理员密钥登录面板并保存 Cloudflare API Token。"
    echo "    保存后面板会自动签发自身证书并切换到 https://$PANEL_DOMAIN（约 1-3 分钟）。"
    echo ""
    print_tunnel_hint "如果从外网打不开 http://$PANEL_DOMAIN（部分机房拦截入站 80 端口，服务器本机 curl 仍会成功）："
    echo ""
  fi
  echo "  之后可通过 MCP 让 AI 操作（以 Claude Code 为例，整条命令写在一行）："
  echo "    claude mcp add --transport http vps-relay https://$PANEL_DOMAIN/mcp --header \"Authorization: Bearer <密钥>\""
else
  print_tunnel_hint "面板只监听服务器本机，要从自己电脑的浏览器访问："
  echo ""
  echo "  MCP 端点: http://127.0.0.1:8787/mcp（仅本机；公网使用请带 --domain 部署）"
fi

echo ""
echo "============================================"

DEPLOY_LOG="/etc/vps-relay/deploy-$(date +%Y%m%d%H%M%S).log"
{
  echo "vps-relay deploy info — $(date)"
  echo "edge_ip=$EDGE_IP"
  echo "container=$(docker inspect -f '{{.State.Status}}' vps-relay 2>/dev/null || echo unknown)"
  echo "version=$(curl -sf http://127.0.0.1:8787/api/healthz | grep -oP '"version":"\K[^"]+' || echo unknown)"
  echo "admin_key=${ADMIN_KEY:-<see keys.json or container logs>}"
  echo "panel_domain=${PANEL_DOMAIN:-}"
} > "$DEPLOY_LOG"
chmod 600 "$DEPLOY_LOG"
log "部署信息已保存到 $DEPLOY_LOG"
