#!/usr/bin/env python3
"""Black-box tests for the Bash installer bootstrap. No network or live state."""
from __future__ import annotations
import base64, hashlib, json, os
from pathlib import Path
import subprocess, tempfile, textwrap, unittest

ROOT=Path(__file__).resolve().parents[2]
SCRIPT=ROOT/"scripts/install.sh"
VERSION="fork-20260924.1-rc9"
SOURCE="a"*40

FAKE_INSTALLER=r'''#!/usr/bin/env python3
import json, os, pathlib, sys
args=sys.argv[1:]
path=pathlib.Path(os.environ["AGS_TEST_ARGS"])
path.write_text(json.dumps(args))
def value(flag):
    i=args.index(flag); return args[i+1]
version=value("--version"); prefix=pathlib.Path(value("--prefix"))
if "--install" in args:
    bindir=prefix/"releases"/version/"bin"; bindir.mkdir(parents=True,exist_ok=True)
    for name in ("gh-server","ags-edge","ags-replication"):
        p=bindir/name; p.write_text("#!/bin/sh\nexit 0\n"); p.chmod(0o755)
if "--activate" in args:
    prefix.mkdir(parents=True,exist_ok=True)
    current=prefix/"current"
    if current.is_symlink(): current.unlink()
    current.symlink_to("releases/"+version)
print(json.dumps({"version":version,"install":"--install" in args,"activate":"--activate" in args}))
'''

def blob_sha(data: bytes) -> str:
    return hashlib.sha1(b"blob "+str(len(data)).encode()+b"\0"+data).hexdigest()

class BootstrapTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory(prefix="ags-install-sh-")
        self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name).resolve()
        self.bin=self.root/"fake-bin"; self.bin.mkdir()
        self.args_file=self.root/"args.json"
        payload=FAKE_INSTALLER.encode()
        gh=textwrap.dedent(f"""\
            #!/usr/bin/env python3
            import json, os, sys
            path=sys.argv[-1]
            version=os.environ["AGS_TEST_VERSION"]
            source=os.environ["AGS_TEST_SOURCE"]
            if path=="repos/yoyooyooo/agent-git-service/releases/tags/"+version:
                print(json.dumps({{"tag_name":version,"draft":False,"immutable":True,"prerelease":True,"target_commitish":source}}))
            elif path=="repos/yoyooyooo/agent-git-service/git/ref/tags/"+version:
                print(json.dumps({{"object":{{"type":"commit","sha":source}}}}))
            elif path=="repos/yoyooyooo/agent-git-service/contents/scripts/install-release.py?ref="+source:
                print(json.dumps({{"type":"file","path":"scripts/install-release.py","encoding":"base64","sha":"{blob_sha(payload)}","content":"{base64.b64encode(payload).decode()}"}}))
            else:
                raise SystemExit("unexpected fake gh path: "+path)
        """)
        fake=self.bin/"gh"; fake.write_text(gh); fake.chmod(0o755)
        self.env={k:v for k,v in os.environ.items() if k not in ("MULTICA_TOKEN","GH_TOKEN","GITHUB_TOKEN")}
        self.env.update(PATH=str(self.bin)+":"+os.environ["PATH"],HOME=str(self.root/"home"),
                        AGS_TEST_ARGS=str(self.args_file),AGS_TEST_VERSION=VERSION,AGS_TEST_SOURCE=SOURCE)
        (self.root/"home").mkdir()

    def invoke(self,*args,ok=True):
        result=subprocess.run(["/bin/bash",str(SCRIPT),*args],env=self.env,capture_output=True,text=True,timeout=30)
        self.assertEqual(result.returncode==0,ok,result.stderr)
        return result

    def test_plan_is_nonactivating_and_requires_prerelease_opt_in(self):
        self.invoke("plan","--version",VERSION,ok=False)
        self.invoke("plan","--version",VERSION,"--allow-prerelease")
        args=json.loads(self.args_file.read_text())
        self.assertNotIn("--install",args); self.assertNotIn("--activate",args)

    def test_install_activates_and_links_commands(self):
        prefix=self.root/"prefix"; bin_dir=self.root/"commands"
        self.invoke("install","--version",VERSION,"--allow-prerelease","--prefix",str(prefix),"--bin-dir",str(bin_dir))
        args=json.loads(self.args_file.read_text())
        self.assertIn("--install",args); self.assertIn("--activate",args)
        self.assertEqual(os.readlink(prefix/"current"),"releases/"+VERSION)
        for name in ("gh-server","ags-edge","ags-replication"):
            self.assertEqual(os.readlink(bin_dir/name),str(prefix/"current"/"bin"/name))

    def test_stage_does_not_activate_or_link(self):
        prefix=self.root/"prefix"; bin_dir=self.root/"commands"
        self.invoke("stage","--version",VERSION,"--allow-prerelease","--prefix",str(prefix),"--bin-dir",str(bin_dir))
        args=json.loads(self.args_file.read_text())
        self.assertIn("--install",args); self.assertNotIn("--activate",args)
        self.assertFalse((prefix/"current").exists()); self.assertFalse(bin_dir.exists())

    def test_upgrade_requires_existing_owned_selector(self):
        prefix=self.root/"prefix"; bin_dir=self.root/"commands"
        self.invoke("upgrade","--version",VERSION,"--allow-prerelease","--prefix",str(prefix),"--bin-dir",str(bin_dir),ok=False)
        current=prefix/"current"; prefix.mkdir(); current.symlink_to("releases/fork-20260924.1-rc8")
        self.invoke("upgrade","--version",VERSION,"--allow-prerelease","--prefix",str(prefix),"--bin-dir",str(bin_dir))
        self.assertEqual(os.readlink(current),"releases/"+VERSION)

    def test_foreign_command_path_is_rejected_before_activation(self):
        prefix=self.root/"prefix"; current=prefix/"current"; prefix.mkdir(); current.symlink_to("releases/fork-20260924.1-rc8")
        bin_dir=self.root/"commands"; bin_dir.mkdir(); foreign=bin_dir/"gh-server"; foreign.write_text("foreign")
        self.invoke("upgrade","--version",VERSION,"--allow-prerelease","--prefix",str(prefix),"--bin-dir",str(bin_dir),ok=False)
        self.assertEqual(os.readlink(current),"releases/fork-20260924.1-rc8")
        self.assertFalse(self.args_file.exists())

    def test_malformed_version_is_rejected_before_github(self):
        self.invoke("install","--version","latest","--allow-prerelease",ok=False)
        self.assertFalse(self.args_file.exists())

if __name__=="__main__":
    unittest.main()
