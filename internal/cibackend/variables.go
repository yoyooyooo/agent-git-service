package cibackend

import "context"

// Variable is a non-secret variable defined at the repository scope.
type Variable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Variables contains one repository-scoped page and its full inventory count.
type Variables struct {
	Items []Variable
	Total int
}

// RepositoryVariableReader reads stored repository variables, without inheritance.
type RepositoryVariableReader interface {
	RepositoryVariables(context.Context, string, int, int) (Variables, error)
	RepositoryVariable(context.Context, string, string) (Variable, error)
}

// ValidVariableName checks the provider's case-insensitive identifier syntax.
// Reads do not reapply reserved-prefix restrictions used when creating variables.
func ValidVariableName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '_' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}
