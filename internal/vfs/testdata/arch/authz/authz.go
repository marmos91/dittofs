package authz

func authorize(f interface{ Authorize() error }) error { return f.Authorize() }
