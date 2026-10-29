package report

import "testing"

// CheckCTRF returns every way data breaks CTRF's schema, so the package's
// external tests can check a real run's summary.json.
func CheckCTRF(t *testing.T, data []byte) []string {
	t.Helper()
	return check(loadSchema(t), decode(t, data), "$")
}
