#!/usr/bin/env python3
"""把 CRD 清单转换成 kubeconform 用的 JSON Schema。

用法：crd2schema.py <CRD 清单所在目录> <输出目录>
输出：<输出目录>/<group>/<kind 小写>_<version>.json，
对应 kubeconform 的 -schema-location '<输出目录>/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
"""
import json
import os
import sys

import yaml


def main(src: str, out: str) -> None:
    count = 0
    for name in sorted(os.listdir(src)):
        with open(os.path.join(src, name), encoding="utf-8") as f:
            for doc in yaml.safe_load_all(f):
                if not doc or doc.get("kind") != "CustomResourceDefinition":
                    continue
                spec = doc["spec"]
                group = spec["group"]
                kind = spec["names"]["kind"].lower()
                for ver in spec.get("versions", []):
                    schema = (ver.get("schema") or {}).get("openAPIV3Schema")
                    if not schema:
                        continue
                    schema = dict(schema)
                    props = dict(schema.get("properties") or {})
                    # CRD 里的 metadata 一般只写了 type: object；放开字段，避免 -strict 把 name / labels 判为多余
                    props["metadata"] = {"type": "object", "additionalProperties": True}
                    schema["properties"] = props
                    os.makedirs(os.path.join(out, group), exist_ok=True)
                    path = os.path.join(out, group, f"{kind}_{ver['name']}.json")
                    with open(path, "w", encoding="utf-8") as w:
                        json.dump(schema, w)
                    count += 1
    if count == 0:
        sys.exit("no CRD schemas generated: check the downloaded manifests")
    print(f"generated {count} schemas")


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
