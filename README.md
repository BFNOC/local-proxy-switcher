# local-proxy-switcher

轻量自用本地混合代理切换器。

它在本机暴露一个固定 mixed 代理端口，客户端软件只需要长期绑定这个本地端口。程序内部只锁定一个当前上游代理；你可以手动从上游 API 获取短效代理，也可以手动指定 HTTP/SOCKS5 上游。普通切换只影响新连接，旧连接默认自然结束。

## 快速开始

```bash
cp config.example.yaml config.yaml
export IPZAN_NO="..."
export IPZAN_SECRET="..."
go run ./cmd/lps serve --config ./config.yaml
```

另开一个终端：

```bash
go run ./cmd/lps status
go run ./cmd/lps switch
curl -x http://127.0.0.1:7890 https://api.ipify.org
curl --proxy socks5h://127.0.0.1:7890 https://api.ipify.org
```

构建本地二进制：

```bash
go build -o bin/lps ./cmd/lps
```

## 命令

```text
lps serve
lps serve --config ./config.yaml
lps status
lps switch
lps switch --interrupt
lps lock http://1.2.3.4:8080
lps lock socks5://user:pass@1.2.3.4:1080
lps clear
lps open
```

控制 API 和 Web 面板默认监听 `127.0.0.1:17990`。Web 面板地址是 `http://127.0.0.1:17990/ui`。

## 配置

发行版是单个可执行文件。默认不传 `--config` 时，程序会读取可执行文件同目录下的 `config.yaml`；如果你要把配置放在别处，可以显式传 `--config`。

复制 `config.example.yaml` 为 `config.yaml`，放到 `lps` 或 `lps.exe` 同目录，再按需设置：

```yaml
listen:
  mixed: "127.0.0.1:7890"
  control: "127.0.0.1:17990"

provider:
  enabled: true
  url: "{{URL}}"
  timeout: "5s"
  default_scheme: "http"
  default_ttl: "10m"
```

真实 `IPZAN_NO` 和 `IPZAN_SECRET` 建议放在环境变量里，不要写进仓库。

JSON 响应形状示例：

```json
{"data":{"list":[{"ip":"IP_ADDR","port":PORT,"expired":TIME,"net":"NET"}]},"code":0,"message":"","status":200}
```

## 控制 API

```text
GET  /status
POST /switch
POST /switch?interrupt=true
POST /lock
POST /clear
GET  /ui
```

`POST /lock` 接收 JSON：

```json
{"url":"http://1.2.3.4:8080"}
```

也支持：

```json
{"url":"socks5://user:pass@1.2.3.4:1080"}
```

## 安全边界

- 不要提交 `config.yaml` 或 `.env`。
- 上游 API 密钥通过环境变量替换读取。
- 状态输出和日志式输出会隐藏 provider `no`、`secret` 和代理密码。
- 控制 API 默认只监听回环地址。
- 普通切换只影响新连接；需要断开旧连接时显式使用 `--interrupt`。
- 当前版本只做手动锁定和手动切换；`auto_refresh` 保持关闭。

## 版本和构建

当前版本是 `0.1.0`，以根目录 `VERSION` 文件为准。

GitHub Actions 只在 `VERSION` 文件变更的 push 上执行三端编译，产物只上传对应平台的单个可执行文件。配置文件不打包进产物，请在目标机器上把 `config.yaml` 放到可执行文件同目录。

## 设计参考

实现时只用 GitHub API 小范围查看了几个开源项目的目录和协议分层，没有 clone，也没有复制代码：

- [mihomo Meta 分支](https://github.com/MetaCubeX/mihomo/tree/Meta/listener)：参考 mixed 入站、协议监听和控制面分层的边界。
- [sing-box](https://github.com/SagerNet/sing-box/tree/testing/protocol)：参考 HTTP/SOCKS 入站与出站协议拆分方式。
- [armon/go-socks5](https://github.com/armon/go-socks5)：参考 SOCKS5 握手和请求处理保持小而清晰的做法。
- [JITOnline/go-pacproxy](https://github.com/JITOnline/go-pacproxy)：参考“小工具只做固定入口和上游转发”的产品边界。

## 不做什么

- 不实现 VLESS、VMess、Trojan、Shadowsocks、WireGuard 等加密协议。
- 不做大代理池、负载均衡、PAC、规则路由或系统代理接管。
- 不引入大型桌面 GUI；当前 Web 面板是单文件嵌入式页面。
