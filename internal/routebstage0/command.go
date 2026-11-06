package routebstage0

import "regexp"

var errorPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,95}$`)

func errorCode(err error) string {
	if err != nil && errorPattern.MatchString(err.Error()) {
		return err.Error()
	}
	return "stage0_failed"
}

func Execute(args []string) error {
	if len(args) != 5 || args[0] != "serve" || args[1] != "--registration" || args[3] != "--operation" {
		return refused("stage0_fixed_command_required")
	}
	if !operationPhase(args[4], args[4]) && args[4] != "serve" {
		return refused("stage0_operation_invalid")
	}
	return Run(args[2], args[4])
}

func ErrorCode(err error) string { return errorCode(err) }
