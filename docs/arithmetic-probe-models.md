# Arithmetic health-check model handling

The exact provider health-check arithmetic template is calculated locally for
any nonempty model identifier when probe caching is enabled. The existing strict
request fields, instructions, example questions, operand limits and stream/token
checks still apply. This shortcut bypasses upstream model execution and reports
synthetic usage; it cannot establish whether a particular provider or model is
healthy. Unknown or unavailable model identifiers must still be validated by the
host's normal authorization and model checks before interception.
