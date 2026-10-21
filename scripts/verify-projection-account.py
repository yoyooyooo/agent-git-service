#!/usr/bin/env python3
"""Evaluate prohibit_login on one newly created sandbox account, never an existing user.
Keeps the test account disabled and retains evidence; credentials never leave memory.
"""
import argparse
import base64
import http.cookiejar
from html.parser import HTMLParser
import json
from pathlib import Path
import secrets
import time
import urllib.error
import urllib.parse
import urllib.request

class Form(HTMLParser):
    def __init__(self):
        super().__init__()
        self.csrf=''
        self.links=[]
    def handle_starttag(self,tag,attrs):
        a=dict(attrs)
        if tag=='input' and a.get('name')=='_csrf':
            self.csrf=a.get('value','')
        if tag=='a':
            self.links.append(a.get('href') or '')

def main():
    ap=argparse.ArgumentParser()
    ap.add_argument('--forgejo',required=True)
    ap.add_argument('--token-file',required=True)
    ap.add_argument('--repo',required=True)
    ap.add_argument('--pr',type=int,required=True)
    ap.add_argument('--commit',required=True)
    ap.add_argument('--mapped-login',required=True)
    ap.add_argument('--output',required=True)
    args=ap.parse_args()
    out=Path(args.output)
    out.mkdir(mode=0o700,parents=True,exist_ok=False)
    token=Path(args.token_file).read_text().strip()
    login='ags-attribution-test-'+time.strftime('%Y%m%d%H%M%S')
    password=secrets.token_urlsafe(32)+'aA1!'
    email=login+'@example.test'
    report={'test_account':login,'checks':[]}
    api=urllib.request.build_opener(urllib.request.ProxyHandler({}))
    def call(path,method='GET',data=None,sudo=''):
        headers={'Authorization':'token '+token,'Content-Type':'application/json'}
        if sudo:
            headers['Sudo']=sudo
        req=urllib.request.Request(args.forgejo+'/api/v1'+path,method=method,headers=headers,data=json.dumps(data).encode() if data is not None else None)
        try:
            response=api.open(req,timeout=30)
        except urllib.error.HTTPError as e:
            response=e
        body=response.read()
        try:
            body=json.loads(body)
        except ValueError:
            body={}
        return response.code,body
    def record(name,**fields):
        entry={'check':name,**fields}
        report['checks'].append(entry)
        print(json.dumps(entry),flush=True)
    def browser_login():
        browser=urllib.request.build_opener(urllib.request.ProxyHandler({}),urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
        with browser.open(args.forgejo+'/user/login',timeout=30) as response:
            form=Form();form.feed(response.read().decode())
        assert form.csrf, 'login form missing CSRF'
        data=urllib.parse.urlencode({'_csrf':form.csrf,'user_name':login,'password':password}).encode()
        with browser.open(urllib.request.Request(args.forgejo+'/user/login',data=data),timeout=30) as response:
            result=response.geturl()
            text=response.read().decode()
        return browser,not urllib.parse.urlsplit(result).path.startswith('/user/login'),text
    created=False
    try:
        status,user=call('/admin/users','POST',{'username':login,'email':email,'password':password,'must_change_password':False,'send_notify':False})
        assert status==201, f'temporary account creation: {status}'
        created=True
        record('created temporary account',status=status,login=login)
        status,_=call('/repos/'+args.repo+'/collaborators/'+login,'PUT',{'permission':'read'})
        assert status==204
        browser,logged_in,_=browser_login()
        record('temporary account web login before prohibition',allowed=logged_in)
        assert logged_in
        with browser.open(args.forgejo+'/'+args.repo+'/commit/'+args.commit,timeout=30) as response:
            html=response.read().decode()
            assert '/user/login' not in response.geturl()
            form=Form();form.feed(html)
            links=[link for link in form.links if urllib.parse.urlsplit(link).path.rstrip('/')=='/'+args.mapped_login]
            (out/'commit-page.html').write_text(html)
            record('commit page mapped account links',status=response.status,mapped_login=args.mapped_login,count=len(links))
            assert len(links)>=1, 'Forgejo collapses identical author/committer into one account link; API checks both'
        status,_=call('/admin/users/'+login,'PATCH',{'email':email,'prohibit_login':True})
        assert status==200, f'prohibit temporary login: {status}'
        status,user=call('/users/'+login)
        record('temporary prohibit_login readback',status=status,prohibit_login=user.get('prohibit_login'))
        assert status==200 and user.get('prohibit_login') is True
        _,logged_in,_=browser_login()
        record('fresh web login after prohibition',allowed=logged_in)
        assert not logged_in
        status,sudo_user=call('/user',sudo=login)
        record('admin Sudo GET user after prohibition',status=status,login=sudo_user.get('login'))
        status,comment=call('/repos/'+args.repo+f'/issues/{args.pr}/comments','POST',{'body':'Temporary prohibit_login Sudo evaluation.'},sudo=login)
        record('admin Sudo comment after prohibition',status=status,actor=(comment.get('user') or {}).get('login'))
        report['sudo_usable_when_prohibited']=status==201 and (comment.get('user') or {}).get('login')==login
        report['result']='evaluated'
    finally:
        if created:
            # Only the exact newly created test account may change login state.
            status,_=call('/admin/users/'+login,'PATCH',{'email':email,'prohibit_login':True})
            report['cleanup']={'test_account_retained':True,'prohibit_login':True,'status':status}
        (out/'report.json').write_text(json.dumps(report,indent=2))
        print(json.dumps({'evidence':str(out/'report.json'),'result':report.get('result','failed')}),flush=True)

if __name__=='__main__':
    main()
