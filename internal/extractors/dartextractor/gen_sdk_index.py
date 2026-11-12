# Regenerates sdk_index.json.gz from a facts.jsonl snapshot of the Dart SDK repository:
#   python3 gen_sdk_index.py <sdk-snapshot>/facts.jsonl sdk_index.json.gz
import json,gzip,sys,re,collections
src,out=sys.argv[1],sys.argv[2]
LIBS=['core','async','collection','convert','math','io','typed_data']
KEEP=['symbol_kind','dart_construct','type_parameters','extends_type','mixin_types','interface_types','return_type','accessor','static','constructor','abstract','extension_on','exported','language','abstract_member','mixin_on']
facts=[];libs={}
for l in open(src):
    f=json.loads(l)
    fl=f.get('file','')
    m=re.match(r'sdk/lib/(\w+)/',fl)
    if not m or m.group(1) not in LIBS or f['kind']!='symbol':continue
    lib=m.group(1)
    segs=f['name'].split('.')[1:]
    if any(s.startswith('_') and s!='_' for s in segs):continue
    p=f.get('props',{})
    if p.get('symbol_kind') not in('class','interface','enum','type','method','function','mixin','extension'):continue
    host=f'sdk/lib/{lib}/{lib}.dart'
    libs['dart:'+lib]=host
    facts.append({'kind':'symbol','name':f['name'],'file':host,'repo':'','props':{k:p[k] for k in KEEP if k in p}})
facts.sort(key=lambda f:(f['file'],f['name']))
with gzip.GzipFile(out,'wb',mtime=0) as g:
    g.write(json.dumps({'libs':libs,'facts':facts},sort_keys=True,separators=(',',':')).encode())
import os
print(len(facts),collections.Counter(f['file'] for f in facts),os.path.getsize(out))
