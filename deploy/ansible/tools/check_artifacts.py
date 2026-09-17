#!/usr/bin/env python3
"""用 Ansible 真正使用的解析器（PyYAML）复核平台渲染出的 ansible 产物。

平台内部的渲染后自校验用的是 Go 的 yaml.v3，与 ansible 侧的 PyYAML 是两套实现；
个别边界上严格程度不同，因此产物落盘后用下游解析器再过一遍。

用法（先在 middleware-ops 目录执行 `go run ./cmd/renderdump ./render-artifacts`）：

    python3 deploy/ansible/tools/check_artifacts.py ./render-artifacts

退出码 0 表示全部可被 PyYAML 解析；非 0 会打印首个出错位置。
注意：playbook 与 inventory 里不含真实口令（渲染器只输出脱敏版本），可安全留存。
"""

from __future__ import annotations

import pathlib
import sys

import yaml


def check(path: pathlib.Path) -> bool:
    try:
        doc = yaml.safe_load(path.read_text(encoding="utf-8"))
    except yaml.YAMLError as exc:
        print(f"[FAIL] {path.name}: {exc}")
        return False
    plays = len(doc) if isinstance(doc, list) else 1
    print(f"[ ok ] {path.name}（plays={plays}）")
    return True


def main() -> int:
    if len(sys.argv) != 2:
        print(__doc__)
        return 2
    root = pathlib.Path(sys.argv[1])
    if not root.is_dir():
        print(f"目录不存在：{root}")
        return 2

    playbooks = sorted(p for p in root.glob("*.yml") if not p.name.endswith(".vars.yml"))
    vars_files = sorted(root.glob("*.vars.yml"))
    if not playbooks:
        print(f"{root} 下没有产物，请先执行：go run ./cmd/renderdump {root}")
        return 2

    failed = [p.name for p in playbooks + vars_files if not check(p)]
    print(f"---- 共 {len(playbooks) + len(vars_files)} 个产物，失败 {len(failed)} 个 ----")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
