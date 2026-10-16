package journalv2

// The lifecycle calls, written exactly as the journal's contract writes them,
// with no options. If a parameter is dropped, reordered or retyped, this file
// stops compiling.
var (
	_ = func() (JournalID, error) { return Init("", InstallationID{}) }
	_ = func() (*Journal, error) { return Open("", Version{}, JournalID{}) }
	_ = func() error { return Restamp("", JournalID{}, InstallationID{}) }
	_ = func(j *Journal) error { return j.Close() }
)
