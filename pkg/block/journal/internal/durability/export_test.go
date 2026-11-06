package durability

import "github.com/cubbit/dittofs/pkg/block/journal/internal/segment"

// SetAfterAppend installs a hook Lose runs once its append step is over, that
// is once the record is appended and synced or the append was refused or
// failed, before anything else of the loss takes effect.
func (d *Durability) SetAfterAppend(f func()) { d.afterAppend = f }

// SetBeforePublish installs a hook Lose runs after the generation is raised
// and the counters are counted, just before its event is published.
func (d *Durability) SetBeforePublish(f func()) { d.beforePublish = f }

// SetReadOnly installs the read-only check Lose consults before it appends.
func (d *Durability) SetReadOnly(f func() error) { d.refuse = f }

// SetBeforeSync installs a hook Lose runs between appending its record and
// syncing the stream.
func (d *Durability) SetBeforeSync(f func()) { d.beforeSync = f }

// BindStreams makes the streams the ones Lose appends through, as Start does,
// for a layer that is not started over them.
func (d *Durability) BindStreams(s *segment.Streams) { d.streams.Store(s) }
