package health

import (
	"fmt"
)

func colorGreen(s string) string  { return "\033[32m" + s + "\033[0m" }
func colorYellow(s string) string { return "\033[33m" + s + "\033[0m" }
func colorRed(s string) string    { return "\033[31m" + s + "\033[0m" }

// colorStatus returns the status string colored according to severity.
func colorStatus(status string) string {
	switch status {
	case "healthy":
		return colorGreen(status)
	case "degraded", "unknown":
		return colorYellow(status)
	default: // "unhealthy" and anything else
		return colorRed(status)
	}
}

// entityRow holds the label and status for a single row in the entity table.
type entityRow struct {
	label  string
	status StatusReport
}

// printSection prints a titled section of entity rows.
// It is a no-op when rows is empty.
func printSection(title string, rows []entityRow) {
	if len(rows) == 0 {
		return
	}
	fmt.Println()
	fmt.Printf("  %s:\n", title)
	for _, r := range rows {
		line := fmt.Sprintf("    %-20s %s", r.label, colorStatus(r.status.Status))
		if r.status.Message != "" {
			line += fmt.Sprintf("  (%s)", r.status.Message)
		}
		fmt.Println(line)
	}
}

// PrintEntityStatus prints a unified status table for all entity types.
// Any nil or empty slice is silently skipped.
func PrintEntityStatus(ent Entities) {
	shareRows := make([]entityRow, len(ent.Shares))
	for i, s := range ent.Shares {
		shareRows[i] = entityRow{s.Name, s.Status}
	}
	printSection("Shares", shareRows)

	bsRows := make([]entityRow, len(ent.BlockStores))
	for i, b := range ent.BlockStores {
		label := b.Name
		if b.Kind != "" {
			label = b.Kind + "/" + b.Name
		}
		bsRows[i] = entityRow{label, b.Status}
	}
	printSection("Block Stores", bsRows)

	msRows := make([]entityRow, len(ent.MetaStores))
	for i, m := range ent.MetaStores {
		msRows[i] = entityRow{m.Name, m.Status}
	}
	printSection("Metadata Stores", msRows)

	adapterRows := make([]entityRow, len(ent.Adapters))
	for i, a := range ent.Adapters {
		adapterRows[i] = entityRow{a.Type, a.Status}
	}
	printSection("Adapters", adapterRows)

	if len(ent.Errors) > 0 {
		fmt.Println()
		fmt.Printf("  %s:\n", colorYellow("Warnings"))
		for _, e := range ent.Errors {
			fmt.Printf("    %s %s\n", colorYellow("!"), e)
		}
	}
}
