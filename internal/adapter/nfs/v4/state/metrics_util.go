package state

import (
	"github.com/prometheus/client_golang/prometheus"
)

// registerOrReuse registers a collector with the given registerer.
// If the collector is already registered, it returns the existing one
// from the registry so that metrics continue to be exported correctly
// on server restart. Panics on non-AlreadyRegisteredError failures.
func registerOrReuse(reg prometheus.Registerer, c prometheus.Collector) prometheus.Collector {
	if err := reg.Register(c); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			return are.ExistingCollector
		}
		panic(err)
	}
	return c
}
