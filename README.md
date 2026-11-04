# FRP 管理面板

面板在运行 `frpc` 的同一台机器上管理多个客户端实例。每个实例使用独立 TOML 配置，可连接不同远端 `frps` 节点。多个面板用户共享实例列表；管理员可以创建普通用户或其他管理员。

## 功能

- 多用户登录、管理员角色、用户创建与删除。
- CLI 初始化管理员、创建用户、重置密码和删除用户；生成的一次性临时密码只输出在命令行，并要求首次登录后修改。
- 密码以 PBKDF2-HMAC-SHA256 派生值和随机盐保存，不存明文。
- 网页编辑 TOML、语法检查、实例启停；修改已有配置前自动备份旧版本。
- 默认跟随设备明暗主题实时切换；工具栏主题按钮可在跟随系统、夜间、明亮三种模式间切换，手动选择保存在当前浏览器本地。
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

保存输出的用户名和临时密码。首次登录会要求设置新密码。账号库位于 `FRP_MANAGER_DATA_DIR/users.json`；数据目录必须由 systemd 选择的运行用户读写。例如模板使用 `frp-manager` 用户时：

```sh
install -d -o frp-manager -g frp-manager -m 0700 /var/lib/frp-manager
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

仓库提供了 [systemd unit 模板](./frp-manager.service) 和 [环境配置模板](./frp-manager.conf)。模板默认使用专用的 `frp-manager` 系统账户；客户可以改为已有服务账户或其他受限账户。`User`/`Group` 是 systemd unit 字段，不在环境配置文件中设置。

以下示例创建专用账户、设置数据目录权限并安装环境配置。若客户使用已有账户，把命令中的 `frp-manager` 替换为实际用户名和组名：

```sh
sudo useradd --system --home /var/lib/frp-manager --shell /usr/sbin/nologin frp-manager
sudo install -d -o frp-manager -g frp-manager -m 0700 /var/lib/frp-manager
sudo install -o root -g root -m 0644 frp-manager.conf /etc/default/frp-manager
sudoedit /etc/default/frp-manager
```

`/etc/default/frp-manager` 中设置面板监听地址、数据目录和 `frpc` 二进制路径；这里不放面板登录密码。管理员账号通过 CLI 初始化并保存在数据目录中。

客户也可以编辑 unit 中的 `User` 和 `Group`，或通过 systemd drop-in 覆盖：

```sh
sudo systemctl edit frp-manager
```

写入所需账户：

```ini
[Service]
User=customer-service-user
Group=customer-service-group
```

确认 [unit 模板](./frp-manager.service) 中的 `WorkingDirectory` 和 `ExecStart` 与安装路径一致后安装并启用：

```sh
sudo install -o root -g root -m 0644 frp-manager.service /etc/systemd/system/frp-manager.service
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

### 用户 TOML 文件位置

假设 `FRP_MANAGER_DATA_DIR=/var/lib/frp-manager`：

- 网页保存的配置和实例元数据：`/var/lib/frp-manager/profiles.json`。每个实例的 `config` 字段保存用户提交的完整 TOML，是面板编辑和持久化的权威副本。
- 启动实例时实际交给 `frpc -c` 的文件：`/var/lib/frp-manager/configs/<实例ID>.toml`。面板启动实例时从 `profiles.json` 写出或更新该运行文件。
- 修改已有配置前保存的旧版本：`/var/lib/frp-manager/backups/<实例ID>.<时间戳>.toml`。
- 面板账号数据库：`/var/lib/frp-manager/users.json`。

以上路径由 `FRP_MANAGER_DATA_DIR` 决定。服务运行用户需要对数据目录有读写权限。直接修改生成的 `configs/` 文件不会改动网页保存的配置；后续启动时会再次由 `profiles.json` 写出。

## FRP 管理接口和数据限制

读取每条隧道的建立状态需要在每个 `frpc` TOML 中启用管理接口：

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

管理接口凭据保存在 TOML 配置中，配置编辑器会回显完整内容。应限制面板账号权限，并为各接口使用独立凭据。面板通过 `frpc` 的 `/api/status` 读取代理列表及 `running`、`start error` 等运行状态。需要在每份客户端 TOML 中配置 `[webServer]` 并重启对应 `frpc`，否则只能看到进程状态，不能确认服务端隧道是否建立。

FRP 0.71.1 的 `frpc` `/metrics` 只提供 Go 运行时指标，不包含代理流量，因此面板不会把缺失数据伪装成 `0 B`，而会显示“未提供”。代理流量统计由 `frps` 服务端 Dashboard API 提供；如需在面板展示每条代理的实时累计流量，需要额外配置对应 `frps` dashboard 地址和账号，再按代理名称查询 `/api/traffic/{name}`。当前版本尚未接入该服务端 API。

`running` 表示 frpc 当前成功启动了对应代理。若显示 `start error`，请检查相应 `err` 信息、frps 端口和本地服务监听；“进程运行中”只代表 frpc 进程还活着，不代表某条隧道可用。配置管理接口后需重启该实例使设置生效。

## 测试环境记录

测试机 `192.168.10.22` 的面板目录为 `/home/seiun/frp-manager`，测试 FRP 版本为 0.71.1。已运行 `go test ./...`、`go vet ./...`，并验证 FRP 示例 TOML。多用户登录、首次强制改密、普通用户实例权限、管理员用户管理权限均通过 HTTP 验证；测试用户已清理，管理员当前设置为 CLI 生成的首次登录临时密码。

测试机面板当前由测试会话手动运行于 `:8080`，没有安装为 systemd 服务。示例 FRP 配置指向 `127.0.0.1:7000`，因无对应运行中的 `frps`，未启动真实隧道。请使用 CLI 输出的管理员临时密码登录，并立即设置新密码；密码不记录在本文件中。
