# 临时切回 guilioenricomeille（2026-11-20）

## 后续调整：增加备选

通过管理接口更新：general 首选 guilioenricomeille、第二首选 API `…pI`；
quality 首选 guilioenricomeille、第二首选 cwhyptucsd；Astra SVG 的有序
首选保持不变。API 账号的保护准入包含 general 和 astra_svg，quality 账号
恢复 quality / astra-or-sol 的准入。更新前备份：
`/home/cwhypt/cliproxyapi/.rollback/codex-fallback-20261121T064942Z/config.before.yaml`。

**重要限制：** 当前实现只有在会话尚无绑定账号时按 `preferred_auth_ids`
依序选择。已有绑定的账号不再是可用候选时，`PickKey` 不重新遍历首选列表；
如有受保护账号被排除，则 `PickProtectedFallback` 按候选的账号 priority 和
weight 选择，否则交给宿主选择。因此首选额度耗尽时无法保证已有会话严格
选择第二首选；已有绑定若仍可用也不会因改配置自动切回第一首选。要保证
已有会话的有序回退，需要修改 Guard 代码并另行构建部署验证。
源码已补充此修复：已绑定账号不在可用候选时先依序尝试首选列表，选中后
更新粘性绑定；首选均不可用仍沿用原回退路径。修复已通过本地测试，**尚未
构建或部署到线上**，当前生产仍有上述限制。

以下为更早的临时切换记录，后续调整以上文为准。

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
