# 临时切回 guilioenricomeille（2026-11-20）

通过 Guard 完整配置管理接口应用，磁盘与接口返回的配置一致。原配置备份：
`/home/cwhypt/cliproxyapi/.rollback/codex-routing-20261121T064510Z/config.before.yaml`。
该文件含敏感配置，不要公开。恢复时先核对其他配置变更，再仅恢复相关
`session_routing` 字段，通过完整 Guard mapping 保存；不要直接覆盖整个配置。

- general 和 quality 的新会话首选 `codex-guilioenricomeille@gmail.com-plus.json`。
- 为防已有绑定继续选择原账号，`cwhyptucsd` 保留原分类限制，暂以
  `model_include_regex: '^$'` 拒绝实际模型；`codex:apikey:5d99976c93e4`
  仅允许 `astra_svg` 分类与 Astra 模型。
- 为已确认的 SVG 模板新增 `astra_svg` 规则，置于原 SVG 规则前：Astra
  首选 guilioenricomeille，第二首选上述 API 账号；其他模型的 SVG 和糖果
  仍为原 benchmark 规则。探针池设置未变。
- 现有会话分类不会因为规则变化自动重分；已有 benchmark SVG 会话在
  绑定过期或新建 session 之前不会进入 `astra_svg`，因此其第二首选不保证
  按新规则生效。已有 general/quality 会话的被限制账号会被候选过滤，随后
  走受保护账号回退：按可用账号优先级选择，并非无条件保证选中目标账号。
- 模型能力、冷却、可用性、已尝试账号和权重均影响候选；未通过实际上游
  请求验证选路，也未修改持久会话绑定。Guard 非活动 Scheduler 时该准入
  限制不构成全局隔离。
