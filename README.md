# SQL Manager

面向 **MySQL、PostgreSQL、MariaDB 和 SQLite** 的轻量级跨平台桌面数据库工作台，基于 **Go + Wails v3 + Vue 3** 构建。

> **当前交付范围（Phase 1A + Phase 1B）**：可运行的 **MySQL 查询工作台**（默认在真实 **MySQL 8.4.13 + InnoDB** 上完成全量端到端验收）与 **PostgreSQL 16.16** 第二驱动抽象中立性检验。网格行内写入、表设计器、MariaDB 与 SQLite 用户库驱动按既定路线图留待后续阶段。

---

## 核心特性（当前可运行版本）

- **人性化桌面交互（浅色 / 深色双主题）**
  - **高频操作走短路径**：左侧对象树支持即时模糊过滤，双击表直接开启 `实时浏览`，新建查询立即进入 CodeMirror 6 SQL 编辑器。
  - **清晰上下文与防误操作**：连接环境色条（生产红 / 预发琥珀 / 开发绿 / 默认蓝）、顶部始终显示当前连接/库/版本/只读标识、`Ctrl+K` 快速跳转面板、可拖动调整侧边栏与上下分屏高度。
  - **连接管理开箱即用**：内置 `测试连接`（展示实测引擎、版本号与毫秒耗时）、编辑连接留空不覆盖原密码、诚实提示“客户端只读保护 ≠ 数据库强只读”。
- **每个查询标签独占一条物理连接（Track B）+ 独立元数据池（Track A）**
  - 标签 A 开启的事务、临时表不会串到标签 B；对象树浏览和补全拉取走独立元数据池，不被慢查询阻塞。
  - 关闭查询标签同步释放该标签的物理连接与所有结果快照缓存。
- **严格验收保障**
  1. **停止仅影响当前语句**：每次执行分配独立 `execID`，通过旁路通道（MySQL `KILL QUERY` / PostgreSQL `CancelRequest`）取消；配合取消栅栏防止迟到取消误杀排队查询 B；结果流关闭且状态明确后才复用连接，无法确认时停止复用并提示“原会话已结束”。
  2. **编辑器翻页绝不重跑 SQL**：编辑器 SQL 只执行一次并写入内存/磁盘透明溢写快照，翻页仅切片读取本次快照；快照过期明确提示且提供手动 `[重新执行 SQL]` 按钮，绝不自动重跑；双击表的 `实时浏览` 与 `一次性快照` 清晰分离。
  3. **数据保真与写入断线诚实提示**：`BIGINT` / `DECIMAL` 原样文本呈现并按内容采样自动扩宽列宽，不经 JS `Number` 失真；`NULL` 灰字斜体、空串 `""` 无框淡灰与有值主色清晰区分，支持一键开启不可见字符显示（`·` / `→` / `↵` / `[ZWSP]`）且复制导出严格保真；网格保持单行，双击单元格弹出可滚动浮窗查看长文本、格式化 JSON 或按需通过 `/stream/{ref}` 查看/下载 BLOB；自动提交写入或显式 `COMMIT` 发出后断线回报 `ERR_COMMIT_OUTCOME_UNKNOWN`（“写入或提交结果未知”），界面仅提供核对后重建会话按钮，**绝不提供重试按钮**。

---

## 环境要求

### 开发与构建
- **Go**：`1.26+`（本次构建实测：`go1.27.4 linux/amd64`）
- **Node.js / pnpm**：Node `22+` / `pnpm 10+`（本次实测：`Node v22.23.5`、`pnpm 12.21.0`）
- **Wails v3 CLI**：`go install github.com/wailsapp/wails/v3/cmd/wails3@v3.1.0`
- **Linux 桌面编译依赖**（Ubuntu 24.04+ / Debian 13+）：
  ```bash
  sudo apt-get install -y build-essential pkg-config libgtk-4-dev libwebkitgtk-6.0-dev
  ```
  > CachyOS / Arch Linux：`sudo pacman -S base-devel pkgconf gtk4 webkitgtk-6.0`
- **Docker Compose**：用于一键启动真实 MySQL 8.4 + PostgreSQL 16 验收测试库。

---

## 快速复现：测试库准备、构建与启动

### 1. 启动真实测试数据库（MySQL 8.4 + PostgreSQL 16）

```bash
docker compose -f docker-compose.test.yml up -d --wait
```

预置测试库与账号：

| 实例 | 地址与端口 | 账号 / 密码 | 库与数据说明 |
| :--- | :--- | :--- | :--- |
| **MySQL 8.4 (InnoDB)** | `127.0.0.1:13306` | 只读：`sqlm_ro` / `sqlm_ro_pw`<br>可写：`sqlm_rw` / `sqlm_rw_pw` | `sqlm_demo`（`customers`、`ledger_entries` 含极限 BIGINT/DECIMAL/NULL/空串/JSON/BLOB/中文🚀、1,250 行 `events`、MyISAM `legacy_logs`、无主键 `no_pk_notes`、视图 `v_customer_summary`）；<br>`sqlm_write_test`（独立可写库：`write_probe`、`side_effect_log`、带副作用存储过程 `sp_side_effect_rows`） |
| **PostgreSQL 16** | `127.0.0.1:15432` | 只读：`sqlm_ro` / `sqlm_ro_pw`<br>可写：`sqlm_rw` / `sqlm_rw_pw` | `sqlm_demo`（`public` + `analytics` 双 Schema、`accounts`、`metrics`、`order_seq` 序列、`v_accounts`、`write_probe`）；<br>`sqlm_alt`（`alt_items`，用于验证切库重建连接） |

停止并清理测试容器：
```bash
docker compose -f docker-compose.test.yml down -v
```

### 2. 生成 Bindings、构建前端与桌面二进制

```bash
# 1) 生成 Wails v3 TypeScript Bindings（如修改 Go binding 方法后）
wails3 generate bindings -ts -i -d frontend/bindings ./...

# 2) 安装并构建 Vue 3 前端
cd frontend && pnpm install && pnpm build && cd ..

# 3) 构建 Linux 原生桌面二进制（GTK4 + WebKitGTK 6.0，约 21 MB 单文件）
go build -trimpath -ldflags="-s -w" -o bin/sql-manager .

# 4) 打包为标准 Linux x86_64 GTK4 Type 2 AppImage（约 8.3 MB，缺库自动弹窗提示，输出至 dist/）
bash scripts/package-appimage.sh

# 可选：构建 Wails v3 无头 HTTP Server 模式二进制（便于无桌面环境通过浏览器实测同一套后端与 UI）
go build -tags server -trimpath -ldflags="-s -w" -o bin/sql-manager-server .
```

### 3. 启动运行

- **桌面模式（Ubuntu 24.04 / Debian 13 / CachyOS）**：
  ```bash
  ./bin/sql-manager
  # 如遇 Wayland + NVIDIA 图形异常，支持安全降级参数：
  ./bin/sql-manager --disable-dmabuf
  ./bin/sql-manager --software-gl
  ```
- **Wails v3 Server 模式（无桌面环境快速体验完整真实前后端）**：
  ```bash
  SQL_MANAGER_PORT=8080 ./bin/sql-manager-server
  # 打开 http://127.0.0.1:8080 即可使用完整工作台，弹窗内可一键预填上述 MySQL/PG 测试库
  ```

---

## 运行全部自动化测试

```bash
# Go 后端单元测试 + 真实 MySQL 8.4 & PostgreSQL 16 集成测试（开启竞态检测）
go test -v -race -count=1 ./...

# Vue 3 前端数据保真与安全语义单元测试（Vitest）
cd frontend && pnpm test
```

---

## 实际验证环境与测试结果

### 实测环境与版本号

| 组件 | 实际运行版本 |
| :--- | :--- |
| **MySQL（真实 Docker 容器）** | **`8.4.13`**（默认 InnoDB，镜像 `mysql:8.4`） |
| **PostgreSQL（真实 Docker 容器）** | **`16.16 (Debian 16.16-1.pgdg13+1)`**（镜像 `postgres:16`） |
| **内嵌 SQLite（本地 `app.db`）** | **`3.54.1`**（`modernc.org/sqlite v1.63.0`） |
| **桌面构建环境** | Go `1.27.4` · Wails `v3.1.0` · GTK4 `4.14.5` · WebKitGTK 6.0 `2.52.6` · Node `v22.23.5` |

### 已验证项目（真实数据库 + 自动化测试）

| 验收项 | 测试用例 / 模块 | 验证类型 | 结果 | 关键验证点 |
| :--- | :--- | :--- | :--- | :--- |
| **验收 1：停止仅影响当前语句** | `TestMySQL84_Criterion1_CancelAndLateCancelRace` | **真实 MySQL 8.4.13** | **通过** | 旁路 `KILL QUERY` 中断慢查询返回 `ERR_STATEMENT_CANCELED` (Error 1317)；`CONNECTION_ID()`、开启的事务与临时表完好保留；5 轮迟到取消竞态从未误杀排队查询 B |
| **验收 1：未确认取消→原会话结束** | `TestUnconfirmedCancelMarksSessionLost` | `driver/fake` 纯单测（不计作真库） | **通过** | 模拟取消信号未获确认时停止复用旧连接并显式回报 `SessionLost` |
| **验收 2：编辑器翻页不重跑 SQL** | `TestMySQL84_Criterion2_ExecuteOncePaginationLiveBrowseAndCloseCleanup` | **真实 MySQL 8.4.13** | **通过** | `CALL sp_side_effect_rows` 跨 4 页翻页含磁盘溢写，副作用表行数严格为 `1`；快照过期返回 `ERR_RESULT_EXPIRED` 且不重跑；实时 `BrowseTable`（1,250 行 `events`）与编辑器快照分离；`CloseTab` 同步释放物理连接与快照 |
| **验收 3：数据保真 / 只读 / 结果未知** | `TestMySQL84_Criterion3_FidelityReadOnlyAndCommitOutcomeUnknown` | **真实 MySQL 8.4.13** | **通过** | `18446744073709551615`、`9007199254740993`、`-9223372036854775808`、`1234567890123456789012345678.1234567890`、`-0.0000000001` 原样保真；`NULL` 与 `""` 严格区分；UTF-8MB4 中文与 `🚀`、`/stream` BLOB 字节一致；真实只读账号 `sqlm_ro` 写表被 MySQL Error 1142 拒绝（`ERR_READ_ONLY_DENIED`）；真实 TCP 断线验证自动提交写入与显式 `COMMIT` 均报 `ERR_COMMIT_OUTCOME_UNKNOWN`（`RetrySafe=false`）且阻断后续执行 |
| **第二驱动抽象中立性检验** | `TestPostgres16_AbstractionNeutralityAndAcceptance` | **真实 PostgreSQL 16.16** | **通过** | `Capabilities.SwitchDBReconnects=true`、`HasSchemas=true`、`SupportsSequences=true`；跨库切换 `sqlm_demo → sqlm_alt` 自动重建物理连接（PID 变更）且事务内切库安全拒绝；`CancelRequest` 取消事务内慢查询返回 `SessionAliveFailedTx`；`INSERT ... RETURNING *` 翻页严格只执行 1 次；只读账号 `sqlm_ro` 写入被拒 |
| **前端保真渲染与安全交互** | `frontend/src/__tests__/fidelity-and-safety.spec.ts` | Vitest (`happy-dom`) | **通过 (8/8)** | `DataGrid` 原样渲染极限 `BIGINT`/`DECIMAL` 且采样自动列宽不截断；`NULL` 灰字、空串 `""` 与不可见空白符开关（`·`/`→`/`↵`/`[NBSP]`/`[ZWSP]`）清晰区分且复制/JSON 导出保真；双击弹出可滚动浮窗；`SessionBanner` 在 `ERR_COMMIT_OUTCOME_UNKNOWN` 下严格无重试按钮 |
| **基础组件单元测试** | `internal/sqlsplit`、`internal/resultbuf`、`internal/repository` | Go `-race` 纯单测 | **通过** | MySQL `DELIMITER`/PG `$$` 切分、内存→JSONL 磁盘溢写/硬上限/TTL 过期、`app.db` SQLite `3.54.1` CRUD/历史/KV |

### PostgreSQL 抽象中立性检验结论

在 MySQL 驱动跑通后接入真实 PostgreSQL `16.16` 检验公共抽象：
- **无需改动**：`DatabaseSwitcher` 采用可选接口设计，PostgreSQL 不实现它即可自动走 `TabSession` 的“非事务状态重建连接、事务中拒绝切库”分支；`SequenceInspector` 采用可选接口，MySQL 零感知。
- **暴露出的唯一真实差异**：PostgreSQL 具备 `Database → Schema → Table` 三层结构，查询会话内需要切换 `search_path`。据此在 `internal/driver/driver.go` 中仅新增了一个单方法可选接口 **`driver.SchemaSwitcher`**（MySQL 驱动未作任何修改）。

### 未验证项目（明确标注为 `待验证`）

- **数据库版本**：MySQL `8.0` / `5.7`（`待验证`）；PostgreSQL `17` / `14` / `12`（`待验证`）；MariaDB `11.4` / `10.5`（`待验证`）；SQLite 用户目标库驱动与 3.2 回归用例（`待验证`）。
- **功能阶段**：网格行内写入与并发冲突检测（Phase 2，`待验证`）；跨库表设计器与五态迁移报告（Phase 3，`待验证`）；SSH 隧道与高级 SSL 双向证书（Phase 2，`待验证`）。
- **桌面平台实机图形验收**：Windows 10/11 WebView2 编译打包与实机运行（`待验证`）；Linux 实体桌面会话（Ubuntu 24.04 GNOME Wayland / Debian 13 / CachyOS KDE/Hyprland + Fcitx5 输入法与 NVIDIA 显卡实测，`待验证`）。

---

## 目录结构

```text
sql-manager/
├── main.go                           # 最小入口：路径解析 → Linux 图形自检 → bootstrap.Run
├── docker-compose.test.yml           # MySQL 8.4 + PostgreSQL 16 真实测试库编排
├── Taskfile.yml                      # 标准化构建、bindings 生成与测试任务
├── sql/001_init.sql                  # 本地元数据配置库 app.db Schema
├── test/{mysql,postgres}/init.sql    # 真实数据库测试表、只读账号与边界数据集
├── docs/                             # 架构、引擎契约、交互规范与路线图设计文档
├── internal/
│   ├── bootstrap/                    # Composition Root（Wails v3 生命周期与静态驱动注册）
│   ├── binding/                      # 极薄 Wails v3 方法适配器与 /stream HTTP Handler
│   ├── service/                      # 纯 Go 用例编排（零 Wails 依赖）
│   ├── session/                      # Track A 元数据池 + Track B 独占会话与取消栅栏
│   ├── driver/                       # 消费侧小接口、TrackedConn 与 mysql / postgres / fake 实现
│   ├── resultbuf/                    # Execute-Once 内存/磁盘溢写快照与大对象流式缓存
│   ├── sqlsplit/                     # MySQL/PG 方言切分、语句分类与只读逃逸识别
│   ├── repository/                   # 本地 app.db SQLite 仓储
│   ├── secret/                       # OS Keyring + AES-256-GCM 降级凭据库
│   ├── platform/                     # Linux Wayland/NVIDIA 启动前图形降级保护
│   ├── config/                       # XDG / AppData / Portable 路径解析
│   ├── entity/                       # 本地持久化实体
│   └── dto/                          # 稳定跨边界 DTO 与结构化 AppError
└── frontend/
    ├── bindings/                     # wails3 自动生成的强类型 TS Bindings
    └── src/
        ├── api/client.ts             # 强类型调用与 AppError 解析
        ├── stores/                   # Pinia：settings / connections / workspace
        ├── components/               # TopBar / ExplorerSidebar / ConnectionModal / SqlEditor / DataGrid / TableBrowseView / QueryTabView / SessionBanner / ValueInspector / TableDdlModal / CommandPalette
        └── __tests__/                # Vitest 保真与安全交互单测
```
