//go:build journalv2_plant_split

package index

// plantSplitBug plants a known bug in the held set's three-way split, so a
// test can show the model-based fuzz finds one.
const plantSplitBug = true
