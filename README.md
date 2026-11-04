# FRP 管理面板

第一版管理运行在面板同一台机器上的多个 `frpc` 客户端实例。每个实例保存独立 TOML 配置，连接一个远端 `frps`。

## 启动

需要 Go 1.22 或更新版本。初次运行会由 Go 下载 `BurntSushi/toml` 解析库。

```sh
export FRP_MANAGER_ADDR=127.0.0.1:8080
export FRP_MANAGER_USER=admin
export FRP_MANAGER_PASSWORD='请设置强密码'
export FRP_MANAGER_DATA_DIR=./data
export FRPC_BINARY=/home/seiun/frp/frpc
go run .
```

默认仅监听回环地址。需要从其他机器访问时，应在受控网络中配置监听地址，并通过 HTTPS 反向代理保护登录凭据和会话 Cookie。不要将面板直接暴露到公网。

## 功能与限制

- 单用户登录，配置通过面板管理多个本机 `frpc` 进程。
- 修改已有配置时会先把上一版 TOML 写入 `data/backups/`。
- 保存时校验 TOML 语法；启动时把配置写到 `data/configs/` 并执行 `frpc -c`。
- 当配置启用 `webServer` 管理接口时，面板读取代理状态和 metrics。FRP 不同版本的指标名称可能不同，需按测试版本验证流量统计结果。
- 第一版流量为当前管理接口暴露的累计值，不提供持久化的历史曲线。

## 验证

```sh
go test ./...
```
