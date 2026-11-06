//go:build linux

package routebstage0

import (
	"strings"
	"testing"
)

func TestFixedUnitRequiresNoForceInDefinitionAndManager(t *testing.T) {
	r := fixtureRegistration()
	l := loaded{registration: r, host: HostProfile{ToolBundle: rootFor(r.ID) + "/bundle"}}
	i := r.Invocations[0]
	raw := "[Service]\nType=simple\nUser=root\nGroup=root\nRestart=no\nKillMode=mixed\nSendSIGKILL=no\nTimeoutStopSec=120\nNoNewPrivileges=yes\nExecStart=" + r.NativeImage.Path + " serve --registration " + r.ID + " --operation " + i.Operation + "\n"
	raw += "ExecStopPost=" + managerStopCommand(r, i) + "\n"
	if err := l.verifyUnitDefinition(i, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{strings.Replace(raw, "SendSIGKILL=no\n", "", 1), strings.Replace(raw, "SendSIGKILL=no", "SendSIGKILL=yes", 1), strings.Replace(raw, "SendSIGKILL=no", "SendSIGKILL=no\nSendSIGKILL=yes", 1)} {
		if err := l.verifyUnitDefinition(i, []byte(value)); err == nil {
			t.Fatal("accepted default, enabled or duplicate force-kill setting")
		}
	}
	properties := map[string]string{"FragmentPath": i.Definition.Path, "DropInPaths": "", "User": "root", "Group": "root", "Type": "simple", "Restart": "no", "KillMode": "mixed", "SendSIGKILL": "no", "NoNewPrivileges": "yes"}
	properties["ExecStopPost"] = "{ path=" + r.NativeImage.Path + " ; argv[]=" + managerStopCommand(r, i) + " ; ignore_errors=no ; start_time=[n/a] ; stop_time=[n/a] ; pid=0 ; code=(null) ; status=0/0 }"
	if err := l.verifyEffectiveUnit(i, properties); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "yes", "false"} {
		properties["SendSIGKILL"] = value
		if err := l.verifyEffectiveUnit(i, properties); err == nil {
			t.Fatal("accepted changed or unknown effective force-kill setting", value)
		}
	}
	properties["SendSIGKILL"] = "no"
	for _, changed := range []string{strings.Replace(raw, "ExecStopPost="+managerStopCommand(r, i)+"\n", "", 1),
		strings.Replace(raw, "manager-complete", "serve", 1), raw + "ExecStopPost=/bin/true\n"} {
		if l.verifyUnitDefinition(i, []byte(changed)) == nil {
			t.Fatal("missing, replaced or duplicate completion hook accepted")
		}
	}
	properties["ExecStopPost"] = ""
	if l.verifyEffectiveUnit(i, properties) == nil {
		t.Fatal("missing effective completion hook accepted")
	}
}
