#!/usr/bin/env python
"""Translator V2 — 保真度对拍（Go 实现 vs V1 Python 实现）

把同一批消息同时喂给两侧，逐条比对判定结果。这是移植保真度的金标准：
任何一条不一致都说明 Go 侧偏离了 V1 行为。

两种模式：
    --pipeline   管线级（默认）：完整接收流程的状态 + 译文
                 （混合拆分 → 片段级跳过/词典 → LLM占位 → 后处理 → 重组）
    --dict       词库级：非文字判定 / 词典命中 / 译文三个字段

用法：
    python dict_fidelity.py                       # 管线级，用真实聊天日志
    python dict_fidelity.py --dict                # 词库级
    python dict_fidelity.py --messages a.txt
    python dict_fidelity.py --limit 5000

退出码：0 = 完全一致；1 = 存在差异。
"""
from __future__ import annotations

import argparse
import glob
import os
import subprocess
import sys
from collections import Counter
from datetime import datetime

for _s in (sys.stdout, sys.stderr):
    if hasattr(_s, "reconfigure"):
        try:
            _s.reconfigure(encoding="utf-8", errors="replace")
        except (ValueError, OSError):
            pass

HERE = os.path.dirname(os.path.abspath(__file__))
V2 = os.path.abspath(os.path.join(HERE, "..", ".."))
REPO = os.path.dirname(V2)
V1_DIR = os.path.join(REPO, "ets2-translator")
BACKEND = os.path.join(V2, "backend")
OUT = os.path.join(HERE, "out")

sys.path.insert(0, V1_DIR)
import chat_dictionary as cd          # noqa: E402
import monitor as mon                 # noqa: E402
from config import get_documents_path  # noqa: E402
from translator import (               # noqa: E402
    _should_skip_internal,
    fix_leftover_shorthand,
    preserve_mention_prefix,
    reassemble_mixed,
    split_mixed_text,
)

GO = os.environ.get("GO_BIN") or r"C:\Go\bin\go.exe"

# 与 Go 侧 dictcmp -pipeline / main.go 的桩完全一致的占位符
LLM_PLACEHOLDER = "〔LLM待接入〕"


# ───────────────────────────────────────────────────────────────
# V1 侧：真实流程模拟
# ───────────────────────────────────────────────────────────────

def v1_call_api(text: str, target: str) -> tuple[str, str]:
    """模拟 V1 _call_api 的本地部分：跳过 → 词典 → LLM占位 → 后处理。

    词典拦截发生在 callAPI 层，因此混合文本的外来**片段**也会经过它
    （"你好 wtf" → 你好什么鬼）。这是与原语级统计的关键差异。
    """
    if cd.is_non_translatable(text):
        return "skip_nontext", text
    if _should_skip_internal(text, target):
        return "skip_target", text
    quick = cd.short_phrase_fallback(text)
    if quick:
        return "local_dict", quick
    out = fix_leftover_shorthand(LLM_PLACEHOLDER)
    out = preserve_mention_prefix(text, out)
    return "llm", out


def v1_pipeline(text: str, target: str = "zh-CN") -> tuple[str, str]:
    """模拟 V1 接收方向单条消息的完整流程
    （_translate_with_mixed_lang + _call_api 的本地部分）。

    状态取值与 Go 侧 engine.Translator 完全一致：
    skip_empty / all_target / mixed / skip_nontext / skip_target / local_dict / llm
    """
    if not text or not text.strip():
        return "skip_empty", text
    segments = split_mixed_text(text, target)
    if not segments:
        return "skip_empty", text

    foreign = [s for s, is_target in segments if not is_target]
    if not foreign:
        return "all_target", text

    target_segs = [s for s, is_target in segments if is_target]
    if not target_segs:
        return v1_call_api(text, target)

    translations: dict[str, str] = {}
    for fseg in foreign:
        stripped = fseg.strip()
        if not stripped:
            translations[fseg] = fseg
        else:
            # v1_call_api 返回 (状态, 译文)——取译文
            _, out = v1_call_api(stripped, target)
            translations[fseg] = out
    return "mixed", reassemble_mixed(segments, translations)


def v1_dict_results(messages: list[str]) -> list[tuple[bool, bool, str]]:
    out = []
    for text in messages:
        non_trans = cd.is_non_translatable(text)
        trans = cd.short_phrase_fallback(text)
        out.append((non_trans, bool(trans), trans))
    return out


# ───────────────────────────────────────────────────────────────
# Go 侧
# ───────────────────────────────────────────────────────────────

def run_go(args: list[str]) -> subprocess.CompletedProcess:
    env = dict(os.environ)
    env["GOCACHE"] = os.path.join(V2, ".gocache")
    proc = subprocess.run(
        [GO, "run", "./cmd/dictcmp", *args],
        cwd=BACKEND, env=env, capture_output=True, text=True, encoding="utf-8",
    )
    if proc.returncode != 0:
        print("[错误] Go 侧执行失败：", file=sys.stderr)
        print(proc.stderr[-2000:], file=sys.stderr)
        sys.exit(1)
    return proc


def go_pipeline_results(msg_path: str) -> list[tuple[str, str]]:
    """跑 Go 侧完整管线，返回 [(状态, 译文)]。"""
    proc = run_go(["-pipeline", msg_path, os.path.join(V2, "resources")])
    rows = []
    for line in proc.stdout.splitlines():
        parts = line.split("\t")
        if len(parts) >= 3:
            rows.append((parts[1], parts[2]))
    return rows


def go_dict_results(msg_path: str) -> list[tuple[bool, bool, str]]:
    proc = run_go([msg_path, os.path.join(V2, "resources")])
    rows = []
    for line in proc.stdout.splitlines():
        parts = line.split("\t")
        if len(parts) >= 4:
            rows.append((parts[1] == "true", parts[2] == "true", parts[3]))
    return rows


# ───────────────────────────────────────────────────────────────
# 语料
# ───────────────────────────────────────────────────────────────

def synthetic_messages() -> list[str]:
    msgs = list(cd.SLANG_TOKENS.keys())
    msgs += list(cd.PHRASE_FALLBACK.keys())
    msgs += list(cd.ETS2_TERMS.keys())
    msgs += [
        "rec ban", "report someplayer", "ban Player123", "kick SPACE_99",
        "cannot connect to server", "connection established",
        "thank youuu", "goood luck", "have fuuun", "sryyy", "srry", "srrry",
        "ty bro", "wtf are you doing at the toll gate",
        "@Player123 hello", "你好 where are you", "...", "12345", "😀😀",
        "why did you ram me near Duisburg", "  spaced  out  ", "WTF!", "ｗｔｆ",
    ]
    return [m for m in msgs if m]


def real_messages(limit: int) -> tuple[list[str], str]:
    log_dir = os.path.join(get_documents_path(), "ETS2MP", "logs")
    if not os.path.isdir(log_dir):
        return [], f"日志目录不存在: {log_dir}"
    files: list[str] = []
    for pat in ("chat_*_log.txt", "chat_*_log_*.txt", "chat_*.txt"):
        files = sorted(glob.glob(os.path.join(log_dir, pat)),
                       key=os.path.getmtime, reverse=True)
        if files:
            break
    if not files:
        return [], "日志目录中无 chat_* 文件"

    msgs: list[str] = []
    with open(files[0], "r", encoding="utf-8", errors="replace") as fh:
        for line in fh:
            cm = mon.parse_line(line)
            if cm and cm.text:
                msgs.append(cm.text)
                if len(msgs) >= limit:
                    break
    return msgs, os.path.basename(files[0])


# ───────────────────────────────────────────────────────────────
# 主流程
# ───────────────────────────────────────────────────────────────

def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--messages", help="自定义消息文件（每行一条）")
    ap.add_argument("--limit", type=int, default=5000)
    ap.add_argument("--show", type=int, default=20, help="最多显示多少条差异")
    ap.add_argument("--dict", action="store_true", help="词库级对拍（默认是管线级）")
    args = ap.parse_args()

    mode = "词库级（非文字/命中/译文）" if args.dict else "管线级（状态+译文）"
    print()
    print("=== 保真度对拍：Go 实现 vs V1 Python 实现 ===")
    print(f"  模式: {mode}")

    if args.messages:
        with open(args.messages, "r", encoding="utf-8", errors="replace") as fh:
            messages = [ln.rstrip("\n") for ln in fh if ln.strip()]
        src = args.messages
    else:
        messages, src = real_messages(args.limit)
        if not messages:
            messages = synthetic_messages()
            src = f"合成语料（真实日志不可用：{src}）"

    print(f"  语料来源: {src}")
    print(f"  消息条数: {len(messages)}")

    os.makedirs(OUT, exist_ok=True)
    msg_path = os.path.join(OUT, "fidelity-messages.txt")
    with open(msg_path, "w", encoding="utf-8", newline="\n") as fh:
        for m in messages:
            fh.write(m.replace("\n", " ").replace("\t", " ") + "\n")

    if args.dict:
        py = v1_dict_results(messages)
        go = go_dict_results(msg_path)
    else:
        py = [v1_pipeline(m) for m in messages]
        go = go_pipeline_results(msg_path)

    if len(py) != len(go):
        print(f"[错误] 结果条数不一致：Python {len(py)} vs Go {len(go)}", file=sys.stderr)
        return 1

    diffs = []
    for i, (p, g) in enumerate(zip(py, go)):
        if p != g:
            diffs.append((i, messages[i], p, g))

    print(f"  对拍时间: {datetime.now().isoformat(timespec='seconds')}")
    print()

    if args.dict:
        py_hit = sum(1 for p in py if p[1])
        py_skip = sum(1 for p in py if p[0])
        print(f"  Python 侧: 命中 {py_hit} / 非文字 {py_skip} / 需 LLM {len(py) - py_hit - py_skip}")
        if py:
            # 这只是**词库原语**的零调用率，不代表真实管线——
            # 真实管线里混合消息的外来片段仍会调 API，见管线级模式。
            print(f"  词库原语零调用率: {(py_hit + py_skip) / len(py):.1%}")
    else:
        hist = Counter(state for state, _ in py)
        print("  Python 侧状态分布:")
        for state, n in sorted(hist.items(), key=lambda kv: -kv[1]):
            print(f"    {state:<14} {n:>4}  ({n / len(py):.1%})")
        zero = hist.get("local_dict", 0) + hist.get("skip_nontext", 0) + \
            hist.get("all_target", 0) + hist.get("skip_empty", 0)
        print(f"  真实管线零 API 调用率: {zero / len(py):.1%}")
    print()

    if not diffs:
        print(f"✅ 完全一致：{len(messages)} 条消息逐条相同")
        return 0

    print(f"❌ 发现 {len(diffs)} 条差异（最多显示 {args.show} 条）：")
    print()
    print(f"  {'#':>5}  {'消息':<32} {'Python':<40} Go")
    print("  " + "-" * 96)
    for i, text, p, g in diffs[: args.show]:
        shown = text if len(text) <= 30 else text[:29] + "…"
        print(f"  {i:>5}  {shown:<32} {str(p):<40} {str(g)}")

    diff_path = os.path.join(OUT, "fidelity-diff.tsv")
    with open(diff_path, "w", encoding="utf-8", newline="\n") as fh:
        fh.write("index\ttext\tpython\tgo\n")
        for i, text, p, g in diffs:
            fh.write("\t".join([str(i), text.replace("\t", " "), str(p), str(g)]) + "\n")
    print()
    print(f"  完整差异已写入: {diff_path}")
    return 1


if __name__ == "__main__":
    sys.exit(main())
