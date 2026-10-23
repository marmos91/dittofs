package journalv2

// Reservation holds what Reserve took for a known number of writes. Its writes
// draw from it, and while it holds enough none of them is refused by a limit.
type Reservation struct{}

// WriteAt is the handle's WriteAt, drawing its record bytes, index entries,
// version and spares from the reservation. While the reservation holds enough,
// it is refused by none of the journal's limits and by no headroom rule.
func (r *Reservation) WriteAt(id FileID, off int64, p []byte, mtime int64) (Version, error) {
	return Version{}, ErrNotImplemented
}

// Close returns what the reservation still holds to the journal.
func (r *Reservation) Close() error {
	return ErrNotImplemented
}
