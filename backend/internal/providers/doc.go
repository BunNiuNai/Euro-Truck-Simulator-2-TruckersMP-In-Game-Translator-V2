// Package providers Provider 接口与注册表、并行竞速、轮转、熔断、模型拉取、连通性测试。
//
// V1 来源：translator.py（_call_provider / _call_api_internal / ProviderHealth）+ model_fetcher.py
//
// 不变量（改动前请确认没有破坏这些行为）：
//   · 竞速语义：全部启用 Provider 并行，先返回且通过 looks_untranslated 校验者胜
//   · 熔断：连续 3 次失败进入冷却 min(30*2^(n-3), 120) 秒，成功即恢复
//   · 轮转分流：单 Provider 直连；多 Provider 按轮转索引分配
//   · 配置字段全部保留（ProviderConfig 13 字段），否则无法迁移用户配置
//   · api_format 支持 openai / anthropic 两种端点拼接
package providers
