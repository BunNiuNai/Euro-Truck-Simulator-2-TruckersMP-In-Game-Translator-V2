#!/usr/bin/env python
"""Translator V2 — 从 V1 数据生成 resources/*.json

把 V1 里「硬编码在 Python 模块中」的数据抽成 V2 的 JSON 资源文件（ADR-006）：

    Translator-V2/resources/dictionary.json   ← chat_dictionary.py
    Translator-V2/resources/providers.json    ← provider_presets.py
    Translator-V2/resources/config.json       ← config.py:AppConfig（去掉 V1 兼容字段）

这一步同时验证 ADR-006 的 schema 是否可落地。

用法：
    python gen_resources.py            # 生成/覆盖
    python gen_resources.py --check    # 只校验，不写盘（CI 用）

只读取 V1 源码，不修改 V1 任何文件。
"""
from __future__ import annotations

import argparse
import json
import os
import sys
from datetime import date

for _s in (sys.stdout, sys.stderr):
    if hasattr(_s, "reconfigure"):
        try:
            _s.reconfigure(encoding="utf-8", errors="replace")
        except (ValueError, OSError):
            pass

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.abspath(os.path.join(HERE, "..", ".."))
V1_DIR = os.path.join(ROOT, "ets2-translator")
OUT_DIR = os.path.join(ROOT, "Translator-V2", "resources")

if not os.path.isdir(V1_DIR):
    sys.exit(f"[错误] 未找到 V1 目录：{V1_DIR}")
sys.path.insert(0, V1_DIR)

import chat_dictionary as cd          # noqa: E402
import provider_presets as pp         # noqa: E402
import win32_constants as wk          # noqa: E402
from config import AppConfig          # noqa: E402

TODAY = date.today().isoformat()

# V2 不迁移的 V1 兼容字段（ADR-008：不写兼容层）
DROPPED_CONFIG_FIELDS = {"api_endpoint", "api_key", "api_model", "version"}


def build_dictionary() -> dict:
    """chat_dictionary.py → dictionary.json（五层术语 + ETS2 词汇）"""
    slang = {
        token: {"text": text, "pauseAfter": bool(pause)}
        for token, (text, pause) in cd.SLANG_TOKENS.items()
    }
    system_msgs = [
        {"patterns": list(patterns), "text": text}
        for patterns, text in cd.SYSTEM_MESSAGE_FALLBACK.items()
    ]
    return {
        "schemaVersion": 1,
        "meta": {
            "generatedFrom": "ets2-translator/chat_dictionary.py",
            "generatedAt": TODAY,
            "note": "五层术语处理：slang(第1层) / phrases(第2层) / structuredActions(第3层) "
                    "/ promptMapping(第4层) / ets2Terms(第5层保留层)",
        },
        "slang": slang,
        "phrases": dict(cd.PHRASE_FALLBACK),
        "structuredActions": dict(cd.STRUCTURED_ACTIONS),
        "systemMessages": system_msgs,
        "ets2Terms": dict(cd.ETS2_TERMS),
        "promptMapping": cd.PROMPT_MAPPING,
    }


def build_providers() -> dict:
    """provider_presets.py → providers.json（20 组预设 + 图标 + 分类）"""
    presets = []
    for p in pp.PRESETS:
        presets.append({
            "id": p.id,
            "name": p.name,
            "websiteUrl": p.website_url,
            "apiKeyUrl": p.api_key_url,
            "endpoint": p.endpoint,          # base URL，不含 /chat/completions
            "apiFormat": p.api_format,       # "openai" | "anthropic"
            "defaultModel": p.default_model,
            "modelsUrl": p.models_url,
            "icon": p.icon,
            "iconColor": p.icon_color,
            "category": p.category,
            "templateHeaders": dict(p.template_headers),
            "templateBody": dict(p.template_body),
            "description": p.description,
            "recommended": bool(p.recommended),
        })
    return {
        "schemaVersion": 1,
        "meta": {
            "generatedFrom": "ets2-translator/provider_presets.py",
            "generatedAt": TODAY,
            "endpointNote": "endpoint 为 base URL；完整地址由 buildEndpoint() 拼接："
                            "anthropic → {base}/v1/messages，其余 → {base}/v1/chat/completions",
        },
        "categories": [{"id": cid, "label": label} for cid, label in pp.CATEGORIES],
        "icons": dict(pp.PROVIDER_ICONS),
        "presets": presets,
    }


def build_config() -> dict:
    """config.py:AppConfig → config.json（V2 默认配置模板）"""
    from dataclasses import asdict
    raw = asdict(AppConfig())
    fields = {k: v for k, v in raw.items() if k not in DROPPED_CONFIG_FIELDS}

    # ── V2 新增字段（V1 没有，不属于 P1 迁移范围）────────────────
    # 主题在 V1 里不存在：V1 的悬浮窗写死深色，设置页另有一套 VS Code 配色
    # （缺陷 D1）。V2 引入统一的主题切换，取值与 Win11 的「个性化 → 颜色 →
    # 选择模式」一致，因此还需要 native 侧配合切换沉浸式深色模式。
    fields["theme"] = "dark"

    return {
        "configVersion": 1,
        "meta": {
            "generatedFrom": "ets2-translator/config.py:AppConfig",
            "generatedAt": TODAY,
            "note": "V2 全新配置目录，不读 V1 配置（ADR-008）。"
                    "已移除 V1 兼容字段：api_endpoint / api_key / api_model。"
                    "theme 为 V2 新增字段（V1 无此配置）。",
        },
        "defaults": fields,
    }


def build_keymap() -> dict:
    """win32_constants.py → keymap.json（键名映射表外置，缺陷 D7）

    V1 里这张表存在两份、热键解析器有 4 处实现（1 处死代码），行为还不一致。
    V2 统一为「单一 JSON 表 + 单一 Go 解析器」，C++ 侧也用同一份表，
    从根上消除漂移。

    注意：KEY_NAME_MAP ⊆ SPECIAL_VK 且取值相同，因此合并为一个 keys 表。
    """
    return {
        "schemaVersion": 1,
        "meta": {
            "generatedFrom": "ets2-translator/win32_constants.py",
            "generatedAt": TODAY,
            "note": "键名映射表外置（缺陷 D7）。合并 KEY_NAME_MAP 与 SPECIAL_VK；"
                    "modFlags 采 Win32 的 MOD_* 取值，其中 win 是 V2 新增"
                    "（V1 的 _MOD_VK_MAP 定义了 win 但热键解析器不接受它）。",
        },
        "modifiers": dict(wk._MOD_VK_MAP),
        "keys": dict(wk.SPECIAL_VK),
        # modFlags 必须覆盖 V1 解析器接受的全部**别名**：
        # V1 的 ("shift","shft") / ("ctrl","control") / ("alt",) 在三个解析器里一致；
        # win/windows 仅出现在 _MOD_VK_MAP 中（V1 没有解析器用它，V2 启用，见 D7）。
        # 漏掉别名会导致 "ctrl+c" 这类配置静默失效——已由单元测试覆盖。
        "modFlags": {
            "shift": wk.MOD_SHIFT, "shft": wk.MOD_SHIFT,
            "ctrl": wk.MOD_CONTROL, "control": wk.MOD_CONTROL,
            "alt": wk.MOD_ALT,
            "win": 0x0008, "windows": 0x0008,
        },
    }


def summarize(name: str, data: dict) -> str:
    if name == "dictionary.json":
        return (f"slang={len(data['slang'])} phrases={len(data['phrases'])} "
                f"structured={len(data['structuredActions'])} "
                f"systemMessages={len(data['systemMessages'])} "
                f"ets2Terms={len(data['ets2Terms'])}")
    if name == "providers.json":
        cats = {}
        for p in data["presets"]:
            cats[p["category"]] = cats.get(p["category"], 0) + 1
        return f"presets={len(data['presets'])} icons={len(data['icons'])} 分类={cats}"
    if name == "config.json":
        return f"defaults={len(data['defaults'])} 字段"
    if name == "keymap.json":
        return (f"modifiers={len(data['modifiers'])} keys={len(data['keys'])} "
                f"modFlags={list(data['modFlags'])}")
    return ""


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--check", action="store_true", help="只校验，不写盘")
    args = ap.parse_args()

    targets = [
        ("dictionary.json", build_dictionary()),
        ("providers.json", build_providers()),
        ("config.json", build_config()),
        ("keymap.json", build_keymap()),
    ]

    # ── 校验：必填 schemaVersion + 关键结构 ──
    problems = []
    for name, data in targets:
        if data.get("schemaVersion", data.get("configVersion")) is None:
            problems.append(f"{name}: 缺少 schemaVersion/configVersion")
    d = dict(targets)["dictionary.json"]
    for i, entry in enumerate(d["systemMessages"]):
        if not entry.get("patterns") or not entry.get("text"):
            problems.append(f"dictionary.json: systemMessages[{i}] 结构不合法")
    for token, v in d["slang"].items():
        if "text" not in v or "pauseAfter" not in v:
            problems.append(f"dictionary.json: slang[{token}] 缺少 text/pauseAfter")
    # pauseAfter 必须存在（影响结构化短语拼接边界，见迁移对照表 10.1）
    missing_pause = [t for t, v in d["slang"].items() if "pauseAfter" not in v]
    if missing_pause:
        problems.append(f"dictionary.json: {len(missing_pause)} 条 slang 缺 pauseAfter")

    if problems:
        print("[校验失败]")
        for p in problems:
            print("  -", p)
        sys.exit(1)

    print("[校验通过] schemaVersion、pauseAfter、systemMessages 结构均正确")
    for name, data in targets:
        print(f"  {name:<18} {summarize(name, data)}")

    if args.check:
        print("\n--check 模式：未写盘")
        return

    os.makedirs(OUT_DIR, exist_ok=True)
    enc = "utf-8"
    for name, data in targets:
        path = os.path.join(OUT_DIR, name)
        with open(path, "w", encoding=enc, newline="\n") as fh:
            json.dump(data, fh, ensure_ascii=False, indent=2)
            fh.write("\n")
        print(f"  已写入 {os.path.relpath(path, ROOT)}  "
              f"({os.path.getsize(path):,} bytes)")


if __name__ == "__main__":
    main()
