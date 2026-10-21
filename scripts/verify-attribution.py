#!/usr/bin/env python3
"""Foreground, isolated AGS -> real Forgejo acceptance. No deployed AGS writes.
Secrets are read from named files, never emitted. Evidence and scratch state
survive cleanup; only processes owned by this invocation are stopped.
"""
import argparse
import base64
import hashlib
import http.client
import http.server
import json
import os
from pathlib import Path
import secrets
import socket
import sqlite3
import ssl
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--binary', required=True)
    ap.add_argument('--forgejo', required=True)
    ap.add_argument('--token-file', required=True)
    ap.add_argument('--forgejo-login', required=True)
    ap.add_argument('--ags-login', default='fixture')
    ap.add_argument('--output', required=True)
    ap.add_argument('--source-sha', default='')
    ap.add_argument('--source-repo', default='')
    args = ap.parse_args()
    root = Path(args.output).resolve()
    root.mkdir(mode=0o700, parents=True, exist_ok=False)
    token = Path(args.token_file).read_text().strip()
    ags_token = secrets.token_hex(24)
    report = {'work': str(root), 'checks': [], 'source_sha': args.source_sha}
    server = proxy = None
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))

    def redact(text):
        return str(text).replace(token, '<redacted>').replace(ags_token, '<redacted>')

    def record(name, value):
        line = {'check': name, **value}
        report['checks'].append(line)
        print(json.dumps(line), flush=True)

    def request(base, path, credential, method='GET', data=None):
        req = urllib.request.Request(base + path, data=None if data is None else json.dumps(data).encode(),
                                     method=method, headers={'Authorization': 'token ' + credential, 'Content-Type': 'application/json'})
        try:
            res = opener.open(req, timeout=30)
        except urllib.error.HTTPError as e:
            res = e
        body = res.read().decode()
        try:
            body = json.loads(body)
        except ValueError:
            pass
        return res.code, body

    def forgejo(path, method='GET', data=None):
        return request(args.forgejo, '/api/v1' + path, token, method, data)

    def run(cmd, extra=None, cwd=None, timeout=90):
        env = {k: os.environ[k] for k in ('PATH', 'GOROOT', 'GOPATH', 'GOCACHE', 'GOMODCACHE') if k in os.environ}
        env.update(HOME=str(root), LANG='C.UTF-8', GIT_TERMINAL_PROMPT='0', NO_PROXY='*', no_proxy='*',
                   GIT_AUTHOR_NAME=args.ags_login, GIT_COMMITTER_NAME=args.ags_login,
                   GIT_AUTHOR_EMAIL=args.ags_login+'@example.test', GIT_COMMITTER_EMAIL=args.ags_login+'@example.test')
        if extra:
            env.update(extra)
        res = subprocess.run(cmd, cwd=cwd or root, env=env, capture_output=True, text=True, timeout=timeout)
        if res.returncode:
            raise RuntimeError(redact(f'{cmd[0]} {cmd[1:3]}: exit {res.returncode}\n{res.stdout}\n{res.stderr}'))
        return res.stdout.strip()

    def port():
        with socket.socket() as s:
            s.bind(('127.0.0.1', 0))
            return s.getsockname()[1]

    try:
        status, user = forgejo('/user')
        assert status == 200 and user['is_admin'], 'existing admin credential required'
        status, mapped = forgejo('/users/' + args.forgejo_login)
        assert status == 200, 'reuse an existing Forgejo user'
        owner = user['login']
        name = 'ags-attribution-sandbox-' + time.strftime('%Y%m%d%H%M%S')
        target = owner + '/' + name
        report['forgejo_repo'] = target
        status, created = forgejo('/user/repos', 'POST', {'name': name, 'private': True, 'auto_init': False, 'default_branch': 'main'})
        assert status == 201, f'sandbox create: {status}'
        # This is a new sandbox only; no existing repository permissions change.
        status, _ = forgejo('/repos/'+target, 'PATCH', {'has_actions': True})
        assert status == 200
        status, permission = forgejo('/repos/'+target+'/collaborators/'+args.forgejo_login+'/permission')
        record('preflight permission', {'status': status, 'permission': permission.get('permission') if isinstance(permission,dict) else None,
                                        'target_repo': target, 'token_owner': owner, 'mapped_login': mapped['login']})
        assert status == 404 or (status == 200 and permission.get('permission') in ('none', 'read')), 'choose a target user without write access'

        http_port, tls_port = port(), port()
        base = f'http://127.0.0.1:{http_port}'
        ags_repo = args.ags_login + '/' + name
        config = {'attribution': {'instance': 'verification', 'principals': {args.ags_login: {'forgejo': args.forgejo_login}}},
                  'forgejo': {'enabled': True, 'base_url': args.forgejo, 'token_file': str(Path(args.token_file).resolve()),
                              'auto_create_repo': False, 'auto_pr': True, 'private_repos': True,
                              'repos': {ags_repo: {'owner': owner, 'repo': name, 'base_branch': 'main', 'auto_pr': True}}}}
        (root/'integrations.yaml').write_text(json.dumps(config))
        (root/'integrations.yaml').chmod(0o600)
        log = open(root/'server.log', 'w')
        server_env = {k: os.environ[k] for k in ('PATH',) if k in os.environ}
        server_env.update(HOME=str(root), PORT=str(http_port), DB_DSN='sqlite:'+str(root/'ags.sqlite')+'?_foreign_keys=on',
                          GIT_REPO_DIR=str(root/'repos'), BASE_URL=base, AGS_API_BASE_URL=base,
                          ADMIN_LOGIN=args.ags_login, ADMIN_TOKEN=ags_token, ALLOW_ANY_TOKEN='false',
                          LISTEN_MODE='production', ENVIRONMENT='development', ENABLE_WORKFLOW_EXEC='0',
                          AGS_INTEGRATIONS_CONFIG=str(root/'integrations.yaml'), LOG_FORMAT='json', NO_PROXY='*')
        server = subprocess.Popen([str(Path(args.binary).resolve())], cwd=root, env=server_env, stdin=subprocess.DEVNULL, stdout=log, stderr=log)
        deadline = time.monotonic()+45
        while time.monotonic()<deadline:
            if server.poll() is not None:
                raise RuntimeError('isolated AGS exited; inspect '+str(root/'server.log'))
            try:
                ready = request(base, '/readyz', ags_token)
                if ready[0] == 200:
                    break
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(.15)
        else:
            raise RuntimeError('isolated AGS readiness timeout')
        status, who = request(base, '/api/v3/user', ags_token)
        assert status == 200 and who['login'] == args.ags_login
        record('doctor', {'status': status, 'ags_login': who['login'], 'server_pid': server.pid})
        status, _ = request(base, '/api/v3/user/repos', ags_token, 'POST', {'name': name, 'private': True, 'auto_init': True, 'default_branch': 'main'})
        assert status == 201
        # Fixture setup only: this synthetic caller owns its repo but is not a
        # site admin during any Git/gh acceptance operation.
        with sqlite3.connect(root/'ags.sqlite') as fixture_db:
            fixture_db.execute('UPDATE users SET site_admin = 0 WHERE login = ?', (args.ags_login,))
        status, who = request(base, '/api/v3/user', ags_token)
        assert status == 200 and who['login'] == args.ags_login and not who['site_admin']
        record('non-admin AGS caller', {'status':status,'login':who['login'],'site_admin':who['site_admin']})

        # Stock gh uses HTTPS for custom host:port. This owned TLS front only
        # forwards to this run's loopback AGS; it never proxies a deployed server.
        run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-keyout',str(root/'key.pem'),'-out',str(root/'cert.pem'),'-days','1','-subj','/CN=127.0.0.1','-addext','subjectAltName=IP:127.0.0.1'])
        class Proxy(http.server.BaseHTTPRequestHandler):
            def log_message(self, *_):
                pass
            def relay(self):
                upstream = http.client.HTTPConnection('127.0.0.1', http_port, timeout=60)
                headers = {k: v for k, v in self.headers.items() if k.lower() not in ('host','connection')}
                body = self.rfile.read(int(self.headers.get('Content-Length','0')))
                upstream.request(self.command,self.path,body,headers)
                response = upstream.getresponse()
                payload = response.read()
                self.send_response(response.status)
                for k,v in response.getheaders():
                    if k.lower() not in ('connection','transfer-encoding','content-length'):
                        self.send_header(k,v)
                self.send_header('Content-Length',str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)
                upstream.close()
            do_GET=do_POST=do_PATCH=do_PUT=do_DELETE=relay
        proxy = http.server.ThreadingHTTPServer(('127.0.0.1',tls_port),Proxy)
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.load_cert_chain(root/'cert.pem',root/'key.pem')
        proxy.socket = tls.wrap_socket(proxy.socket,server_side=True)
        threading.Thread(target=proxy.serve_forever,daemon=True).start()
        gh_home = root/'gh'
        gh_home.mkdir(mode=0o700)
        (gh_home/'config.yml').write_text('prompt: disabled\n')
        gh_env = {'GH_CONFIG_DIR':str(gh_home),'GH_HOST':f'127.0.0.1:{tls_port}','GH_ENTERPRISE_TOKEN':ags_token,'SSL_CERT_FILE':str(root/'cert.pem'),'GH_PROMPT_DISABLED':'1'}
        git_env = {'GIT_CONFIG_COUNT':'1','GIT_CONFIG_KEY_0':'http.extraHeader','GIT_CONFIG_VALUE_0':'Authorization: Bearer '+ags_token}
        checkout = root/'checkout'
        run(['git','clone',base+'/'+ags_repo+'.git',str(checkout)],git_env)
        # Forgejo workflow is sandbox scaffolding, not a public source change.
        workflows=checkout/'.forgejo/workflows'
        workflows.mkdir(parents=True)
        command = 'echo "attribution sandbox ${{ gitea.sha }} actor=${{ gitea.actor }}"'
        if args.source_sha:
            assert len(args.source_sha)==40 and all(c in '0123456789abcdef' for c in args.source_sha)
            assert args.source_repo.startswith('https://github.com/') and args.source_repo.endswith('.git')
            command += f'\ngit clone --filter=blob:none --no-checkout {args.source_repo} source\ngit -C source fetch origin {args.source_sha}\ngit -C source checkout --detach {args.source_sha}\ntest "$(git -C source rev-parse HEAD)" = {args.source_sha}\ncd source\ngo version\ngo test ./internal/attribution ./internal/forgejointegration ./internal/githubintegration ./internal/integrations -count=1 -timeout=120s'
        workflow = {'name':'Attribution acceptance','on':['push','pull_request'],'jobs':{'verify':{'runs-on':'ubuntu-latest','steps':[{'name':'Verify sandbox and exact candidate','run':command}]}}}
        if args.source_sha:
            source_root=Path(__file__).resolve().parents[1]
            version=(source_root/'.go-version').read_text().strip()
            assert all(c in '0123456789.' for c in version)
            workflow['jobs']['verify']['container']={'image':'golang:'+version+'-bookworm'}
        (workflows/'attribution.yml').write_text(json.dumps(workflow))
        run(['git','add','.forgejo'],git_env,checkout)
        run(['git','commit','-m','test: sandbox attribution workflow'],git_env,checkout)
        run(['git','push','origin','main'],git_env,checkout)
        run(['git','switch','-c','agent/attribution'],git_env,checkout)
        (checkout/'attribution.txt').write_text('sandbox attribution\n')
        run(['git','add','attribution.txt'],git_env,checkout)
        run(['git','commit','-m','test: attributed change'],git_env,checkout)
        push=run(['git','push','-u','origin','agent/attribution'],git_env,checkout)
        head=run(['git','rev-parse','HEAD'],git_env,checkout)
        report['head_sha']=head
        record('git push through AGS',{'exit':0,'head_sha':head,'stdout':push})
        command=['gh','pr','create','--repo',ags_repo,'--head','agent/attribution','--base','main','--title','Sandbox attribution acceptance','--body','Isolated attribution verification; no business repository changes.']
        opened=run(command,gh_env,checkout)
        record('gh pr create',{'exit':0,'stdout':opened,'ags_login':args.ags_login})
        deadline=time.monotonic()+60
        while time.monotonic()<deadline:
            status, prs=forgejo('/repos/'+target+'/pulls?state=open')
            if status==200 and prs:
                pr=prs[0]
                if pr['head']['sha']==head:
                    break
            time.sleep(.25)
        else:
            raise RuntimeError('projection not ready; inspect server.log')
        number=pr['number']
        report['forgejo_pr']=pr['html_url']
        record('Forgejo PR readback',{'status':status,'number':number,'user':pr['user']['login'],'head_sha':pr['head']['sha'],'url':pr['html_url']})
        assert pr['user']['login']==args.forgejo_login
        status, permission=forgejo('/repos/'+target+'/collaborators/'+args.forgejo_login+'/permission')
        record('permission recovery readback',{'status':status,'permission':permission.get('permission')})
        assert status==200 and permission.get('permission')=='write'
        # A second push after opening exercises synchronize, not only branch creation.
        (checkout/'attribution.txt').write_text('sandbox attribution update\n')
        run(['git','add','attribution.txt'],git_env,checkout)
        run(['git','commit','-m','test: update attributed PR'],git_env,checkout)
        run(['git','push','origin','agent/attribution'],git_env,checkout)
        head=run(['git','rev-parse','HEAD'],git_env,checkout)
        report['head_sha']=head
        deadline=time.monotonic()+30
        while time.monotonic()<deadline:
            status,pr=forgejo('/repos/'+target+f'/pulls/{number}')
            if status==200 and pr['head']['sha']==head:
                break
            time.sleep(.25)
        else:
            raise RuntimeError('PR update head did not synchronize')
        record('post-open git push readback',{'status':status,'head_sha':head,'user':pr['user']['login']})
        assert pr['user']['login']==args.forgejo_login
        comment=run(['gh','pr','comment','1','--repo',ags_repo,'--body','attribution acceptance comment'],gh_env,checkout)
        record('gh pr comment',{'exit':0,'stdout':comment})
        status, comments=forgejo('/repos/'+target+f'/issues/{number}/comments')
        projected=[c for c in comments if c.get('body')=='attribution acceptance comment']
        record('Forgejo comment readback',{'status':status,'comments':[{'id':c['id'],'user':c['user']['login'],'body':c['body']} for c in projected]})
        assert projected and all(c['user']['login']==args.forgejo_login for c in projected)
        close=run(['gh','pr','close','1','--repo',ags_repo],gh_env,checkout)
        record('gh pr close',{'exit':0,'stdout':close})
        status, timeline=forgejo('/repos/'+target+f'/issues/{number}/timeline')
        events=[{'type':c.get('type'),'user':c.get('user',{}).get('login')} for c in timeline if isinstance(c,dict)] if isinstance(timeline,list) else []
        record('Forgejo timeline readback',{'status':status,'events':events})
        assert status==200 and any(e['type']=='close' and e['user']==args.forgejo_login for e in events)
        # git pushes deliberately remain bot-owned. Confirm logs preserve the AGS actor.
        log.flush()
        records=[]
        for line in (root/'server.log').read_text().splitlines():
            try:
                entry=json.loads(line)
            except ValueError:
                continue
            if entry.get('msg')=='projection attribution':
                records.append(entry)
        assert any(e.get('op')=='collaborator_grant' and e.get('grant_applied') for e in records)
        assert any(e.get('op')=='push_ref' and e.get('ags_login')==args.ags_login and e.get('attribution')=='bot_only' for e in records)
        (root/'attribution.json').write_text(json.dumps(records,indent=2))
        with sqlite3.connect(root/'ags.sqlite') as observed_db:
            audits=observed_db.execute("SELECT action, actor_login, details FROM audit_log_entries WHERE action LIKE 'projection.%' ORDER BY id").fetchall()
        assert any(action=='projection.collaborator_grant' and login==args.ags_login for action,login,_ in audits)
        (root/'audit.json').write_text(json.dumps([{'action':action,'actor_login':login,'details':json.loads(details)} for action,login,details in audits],indent=2))
        record('structured attribution',{'grant_applied':True,'push_native_actor':owner,'ags_actor':args.ags_login,'evidence':str(root/'attribution.json')})
        status, runs=forgejo('/repos/'+target+'/actions/runs?limit=30')
        (root/'forgejo-ci-initial.json').write_text(json.dumps(runs,indent=2))
        record('Forgejo CI initial readback',{'status':status,'runs':[{k:r.get(k) for k in ('id','commit_sha','status','event','html_url')} for r in runs.get('workflow_runs',[])]})
        report['result']='passed'
    except Exception as e:
        report['result']='failed'
        report['error']=redact(e)
        raise
    finally:
        if proxy:
            proxy.shutdown()
            proxy.server_close()
        if server and server.poll() is None:
            server.terminate()
            try:
                server.wait(timeout=15)
            except subprocess.TimeoutExpired:
                server.kill()
                server.wait(timeout=5)
        report['cleanup']='only owned processes stopped; sandbox repo closed PR and evidence retained'
        (root/'report.json').write_text(json.dumps(report,indent=2))
        print(json.dumps({'result':report.get('result'),'evidence':str(root/'report.json')}),flush=True)

if __name__=='__main__':
    main()
