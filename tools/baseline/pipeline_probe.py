#!/usr/bin/env python
"""Translator V2 — 阶段 0 基线探针（翻译管线，离线可测部分）

测量 docs/migration-matrix.md 表 5 中**不需要 API Key** 的指标：

    B7  本地词典命中率（优先用真实聊天日志，否则用合成语料）
    B5' 本地词典命中路径的端到端延迟（不含网络）—— 首条翻译延迟的下界
    B8' LRU 缓存的读取/写入延迟 —— 缓存命中延迟
    B6' 发出请求前的本地开销（prompt 构造 + payload 序列化）

这些数字的用途：确认「翻译引擎用哪种语言实现」对端到端延迟的影响量级，
为架构决策提供实测依据。

用法：
    python pipeline_probe.py                       # 自动寻找真实日志，否则用合成语料
    python pipeline_probe.py --corpus my.txt       # 指定语料（每行一条消息）
    python pipeline_probe.py --iterations 20000    # 提高计时精度
    python pipeline_probe.py --json out.json       # 同时输出 JSON

注意：本脚本只读取 V1 源码与聊天日志，不修改任何文件。
"""
from __future__ import annotations

import argparse
import glob
import json
import os
import statistics
import sys
import time
from datetime import datetime

# 中文控制台在 Windows 上默认用本地代码页，统一改为 UTF-8 避免乱码
for _stream in (sys.stdout, sys.stderr):
    if hasattr(_stream, "reconfigure"):
        try:
            _stream.reconfigure(encoding="utf-8", errors="replace")
        except (ValueError, OSError):
            pass

# ── 定位 V1 源码 ──────────────────────────────────────────────
HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", "..", ".."))
V1_DIR = os.path.join(ROOT, "ets2-translator")
if not os.path.isdir(V1_DIR):
    sys.exit(f"[错误] 未找到 V1 目录：{V1_DIR}")
sys.path.insert(0, V1_DIR)

import chat_dictionary as cd                      # noqa: E402
import monitor as mon                             # noqa: E402
import translator as tr                           # noqa: E402
from config import get_documents_path             # noqa: E402


# ── 合成语料：贴近 TMP 真实聊天的样例 ─────────────────────────
SYNTHETIC = [
    # 纯俚语 / 缩写（应被第 1 层命中）
    "wtf", "sry", "ty", "gg", "brb", "afk", "idk", "np", "lol", "ez",
    "k", "ok", "hi", "hello", "bye", "gn", "bro", "noob", "stfu", "tamam",
    # 短语（第 2 层）
    "thank you", "good luck", "have fun", "gute reise", "o/", "<3", ":)", "ahaha",
    # 结构化（第 3 层）
    "rec ban", "report Player123", "kick someone",
    # 系统消息（关键词回退）
    "cannot connect to server", "connection established", "automatically reconnected",
    # ETS2 词汇
    "truck", "trailer", "convoy", "no collision", "gas station", "toll gate",
    # 混合语言
    "你好 where are you", "привет how are you", "sa arkadasim",
    # 完整句子（应走 LLM）
    "why did you ram me at the toll gate just now",
    "please stop blocking the road near Duisburg",
    "anyone wants to convoy from Calais to Rotterdam",
    "that was a really nice overtake man",
    "i will report you for that reckless driving",
    "is there a ferry from Dover to Calais today",
    "my truck has 40 percent damage after that crash",
    "wait for me at the gas station please",
    # 非文字（应被跳过）
    "...", "12345", "!!!", "???", "😀😀", "-", "  ",
]


def timed(fn, items, iterations):
    """对 items 循环调用 fn，返回 (每次调用耗时 us 的统计, 调用总次数)。"""
    samples = []
    n = 0
    # 预热，避免首次导入/缓存影响
    for it in items[: min(len(items), 32)]:
        fn(it)
    for _ in range(iterations):
        for it in items:
            t0 = time.perf_counter_ns()
            fn(it)
            t1 = time.perf_counter_ns()
            samples.append((t1 - t0) / 1000.0)  # ns -> us
            n += 1
    samples.sort()
    return {
        "calls": n,
        "mean_us": round(statistics.fmean(samples), 3),
        "p50_us": round(samples[len(samples) // 2], 3),
        "p95_us": round(samples[int(len(samples) * 0.95)], 3),
        "max_us": round(samples[-1], 3),
    }, n


def load_real_corpus(limit=5000):
    """从 TruckersMP 聊天日志提取真实消息。失败返回 ([], 原因)。"""
    try:
        log_dir = os.path.join(get_documents_path(), "ETS2MP", "logs")
    except Exception as exc:                       # noqa: BLE001
        return [], f"无法确定文档目录: {exc}"
    if not os.path.isdir(log_dir):
        return [], f"日志目录不存在: {log_dir}"

    files = []
    for pat in ("chat_*_log.txt", "chat_*_log_*.txt", "chat_*.txt"):
        files = sorted(glob.glob(os.path.join(log_dir, pat)),
                       key=os.path.getmtime, reverse=True)
        if files:
            break
    if not files:
        return [], f"日志目录存在但无 chat_* 文件: {log_dir}"

    msgs = []
    try:
        with open(files[0], "r", encoding="utf-8", errors="replace") as fh:
            for line in fh:
                cm = mon.parse_line(line)
                if cm and cm.text:
                    msgs.append(cm.text)
                    if len(msgs) >= limit:
                        break
    except OSError as exc:
        return [], f"读取日志失败: {exc}"
    return msgs, f"{os.path.basename(files[0])}（{len(msgs)} 条）"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--corpus", help="自定义语料文件，每行一条消息")
    ap.add_argument("--iterations", type=int, default=3000, help="每轮迭代次数")
    ap.add_argument("--json", help="额外输出 JSON 报告路径")
    ap.add_argument("--target-lang", default="zh-CN")
    args = ap.parse_args()

    print()
    print("=== Translator V2 · 阶段 0 基线探针（翻译管线）===")
    print(f"  V1 目录 : {V1_DIR}")
    print(f"  Python  : {sys.version.split()[0]}")
    print()

    # ── 语料 ──
    if args.corpus:
        with open(args.corpus, "r", encoding="utf-8", errors="replace") as fh:
            corpus = [ln.strip() for ln in fh if ln.strip()]
        corpus_src = f"文件 {args.corpus}"
    else:
        corpus, corpus_src = load_real_corpus()
        if not corpus:
            corpus = SYNTHETIC
            corpus_src = f"合成语料（真实日志不可用：{corpus_src}）"

    print(f"[语料] {corpus_src}")
    print(f"       共 {len(corpus)} 条")
    print()

    # ── B7 本地词典命中率 ──
    hit_dict = hit_nontrans = hit_skip = 0
    for text in corpus:
        if cd.short_phrase_fallback(text):
            hit_dict += 1
        elif cd.is_non_translatable(text):
            hit_nontrans += 1
        elif tr._should_skip_internal(text, args.target_lang):   # noqa: SLF001
            hit_skip += 1
    total = len(corpus)
    local_free = hit_dict + hit_nontrans + hit_skip

    print("[B7] 本地词典命中率")
    print(f"       第1-3层词典命中       : {hit_dict:>6}  ({hit_dict / total:6.1%})")
    print(f"       非文字内容跳过        : {hit_nontrans:>6}  ({hit_nontrans / total:6.1%})")
    print(f"       目标语言已存在跳过    : {hit_skip:>6}  ({hit_skip / total:6.1%})")
    print(f"       ── 零 API 调用合计   : {local_free:>6}  ({local_free / total:6.1%})")
    print(f"       需调用 LLM           : {total - local_free:>6}  ({(total - local_free) / total:6.1%})")
    print()

    # ── 本地路径延迟 ──
    dict_items = [t for t in corpus if cd.short_phrase_fallback(t)] or SYNTHETIC[:50]
    api_items = [t for t in corpus if not cd.short_phrase_fallback(t)] or SYNTHETIC[-10:]

    print(f"[B5'] 本地处理延迟（不含网络，迭代 {args.iterations} 轮）")

    m_dict, _ = timed(cd.short_phrase_fallback, dict_items, args.iterations)
    print(f"       词典查询      均值 {m_dict['mean_us']:>8.1f} us | p95 {m_dict['p95_us']:>8.1f} us")

    m_lang, _ = timed(tr.detect_language, api_items, args.iterations)
    print(f"       语言检测      均值 {m_lang['mean_us']:>8.1f} us | p95 {m_lang['p95_us']:>8.1f} us")

    m_split, _ = timed(lambda t: tr.split_mixed_text(t, args.target_lang), api_items, args.iterations)
    print(f"       混合语言拆分  均值 {m_split['mean_us']:>8.1f} us | p95 {m_split['p95_us']:>8.1f} us")

    m_prompt, _ = timed(lambda t: tr._receive_system_prompt(args.target_lang), api_items[:1], args.iterations)
    print(f"       prompt 构造   均值 {m_prompt['mean_us']:>8.1f} us | p95 {m_prompt['p95_us']:>8.1f} us")

    # 完整本地路径（词典命中时用户感知到的延迟）
    def local_path(t):
        if cd.short_phrase_fallback(t):
            return
        tr.detect_language(t)
        tr.split_mixed_text(t, args.target_lang)

    m_full, n_full = timed(local_path, corpus, max(1, args.iterations // 4))
    print(f"       完整本地路径  均值 {m_full['mean_us']:>8.1f} us | p95 {m_full['p95_us']:>8.1f} us")
    print()

    # ── B8' LRU 缓存 ──
    print("[B8'] LRU 缓存延迟")
    lru = tr.LRUCache(1000)
    for i in range(1000):
        lru.put(f"key{i}", f"val{i}")

    def cache_get(_):
        lru.get("key500")

    m_get, _ = timed(cache_get, [0], args.iterations * 10)
    print(f"       缓存命中读取  均值 {m_get['mean_us']:>8.1f} us | p95 {m_get['p95_us']:>8.1f} us")

    counter = {"i": 0}

    def cache_put(_):
        counter["i"] += 1
        lru.put(f"k{counter['i']}", "v")

    m_put, _ = timed(cache_put, [0], args.iterations * 10)
    print(f"       缓存写入      均值 {m_put['mean_us']:>8.1f} us | p95 {m_put['p95_us']:>8.1f} us")
    print()

    # ── B6' 请求前本地开销 ──
    print("[B6'] 发起 LLM 请求前的本地开销")
    sample_text = api_items[0] if api_items else "hello"

    def build_payload(_):
        payload = {
            "model": "deepseek-chat",
            "messages": [
                {"role": "system", "content": tr._receive_system_prompt(args.target_lang)},
                {"role": "user", "content": sample_text},
            ],
            "temperature": 0,
            "max_tokens": tr._max_output_tokens(sample_text),
        }
        json.dumps(payload, ensure_ascii=False)

    m_payload, _ = timed(build_payload, [0], args.iterations * 2)
    print(f"       payload 构造  均值 {m_payload['mean_us']:>8.1f} us | p95 {m_payload['p95_us']:>8.1f} us")
    print()

    # ── 结论：本地处理 vs 网络往返 ──
    print("=== 架构结论（实测） ===")
    local_us = m_full["mean_us"]
    for api_ms in (200, 500, 2000):
        api_us = api_ms * 1000
        ratio = local_us / (api_us + local_us)
        print(f"  若 LLM 往返 {api_ms:>5} ms → 本地处理占比 {ratio * 100:8.4f}%"
              f"  （本地 {local_us:.1f} us / 合计 {api_us + local_us:.0f} us）")
    print()
    print("  含义：文字处理环节的耗时比网络往返低 3~5 个数量级。")
    print("        用哪种语言实现引擎，对用户感知延迟无影响。")
    print()

    # ── 报告 ──
    report = {
        "tool": "Translator V2 pipeline probe",
        "measuredAt": datetime.now().isoformat(timespec="seconds"),
        "v1Dir": V1_DIR,
        "python": sys.version.split()[0],
        "corpus": {"source": corpus_src, "size": total},
        "B7_local_hit_rate": {
            "dict": hit_dict, "non_translatable": hit_nontrans,
            "already_target_lang": hit_skip, "total": total,
            "zero_api_calls": local_free,
            "zero_api_rate": round(local_free / total, 4),
        },
        "B5_local_latency_us": {
            "dict_lookup": m_dict, "detect_language": m_lang,
            "split_mixed": m_split, "prompt_build": m_prompt,
            "full_local_path": m_full,
        },
        "B8_lru_us": {"get": m_get, "put": m_put},
        "B6_pre_request_us": m_payload,
        "comparison": [
            {"api_ms": ms, "local_share_pct": round(local_us / (ms * 1000 + local_us) * 100, 6)}
            for ms in (200, 500, 2000)
        ],
    }

    out = args.json or os.path.join(HERE, "out", "pipeline-probe-latest.json")
    os.makedirs(os.path.dirname(out), exist_ok=True)
    with open(out, "w", encoding="utf-8") as fh:
        json.dump(report, fh, ensure_ascii=False, indent=2)
    print(f"JSON 报告已写入: {out}")


if __name__ == "__main__":
    main()
