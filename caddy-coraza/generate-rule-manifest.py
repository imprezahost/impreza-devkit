"""Generate static metadata from the pinned, embedded CRS 4.25.0 source.
Usage: python generate-rule-manifest.py /path/to/coraza-coreruleset/v4@v4.25.0
Writes the SDK manifest; the API carries an identical byte-for-byte copy.
No request-derived rule messages or matched values enter this manifest.
"""
import json,pathlib,re,sys
source=pathlib.Path(sys.argv[1]).resolve()
assert source.name.endswith('v4.25.0'), 'Only the pinned CRS v4.25.0 source is accepted'
rules={}
for file in sorted((source/'rules/@owasp_crs').glob('*.conf')):
 text=file.read_text(encoding='utf-8')
 category='other'
 for prefix,cat in [('REQUEST-942','SQLi'),('REQUEST-941','XSS'),('REQUEST-930','LFI'),('REQUEST-931','LFI'),('REQUEST-932','RCE'),('REQUEST-933','RCE'),('REQUEST-944','RCE'),('REQUEST-920','protocol'),('REQUEST-921','protocol'),('REQUEST-911','protocol')]:
  if file.name.startswith(prefix):category=cat
 for found in re.finditer(r'\bid\s*:\s*[\'\"]?(9[0-9]{5})',text):
  ident=int(found.group(1));start=text.rfind('\nSec',0,found.start());end=text.find('\nSec',found.end());chunk=text[start+1:end if end>=0 else len(text)];chunk=re.sub(r'\\\r?\n','',chunk)
  msg=re.search(r'''\bmsg\s*:\s*'((?:\\.|[^'])*)' '''.strip(),chunk)
  # Control/initialization and anomaly summation are not threat detections.
  detection=bool(msg) and re.match(r'^(REQUEST-(?:911|912|913|920|921|930|931|932|933|934|941|942|943|944)|RESPONSE-(?:950|951|952|953|954|955|956))-',file.name) is not None
  description=msg.group(1).replace("\\'","'") if detection else 'OWASP CRS control or anomaly evaluation.'
  description=re.sub(r'%\{[^}]+\}','the matched input',description)
  description=' '.join(description.split())[:160]
  blocking=bool(re.search(r'(?:^|,)\s*(?:deny|drop)\s*(?:,|$)',chunk))
  rules[str(ident)]={'blocking':blocking,'category':category,'description':description,'detection':detection}
assert len(rules)==625, 'The exact embedded CRS rule inventory changed'
assert rules['942100']['detection'] and rules['949110']['blocking'] and not rules['949110']['detection']
target=pathlib.Path(__file__).resolve().parents[1]/'sdk-go/client/shield_rules.json'
target.write_text(json.dumps({'crs_version':'4.25.0','rules':rules},indent=2,sort_keys=True)+'\n',encoding='utf-8',newline='\n')
print(json.dumps({'rules':len(rules),'detections':sum(r['detection'] for r in rules.values())}))
