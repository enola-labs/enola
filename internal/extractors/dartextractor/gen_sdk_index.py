# Regenerates sdk_index.json.gz from facts.jsonl snapshots of the Dart SDK and the
# Flutter repositories:
#   python3 gen_sdk_index.py <dart-sdk>/facts.jsonl <flutter>/facts.jsonl sdk_index.json.gz
import collections
import gzip
import json
import os
import re
import sys

sdk_facts, flutter_facts, out = sys.argv[1:4]
SDK_LIBS = ['core', 'async', 'collection', 'convert', 'math', 'io', 'typed_data']
FLUTTER_LIBS = ['animation', 'cupertino', 'foundation', 'gestures', 'material', 'painting', 'physics', 'rendering', 'scheduler', 'semantics', 'services', 'widgets']
KEEP = ['symbol_kind', 'dart_construct', 'type_parameters', 'extends_type', 'mixin_types', 'interface_types', 'return_type', 'accessor', 'static', 'constructor', 'abstract', 'extension_on', 'exported', 'language', 'abstract_member', 'mixin_on']
KINDS = {'class', 'interface', 'enum', 'type', 'method', 'function', 'mixin', 'extension'}
UI_HOST = 'engine/src/flutter/lib/ui/ui.dart'

libs, entries = {}, []


def keep(f, host):
    p = f.get('props', {})
    segs = f['name'].split('.')[1:]
    if any(s.startswith('_') and s != '_' for s in segs) or p.get('symbol_kind') not in KINDS:
        return
    entries.append({'kind': 'symbol', 'name': f['name'], 'file': host, 'repo': '', 'props': {k: p[k] for k in KEEP if k in p}})


for line in open(sdk_facts):
    f = json.loads(line)
    m = re.match(r'sdk/lib/(\w+)/', f.get('file', ''))
    if f['kind'] == 'symbol' and m and m.group(1) in SDK_LIBS:
        host = 'sdk/lib/%s/%s.dart' % (m.group(1), m.group(1))
        libs['dart:' + m.group(1)] = host
        keep(f, host)

for line in open(flutter_facts):
    f = json.loads(line)
    fl = f.get('file', '')
    if f['kind'] == 'symbol':
        m = re.match(r'packages/flutter/lib/src/(\w+)/', fl)
        if m and m.group(1) in FLUTTER_LIBS:
            host = 'packages/flutter/lib/%s.dart' % m.group(1)
            libs['package:flutter/%s.dart' % m.group(1)] = host
            keep(f, host)
        elif fl.startswith('engine/src/flutter/lib/ui/'):
            libs['dart:ui'] = UI_HOST
            keep(f, UI_HOST)
    elif f['kind'] == 'dependency':
        m = re.match(r'packages/flutter/lib/(\w+)\.dart$', fl)
        p = f.get('props', {})
        if not (m and m.group(1) in FLUTTER_LIBS and p.get('reexport')):
            continue
        uri = p.get('uri', '')
        if uri == 'dart:ui':
            target = uri
        elif p.get('resolved_file', '').startswith('packages/flutter/lib/') and '/src/' not in p['resolved_file']:
            target = 'package:flutter/%s' % os.path.basename(p['resolved_file'])
        else:
            continue
        props = {'uri': target, 'reexport': True, 'language': 'dart'}
        for k in ('show_names', 'hide_names'):
            if k in p:
                props[k] = p[k]
        entries.append({'kind': 'dependency', 'name': '%s -> %s' % (fl, target), 'file': 'packages/flutter/lib/%s.dart' % m.group(1), 'repo': '', 'props': props})

entries.sort(key=lambda f: (f['file'], f['kind'], f['name'], json.dumps(f['props'], sort_keys=True)))
with gzip.GzipFile(out, 'wb', mtime=0) as g:
    g.write(json.dumps({'libs': libs, 'facts': entries}, sort_keys=True, separators=(',', ':')).encode())
print(len(entries), os.path.getsize(out), collections.Counter(e['file'].split('/')[-1] for e in entries).most_common(6))
