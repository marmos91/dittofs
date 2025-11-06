package metadata

// TimeDelta represents time resolution with seconds and nanoseconds.
type TimeDelta struct {
	// Seconds component of time resolution
	Seconds uint32

	// Nseconds component of time resolution (0-999999999)
	Nseconds uint32
}
