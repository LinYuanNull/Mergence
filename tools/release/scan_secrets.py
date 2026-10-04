# -*- coding: utf-8 -*-
"""提交前泄露扫描（只读）。

检查导出白名单覆盖范围内会进 git 的文件，找：
  1. 个人路径痕迹：Windows 绝对路径里的用户名、盘符下的个人目录名
  2. 私有仓库 / AGPL 上游的绝对路径引用（只应出现在计划文档与许可声明里）
  3. 疑似密钥：sk-、ghp_、github_pat_、Bearer、AIza、AKIA
  4. 数据库 / 实例数据（*.db、accounts.json、config.json 等）被误纳入

用法：python tools/release/scan_secrets.py [--json]
退出码：0 = 干净（可能有已登记的豁免）；1 = 命中需人工确认。
"""
from __future__ import annotations

import json
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

# 与 export_for_github.py 保持一致的导出范围
DIRS = {
    "src": None,
    "web": {".html", ".js", ".css"},
    "docs": {".md"},
    "tools": {".py", ".sh", ".md"},
    "test": {".py"},
}
ROOT_FILES = {"README.md", "LICENSE", ".gitignore"}

# 计划文档与许可声明天然要提到这些项目名/路径，只是不该带绝对路径
SECRET_PATTERNS = [
    ("openai_key", re.compile(r"\bsk-[A-Za-z0-9_\-]{20,}")),
    ("github_token", re.compile(r"\bgh[pousr]_[A-Za-z0-9]{20,}|\bgithub_pat_[A-Za-z0-9_]{20,}")),
    ("bearer_literal", re.compile(r"Bearer\s+[A-Za-z0-9_\-\.]{24,}")),
    ("google_key", re.compile(r"\bAIza[0-9A-Za-z_\-]{30,}")),
    ("aws_key", re.compile(r"\bAKIA[0-9A-Z]{16}\b")),
]

PATH_PATTERNS = [
    ("user_home", re.compile(r"(?i)\bC:\\\\?Users\\\\?lcq\b|\bC:/?Users/lcq\b")),
    ("workdir_abs", re.compile(r"(?i)\bD:\\\\?AiWork\\\\?|\bD:/AiWork\b")),
    ("temp_abs", re.compile(r"(?i)\bC:\\\\?Users\\\\?lcq\\\\?AppData\\\\?Local\\\\?Temp")),
]

# 这些文件里出现绝对路径/上游名是正常的（计划文档、许可声明、发布工具、契约证据）
ALLOW = {
    "docs/zcode-native-port-plan.md": ["workdir_abs", "user_home"],
    "docs/CHANGELOG.md": ["workdir_abs", "user_home"],
    "docs/Mergence-设计方案.md": ["workdir_abs", "user_home"],
    "src/THIRD-PARTY-LICENSES/README.md": ["workdir_abs", "user_home"],
    "README.md": ["workdir_abs", "user_home"],
    # zcode 的契约证据（Track 3 随包落位）：observations.md 逐条记录采样方法与
    # 采样靶机目录（一个本机绝对路径），与计划文档同性质 —— 是「这份契约从哪来」
    # 的登记，不是把用户配置写进仓库。其中的凭据一律是**合成值**
    # （`fixture-token-*` / `fixture-gw-key-*`，见下面 DATA_ALLOW_PREFIXES 说明）。
    "src/internal/provider/zcode/contract/observations.md": ["workdir_abs", "user_home"],
    # bodytransform 的 outbound 夹具：note 字段里写了一句复核时用的临时文件路径。
    # 同属契约定性记录（说明这条样本怎么来的），不含任何真实凭据。
    "src/internal/provider/zcode/bodytransform/testdata/outbound-requests.json": [
        "user_home", "temp_abs", "workdir_abs"],
}

# 契约夹具里的数据文件例外：**.db 是采样靶机写出的 SQLite，store 的「落盘契约」
# 测试直接拿它做逐字节对齐（`docs/contract/store/` 的结论就来自它）。
# 已核验内容全部为合成值：账号 token 一律 `fixture-token-*`、网关密钥
# `fixture-gw-key-abcdef`、后台密码 `1234`（采样机已知测试值），不含任何真实凭据。
DATA_ALLOW_PREFIXES = (
    "src/internal/provider/zcode/store/testdata/",
)

# 测试夹具里的 key 是显式占位串（abcdefghijklmnop / xxxx / zzzz / 0000），
# 不是真凭据。豁免按「文件 + 命中行必须含占位符」双条件，避免将来误放真 key。
TEST_PLACEHOLDER = {
    "test/drive_ui.py": True,
    "test/gui_check.py": True,
    "test/shot_tools/shots.py": True,
    # zcode 账号面板 e2e 的 GO_MODE 分支：给上游起一个网关 Key，好让面板的
    # 「密钥以掩码回显」那一项有值可渲染。值是写死的假串（`abcdef` 占位），
    # 不是任何真实凭据。
    "test/verify_zcode_accounts.py": True,
}
PLACEHOLDER_MARK = re.compile(
    r"x{4,}|z{4,}|0{4,}|abcdefghijklmnop|abcdef|placeholder|example|fixture", re.I)

# 数据类扩展名，出现在导出范围里就是意外
DATA_SUFFIX = {".db", ".sqlite", ".sqlite3", ".exe", ".syso", ".zip", ".log", ".key", ".pem"}

# 例外：Go 编译必需的图标资源与 syso 资源
DATA_ALLOW_FILES = {"src/app.ico", "src/appres.syso"}


def exported_files() -> list[str]:
    out: list[str] = []
    for f in sorted(ROOT_FILES):
        p = os.path.join(ROOT, f)
        if os.path.isfile(p):
            out.append(f)
    for d, exts in DIRS.items():
        base = os.path.join(ROOT, d)
        if not os.path.isdir(base):
            continue
        for dirpath, dirnames, filenames in os.walk(base):
            dirnames[:] = [x for x in dirnames if not x.startswith(".")]
            for fn in filenames:
                rel = os.path.relpath(os.path.join(dirpath, fn), ROOT).replace("\\", "/")
                ext = os.path.splitext(fn)[1].lower()
                if exts is None or ext in exts:
                    out.append(rel)
    return out


def main() -> int:
    as_json = "--json" in sys.argv
    files = exported_files()
    findings: list[dict] = []

    for rel in files:
        full = os.path.join(ROOT, rel)
        low = rel.lower()
        ext = os.path.splitext(low)[1]

        if ext in DATA_SUFFIX and low not in DATA_ALLOW_FILES:
            if not any(rel.startswith(p) for p in DATA_ALLOW_PREFIXES):
                findings.append({"kind": "data_file", "file": rel, "line": 0, "detail": ext})

        try:
            with open(full, "rb") as fh:
                raw = fh.read()
            text = raw.decode("utf-8", errors="replace")
        except OSError as exc:
            findings.append({"kind": "unreadable", "file": rel, "line": 0, "detail": str(exc)})
            continue

        allowed = set(ALLOW.get(rel, []))
        is_test_fixture = TEST_PLACEHOLDER.get(rel, False)
        for lineno, line in enumerate(text.splitlines(), 1):
            for name, pat in SECRET_PATTERNS:
                if not pat.search(line):
                    continue
                # 占位 key 豁免只在确属测试夹具且该行含占位标记时生效
                if is_test_fixture and PLACEHOLDER_MARK.search(line):
                    continue
                findings.append({"kind": name, "file": rel, "line": lineno,
                                 "detail": line.strip()[:120]})
            for name, pat in PATH_PATTERNS:
                if name in allowed:
                    continue
                if pat.search(line):
                    findings.append({"kind": name, "file": rel, "line": lineno,
                                     "detail": line.strip()[:120]})

    report = {"scanned": len(files), "findings": findings}
    if as_json:
        print(json.dumps(report, ensure_ascii=False, indent=2))
    else:
        print("扫描文件数：%d" % len(files))
        if not findings:
            print("结果：干净，无命中。")
        else:
            print("结果：命中 %d 处" % len(findings))
            for f in findings:
                print("  [%s] %s:%d  %s" % (f["kind"], f["file"], f["line"], f["detail"]))
    return 1 if findings else 0


if __name__ == "__main__":
    sys.exit(main())
