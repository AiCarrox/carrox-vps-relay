# vps-relay

极简反代加速面板。部署在 CN2/精品线路 VPS 上，为普通线路上的服务提供 L7 反代加速，自动完成 DNS 配置、证书签发和 Nginx 配置。

## 功能

- **一键建站**：填入加速域名 + 上游地址，自动创建 CF DNS 记录、签发 Let's Encrypt 证书、渲染 Nginx 配置并热重载
- **多用户支持**：管理员可通过面板或 API 创建用户密钥，每个用户独立管理自己的 Cloudflare Token 和站点
- **站点隔离**：普通用户只能看到和操作自己创建的站点，管理员可管理所有站点
- **自动续签**：每 12 小时扫描，证书剩余 ≤30 天时自动续签
- **WebSocket/SSE 透传**：默认支持，无需额外配置
- **MCP 接入**：首次在网页配置好 Cloudflare Token 后，后续操作都可以交给 AI（Claude Code 等 MCP 客户端）完成
- **面板自动 HTTPS**：带 `--domain` 部署时，管理员保存 Cloudflare Token 后面板自动签发自身证书并切换到 HTTPS
- **无数据库**：所有状态存储在 Nginx 配置文件的 `relay-meta` 注释中，文件即真相
- **证书脱敏**：CF Token 在界面中脱敏显示

## 环境要求

宿主机需已具备：

- **Nginx** ≥ 1.28（需编译 http_ssl / http_v2 / http_v3 / stream / http_realip 模块）
- **Docker** + **Docker Compose**
- 公网 80 / 443 端口可达
- Cloudflare 账号（用于 DNS 管理）

其余依赖（Go、acme.sh、jq、dig、openssl 等）全部封装在 Docker 镜像内。

## AI 部署

如果是让 AI 代理或自动化脚本直接部署，请优先阅读：

- `AI_DEPLOY.md`

该文档说明了无交互部署约束、`--domain` 参数、fallback 行为以及部署后的标准验证步骤。

## 快速部署

```bash
# 1. 克隆项目
git clone https://github.com/AiCarrox/carrox-vps-relay.git
cd carrox-vps-relay

# 2. 一键部署（无域名，面板走本机 HTTP）
bash deploy/install.sh

# 3. 一键部署（有域名，面板走域名 HTTP）
bash deploy/install.sh --domain relay.example.com
```

部署完成后，终端会输出管理员密钥和面板地址。首次启动时程序会自动生成一个管理员密钥并打印到日志中。

```
============================================
  vps-relay 部署完成
============================================

  面板地址:   http://127.0.0.1:8787
  # 或（使用 --domain 时）
  面板地址:   http://relay.example.com
  边缘 IP:    1.2.3.4
  管理员密钥: Kj8mN2pQ5xRt

  ⚠  请立即登录面板，配置 Cloudflare API Token。
  ⚠  可通过面板「密钥管理」为其他用户创建密钥。
============================================
```

使用 `--domain` 时，终端还会打印首次配置步骤和可直接粘贴的 MCP 接入命令。

## 首次配置（唯一需要网页的一步）

1. 用终端输出的管理员密钥登录面板
2. 在「① Cloudflare 凭据」保存 Cloudflare API Token（需要 Zone:Read + DNS:Edit）

如果部署时带了 `--domain`，且该域名所在的 Zone 归这个 Token 管理，保存后面板会在后台为自己的域名签发证书，并把面板切换到 `https://<域名>`（约 1-3 分钟，页面会自动跳转）。此后 80 端口只做跳转。

切换失败时面板保持 HTTP，原因显示在同一张卡片里，也可以通过 `GET /api/panel` 查看。

### 面板打不开时：用 SSH 隧道

有些机房会在上游拦截入站 80 端口，此时 `http://<面板域名>` 从外网打不开（在服务器上 `curl` 仍然成功，容易误判）。面板程序只监听服务器本机的 `127.0.0.1:8787`，可以用 SSH 隧道把它映射到自己电脑上：

```bash
ssh -p <SSH端口> -L 8787:127.0.0.1:8787 root@<服务器IP>
```

保持这个窗口不关，浏览器打开 `http://127.0.0.1:8787` 完成首次配置即可。面板证书走 DNS 验证，不依赖 80 端口，保存 Token 后照常切换到 HTTPS。部署脚本结束时会打印填好端口和 IP 的这条命令。

## 通过 MCP 让 AI 操作

面板内置一个 MCP 端点（Streamable HTTP），鉴权沿用登录密钥：

- 地址：`https://<面板域名>/mcp`
- 请求头：`Authorization: Bearer <密钥>`

以 Claude Code 为例：

```bash
claude mcp add --transport http vps-relay https://relay.example.com/mcp --header "Authorization: Bearer <密钥>"
```

整条命令写在一行，Linux、macOS、Windows（PowerShell / cmd）通用。

之后直接用自然语言即可，例如「把 app.example.com 反代到 https://origin.example.com」。

| 工具 | 作用 |
|------|------|
| `get_status` | 当前身份、CF Token 是否已配置、边缘 IP、面板 HTTPS 状态 |
| `list_zones` | 列出可用的 Cloudflare Zone |
| `list_sites` | 列出站点及健康状态 |
| `create_site` | 新建站点（DNS + 证书 + Nginx，同步执行，约 1-3 分钟） |
| `update_site_upstream` | 修改上游地址 |
| `enable_site` / `disable_site` | 启用 / 停用（重复调用不会来回翻转） |
| `delete_site` | 删除站点并清理 DNS 和证书（不可恢复） |
| `adopt_site` | 接管 conf.d 下已有的手写配置 |
| `list_keys` / `create_key` / `delete_key` | 密钥管理（仅管理员可见） |

注意事项：

- **权限随密钥**：普通用户的密钥只能看到和操作自己的站点，也看不到密钥管理工具。
- **只走 HTTPS**：经 Nginx 进来的明文 HTTP 请求会被 `/mcp` 拒绝。面板域名不在 Cloudflare 上、无法自动签证书时，可以用 SSH 隧道访问 `http://127.0.0.1:8787/mcp`。
- **Cloudflare Token 不经过 MCP**：只能在网页里配置，不会出现在 AI 对话里。
- **`create_key` 会把新密钥明文返回给 AI**，它会留在对话记录中；介意的话请在网页里创建。
- `/mcp` 只认 `Authorization` 请求头，不认浏览器 Cookie；连续认证失败会按 IP 限流 5 分钟。

## 与其他服务共用 443（SNI 分流）

如果 443 端口还要给 Reality 这类必须直接拿到 TLS 流量的服务用，可以让宿主机 Nginx 用 `stream` + `ssl_preread` 在 443 上按 SNI 分流，网站改为监听本机端口（例如 `127.0.0.1:8443`，用 PROXY protocol 传真实客户端 IP）。

部署脚本一开始会检查 443：如果它被 Nginx 以外的进程占用，脚本在改动任何东西之前报错退出，先做好分流再重跑即可。Nginx 自己监听 443（包括已经分流的机器）不受影响。

vps-relay 不负责搭建分流，但会自动适配：只要宿主机存在 `/usr/local/nginx/conf/snippets/https_listen.conf`，面板和所有站点的 HTTPS server 块就会 `include` 这个片段，而不是写死 `listen 443 ssl`。片段里是直接监听 443 还是监听分流后的本机端口，由宿主机决定，切换模式不需要重新渲染站点。没有这个片段时行为不变。

镜像内用于 `nginx -t` 的 Nginx 带有 stream 模块，宿主机 `nginx.conf` 里有 `stream {}` 块也能正常校验。

## 多用户管理

### 通过面板

管理员登录后，在底部「密钥管理」卡片中：

1. 输入用户 ID 和备注，点击「创建」
2. 复制生成的密钥，分发给对应用户
3. 用户登录后配置自己的 Cloudflare API Token 即可使用

### 通过 API

```bash
# 创建用户密钥（需 admin cookie 或 Bearer token）
curl -X POST http://127.0.0.1:8787/api/keys \
  -H 'Content-Type: application/json' \
  -b 'vps-relay-auth=<admin-key>' \
  -d '{"id":"user01","role":"user","note":"用户01"}'

# 列出所有密钥（key 脱敏）
curl http://127.0.0.1:8787/api/keys -b 'vps-relay-auth=<admin-key>'

# 删除密钥
curl -X DELETE http://127.0.0.1:8787/api/keys/user01 \
  -b 'vps-relay-auth=<admin-key>'
```

## API 列表

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/healthz` | 健康检查 |
| POST | `/api/login` | 登录（body: `{"token":"<key>"}`） |
| GET | `/api/whoami` | 当前用户信息 |
| GET | `/api/cf-token/status` | CF Token 状态（脱敏） |
| PUT | `/api/cf-token` | 保存 CF Token |
| GET | `/api/zones` | 列出 Cloudflare Zone |
| GET | `/api/sites` | 列出站点（admin 看全部） |
| POST | `/api/sites` | 创建站点 |
| PUT | `/api/sites/{domain}` | 编辑上游地址 |
| POST | `/api/sites/{domain}/toggle` | 启用/停用站点（翻转） |
| POST | `/api/sites/{domain}/enable` | 启用站点（幂等） |
| POST | `/api/sites/{domain}/disable` | 停用站点（幂等） |
| GET | `/api/panel` | 面板域名与 HTTPS 状态 |
| POST | `/mcp` | MCP 端点（JSON-RPC，Bearer 密钥） |
| DELETE | `/api/sites/{domain}` | 删除站点 |
| POST | `/api/sites/adopt` | 接管已有 Nginx 配置 |
| GET | `/api/keys` | 列出密钥（admin only，脱敏） |
| POST | `/api/keys` | 创建密钥（admin only） |
| DELETE | `/api/keys/{key_id}` | 删除密钥（admin only） |

## 目录约定

| 路径 | 用途 |
|------|------|
| `/etc/vps-relay/keys.json` | 密钥表（程序自动管理） |
| `/etc/vps-relay/users/{id}/cf-token` | 用户私有 Cloudflare Token |
| `/etc/vps-relay/panel-domain` | 面板域名（`install.sh --domain` 写入） |
| `/etc/vps-relay/panel-cert-owner` | 面板证书由哪个管理员的 Token 签发（用于自动续签） |
| `/usr/local/nginx/conf/conf.d/relay-*.conf` | 站点配置 |
| `/usr/local/nginx/conf/ssl/{domain}/` | TLS 证书 |

## 更新

```bash
cd carrox-vps-relay
git pull
docker compose build --no-cache
docker compose up -d
```

## 许可证

MIT
