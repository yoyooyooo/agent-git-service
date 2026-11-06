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
	if err := l.verifyUnitDefinition(i, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{strings.Replace(raw, "SendSIGKILL=no\n", "", 1), strings.Replace(raw, "SendSIGKILL=no", "SendSIGKILL=yes", 1), strings.Replace(raw, "SendSIGKILL=no", "SendSIGKILL=no\nSendSIGKILL=yes", 1)} {
		if err := l.verifyUnitDefinition(i, []byte(value)); err == nil {
			t.Fatal("accepted default, enabled or duplicate force-kill setting")
		}
	}
	properties := map[string]string{"FragmentPath": i.Definition.Path, "DropInPaths": "", "User": "root", "Group": "root", "Type": "simple", "Restart": "no", "KillMode": "mixed", "SendSIGKILL": "no", "NoNewPrivileges": "yes"}
	if err := verifyEffectiveUnit(i, properties); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "yes", "false"} {
		properties["SendSIGKILL"] = value
		if err := verifyEffectiveUnit(i, properties); err == nil {
			t.Fatal("accepted changed or unknown effective force-kill setting", value)
		}
	}
}
