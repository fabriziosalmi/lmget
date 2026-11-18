#!/usr/bin/env python3
"""check-cyclonedx.py SCHEMA_DIR BOM...: validate CycloneDX JSON documents
against the official schema (bom-1.6.schema.json and the schemas it refers
to, all in SCHEMA_DIR). Exits non-zero on the first invalid document.
Used by CI on the output of lmget sbom; needs the jsonschema package.
"""
import json
import pathlib
import sys

from jsonschema import Draft7Validator
from referencing import Registry, Resource

schema_dir = pathlib.Path(sys.argv[1])
registry = Registry()
for p in sorted(schema_dir.glob("*.json")):
    doc = json.loads(p.read_text())
    registry = registry.with_resource(doc.get("$id", p.name), Resource.from_contents(doc))
    registry = registry.with_resource(p.name, Resource.from_contents(doc))
bom_schema = json.loads((schema_dir / "bom-1.6.schema.json").read_text())
validator = Draft7Validator(bom_schema, registry=registry)
bad = 0
for name in sys.argv[2:]:
    errors = sorted(validator.iter_errors(json.loads(pathlib.Path(name).read_text())), key=lambda e: list(e.path))
    for e in errors[:10]:
        print(f"{name}: {'/'.join(map(str, e.path)) or '(root)'}: {e.message}")
    bad += bool(errors)
    print(f"{name}: {'INVALID' if errors else 'valid'} CycloneDX 1.6")
sys.exit(1 if bad else 0)
