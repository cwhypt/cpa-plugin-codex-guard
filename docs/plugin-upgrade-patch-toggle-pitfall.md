# 插件升级规范：走版本 bump，不碰 PATCH 开关

> 来源：`cpa-plugin-tg-notify` `feat/test-window-isolation` 分支实测（`ff3d26d` timestamps 功能上线过程，
> `8d51093` v0.1.1、`dbdc176` v0.1.2 两次热重载验证）。
> 适用所有 CPA 插件（tg-notify / codex-guard / key-policy …），Linux 宿主侧。

## 现象

TG 通知中间断过一阵。排查确认是人为用管理面 **PATCH `enabled` 开关** toggle 插件导致的：

- `PATCH enabled=false`：注销成功，插件下线。
- `PATCH enabled=true`（**同版本号**重注册）：走不完——无日志、无报错，静默卡住，插件回不来。

## 两条规律（实测结论）

1. **同版本 toggle 是死路**——关得掉、开不回来。`plugin.Open` 句柄是 sticky 的，
   同版本 off/on 只会原地复活旧句柄（`/proc/<pid>/maps` 仍是旧版），重注册流程静默挂起。
2. **版本 bump + 手改 `config.yaml` 是活路**：文件监听触发完整 reload，
   v0.1.1（16:53:58）与 v0.1.2 两次都一次成功。

附带要求：**`store.version` 必须同步改**，否则选择器按旧版本 pin 住新文件——
这就是 v0.1.1 一度不被选中的原因（`config.yaml` 里该插件 `store.version` 必须等于
新 `.so` 文件名版本，否则宿主静默跳过加载，无 error 日志）。

## 标准升级步骤（以后统一走这条路）

```bash
# 1. 版本常量 bump（X.Y.Z → 新版本）
# 2. 构建（tg-notify 别漏 -tags cshared，见各仓构建文档）
# 3. 拷 .so + .h 到插件目录（旧版保留原位即回滚手段）
# 4. 手改 config.yaml：该插件 store.version → 新版本
#    （config.yaml 被 gitignore，不入库，只改线上）
# 5. 等文件监听触发热重载
# 6. maps + 日志双确认（/proc/<pid>/maps 是新版 + 日志有完整 reload 记录）
```

回滚：把 `.so` 拷回旧版 + `store.version` 改回旧版本，重载即可。

## 禁止事项

- **不再用 PATCH `enabled` 开关做插件升级/重载**。开关只保留语义上的启用/禁用，
  任何“换版本”操作一律走上面的 bump 流程。

## 关联文档

- `docs/cpa-plugin-tg-notify.md` §8（版本切换语义、store.version 三处同步、cshared 构建坑）
- `tg_notify.md`（插件加载匹配规则：宿主按 `store.version` 匹配 `.so`）
- `key-policy-host-routing.md`（`store.version` 是期望版本过滤器 + 回滚示例）
