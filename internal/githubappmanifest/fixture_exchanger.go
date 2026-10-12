package githubappmanifest

import "fmt"

// FixtureExchanger returns a fixed conversion and records each code once.
type FixtureExchanger struct {
	Conversion Conversion
	Calls      []string
	FailCode   string
}

// Convert returns the fixture. A repeated code fails.
func (e *FixtureExchanger) Convert(code string) (Conversion, error) {
	e.Calls = append(e.Calls, code)
	if code == e.FailCode {
		return Conversion{}, fmt.Errorf("fixture conversion rejected")
	}
	return e.Conversion, nil
}
