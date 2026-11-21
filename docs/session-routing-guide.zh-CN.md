# 会话路由配置维护指南

生产配置位置：CPA `config.yaml` → `plugins.configs.cpa-codex-guard.session_routing`。
带注释的完整上线片段：[releases/0.4.0-routing.yaml](releases/0.4.0-routing.yaml)。

## 先理解三个不同的“优先级”

| 想改变什么 | 修改哪里 | 如何比较 |
|---|---|---|
| 让 Guard 还是另一个插件负责选账号 | Guard 顶层 `priority: 5` | 插件数值越大越优先；宿主只调用第一个有效 Scheduler |
| 账号池先使用哪个层级的账号 | 账号自身的 `priority` | 在可用候选中先最高层；同层按 `weight` 分配 |
| 请求同时符合多个分类时，哪个分类获胜 | `rules` 中规则的位置 | 从上到下，首条全部匹配即停止；category 没有 `priority` 字段 |

**不要为调整探针池去修改 Guard 的 `priority: 5`。** 这会改变插件竞争关系，不能改变账号池上限。
Key Policy X 的鉴权与模型权限检查独立于 Scheduler；当前使用其 `routing_mode: host`。
Guard 返回未处理时，宿主回退到内置选择器，不会再调用第二个 Scheduler 插件。

### 如何修改账号 priority / weight

- **Codex API-key 账号：** 在 CPA `config.yaml` 顶层 `codex-api-key` 列表的对应账号项中修改 `priority` / `weight`，或使用管理界面对应的凭据配置。
- **OAuth 账号：** 在管理界面的对应 OAuth 凭据设置中修改账号优先级/权重。账号信息的查询入口是 `/v8/management/credentials`；这是查询入口，不是本文提供的修改接口。不要把邮箱标签当作 auth ID。
- `weight` 主要影响**同优先级池的新会话分配**；省略默认 1，0 不参与 Guard 轮转。不是每条请求严格按比例，已有会话继续用已绑定账号。
- `preferred_auth_ids` 是指定账号的有序首选，不是权重池；在可用条件下可跨账号 priority 选择。因此 quality 账号 priority=4 仍可被 quality 类首选，不需要提高到 7。
- 所有选择都以宿主提供的可用候选为前提：模型支持、禁用、冷却、已尝试过滤等仍有效。

例如，把探针池从 `priority < 7` 改成 `priority <= 4`，只需改：

```yaml
categories:
  probe:
    max_priority: 4  # 包含 4；保留原来的 exclude_auth_ids
```

这只是修改片段，不要覆盖整个 `categories` 或丢掉其他字段。`max_priority: 6` 的意思是“账号优先级最高允许 6”，**不是给 probe 类设置优先级 6**。池中先选最高可用层，同层按权重轮转；高层没有候选才考虑低层。

## 分类如何运作

1. 先鉴权、模型权限、Guard 校验/修复。
2. 先本地算术/缓存。命中不分类、不新建绑定、不选上游账号；可给已有显式 session 续期。
3. 未命中时，按“调用方 + canonical session”查询绑定。已有绑定继续原分类，不重新判断规则。
4. 新/过期会话取**首条有效 user 文本**判断 `rules`，不是最后一条，也不是 system/developer。
5. 首条匹配规则决定分类；均未匹配使用 `default_category: general`。
6. 按该分类选账号。已有可用绑定优先；首选不可用允许回退，并记录实际选中的回退账号。

当前 quality 门槛是：astra/sol + 明确的 `medium/high/xhigh/max` +（首条 user ≥1000 Unicode 字符 **或** 对话上下文 ≥16000 字符）。缺失 effort 不猜测 medium；允许可信宿主 metadata 补充。
上下文包含 user/assistant/tool 结果文本，不包含 system/developer、工具定义、图片、加密 reasoning 或函数调用参数。

### AND / OR 与规则顺序

```yaml
rules:
  - category: translation
    model_include_regex: '^gpt-'
    text_all_regex:
      - '(?i)^translate this document:'
      - '(?i)target language: french'
```

上例必须同时满足模型约束和**两条**文本正则。如果想匹配英文模板**或**中文模板，应写两条 `category: translation` 规则。
要让翻译任务优先于 quality，把翻译规则放到 quality 规则前。`categories` 字典排列顺序不影响分类。
正则采用 Go RE2；不支持前后查找/lookaround、反向引用。推荐用 YAML 单引号保留 `\s`、`\b` 等反斜杠。

## 如何新增 category

以新增 `translation` 为例，**分别合并**下面两部分到现有配置，不能重复创建 `categories:` 或 `rules:` 键。

第一步，在 `categories` 定义账号：

```yaml
categories:
  translation:
    preferred_auth_ids:
      - '<从凭据管理复制的精确 auth ID>'
      - '<第二首选 auth ID，可选>'
```

第二步，在 `rules` 的合适位置定义分类条件：

```yaml
rules:
  - category: translation
    text_all_regex: ['(?i)^translate this document:']
  - category: translation
    text_all_regex: ['^请将以下文档翻译成法语：']
```

第三步，如果这个账号要**专用**，添加准入并考虑从探针池排除：

```yaml
protected_auths:
  '<translation 的真实 auth ID>':
    categories: [translation]
    model_include_regex: '^gpt-'
```

`preferred_auth_ids` 只表示这类请求首选谁，**不阻止别的分类回退到该账号**。专用限制由 `protected_auths` 提供；它也会过滤回退候选。不能给现有 quality 保留账号新增分类首选而不更新准入，否则选不到它。
当前准入限制只在 Guard 为活动 Scheduler 时有效；若 Guard 被禁用、熔断或被其他 Scheduler 抢占，不构成宿主级全局隔离。

### 新增探针池分类的额外要求

类别使用 `max_priority` 池时，对应规则必须同时配置：

- `text_all_regex`：已确认的模板，不是宽泛关键词。
- `standalone: true`：独立单条 user，拒绝多轮、工具和 continuation。
- 正数 `max_input_bytes`：限制**整个 JSON 请求字节数**。
- `user_agent_regex` 或非空 `caller_scopes`：限定客户端提示或已鉴权调用方。

推荐同时写 `disallow_tools: true` 明确意图。UA 可伪造，不是身份认证；`caller_scopes` 是宿主给出的不透明调用方范围，不是 key 名称或 key 后缀。池类别不能作为 `default_category`。

## 字段速查

| 字段 | 含义 |
|---|---|
| `binding_ttl: 1h` | 滑动绑定有效期，持续请求会续期；不是每整点强制重分类 |
| `state_file` | 绑定状态文件路径，不是编辑规则的位置 |
| `default_category` | 新会话未命中任何规则时使用的分类 |
| `models: [...]` | 精确模型名列表，列表内任选其一 |
| `model_include_regex` | 模型名正则；顶层控制路由范围，rule 内进一步限制 |
| `reasoning_efforts: [...]` | 接受的 effort 列表，列表内任选其一；不自动推断默认值 |
| `min_user_chars` | 首条有效 user 文本的 Unicode 字符数下限 |
| `min_context_chars` | 实际对话上下文字符数下限 |
| `max_input_bytes` | 整个请求 JSON 的字节数上限，不是字符数/Token 数 |
| `preferred_auth_ids` | 精确账号 ID 的有序首选列表 |
| `max_priority` | 池可接受的账号 priority 上限（包含边界） |
| `exclude_auth_ids` | 从该类别可选择账号中排除的 ID |
| `protected_auths` | 某账号允许的分类及模型；回退也必须遵守 |

## 修改后如何生效与验证

- **2026-11-20 上线遗留状态：** CPA 的 config 文件监听丢失尚未通过维护重启恢复。当前只保存 YAML 不保证运行时生效；应在管理界面 Guard 的完整配置编辑器保存，或调用 `PUT /v0/management/plugins/cpa-codex-guard/config` 应用**完整 Guard mapping**。不要只把 `session_routing` 片段作为完整配置提交，以免丢失其他 Guard 参数。认证沿用现有管理凭据，不在文档写明文密钥。
- 管理保存会重新序列化 YAML，可能丢失手工注释；本指南和带注释发布片段是持久参考。保存后重新核对磁盘及运行时配置。
- 直接编辑文件时保留 inode，避免用临时文件 rename/原子替换配置；发布脚本已改为原地写入。不要为恢复监听擅自重启 CPA。
- 修改规则、首选列表通常只影响新/过期绑定；同一活跃会话不会自动从 general 升级 quality，或因短追问降级。保留账号准入会在每次 Scheduler 调用重新检查。
- 验证请用**新 session**、确保未命中缓存，核对请求日志 `API REQUEST` 的实际 `auth_id`；绑定状态仅说明当前偏好，不能单独证明发生过上游调用。无需直接编辑/删除生产绑定状态。
- 非匹配模型、缺少可分类 user 文本、缺少可信 caller scope 会交回宿主；受保护账号仍按活动 Guard Scheduler 的准入限制过滤。
- 配置错误（未知分类、无效正则、无效 TTL 等）会拒绝新路由配置。修改后检查管理接口的生效状态及请求日志，不要仅凭保存成功判断。

历史实现文档：[session-content-routing.md](session-content-routing.md)；本次上线与文件监听问题：[releases/0.4.0-deployment.md](releases/0.4.0-deployment.md)。
