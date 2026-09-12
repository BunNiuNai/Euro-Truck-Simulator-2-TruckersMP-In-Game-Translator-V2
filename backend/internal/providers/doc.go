// Package providers Provider 接口与注册表、并行竞速、熔断、模型拉取、连通性测试。
//
// V1 来源：translator.py（_call_provider / _call_api_internal / ProviderHealth）+ model_fetcher.py
//
// 不变量（改动前请确认没有破坏这些行为）：
//   · 竞速语义：全部启用 Provider 并行，先返回且通过 looks_untranslated 校验者胜
//   · **只有竞速，没有轮转**：V1 的 `_rr_index` 全项目只被赋过一次初值、从未被读取
//     （缺陷 D12）。README 曾宣称"多模型轮转负载均衡"，那是文档与代码不符，
//     实现轮转等于新增 V1 没有的功能（违反 P1）
//   · 只有**赢家**会被记账：首个成功者 cancel 其余请求并立即返回，落败者不记失败。
//     这与 V1 逐行一致（V1 也是首个成功就 `return` + `cancel_futures=True`），
//     所以一直输的那家永远不会进入熔断——是继承行为，不是缺陷
//   · 熔断：连续 3 次失败进入冷却 min(30*2^(n-3), 120) 秒，成功即恢复
//   · 全部处于冷却时**强制重试全部**（V1 行为），不允许出现"谁都不试"
//   · 配置字段全部保留（ProviderConfig 13 字段），否则无法迁移用户配置
//   · api_format 支持 openai / anthropic 两种端点拼接
package providers
