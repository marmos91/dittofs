package backend

// Plan is a resolved (backend, protocol) target the matrix expands over passes.
type Plan struct {
	Backend  *Backend
	Protocol Protocol
	Support  Support
}

// SystemLabel is the cell's System string, e.g. "dittofs-s3-nfs3".
func (p Plan) SystemLabel() string { return p.Backend.Name + "-" + string(p.Protocol) }

// Cell is one unit of work in the matrix: a (system, workload, size, protocol,
// pass) target and the mount/target dir fio runs against.
type Cell struct {
	System   string
	Workload string
	Size     string // selector (class name or explicit)
	Protocol string
	Pass     string
	Target   string // mount/target dir for this cell
}

// ReadWorkloads are the workloads whose cold (post-evict) pass is meaningful —
// only reads exercise the from-S3 path. Write/metadata workloads run warm only,
// so the cold pass isn't padded with numbers that say nothing about S3 latency.
var ReadWorkloads = map[string]bool{
	"seq-read":     true,
	"rand-read-4k": true,
	"mixed-rw":     true,
}

// ManagedMatrix expands resolved plans into cells: a warm pass for every
// workload, plus a cold pass (post-evict) for read workloads when eviction is
// on. target is left empty — the runner fills it per plan after Mount.
func ManagedMatrix(plans []Plan, workloads, sizes []string, evict bool) []Cell {
	if len(sizes) == 0 {
		sizes = []string{"medium"}
	}
	var cells []Cell
	for _, p := range plans {
		for _, w := range workloads {
			for _, s := range sizes {
				cells = append(cells, Cell{
					System: p.SystemLabel(), Workload: w, Size: s,
					Protocol: string(p.Protocol), Pass: "warm",
				})
				if evict && ReadWorkloads[w] {
					cells = append(cells, Cell{
						System: p.SystemLabel(), Workload: w, Size: s,
						Protocol: string(p.Protocol), Pass: "cold",
					})
				}
			}
		}
	}
	return cells
}
