# 04. 跨平台构建、分阶段验收与实测矩阵

本文档定义 **Windows 与 Linux（Ubuntu 24.04+、Debian 13+、CachyOS）构建打包规范、Linux 图形兼容防护、分阶段验收目标与关键回归场景，以及各驱动版本验证状态追踪**。行为规则的权威定义统一见 [02-database-engine-design.md](./02-database-engine-design.md)。

---

## 1. 开发构建基线与跨平台打包

### 1.1 工具链与编译依赖
- **基础工具链**：Go `1.26+`、Node.js `22 LTS`、`pnpm 10+`、`wails3` CLI、Taskfile（`Taskfile.yml`）。
- **Ubuntu 24.04+ / Debian 13+**：`build-essential`, `pkg-config`, `libgtk-4-dev`, `libwebkitgtk-6.0-dev`（运行时仅依赖系统 `libgtk-4-1` 与 `libwebkitgtk-6.0-4`）。
- **CachyOS / Arch Linux**：`base-devel`, `pkgconf`, `gtk4`, `webkitgtk-6.0`。
- **Windows 10 / 11**：Go `1.26+` + Edge WebView2 Runtime（因 SQLite 采用纯 Go `modernc.org/sqlite`，Windows 编译无需 CGO）。

### 1.2 分发产物矩阵

| 平台 | 产物 | 构建方式与更新策略 |
| :--- | :--- | :--- |
| **Windows (`amd64`)** | `sql-manager-setup-x64.exe`（NSIS）+ `portable.zip`（含 `portable.flag`） | Taskfile + NSIS；安装版支持 Wails v3 内置签名更新器 |
| **Ubuntu 24.04+ / Debian 13+** | `sql-manager_<ver>_amd64.deb` | `nfpm` 打包并声明 GTK4 / WebKitGTK 6.0 / `libsecret-1-0` 依赖；检测到包管理器安装时仅提示升级命令 |
| **CachyOS / Arch Linux** | `PKGBUILD`（AUR `sql-manager-bin` / `sql-manager`） | CI 自动刷新校验和；原生复用系统 GTK4/WebKitGTK 6.0，包体极小 |
| **全 Linux 通用** | `SQL-Manager-<ver>-x86_64.AppImage` | 精简 GTK4 + WebKitGTK 6.0 单二进制 AppImage（约 8.3 MB，Type 2 FUSE3；`AppRun` 通过 `ldd -r` 预检并在缺库时弹出原生图形错误窗口） |
| **macOS（二期）** | `sql-manager-universal.dmg` | Apple Silicon + Intel 通用二进制与公证 |

---

## 2. Linux 桌面兼容防护（Ubuntu / Debian / CachyOS）

针对 Linux 下 **Wayland（GNOME / KDE Plasma / Hyprland）+ NVIDIA 专有驱动** 可能导致 WebKitGTK 6.0 白屏或 `Error 71` 崩溃的问题，在 `main.go` 调用 Wails 初始化之前执行 `internal/platform/graphics_linux.go` 自检：
1. **显式配置与命令行覆盖**：支持 `--disable-dmabuf`、`--software-gl` 与独立轻文件 `graphics.json`（不依赖 `app.db` 即可在进程启动最早期读取）。
2. **启发式安全降级**：在 `自动` 模式下，检测到 NVIDIA 专有驱动且 `XDG_SESSION_TYPE=wayland` 时自动补齐 `__NV_DISABLE_EXPLICIT_SYNC=1`；若通过启动锁 `startup.lock` 发现上次启动在 3 秒内异常崩溃，下次自动降级启用 `WEBKIT_DISABLE_DMABUF_RENDERER=1` 并在设置页给出提示。
3. **非 WebGL 界面栈与桌面集成**：界面全面采用 CSS + DOM 虚拟滚动（不强依赖 WebGL），打包内置 `.desktop` 文件（支持 `.db`/`.sqlite` 关联），并在验收时覆盖 Ubuntu 24.04（GNOME Wayland）、Debian 13、CachyOS（KDE Wayland + Hyprland）下的 Fcitx5/IBus 中文输入法、剪贴板与 HiDPI 小数倍缩放。

---

## 3. 分阶段验收目标与核心检验场景

原则：**四库共用同一套验收目标，各用适合自己方言的 SQL 与故障注入条件；先验收查询，再开放网格写入，最后开放改表。**

### 3.1 四阶段核心验收规则表

| 阶段 | 可观察验收目标（权威定义见 `02`） | 关键检验场景（必须自动化覆测） |
| :--- | :--- | :--- |
| **阶段 1A/1B：查询 — 取消与断线** | `execID` 仅标识请求；须通过取消栅栏确认上次执行结束且迟到取消信号不会再影响下一条，才可复用同一物理连接，否则停止复用；界面准确区分“语句已取消”“原会话已丢失”“提交结果未知”。 | ① **迟到取消竞态**：取消 A 已通过校验，A 自然结束，B 已排队 → 验证迟到的取消信号不能停掉 B（若无法确认则停止复用旧连接并显式回报 `SessionLost`）；<br>② **会话保活**：取消慢查询后验证 MySQL/PG 原连接及临时表完好（PG 事务内标 `AliveFailedTx`）；<br>③ **`COMMIT` 断线**：模拟发 `COMMIT` 后确认前断线，返回 `ERR_COMMIT_OUTCOME_UNKNOWN`。 |
| **阶段 1A/1B：查询 — 只读与翻页** | 区分客户端防误写与数据库真实权限（SQLite 支持只读打开文件）；表浏览实时读取、按稳定键排序（不承诺并发改排序值时不重不漏）；编辑器 SQL 只执行一次，翻页读有容量上限的快照，过期绝不自动重跑。 | ① **受限写入拒绝**：只读账号（及客户端只读连接、SQLite `mode=ro`）从编辑器写业务表被拒绝；<br>② **Execute-Once 验证**：在独立可写测试库执行带副作用语句（如 `INSERT ... RETURNING *` 或触发计数器的语句）并翻到第 2 页，校验数据库仅执行 1 次；<br>③ **上限与释放**：超大结果正确截断、关 Tab 释放缓存文件、过期返回 `ERR_RESULT_EXPIRED` 且不自动重跑。 |
| **阶段 2：网格写入** | 三维度独立判定（允许写 / 行可唯一定位 / 批次可回滚）；改删要求显式主键或覆盖全表的非空唯一键；用可靠版本或约定原值检测并发冲突，区分冲突与未改变值；预览与执行共用参数化 `ChangePlan`；无稳定键有权限可新增；非事务表首版不开放“整批提交”。 | ① **SQLite 键边界**：显式 `INTEGER PRIMARY KEY` 允许更新/删除，**隐式 `rowid` 拒绝用于延迟提交**；<br>② **非事务表拒绝整批**：MySQL/MariaDB 的 MyISAM 表即使有主键也不开放整批提交（返回 `ERR_BATCH_NON_TRANSACTIONAL`）；<br>③ **双会话冲突与 No-op 区分**：A 先改提交、B 后提交命中 `ERR_CONCURRENCY_CONFLICT` 回滚且保留 B 草稿；未改变值不误报冲突；<br>④ **无稳定键表**：允许按权限 `INSERT`，拒绝 `UPDATE`/`DELETE`。 |
| **阶段 3：表设计器** | 原子性由版本、存储引擎与具体 DDL 共同决定；逐步准确回报 `Applied` / `RolledBack` / `Failed` / `NotRun` / `OutcomeUnknown` 五态；确认丢失时禁止提示直接重试、须先核对结构；SQLite 同步恢复依赖视图及挂在视图上的 `INSTEAD OF` 触发器。 | ① **确认丢失不诱导重试**：模拟 MySQL/MariaDB 改表确认前断线，报告 `StepOutcomeUnknown` 且 `RequireInspectDDL=true`；<br>② **多步非事务部分生效**：模拟 MySQL 多步 DDL 第 2 步失败，准确回报第 1 步 `Applied`、第 2 步 `Failed`；<br>③ **SQLite 可写视图全链路**：改表后既能查依赖视图，也能通过原 `INSTEAD OF` 触发器经视图修改底表；删除视图依赖列时验证整事务回滚。 |

### 3.2 必跑边界回归用例（源自 SQLite 3.38.2 实测验证）

此前已用 SQLite 3.38.2 内存库实测确认的三个真实边界问题，必须直接改写为客户端自动化回归测试：
1. **排序列并发修改边界**：表浏览按某列排序翻页期间修改该排序列值，观察跨页重复/漏读现象，回归校验界面与文档不做虚假“实时分页绝对不重不漏”承诺，而编辑器快照翻页不受并发写影响。
2. **`VACUUM` 重排隐式 `rowid` 回归**：构造无显式主键表并执行 `VACUUM` 复现隐式 `rowid` 指向变化；回归断言客户端对无显式主键的 SQLite 表**拒绝使用隐式 `rowid` 进行网格延迟更新/删除**，而对显式 `INTEGER PRIMARY KEY` 表稳定通过。
3. **视图 `INSTEAD OF` 触发器级联丢失回归**：构造含底表触发器 + 依赖视图 + 挂在视图上的 `INSTEAD OF UPDATE/INSERT` 触发器的 SQLite 库，执行表结构重建；回归断言**改表后不仅视图可查，通过该视图执行 `UPDATE/INSERT` 修改底表依然成功**，且删除视图引用列时整事务无损回滚。

---

## 4. 驱动与版本实测矩阵（按阶段更新）

状态取值约定：`已验证`（CI 自动化全绿） / `待验证`（已纳入范围但尚未跑通实测） / `不适用`（该数据库无此特性）。

| 验证顺序 | 数据库与版本 | 定位 | 取消与断线 | 只读与翻页 | 网格写入（含键/引擎） | 表设计器（含部分失败） | 实测备注 |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **1** | **MySQL 8.4.13 (InnoDB)** | 默认首验版本 | **已验证** | **已验证** | 待验证 | 待验证 | `mysql:8.4` 真实容器实测通过（含 `KILL QUERY`、迟到取消竞态、Execute-Once、只读账号拒写、自动提交/COMMIT 断线报结果未知） |
| **1** | **MySQL 8.0 / 5.7** | 主力 / 承诺下限 | 待验证 | 待验证 | 待验证 | 待验证 | 5.7 无原子 DDL，另需覆盖 MyISAM 非事务表完整写入验收 |
| **2** | **PostgreSQL 16.16** | 第二个验证驱动 | **已验证** | **已验证** | 待验证 | 待验证 | `postgres:16` (`16.16-1.pgdg13+1`) 实测通过；验证切库重建连接、`SchemaSwitcher`、`CancelRequest`、`AliveFailedTx`、`INSERT ... RETURNING` Execute-Once |
| **2** | **PostgreSQL 17 / 14 / 12** | 主力 / 承诺下限 | 待验证 | 待验证 | 待验证 | 待验证 | 需在 CI 矩阵中补测 |
| **3** | **MariaDB 11.4.13** | 主力验证版本 | **已验证** | **已验证** | 待验证 | 待验证 | `mariadb:11.4` (`11.4.13-MariaDB-ubu2404`) 实测通过；验证原生 `SEQUENCE` 浏览、`KILL QUERY` 与迟到取消竞态、`INSERT ... RETURNING *` Execute-Once、MyISAM/无主键判定、只读拒写与断线报结果未知 |
| **3** | **MariaDB 10.5** | 承诺下限 | 待验证 | 待验证 | 待验证 | 待验证 | 需在 CI 矩阵中补测 |
| **4** | **SQLite（内嵌 `3.54.1`）** | 内嵌单版本 | **已验证** | **已验证** | 待验证 | 待验证 | 纯 Go `modernc.org/sqlite v1.63.0` 真实文件库实测通过：不静默建空库、`mode=ro` + `query_only=ON` 引擎级拒写、`sqlite3_interrupt` 中断保活、**3.2 `VACUUM` 隐式 `rowid` 重排 (`2→1`) 回归与显式主键判定** |

> 注：非事务引擎（MyISAM）用例仅适用于 MySQL / MariaDB，对 PostgreSQL 与 SQLite 标为 `不适用`；`SEQUENCE` 浏览仅适用于 PostgreSQL 与 MariaDB，对 MySQL 与 SQLite 标为 `不适用`；**绝不把 `driver/fake` 模拟驱动通过算作真实数据库通过**。

---

## 5. 分阶段开发路线图与门禁

### Phase 0：工程骨架与基础通路（已完成 Linux 端验证；Windows 实机打包待验证）
- [x] 初始化 Go 1.27.4 / Wails v3.1.0 / Vue 3 + TS + Tailwind CSS 4 + CodeMirror 6 + TanStack Virtual 紧凑桌面工作台骨架，已登记 `modernc.org/sqlite v1.63.0` 内嵌 SQLite 版本为 **`3.54.1`**。
- [x] 落地 `bootstrap → binding → service → repository / driver` 分层、`internal/platform` Linux 图形自检、`sql/001_init.sql` 本地 `app.db`、`internal/secret` 密钥环与降级加密库，以及支持故障注入的 `internal/driver/fake/`（仅用于纯单测，不替代真库验收）。
- [x] **已验证（Linux）**：Linux（GTK4 `4.14.5` + WebKitGTK 6.0 `2.52.6`）原生桌面二进制与精简 GTK4 AppImage 已在真实 Linux 桌面通过验证；`internal/session` 通过基于 `driver/fake` 的未确认取消→`SessionLost` 与写后断线→`CommitOutcomeUnknown` 单元测试。（Windows 实机编译与打包待验证）

### Phase 1A：MySQL 8.4 查询工作台纵向验收（已完成真库验收）
- [x] 实现 **MySQL 驱动（`go-sql-driver/mysql`）** 与 `internal/session`（双轨连接 + 旁路 `KILL QUERY` 取消栅栏）、`internal/resultbuf`（内存/临时文件快照）、`internal/sqlsplit`。
- [x] 打通完整人性化查询工作台：连接管理 → 双击表实时浏览 → CodeMirror 6 执行与停止 SQL → 编辑器 Execute-Once 快照翻页 → 关闭标签释放连接与快照缓存。
- [x] **真库验收（真实 MySQL `8.4.13` + InnoDB）**：
  1. 停止仅影响当前语句，5 轮迟到取消竞态未误杀排队查询 B，流关闭且状态明确才复用连接，无法确认则提示原会话已结束；
  2. 编辑器翻页不重跑 SQL（存储过程副作用计数 = 1），快照过期明确提示且不自动重跑；实时表浏览与编辑器快照严格分离；
  3. `BIGINT`/`DECIMAL` 精度完整、`NULL` 与空串明确区分；真实只读账号 `sqlm_ro` 写表被数据库 Error 1142 拒绝；独立可写库 `sqlm_write_test` 验证自动提交写入与显式 `COMMIT` 确认丢失均报 `ERR_COMMIT_OUTCOME_UNKNOWN`（`RetrySafe=false`）且界面无重试按钮；
  4. 已交付可复现源码、启动命令、`docker-compose.test.yml`、实测版本号与结果记录（见根目录 `README.md`）。

### Phase 1B：PostgreSQL 抽象中立性检验（已完成真库验收）
- [x] 实现 **PostgreSQL 驱动（`pgx/v5`）**，用真实 PostgreSQL `16.16` 验证公共接口没有写死 MySQL 假设（切库自动重建连接、`Database → Schema → Table` 三层浏览、`SEQUENCE` 浏览、`CancelRequest`、`AliveFailedTx`、原生 `INSERT ... RETURNING *` Execute-Once）；**仅在暴露真实差异处新增 1 个单方法可选接口 `driver.SchemaSwitcher`**，MySQL 驱动与公共核心零侵入。

### Phase 1C：MariaDB 与 SQLite 分别验收 — 门禁 Gate 1（已完成真库验收，Gate 1 达成）
- [x] 完成 **MariaDB `11.4.13`**（复用 MySQL 传输层 + 原生 `SEQUENCE` 枚举 + 超时错误码 `1969` + `MariaSQL` 编辑器方言）真库全量验收，通过取消/迟到取消竞态、`INSERT ... RETURNING *` Execute-Once、MyISAM/无主键判定、只读账号拒写与断线报结果未知。
- [x] 完成 **SQLite `3.54.1`** 纯 Go 目标库驱动（原生文件选择器、不静默建空库、引擎级 `mode=ro` + `PRAGMA query_only = ON`、单文件对象树与 `SQLite` 编辑器方言）真库全量验收，并提前通过 **3.2 `VACUUM` 隐式 `rowid` 重排 (`2→1`) 回归用例**（严格拒绝隐式 `rowid` 与部分唯一索引作为稳定行键）。
- [x] **Gate 1 门禁达成**：MySQL `8.4.13`、PostgreSQL `16.16`、MariaDB `11.4.13`、SQLite `3.54.1` 全部通过查询/取消/只读/翻页实测验收，可正式进入 Phase 2。

### Phase 2：安全网格写入、SSH 隧道与智能补全 — 门禁 Gate 2（2–3 周）
- [x] **Phase 2C（已完成真库验收）**：实现零本地监听端口 SSH 隧道池（TOFU 指纹、可选长效保存/每次输入/弹窗覆盖保存、`tunnelConn` 通道超时与防死锁升级）与完整 5 模式 SSL/TLS（含 `verify-ca` 证书链严校、`verify-full`、`mTLS`、禁明文降级），并在真实 MySQL 8.4 / MariaDB 11.4 / PostgreSQL 16 Docker 集群完成全链路实测。
- [ ] 实现后端 `InspectTableEditability` 三维度判定、`internal/changeset` 参数化 `ChangePlan` 与乐观并发冲突检测（MySQL 开启 `clientFoundRows=true`）。
- [ ] 上线网格行内编辑（无稳定键仅允许新增、SQLite 禁隐式 `rowid`、MyISAM 禁整批提交、冲突保留草稿）、Value Inspector、基于 Schema 的 SQL 自动补全、查询历史与生产高危 SQL 二次确认。
- [ ] **Gate 2 门禁**：通过 3.1“网格写入”全部场景与 3.2 的 `VACUUM` `rowid` 回归用例（行定位判定部分已于 Phase 1C 回归通过，Phase 2 补齐端到端写入校验）。

### Phase 3：透明表设计器与流式导入导出 — 门禁 Gate 3（2–3 周）
- [ ] 各驱动实现 `MigrationPlanner` 与 `MigrationExecutor`，输出准确原子性边界与五态 `MigrationReport`；实现 SQLite 含依赖视图与 `INSTEAD OF` 触发器的完整重建引擎。
- [ ] **Gate 3 门禁**：通过 3.1“表设计器”全部场景与 3.2 的 SQLite 可写视图 `INSTEAD OF` 触发器回归用例后，开放表设计器执行入口。
- [ ] 上线表结构设计器界面与流式导入导出引擎（CSV / JSON / SQL Insert / 大 `.sql` 流式执行），发布 `v0.5` 公测版（Windows NSIS/Zip、`.deb`、CachyOS `PKGBUILD`、AppImage）。

### Phase 4：进阶能力与 macOS 扩展（持续演进）
- [ ] 可视化 ER 图（`@vue-flow/core`）、`EXPLAIN` 树形分析、结构对比与可选 `pg_dump`/`mysqldump` 图形化备份。
- [ ] 启用 Wails v3 内置自动更新器、macOS 适配与 `.dmg` 公证发布，按需探索 Wails v3 Server Mode。
