// Package providers LLM 供应商调用：并行竞速、熔断冷却、连通性测试、模型列表拉取。
//
// V1 来源：
//   - _call_provider            translator.py:534~599
//   - _call_api_internal（竞速） translator.py:646~690
//   - 熔断 _is_cooling / _note_provider_result  translator.py:501~530
//   - 错误映射 _format_error     translator.py:725~745
//   - test_connection / fetch_models  （translator.py:755 + model_fetcher.py）
//
// 不变量（改动前请确认没有破坏这些行为）：
//   - **只有并行竞速，没有轮转**：`_rr_index` 是 V1 的残留变量，轮转功能不存在（D12）
//   - 竞速语义：全部启用 Provider 并行，**先返回且通过 looks_untranslated 校验者胜**；
//     结果无效或报错的 Provider 记一次失败后继续等其他家，不是直接放弃
//   - 熔断：连续 3 次失败进入冷却 min(30·2^(n-3), 120) 秒；成功立即清零
//   - 全部处于冷却时**强制重试全部**（V1 行为），不允许出现"谁都不试"
//   - 错误文案必须与 V1 逐字一致（domain.ErrorCode.Message）
//   - Provider 配置 13 个字段全部生效：extra_headers 支持 {api_key} 替换、
//     extra_body 覆盖基础 payload、timeout 逐家覆盖
package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/config"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/dictionary"
	"github.com/BunNiuNai/ets2-translator-v2/backend/internal/domain"
)

// ── 错误类型 ──────────────────────────────────────────────────

// Error 携带 V1 的错误分类，文案由 domain.ErrorCode 统一生成。
type Error struct {
	Code   domain.ErrorCode
	Detail string
	Label  string // 出错的 Provider 名称（便于日志定位）
}

func (e *Error) Error() string { return e.Code.Message(e.Detail) }

// AsError 把任意错误转成 *Error（便于调用方取错误码）。
func AsError(err error) *Error {
	var pe *Error
	if errors.As(err, &pe) {
		return pe
	}
	return &Error{Code: domain.ErrTranslateFailed, Detail: err.Error()}
}

// ── 熔断 ──────────────────────────────────────────────────────

// Health 记录单个 Provider 的健康状态（V1 ProviderHealth）。
type Health struct {
	Failures  int
	CoolUntil time.Time
}

// ── 客户端 ────────────────────────────────────────────────────

const (
	defaultTimeout   = 8 * time.Second
	breakerFailures  = 3 // 连续失败达到此数进入冷却
	breakerBaseCool  = 30 * time.Second
	breakerMaxCool   = 120 * time.Second
)

// Result 是一次成功翻译的结果。
type Result struct {
	Translated string
	Provider   string // Provider 的 label
	Model      string
}

// Logf 是可选日志回调（nil 时静默）。
type Logf func(level, module, msg string)

// Client 调用一组 Provider。
type Client struct {
	providers    []config.Provider
	target       string
	http         *http.Client
	logf         Logf
	systemPrompt func(target string) string

	mu     sync.Mutex
	health map[string]*Health
}

// Options 构造参数。
type Options struct {
	Providers []config.Provider
	// Target 是目标语言（用于结果校验），例如 "zh-CN"
	Target string
	Timeout time.Duration // 0 表示默认 8 秒
	Logf    Logf
	// SystemPrompt 由调用方注入（engine.ReceiveSystemPrompt），
	// 这样 providers 不必知道 prompt 文案，避免两个包互相依赖。
	SystemPrompt func(target string) string
}

// New 创建客户端。
func New(opts Options) *Client {
	t := opts.Timeout
	if t <= 0 {
		t = defaultTimeout
	}
	return &Client{
		providers:    opts.Providers,
		target:       opts.Target,
		http:         &http.Client{Timeout: t},
		logf:         opts.Logf,
		systemPrompt: opts.SystemPrompt,
		health:       map[string]*Health{},
	}
}

// SetProviders 替换 Provider 列表（配置热重载时使用）。
func (c *Client) SetProviders(list []config.Provider) {
	c.mu.Lock()
	c.providers = list
	c.mu.Unlock()
}

// SetTarget 更新目标语言。
func (c *Client) SetTarget(target string) {
	c.mu.Lock()
	c.target = target
	c.mu.Unlock()
}

// EnabledCount 返回当前启用中的 Provider 数量。
//
// 供**调用时**判断「现在到底有没有可用的 Provider」。必须在锁下读：
// 设置页保存会经 SetProviders 换掉整个列表，直接读那份切片的调用方
// 会与它并发（切片的头是两个字，无锁读可能读到撕裂值）。
//
// ⚠️ 为什么需要这个而不是在构造时判断一次：见 main.go 里 providerLLM.Call
// 的说明——判断一次会让「启动时没配、后来在设置页配好」永远不生效。
func (c *Client) EnabledCount() int {
	list, _ := c.snapshot()
	n := 0
	for _, p := range list {
		if p.Enabled {
			n++
		}
	}
	return n
}

func (c *Client) snapshot() ([]config.Provider, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]config.Provider, len(c.providers))
	copy(out, c.providers)
	return out, c.target
}

func (c *Client) log(level, msg string) {
	if c.logf != nil {
		c.logf(level, "LLM", msg)
	}
}

// ── 熔断状态 ──────────────────────────────────────────────────

func (c *Client) isCooling(label string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.health[label]
	return h != nil && !h.CoolUntil.IsZero() && time.Now().Before(h.CoolUntil)
}

// noteResult 记录一次调用结果并维护熔断状态（V1 _note_provider_result）。
func (c *Client) noteResult(label string, success bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	h := c.health[label]
	if h == nil {
		h = &Health{}
		c.health[label] = h
	}

	if success {
		if h.Failures > 0 {
			c.log("INFO", fmt.Sprintf("Provider %s 已恢复（之前 %d 次失败）", label, h.Failures))
		}
		h.Failures = 0
		h.CoolUntil = time.Time{}
		return
	}

	h.Failures++
	if h.Failures >= breakerFailures {
		// min(30 * 2^(n-3), 120) 秒
		mult := 1 << (h.Failures - breakerFailures)
		d := breakerBaseCool * time.Duration(mult)
		if d > breakerMaxCool {
			d = breakerMaxCool
		}
		h.CoolUntil = time.Now().Add(d)
		c.log("WARN", fmt.Sprintf("Provider %s 进入冷却 %ds（连续 %d 次失败）",
			label, int(d.Seconds()), h.Failures))
	}
}

// HealthSnapshot 返回当前健康状态副本（供 UI /api/health 使用）。
func (c *Client) HealthSnapshot() map[string]Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]Health, len(c.health))
	for k, v := range c.health {
		out[k] = *v
	}
	return out
}

// ── 竞速 ──────────────────────────────────────────────────────

type outcome struct {
	result Result
	label  string
	err    error
}

// Translate 并行竞速：先返回且通过校验的译文胜出。
//
// 对应 V1 _call_api_internal。**没有轮转分支**——V1 的轮转是残留变量（D12）。
func (c *Client) Translate(ctx context.Context, text string) (Result, error) {
	all, target := c.snapshot()

	var enabled []config.Provider
	for _, p := range all {
		if p.Enabled {
			enabled = append(enabled, p)
		}
	}
	if len(enabled) == 0 {
		return Result{}, &Error{
			Code:   domain.ErrTranslateFailed,
			Detail: "未配置任何启用的 Provider",
		}
	}

	// 过滤冷却中的 Provider；若全部冷却则强制重试全部（V1 行为）
	var active []config.Provider
	for _, p := range enabled {
		if !c.isCooling(p.Label) {
			active = append(active, p)
		}
	}
	if len(active) == 0 {
		c.log("WARN", "所有 Provider 均处于冷却期，强制重试全部")
		active = enabled
	}

	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch := make(chan outcome, len(active))
	for _, p := range active {
		go func(p config.Provider) {
			out, err := c.callProvider(raceCtx, p, text, target)
			if err != nil {
				ch <- outcome{label: p.Label, err: err}
				return
			}
			// 无效结果（空/等于原文/目标中文却无中文）视为失败，继续等其他家
			if dictionary.LooksUntranslated(text, out, target) {
				ch <- outcome{label: p.Label, err: &Error{
					Code:   domain.ErrBadResponse,
					Detail: "竞速结果无效（未通过未翻译校验）",
					Label:  p.Label,
				}}
				return
			}
			ch <- outcome{label: p.Label, result: Result{
				Translated: out, Provider: p.Label, Model: p.Model,
			}}
		}(p)
	}

	var lastErr error
	for i := 0; i < len(active); i++ {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case o := <-ch:
			if o.err == nil {
				cancel() // 取消其余还在飞的请求（V1 cancel_futures=True）
				c.noteResult(o.label, true)
				c.log("INFO", fmt.Sprintf("竞速成功: %s", o.label))
				return o.result, nil
			}
			lastErr = o.err
			c.noteResult(o.label, false)
			c.log("WARN", fmt.Sprintf("竞速失败: %s - %s", o.label, o.err.Error()))
		}
	}
	_ = lastErr // 具体错误已在上面逐条记入日志

	// 全部失败：V1 的 _call_api_internal 把所有异常包装成一个通用异常，
	// 经 _format_error 得到 "[翻译失败] 所有 Provider 翻译失败"。
	// 具体错误码（认证失败/超时/…）只出现在日志里，不出现在用户可见文案中——
	// 这是 V1 的真实行为，保持逐字一致。
	return Result{}, &Error{
		Code:   domain.ErrTranslateFailed,
		Detail: "所有 Provider 翻译失败",
	}
}

// ── 单家调用 ──────────────────────────────────────────────────

// callProvider 调用单个 Provider。对应 V1 _call_provider。
func (c *Client) callProvider(
	ctx context.Context, p config.Provider, text, target string,
) (string, error) {
	endpoint := strings.TrimSpace(p.Endpoint)
	if endpoint == "" {
		return "", &Error{Code: domain.ErrTranslateFailed, Detail: "Provider endpoint 为空", Label: p.Label}
	}
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "https://" + endpoint
	}

	timeout := defaultTimeout
	if p.Timeout > 0 {
		timeout = time.Duration(p.Timeout) * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	maxTokens := MaxOutputTokens(text, "\n---\n")
	system := c.buildSystemPrompt(target)

	var payload map[string]any
	if p.APIFormat == "anthropic" {
		payload = anthropicPayload(p.Model, system, text, maxTokens)
	} else {
		payload = openAIPayload(p.Model, system, text, maxTokens)
	}

	// DeepSeek / MiMo / 小米 关闭 thinking（避免思考链消耗 token）
	kind := strings.ToLower(p.Label + " " + p.Model)
	if strings.Contains(kind, "deepseek") || strings.Contains(kind, "mimo") || strings.Contains(kind, "xiaomi") {
		payload["thinking"] = map[string]any{"type": "disabled"}
	}

	// extra_body 覆盖基础 payload（V1: payload.update(extra_body)）
	for k, v := range p.ExtraBody {
		payload[k] = v
	}

	headers := map[string]string{"Content-Type": "application/json"}
	if p.APIFormat == "anthropic" {
		headers["x-api-key"] = p.APIKey
		headers["anthropic-version"] = "2023-06-01"
	} else {
		headers["Authorization"] = "Bearer " + p.APIKey
	}
	// extra_headers 支持 {api_key} 模板替换
	for k, v := range p.ExtraHeaders {
		headers[k] = strings.ReplaceAll(v, "{api_key}", p.APIKey)
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", &Error{Code: domain.ErrTranslateFailed, Detail: err.Error(), Label: p.Label}
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", &Error{Code: domain.ErrTranslateFailed, Detail: err.Error(), Label: p.Label}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", mapTransportError(err, p.Label)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", &Error{Code: domain.ErrBadResponse, Detail: err.Error(), Label: p.Label}
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", mapStatusError(resp.StatusCode, resp.Status, raw, p.Label)
	}

	out, err := parseResponse(p.APIFormat, raw, p.Label)
	if err != nil {
		return "", err
	}
	return out, nil
}

// buildSystemPrompt 生成 system prompt；未注入时用一个最小可用版本。
func (c *Client) buildSystemPrompt(target string) string {
	if c.systemPrompt != nil {
		return c.systemPrompt(target)
	}
	return "You translate TruckersMP/ETS2 multiplayer chat into " + target +
		". Translate ONLY: output the direct translation and nothing else."
}

// openAIPayload 构造 OpenAI 格式请求体（V1 的基础 payload）。
func openAIPayload(model, system, text string, maxTokens int) map[string]any {
	return map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": text},
		},
		"temperature": 0,
		"max_tokens":  maxTokens,
	}
}

// anthropicPayload 构造 Anthropic /v1/messages 请求体。
//
// ⚠️ 这是 V2 修正（缺陷 D13）：V1 对 anthropic 格式**也用 OpenAI 的请求体**，
// 而 /v1/messages 不接受 messages 里的 "system" 角色（要求顶层 system 字段），
// 且响应结构是 content[0].text 而非 choices[0].message.content。
// 结果是预设列表里的 anthropic 项在 V1 里**完全不可用**。
func anthropicPayload(model, system, text string, maxTokens int) map[string]any {
	return map[string]any{
		"model":  model,
		"system": system,
		"messages": []map[string]string{
			{"role": "user", "content": text},
		},
		"temperature": 0,
		"max_tokens":  maxTokens,
	}
}

// parseResponse 解析响应体。
func parseResponse(apiFormat string, raw []byte, label string) (string, error) {
	if apiFormat == "anthropic" {
		var r struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return "", &Error{Code: domain.ErrBadResponse, Detail: err.Error(), Label: label}
		}
		for _, blk := range r.Content {
			if blk.Text != "" {
				return strings.TrimSpace(blk.Text), nil
			}
		}
		return "", &Error{Code: domain.ErrBadResponse, Detail: "Anthropic 响应里没有文本内容", Label: label}
	}

	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", &Error{Code: domain.ErrBadResponse, Detail: err.Error(), Label: label}
	}
	if len(r.Choices) == 0 {
		// 对应 V1 的 KeyError/IndexError → 响应格式错误
		return "", &Error{Code: domain.ErrBadResponse, Detail: "响应缺少 choices[0].message.content", Label: label}
	}
	return strings.TrimSpace(r.Choices[0].Message.Content), nil
}

// mapTransportError 对应 V1 _format_error 里的 httpx 异常分支。
func mapTransportError(err error, label string) error {
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "context deadline exceeded"),
		strings.Contains(lower, "timeout"),
		strings.Contains(lower, "timed out"):
		return &Error{Code: domain.ErrTimeout, Label: label}
	case strings.Contains(lower, "connection refused"),
		strings.Contains(lower, "no such host"),
		strings.Contains(lower, "connect"),
		strings.Contains(lower, "dial"):
		return &Error{Code: domain.ErrNetwork, Label: label}
	}
	return &Error{Code: domain.ErrTranslateFailed, Detail: msg, Label: label}
}

// mapStatusError 对应 V1 _format_error 的 HTTPStatusError 分支，
// 状态码 → 错误分类的映射必须与 V1 一致。
func mapStatusError(status int, statusText string, body []byte, label string) error {
	detail := parseAPIError(body)
	switch status {
	case 401:
		return &Error{Code: domain.ErrAuthFailed, Detail: detail, Label: label}
	case 403:
		return &Error{Code: domain.ErrForbidden, Detail: detail, Label: label}
	case 429:
		return &Error{Code: domain.ErrRateLimited, Detail: detail, Label: label}
	case 500, 502, 503:
		return &Error{Code: domain.ErrServerError, Detail: fmt.Sprint(status), Label: label}
	}
	return &Error{Code: domain.ErrHTTPError,
		Detail: fmt.Sprintf("%d %s", status, strings.TrimSpace(statusText)), Label: label}
}

// parseAPIError 对应 V1 _parse_api_error：取 error.message 作为附加说明。
func parseAPIError(body []byte) string {
	var r struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err == nil && r.Error.Message != "" {
		return " — " + r.Error.Message
	}
	return ""
}

// MaxOutputTokens 对应 V1 _max_output_tokens：按输入长度动态计算。
func MaxOutputTokens(text, batchSeparator string) int {
	if strings.Contains(text, batchSeparator) {
		return 500*strings.Count(text, batchSeparator) + 500
	}
	n := 56 + len([]rune(text))/4
	if n < 64 {
		n = 64
	}
	if n > 160 {
		n = 160
	}
	return n
}

// ── 连通性测试与模型列表 ──────────────────────────────────────

// TestResult 是连通性测试结果。
type TestResult struct {
	Success   bool
	Message   string
	LatencyMs float64
}

// TestConnection 对应 V1 translator.test_connection：
// 发一个最小请求验证端点、密钥与模型名。
func (c *Client) TestConnection(ctx context.Context, p config.Provider) TestResult {
	endpoint := strings.TrimSpace(p.Endpoint)
	if endpoint != "" &&
		!strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = "https://" + endpoint
	}

	start := time.Now()
	payload := map[string]any{
		"model": p.Model,
		"messages": []map[string]string{
			{"role": "user", "content": "Hi"},
		},
		"max_tokens":  5,
		"temperature": 0,
	}
	body, _ := json.Marshal(payload)

	reqCtx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return TestResult{Message: "连接失败: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	if p.APIFormat == "anthropic" {
		req.Header.Set("x-api-key", p.APIKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	resp, err := c.http.Do(req)
	latency := float64(time.Since(start).Milliseconds())
	if err != nil {
		code := AsError(mapTransportError(err, p.Label)).Code
		return TestResult{Message: code.Message(""), LatencyMs: latency}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := parseAPIError(raw)
		switch resp.StatusCode {
		case 401:
			return TestResult{Message: "API Key 无效 (401)" + detail, LatencyMs: latency}
		case 403:
			return TestResult{Message: "无权访问 (403)" + detail, LatencyMs: latency}
		case 404:
			return TestResult{Message: "未找到 (404)" + detail + "\n请检查 API 地址路径和模型名称", LatencyMs: latency}
		case 429:
			return TestResult{Message: "请求过于频繁 (429)，请稍后重试", LatencyMs: latency}
		}
		return TestResult{Message: fmt.Sprintf("HTTP 错误 %d%s", resp.StatusCode, detail), LatencyMs: latency}
	}

	content, err := parseResponse(p.APIFormat, raw, p.Label)
	if err != nil {
		return TestResult{Message: "API 响应格式异常，请确认 API 地址指向 chat/completions 端点", LatencyMs: latency}
	}
	if len(content) > 60 {
		content = content[:60]
	}
	return TestResult{Success: true, Message: "连通成功 — " + content, LatencyMs: latency}
}

// ModelList 是模型拉取结果。
type ModelList struct {
	Success   bool
	Models    []string
	Error     string
	LatencyMs float64
}

// FetchModels 对应 V1 model_fetcher.fetch_models：
// 从 endpoint 推导 base，再 GET /v1/models。
func (c *Client) FetchModels(ctx context.Context, p config.Provider) ModelList {
	base := strings.TrimRight(strings.TrimSpace(p.Endpoint), "/")
	// ⚠️ 后缀必须**从长到短**匹配。
	//
	// V1 `model_fetcher.py:40` 的表是短的在前（"/chat/completions" 排在
	// "/v1/chat/completions" 之前），于是最常见的写法
	// `https://api.deepseek.com/v1/chat/completions` 会先被 `/chat/completions`
	// 匹配，剥完剩下 `.../v1`，再拼 `/v1/models` 就成了 `/v1/v1/models`——必然 404。
	// 这是 V1 的缺陷 D21，V2 在这里修掉。
	for _, suffix := range []string{
		"/v1/chat/completions", "/v1/messages", "/chat/completions", "/messages",
	} {
		if strings.HasSuffix(base, suffix) {
			base = strings.TrimSuffix(base, suffix)
			break
		}
	}
	// 再统一去掉结尾的 /v1：下面拼的本来就是 /v1/models。
	// 用户直接填 `https://api.x.com/v1` 时这一步是必需的。
	base = strings.TrimRight(base, "/")
	base = strings.TrimSuffix(base, "/v1")
	modelsURL := base + "/v1/models"

	start := time.Now()
	reqCtx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return ModelList{Error: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	if p.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.APIKey)
	}

	resp, err := c.http.Do(req)
	latency := float64(time.Since(start).Milliseconds())
	if err != nil {
		return ModelList{Error: err.Error(), LatencyMs: latency}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode != 200 {
		return ModelList{
			Error:     fmt.Sprintf("HTTP %d", resp.StatusCode),
			LatencyMs: latency,
		}
	}

	models := parseOpenAIModelList(raw)
	if len(models) == 0 {
		return ModelList{Error: "未解析到模型列表", LatencyMs: latency}
	}
	return ModelList{Success: true, Models: models, LatencyMs: latency}
}

// parseOpenAIModelList 对应 V1 model_fetcher._parse_openai_model_list：
// 支持 data[].id 与 data[] 为字符串两种形态，并把 preview/beta/alpha/deprecated
// 排到后面（V1 的 sort_key）。
func parseOpenAIModelList(raw []byte) []string {
	var r struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil
	}

	var models []string
	for _, item := range r.Data {
		var asObj struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(item, &asObj); err == nil && asObj.ID != "" {
			models = append(models, strings.TrimSpace(asObj.ID))
			continue
		}
		var asStr string
		if err := json.Unmarshal(item, &asStr); err == nil && asStr != "" {
			models = append(models, strings.TrimSpace(asStr))
		}
	}

	tags := []string{"preview", "beta", "alpha", "deprecated"}
	sort.SliceStable(models, func(i, j int) bool {
		pi, pj := isPreview(models[i], tags), isPreview(models[j], tags)
		if pi != pj {
			return !pi // 非预览的排前面
		}
		return strings.ToLower(models[i]) < strings.ToLower(models[j])
	})
	return models
}

func isPreview(name string, tags []string) bool {
	lower := strings.ToLower(name)
	for _, t := range tags {
		if strings.Contains(lower, t) {
			return true
		}
	}
	return false
}
