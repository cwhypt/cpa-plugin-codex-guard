# Responses reasoning summary 缓存回放

这次扩展只放宽**响应样本**的可回放范围：允许若干未加密 reasoning summary 输出，最后接一个已完成的 assistant 纯文本消息。请求仍必须满足原有的无状态、单条纯文本限制；带工具、conversation / previous_response_id 或非空 include 的请求不因此变得可缓存。

## 保留的边界

- 最多 16 个输出项，每个 reasoning 项最多 16 个 summary part。
- reasoning 只接受明确允许的字段，summary part 只接受 `type: summary_text` 和字符串 `text`。
- 非空 `encrypted_content`、未完成状态、实际 reasoning content、未知字段、重复/空输出 ID 都拒绝回放，并交回上游。拒绝样本也不计入缓存激活数量。
- 最后仍必须是已完成的单个 assistant 文本输出，不能混入工具调用或多个消息。

## 容易出错的地方

- **ID 必须一起改：** 每次命中新建 response / reasoning / message ID；usage attribution 的相应条目使用同一份映射，不能只改消息 ID。
- **流式索引必须对应最终 output：** reasoning 项占据自己的 `output_index`；每段 summary 使用自己的 `summary_index`，最终消息不能固定为索引 0。
- **summary 不是最终答案：** 回放输出 summary 事件，但缓存答案仍是 assistant 的文本。流式文本提取以完整完成事件中的输出校正，不能把 reasoning delta 拼入答案。
- **usage 保留真实值：** reasoning token 计数和 provider-specific attribution 不因最终答案很短而被删掉或重新估算。
- **不支持就回退：** 无法完整重建引用的响应不能伪造为成功的缓存结果。原始样本只读解析，重复命中不改写存储中的 ID。

验证命令（插件仓库内）：

```sh
go test -race ./internal/probecache
```

回归检查涵盖单/多 reasoning、空/多 summary、流式索引与 ID/usage 一致性、非流式输出、summary 不混入答案，以及畸形/加密样本的回退和缓存激活阻断。
