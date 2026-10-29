package seeded

func authorize(f interface{ Authorize() error }) error { return f.Authorize() }

func granted(f interface{ Granted() error }) error { return f.Granted() }
