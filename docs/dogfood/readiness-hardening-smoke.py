"""Isolated built-CLI smoke test; GitHub and provider responses are local fixtures.
Run: python3 docs/dogfood/readiness-hardening-smoke.py /absolute/path/to/forgecell
Leaves its temporary Lab for evidence inspection. Never calls a real provider or GitHub.
"""
import json, os, pathlib, subprocess, sys, tempfile, hashlib
binary = os.path.abspath(sys.argv[1])
root = pathlib.Path(tempfile.mkdtemp(prefix='forgecell-hardening-smoke-'))
repo = root / 'repo'
repo.mkdir()
lab = root / 'lab'
(lab / 'formulas').mkdir(parents=True)
bin_dir = root / 'bin'
bin_dir.mkdir()
env = dict(os.environ)
env['PATH'] = str(bin_dir) + os.pathsep + env['PATH']

def cmd(args):
    p = subprocess.run(args, cwd=repo, env=env, text=True, capture_output=True)
    if p.returncode and (not (p.stdout.startswith('{') and args[1] == 'run')):
        raise RuntimeError(str(args) + ': ' + p.stderr)
    return p.stdout
for args in [['init', '-q'], ['config', 'user.name', 'Fixture'], ['config', 'user.email', 'fixture@example.test']]:
    cmd(['git'] + args)
(repo / 'code.txt').write_text('')
(repo / 'package.json').write_text('{"scripts":{"test":"false"}}')
cmd(['git', 'add', '.'])
cmd(['git', 'commit', '-qm', 'fixture'])
cmd(['git', 'remote', 'add', 'origin', 'https://github.com/fixture/fixture.git'])
(bin_dir / 'gh').write_text('#!/usr/bin/env python3\nimport json,sys\nprint(json.dumps({"number":1,"title":"Fixture","body":"Change code.txt","state":"OPEN","url":"https://github.com/fixture/fixture/issues/1","updatedAt":"fixture"}) if sys.argv[1]=="issue" else "[]")\n')
(bin_dir / 'gh').chmod(448)
provider = bin_dir / 'fixture-provider'
provider.write_text("#!/usr/bin/env python3\nimport json,pathlib,sys\nif '--help' in sys.argv:print('fixture provider');sys.exit()\nif 'mcp' in sys.argv:print('[]');sys.exit()\nx=json.loads(sys.stdin.read().split('Forgecell request (data):\\n',1)[1]);output=pathlib.Path(sys.argv[sys.argv.index('--output-last-message')+1])\nif x['kind']=='ticket-analysis':\n e=x['repository']['evidence'][0]['id']\n value={'summary':'Fixture change','scope':[{'path':'code.txt','reason':'Requested','evidence':['issue']}],'acceptance':[{'description':'One coding invocation','evidence':['issue']}],'checks':[{'id':'fixture-check','category':'candidate','argv':['/bin/sh','-c','echo first-failure; exit 1'],'dir':'.','timeoutMs':5000,'required':True,'definitions':[e],'reason':'Fixture','evidence':['issue',e],'independentProvenance':''}],'impacts':[],'unknowns':[],'questions':[]}\n output.write_text(json.dumps(value))\nelse:\n p=pathlib.Path('code.txt');p.write_text(p.read_text()+'C');output.write_text('fixture coded')\n")
provider.chmod(448)
command = json.dumps([binary, '__adapter', 'codex', str(provider)])
(lab / 'formulas/sample.yaml').write_text('schemaVersion: v0\nkind: formula\nid: sample\nintake: {source: github-issues, repo: fixture/fixture}\nharness:\n  binding: codex\n  command: ' + command + '\natoms:\n  - {id: intake, type: intake}\n  - {id: scope, type: gate, purpose: scope}\n  - {id: harness, type: harness}\n  - {id: check, type: check}\n  - {id: gate, type: gate, purpose: review}\n  - {id: ship, type: ship}\n  - {id: document, type: document}\n')
(lab / 'lab.json').write_text('{"schemaVersion":"v0","activeFormulaId":"sample"}')

def run(*args):
    return json.loads(cmd([binary, 'run', '1', '--lab', str(lab), '--target', 'main', '--json', *args]))
r = run()
assert r['readiness']['phase'] == 'scope-waiting', r
r = run('--approve', r['readiness']['plans'][-1]['digest'])
assert r['readiness']['phase'] == 'failed', r
first = r['verificationAttempts'][0]
path = lab / first['result']['path']
before = path.read_bytes()
assert hashlib.sha256(before).hexdigest() == first['result']['sha256']
p = r['readiness']['plans'][-1]
plan = p['plan']
plan['continuation'] = 'checks-only'
plan['analysis']['checks'][0]['argv'] = ['/bin/sh', '-c', 'test "$(cat code.txt)" = C']
plan_path = root / 'retry.json'
plan_path.write_text(json.dumps(plan))
cmd([binary, 'amend', r['id'], '--lab', str(lab), '--parent', p['digest'], '--plan', str(plan_path)])
r = json.loads((lab / 'ledgers' / f"{r['id']}.json").read_text())
r = run('--approve', r['readiness']['plans'][-1]['digest'])
assert r['readiness']['phase'] == 'review-waiting', r
assert (pathlib.Path(r['workspace']['path']) / 'code.txt').read_text() == 'C'
assert path.read_bytes() == before
assert [a['status'] for a in r['verificationAttempts']] == ['failed', 'passed']
print(json.dumps({'fixtureRoot': str(root), 'molecule': r['id'], 'outcome': 'passed', 'codingInvocations': 1, 'verificationAttempts': 2, 'oldResultUnchanged': True, 'finalPhase': r['readiness']['phase']}))
