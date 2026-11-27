# Hesper

### 长夜有星，数据有序。

> 一个纯粹、利落的跨平台数据库工作台。

基于 **Go + Wails v3 + Vue 3** 构建，面向 **MySQL、PostgreSQL、MariaDB 与 SQLite**，专注流畅的日常查询、表数据浏览与安全的远程连接体验。

---

## 核心特性

### 多引擎统一工作台

- **MySQL / MariaDB / PostgreSQL / SQLite**：统一操作习惯，并按引擎原生能力自动展现层级结构 (PostgreSQL 展示 Database → Schema → Table / View / Sequence；MariaDB 支持原生 Sequence；SQLite 直接打开本地数据库文件)。
- **四方言 SQL 编辑器**：基于 CodeMirror 6，支持语法高亮、表与列自动补全，切换标签保留编辑历史、光标与分屏比例。
- **高效浏览与导航**：虚拟滚动对象树与数据网格，双击表开启实时浏览 (支持 `WHERE` 过滤、排序与 DDL 查看)，内置浅色 / 深色 / 跟随系统主题及多档字号。

### 安全连接与加密传输

- **SSH 隧道**：支持密码、私钥文件 (含口令) 与本机 SSH Agent (Linux / macOS `SSH_AUTH_SOCK` 与 Windows OpenSSH Agent)，内置主机指纹首次确认与变更警示。
- **完整 TLS / mTLS**：支持 `disable`、`prefer`、`require`、`verify-ca`、`verify-full` 五种模式，支持自定义 CA 根证书与双向客户端证书，严格模式禁止明文降级。
- **安全凭据存储**：优先写入系统钥匙串 (Windows 凭据管理器 / Linux Secret Service)，无钥匙串环境自动降级至本地 AES-256-GCM 加密库。

### 独立会话与安全执行

- **会话隔离与精准取消**：每个查询标签独占独立数据库连接与事务上下文，元数据浏览不阻塞慢查询；取消仅中断当前语句，不影响同连接下的其他标签。
- **翻页不重跑 SQL**：编辑器查询结果写入快照供翻页浏览，避免带副作用的 SQL 被重复执行。
- **防误操作保护**：连接环境色标区分、只读保护模式 (SQLite 直达引擎级 `mode=ro` + `query_only`)、生产环境高危语句 (`DROP` / `TRUNCATE` / 无 `WHERE` 的 `UPDATE` 与 `DELETE`) 二次确认，以及写入断线后的结果未知提示。

### 高保真数据呈现

- `BIGINT` / `DECIMAL` 原样文本展示，避免精度丢失。
- 明确区分 `NULL` 与空字符串 `""`，支持不可见字符显示，长文本、JSON 与二进制 BLOB 支持弹窗预览与按需下载。
- 支持复制与导出为 TSV、CSV、大数保真 JSON 及按方言转义的 `INSERT SQL`。

---

## 常用快捷键

| 快捷键 | 功能 |
| :--- | :--- |
| `Ctrl+Enter` | 执行当前 SQL 语句 |
| `Esc` | 优先关闭顶层弹窗；无弹窗时取消运行中的查询 |
| `Ctrl+.` | 取消当前运行中的查询 |
| `Ctrl+K` | 打开命令面板 |
| `Ctrl+N` | 新建查询标签页 |
| `Ctrl+W` | 关闭当前标签页 |
| `Ctrl+,` | 打开偏好设置 |
| `F5` | 刷新当前表浏览页 |

---

## 从源码构建

### 环境要求

- **Go**：`1.27+`
- **Node.js / pnpm**：Node `22+` / pnpm `12+`
- **Wails v3 CLI** (仅修改 Go 绑定方法时需要)：
  ```bash
  go install github.com/wailsapp/wails/v3/cmd/wails3@v3.1.0
  ```
- **Linux 桌面编译依赖** (Ubuntu 24.04+ / Debian 13+)：
  ```bash
  sudo apt-get install -y build-essential pkg-config libgtk-4-dev libwebkitgtk-6.0-dev
  ```

### 编译步骤

```bash
# 1. 构建前端资源 (编译 Go 二进制时通过 go:embed 内嵌)
cd frontend && pnpm install && pnpm build && cd ..

# 2. 构建 Linux 桌面端 (可选再执行 bash scripts/package-appimage.sh 打包 AppImage)
go build -tags production -trimpath -ldflags="-s -w" -o bin/hesper .

# 3. 交叉编译 Windows x86_64 桌面端 (纯 Go，无需 CGO)
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
  go build -tags production -trimpath -ldflags="-s -w -H=windowsgui" -o bin/hesper.exe .

# 可选：构建无头 HTTP Server 模式，通过浏览器访问完整工作台
go build -tags server -trimpath -ldflags="-s -w" -o bin/hesper-server .
```

> **便携模式**：在可执行文件同级目录新建空文件 `portable.flag`，应用配置与历史记录将保存到同目录 `data/` 下，而非系统用户配置目录。
>
> **Linux 图形兼容**：如在 Wayland 或 NVIDIA 环境下遇到渲染异常，可使用 `--disable-dmabuf` 或 `--software-gl` 启动参数。

---

## 运行测试

```bash
# Go 后端单元测试与 SQLite 集成测试
go test -race -count=1 ./internal/...

# 前端单元测试 (Vitest)
cd frontend && pnpm test
```

如需运行 MySQL 8.4 / PostgreSQL 16 / MariaDB 11.4 真库与 SSH + TLS/mTLS 集成测试：

```bash
./scripts/gen-test-tls-certs.sh
docker compose -f docker-compose.test.yml -f docker-compose.tls.test.yml up -d --wait
SQLM_REQUIRE_REAL_DB=1 go test -v -race -count=1 ./internal/driver/... ./internal/tunnel ./internal/session ./internal/service
docker compose -f docker-compose.test.yml -f docker-compose.tls.test.yml down -v
rm -rf test/.tls-certs
```

---

## 目录结构

```text
hesper/
├── main.go                 # 程序入口与前端资源嵌入
├── internal/
│   ├── bootstrap/          # Wails 生命周期与依赖装配
│   ├── binding/            # 前端 RPC 绑定与二进制流式接口
│   ├── service/            # 连接、查询、元数据用例编排
│   ├── tunnel/             # SSH 隧道池、通道适配、Known Hosts 与 Agent
│   ├── session/            # 会话管理、取消栅栏与状态同步
│   ├── driver/             # 共享 TLS/mTLS 与四库驱动实现
│   ├── resultbuf/          # 查询结果快照与磁盘溢写缓存
│   ├── sqlsplit/           # 多方言 SQL 切分与高危语句识别
│   ├── repository/         # 本地 SQLite 配置与历史存储
│   └── secret/             # 系统钥匙串与加密回退凭据库
├── frontend/               # Vue 3 + TypeScript + Pinia + CodeMirror 6
├── scripts/                # AppImage 打包与测试证书脚本
└── docs/                   # 系统架构、引擎契约、交互规范与路线图
```

更多设计细节与阶段规划请查阅 [docs/](docs/README.md)。

---

## 开源许可

本项目基于 [Apache License 2.0](LICENSE) 开源发布。
