// Package durability is the layer between a stream's records and the moment
// they are durable: a ledger of every record a stream wrote and no sync has
// yet covered, and one worker per stream that completes the entries a sync
// covered.
//
// A stream reports to the layer through its Sink: Track registers each record
// as Append wrote it, Synced reports how far each completed sync of a segment
// reached, whoever issued the sync. A record is durable once its segment has
// been synced through the record's end, and Durable reads exactly that.
//
// Both sinks run with the stream's segment append domain held, and Track
// possibly with a file append domain too, so neither blocks, takes a lock of
// the segment package, reads the clock or takes a file domain. They update the
// stream's state under a leaf mutex and hand the rest to the stream's worker.
// The worker drops a covered entry from the ledger only under the file's
// append domain (RFC 1 §8.2), never two domains at once.
package durability
