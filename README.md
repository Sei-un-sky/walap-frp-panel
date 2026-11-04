# FRP 管理面板

面板在运行 `frpc` 的同一台机器上管理多个客户端实例。每个实例使用独立 TOML 配置，可连接不同远端 `frps` 节点。多个面板用户共享实例列表；管理员可以创建普通用户或其他管理员。

## 功能

- 多用户登录、管理员角色、用户创建与删除。
- CLI 初始化管理员、创建用户、重置密码和删除用户；生成的一次性临时密码只输出在命令行，并要求首次登录后修改。
- 密码以 PBKDF2-HMAC-SHA256 派生值和随机盐保存，不存明文。
- 网页编辑 TOML、语法检查、实例启停；修改已有配置前自动备份旧版本。
- 夜间模式开关，选择保存在当前浏览器本地。
- 显示 `frpc` 进程状态、管理接口状态、隧道状态及接口提供的流量累计值。

## 环境要求与构建

- Go 1.22 或更新版本。
- 与目标系统/架构匹配的 `frpc` 可执行文件。
- 首次构建需要下载 `github.com/BurntSushi/toml` Go 模块。

```sh
go test ./...
go vet ./...
go build -o frp-manager .
```

## 账号初始化与管理

服务第一次启动前必须创建管理员。命令会生成随机临时密码并只在终端输出一次：

```sh
FRP_MANAGER_DATA_DIR=/var/lib/frp-manager ./frp-manager admin-init admin
```

保存输出的用户名和临时密码。首次登录会要求设置新密码。Linux systemd 服务使用 `seiun` 用户时，应确保该目录可由服务账户读写，例如：

```sh
install -d -o seiun -g seiun -m 0700 /var/lib/frp-manager
```

管理员也可以在网页的“账号设置”中创建普通用户或管理员。新账号的临时密码会在创建结果中显示一次，首次登录必须修改。网页管理员可以删除其他账号，但不能删除自己；最后一个管理员需通过 CLI 保留。

CLI 命令直接更新磁盘账号库。面板运行时不会热加载 CLI 修改，因此对现有账号执行 CLI 重置或删除前先停止服务，操作完成后再启动：

```sh
sudo systemctl stop frp-manager
# 执行下面的 user-reset / user-delete 命令
sudo systemctl start frp-manager
```

CLI 命令示例：

```sh
# 初始化首个管理员；可省略用户名，默认 admin
FRP_MANAGER_DATA_DIR=/var/lib/frp-manager ./frp-manager admin-init admin

# 创建账号，生成并显示一次性临时密码
FRP_MANAGER_DATA_DIR=/var/lib/frp-manager ./frp-manager user-add ops user
FRP_MANAGER_DATA_DIR=/var/lib/frp-manager ./frp-manager user-add backup-admin admin

# 重置账号密码，生成要求首次登录更改的临时密码
FRP_MANAGER_DATA_DIR=/var/lib/frp-manager ./frp-manager user-reset ops

# 删除账号（不能删除最后一个管理员）
FRP_MANAGER_DATA_DIR=/var/lib/frp-manager ./frp-manager user-delete ops
```

所有账号数据保存在 `FRP_MANAGER_DATA_DIR/users.json`，文件权限为仅服务账户可读写。请把 CLI 输出的临时密码通过受控渠道交付，不要放进 shell 历史、unit 文件或日志。

## 手动启动

```sh
export FRP_MANAGER_ADDR=127.0.0.1:8080
export FRP_MANAGER_DATA_DIR=/var/lib/frp-manager
export FRPC_BINARY=/opt/frp-manager/frpc
./frp-manager
```

默认只监听回环地址。浏览器访问 `http://127.0.0.1:8080`。远程访问请使用受控网络和 HTTPS 反向代理，不要将未加 TLS 的面板直接暴露到公网。

## systemd 管理

仓库提供了参考 unit 文件 [frp-manager.service.example](./frp-manager.service.example)。按实际安装路径调整 `User`、`Group`、`WorkingDirectory`、`ExecStart` 和 `FRPC_BINARY` 后安装：

```sh
sudo install -o root -g root -m 0644 frp-manager.service.example /etc/systemd/system/frp-manager.service
sudo systemctl daemon-reload
sudo systemctl enable --now frp-manager
```

常用管理命令：

```sh
sudo systemctl status frp-manager
sudo systemctl restart frp-manager
sudo systemctl stop frp-manager
sudo systemctl start frp-manager
sudo journalctl -u frp-manager -f
```

unit 示例将面板绑定在 `127.0.0.1:8080`，适合由本机 HTTPS 反向代理转发。若直接绑定内网地址，请自行评估网络访问控制和 TLS，不建议将服务暴露到公网。

## FRP 管理接口和数据限制

读取隧道状态和流量指标需要在每个 `frpc` TOML 中启用管理接口：

```toml
serverAddr = "frps.example.com"
serverPort = 7000

[webServer]
addr = "127.0.0.1"
port = 7400
user = "frpc-admin"
password = "替换为独立强密码"

[[proxies]]
name = "web"
type = "tcp"
localIP = "127.0.0.1"
localPort = 8080
remotePort = 6000
```

管理接口凭据保存在 TOML 配置中，配置编辑器会回显完整内容。应限制面板账号权限，并为各接口使用独立凭据。面板通过 `/api/status` 读取代理信息，通过 `/metrics` 解析流量计数；不同 FRP 版本的 API/指标可能有差异。当前流量为接口提供的累计值，不保存历史采样，也不保证所有版本都能提供逐隧道流量。进程运行状态不等于已连接远端 `frps`。

## 测试环境记录

测试机 `192.168.10.22` 的面板目录为 `/home/seiun/frp-manager`，测试 FRP 版本为 0.71.1。已运行 `go test ./...`、`go vet ./...`，并验证 FRP 示例 TOML。多用户登录、首次强制改密、普通用户实例权限、管理员用户管理权限均通过 HTTP 验证；测试用户已清理，管理员当前设置为 CLI 生成的首次登录临时密码。

测试机面板当前由测试会话手动运行于 `:8080`，没有安装为 systemd 服务。示例 FRP 配置指向 `127.0.0.1:7000`，因无对应运行中的 `frps`，未启动真实隧道。请使用 CLI 输出的管理员临时密码登录，并立即设置新密码；密码不记录在本文件中。
