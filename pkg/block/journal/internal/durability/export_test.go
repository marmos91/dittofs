package durability

// SetAfterAppend installs a hook Lose runs once its record is appended and
// synced, before anything else of the loss takes effect.
func (d *Durability) SetAfterAppend(f func()) { d.afterAppend = f }

// SetBeforePublish installs a hook Lose runs after the generation is raised
// and the counters are counted, just before its event is published.
func (d *Durability) SetBeforePublish(f func()) { d.beforePublish = f }

// SetReadOnly installs the read-only check Lose consults before it appends.
func (d *Durability) SetReadOnly(f func() error) { d.readOnly = f }
