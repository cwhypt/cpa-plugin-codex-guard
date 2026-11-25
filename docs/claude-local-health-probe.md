# Claude local health probe

When probe caching is enabled, the exact observed non-streaming Claude Messages
request (`claude-haiku-5-5`, `max_tokens: 1`, single user message `.` and no
other request fields) gets a local Claude-shaped response with zero usage. It
does not contact the upstream and therefore does not establish provider health.
Requests with additional fields, different models or content, or streaming pass
through normally. The host must authenticate and authorize requests before the
interceptor; the shortcut must not be used to bypass those checks.
