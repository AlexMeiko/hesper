# Hesper 设计文档目录

本项目是一款面向 **MySQL、PostgreSQL、MariaDB 和 SQLite** 的跨平台桌面数据库客户端，优先支持 **Windows (10/11)** 与 **Linux (Ubuntu 24.04+、Debian 13+、CachyOS)**，后续平滑扩展至 **macOS**。

四款数据库共用同一套工作台与高频交互语义；方言语法、对象类型、可编辑性与事务原子性由各驱动按真实数据库能力解释。验证顺序按 **MySQL (默认 8.4 + InnoDB) 第一 → PostgreSQL 第二 → MariaDB 与 SQLite 随后分别验收** 推进，首批交付范围始终覆盖全部四款数据库。

---

## 核心技术栈

| 维度 | 最终选型 | 说明 |
| :--- | :--- | :--- |
| **桌面框架** | **Wails v3** (v3.1+) | Go 后端 + 系统原生 WebView，多窗口/菜单/托盘/自动生成 TS Bindings |
| **后端语言与分层** | **Go 1.26+** (`bootstrap → binding → service → repository / driver`) | Wails 锁死在 `bootstrap`/`binding`；`repository` 管本地配置库，`driver`/`session` 访问用户库 |
| **前端框架** | **Vue 3 + TypeScript + Vite + Pinia** | `<script setup>` 高效构建复杂桌面表单与工作台 |
| **工作台与重型组件** | **Tailwind CSS 4 + 自建轻量基础组件与标签栏/分屏 + CodeMirror 6 + `@tanstack/vue-virtual`** | 紧凑桌面 IDE 风格、方言 SQL 补全、行/列双虚拟滚动网格 |
| **数据库驱动** | `go-sql-driver/mysql` / `pgx/v5` / `modernc.org/sqlite` | 静态显式注册，底层驱动直连 (非 ORM)，SQLite 纯 Go 免 CGO |
| **Linux 运行基线** | **GTK4 + WebKitGTK 6.0** | 覆盖 Ubuntu 24.04+、Debian 13+、CachyOS；AppImage 缺库时弹出原生图形错误窗口，内置 Wayland/NVIDIA 图形降级保护 |

---

## 共同用户体验基线

- **高频操作走短路径**：双击表直接浏览数据，在编辑器执行 SQL 直接看结果；当前连接与目标库始终可辨；普通查询成功不弹窗，SQL 预览与执行明细按需展开。
- **会改变下一步决定的事实放在操作旁**：表不可编辑时说明原因，改表不能整体回滚时在确认前明示，提交或改表确认丢失时醒目提示先核对现状。
- **统一语义、按能力呈现**：四库保持相同的按钮含义，当前数据库不支持的对象 (如 SQLite 的 Schema / 序列) 不出现在对象树。

---

## 支持版本与验证状态入口

> 下表为各阶段验收的权威追踪入口，按实际运行结果将状态更新为 `已验证`、`待验证` 或 `不适用` (**不把模拟驱动通过算作真实数据库通过**)。详细验收用例见 [04-build-packaging-roadmap.md](./04-build-packaging-roadmap.md)。

| 验证顺序 | 数据库 | 承诺范围 | 重点/下限版本 | 查询与取消 | 只读与翻页 | 网格写入 | 表设计器 | 备注 |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **1** | **MySQL** | 5.7 / 8.0 / 8.4 | **8.4 (实测通过)**；8.0、5.7 (待验证) | 已验证 (8.4) | 已验证 (8.4) | 后端 2A 已验证 (8.4)；前端 2B 待实施 | 待验证 | 真实 MySQL 8.4 + InnoDB 已通过 Phase 1A、2C (SSH+TLS) 与 2A (后端安全网格写入) 全量验收 |
| **2** | **PostgreSQL** | 12+ | **16 (实测通过)**；17、14、12 (待验证) | 已验证 (16) | 已验证 (16) | 后端 2A 已验证 (16)；前端 2B 待实施 | 待验证 | 真实 PostgreSQL 16 通过 Phase 1B、2C (SSH+mTLS) 与 2A (后端安全网格写入) 全量验收 |
| **3** | **MariaDB** | 10.5+ / 11.x | **11.4 (实测通过)**；10.5 (待验证) | 已验证 (11.4) | 已验证 (11.4) | 后端 2A 已验证 (11.4)；前端 2B 待实施 | 待验证 | 真实 MariaDB 11.4 通过 Phase 1C、2C 与 2A (后端安全网格写入) 全量验收 |
| **4** | **SQLite** | 3.x (内嵌) | 内嵌 `modernc.org/sqlite v1.63.0` → **SQLite 3.54.1 (实测通过)** | 已验证 (3.54.1) | 已验证 (3.54.1) | 后端 2A 已验证 (3.54.1)；前端 2B 待实施 | 待验证 | 真实文件库通过 Phase 1C 与 2A 全量验收 (含严格主键、`STRICT`/`WITHOUT ROWID`、`table_xinfo` 生成列、No-op 触发器与 `VACUUM` 回归) |

---

## 文档导航（单一事实源）

遵循**“同一条规则只在一处定义，其他文档引用它”**原则，各文档职责边界如下：

| 文档 | 职责（本文件为以下内容的唯一权威来源） |
| :--- | :--- |
| [**README.md**](./README.md) | 四库产品范围、共同体验基线、支持版本与验证状态入口 |
| [**01-system-architecture.md**](./01-system-architecture.md) | 技术栈清单、模块分层与依赖方向、小接口由谁消费、状态由谁持有、跨界通信通道与本地安全存储 |
| [**02-database-engine-design.md**](./02-database-engine-design.md) | 驱动接口与能力模型、取消与断线状态机、只读与翻页语义、网格写入与并发冲突规则、跨库改表计划与五态迁移报告的**唯一权威定义** |
| [**03-ui-ux-and-frontend.md**](./03-ui-ux-and-frontend.md) | 高频短路径交互流程、异常提示落点与用户决策引导、前端状态归属与快捷键规范 |
| [**04-build-packaging-roadmap.md**](./04-build-packaging-roadmap.md) | 跨平台构建与 Linux 适配、分阶段验收目标与回归用例、各驱动版本实测矩阵 |
