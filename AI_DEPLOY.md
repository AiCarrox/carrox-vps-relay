# AI 部署说明

本文件用于让 AI 代理或自动化脚本在尽量少交互的前提下，把 `vps-relay` 部署到一台已经安装好 Nginx 与 Docker 的 VPS。

## 适用范围

适用于以下前提：

- 宿主机已安装 **Nginx 1.28+**
- 宿主机已安装 **Docker** 且 `docker compose` 可用
- 宿主机 Nginx 安装路径为 `/usr/local/nginx`
- 宿主机允许写入：
  - `/usr/local/nginx/conf/nginx.conf`
  - `/usr/local/nginx/conf/conf.d/`
  - `/etc/vps-relay/`
- 部署用户具备 root 权限

如果宿主机是按 `vps-manager` 的 base/nginx 脚本初始化，通常满足上述要求。

## 设计约束

AI 在部署时应遵守以下规则：

- **不得覆盖**已有业务站点配置
- 只允许：
  - 增量补充 `nginx.conf` 中缺失的 `connection_upgrade` map
  - 增量补充 `nginx.conf` 中缺失的 `resolver`（站点在请求时解析上游域名需要它）
  - 新增 `relay-panel.conf`
  - 新增 `/etc/vps-relay/*`
  - 构建并启动 `vps-relay` 容器
- 如果发现目标域名已被其他 Nginx 配置占用，必须立即停止
- 如果 `nginx -t` 失败，必须恢复原始 `nginx.conf`

## 部署前检查 443 端口

```bash
ss -ltnp 'sport = :443'
```

如果 443 已被 Nginx 以外的进程占用（例如 xray/Reality），`install.sh` 会在改动任何东西之前直接报错退出。此时**先停下来问用户**，不要自行停掉或改动那个服务。vps-relay 的面板 HTTPS 和所有站点都需要 443；常见解法是宿主机做 SNI 分流（见 README「与其他服务共用 443」），分流就绪后再部署，vps-relay 会自动适配。

## 推荐部署命令

### 1. 有面板域名

```bash
bash deploy/install.sh --domain relay.example.com
```

行为：

- 自动检查 Nginx / Docker / Docker Compose
- 若 `nginx.conf` 缺少 `connection_upgrade`，自动补齐
- 新增 `/usr/local/nginx/conf/conf.d/relay-panel.conf`
- 写入 `/etc/vps-relay/panel-domain`
- 面板通过 `http://relay.example.com` 访问
- 构建并启动 `vps-relay`
- 重跑时如果面板证书已存在（`/usr/local/nginx/conf/ssl/<域名>/`），渲染 HTTPS 版面板配置，不会把面板降回 HTTP

### 2. 无面板域名

```bash
bash deploy/install.sh
```

行为：

- 不创建 `relay-panel.conf`
- 只启动容器
- 面板通过 `http://127.0.0.1:8787` 访问

## 部署后必须验证

AI 在部署完成后，至少应验证以下项目：

```bash
/usr/local/nginx/sbin/nginx -t
curl -sf http://127.0.0.1:8787/api/healthz
docker inspect -f '{{.State.Status}} {{if .State.Health}}{{.State.Health.Status}}{{end}}' vps-relay
```

如果部署使用了 `--domain`，再额外验证：

```bash
curl -H 'Host: relay.example.com' http://127.0.0.1/
```

## 部署后的交接（需要人工一次）

部署脚本到此为止。接下来**必须由用户本人**完成一步网页操作，AI 不应代办，也不应索要 Cloudflare Token：

1. 用户用管理员密钥登录面板，保存 Cloudflare API Token
2. 面板自动为自身域名签发证书并切换到 HTTPS（约 1-3 分钟）

完成后，所有后续操作都可以通过 MCP 进行：

- 端点：`https://<面板域名>/mcp`
- 请求头：`Authorization: Bearer <密钥>`
- 先调用 `get_status`：`cf_token.configured` 为 `true` 且 `panel.state` 为 `https` 即表示就绪

`panel.state` 的取值：`none`（未带 `--domain`）、`http`（等待管理员保存 Token）、`issuing`（签发中）、`https`（就绪）、`error`（失败，原因见 `panel.error`）。

### 入站 80 端口被机房拦截时

部分机房在上游拦截入站 80。此时 `http://<面板域名>` 从外网打不开（本机 `curl` 仍然返回 200，容易误判），用户无法完成上面的网页步骤。先从外部确认 80 是否可达；不可达时二选一：

- 让用户用 SSH 隧道访问 `http://127.0.0.1:8787` 完成首次配置（`install.sh` 结束时会打印现成的 `ssh -L` 命令），保存 Token 后面板会自动切到 HTTPS（证书走 DNS-01，不依赖 80）
- 先用其他方式为面板域名签好证书，放到 `/usr/local/nginx/conf/ssl/<域名>/{fullchain.pem,key.pem}`，再重跑 `install.sh --domain`，面板直接以 HTTPS 提供；管理员保存 Token 后，面板会在证书到期前自动接管续签

## 关键文件

- 安装脚本：`deploy/install.sh`
- Nginx fallback 补丁器：`deploy/patch-nginx-map.py`
- 面板 vhost 模板：`templates/panel.conf.tmpl`（HTTP）、`templates/panel-ssl.conf.tmpl`（HTTPS）
- 面板后端监听：`127.0.0.1:8787`
- MCP 端点实现：`mcp.go`；面板自动 HTTPS：`panel.go`

## 失败处理原则

### 1. `nginx.conf` 缺少 `connection_upgrade`

这是预期内情况，安装脚本会自动补齐，不需要人工中断。

### 2. `nginx -t` 失败

必须立即停止，并恢复原始 `nginx.conf`。

### 3. 域名冲突

如果 `--domain` 指定的域名已经被其他 `conf.d/*.conf` 使用，必须停止，不得覆盖。

脚本只检查 Nginx 实际加载的顶层 `conf.d/*.conf`，按完整域名匹配 `server_name`，并跳过 `relay-panel.conf` 自身；`conf.d` 为空的全新机器可以直接部署。面板配置渲染后若 `nginx -t` 失败，脚本会恢复原有的 `relay-panel.conf`（首次部署则删除新文件）并打印 `nginx -t` 的输出。

### 4. 容器已存在

允许重跑部署脚本。脚本应支持幂等重建和重启。

## 当前已验证结论

本项目已在真实 VPS 上验证通过以下场景：

- 首次部署成功
- 自动补齐 `connection_upgrade` 成功
- `--domain relay.6553602.xyz` 成功创建面板 vhost
- 二次重跑部署脚本成功（幂等）

因此，后续 AI 部署应优先直接使用本文件与 `deploy/install.sh`，不再依赖源码外部文档。
