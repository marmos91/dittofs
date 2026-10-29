package seeded

func methodValue(f interface{ Authorize() error }) func() error { return f.Authorize }
