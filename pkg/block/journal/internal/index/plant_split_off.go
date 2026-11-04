//go:build !journal_plant_split

package index

// plantSplitBug, when set, plants a known bug in the held set's three-way
// split, so a test can show the model-based fuzz finds one. Only a build with
// the journal_plant_split tag sets it; everywhere else it is false and the
// branch it guards compiles away.
const plantSplitBug = false
