package main

// System describes a competitor to benchmark.
type System struct {
	// Name is the unique identifier (e.g., "kernel-nfs").
	Name string

	// Protocol is the mount protocol: "nfs", "smb", or "fuse".
	Protocol string

	// Port is the service port on the server VM (0 for FUSE).
	Port int

	// MountOpts are additional mount options beyond the defaults.
	MountOpts string

	// InstallScript is the filename in infra/scripts/ to provision this system.
	InstallScript string
}

// allSystems is the full list of competitors to benchmark.
var allSystems = []System{
	{
		Name:          "dittofs-badger-fs",
		Protocol:      "nfs",
		Port:          12049,
		MountOpts:     "tcp,port=12049,mountport=12049",
		InstallScript: "dittofs-badger-fs.sh",
	},
	{
		Name:          "dittofs-badger-s3",
		Protocol:      "nfs",
		Port:          12049,
		MountOpts:     "tcp,port=12049,mountport=12049",
		InstallScript: "dittofs-badger-s3.sh",
	},
	{
		Name:          "dittofs-edge",
		Protocol:      "nfs",
		Port:          12049,
		MountOpts:     "tcp,port=12049,mountport=12049",
		InstallScript: "dittofs-edge.sh",
	},
	{
		Name:          "kernel-nfs",
		Protocol:      "nfs",
		Port:          2049,
		MountOpts:     "tcp",
		InstallScript: "kernel-nfs.sh",
	},
	{
		Name:          "ganesha",
		Protocol:      "nfs",
		Port:          2049,
		MountOpts:     "tcp",
		InstallScript: "ganesha.sh",
	},
	{
		Name:          "rclone",
		Protocol:      "nfs",
		Port:          2049,
		MountOpts:     "tcp",
		InstallScript: "rclone.sh",
	},
	{
		Name:          "samba",
		Protocol:      "smb",
		Port:          445,
		MountOpts:     "username=bench,password=bench",
		InstallScript: "samba.sh",
	},
	{
		Name:          "juicefs",
		Protocol:      "fuse",
		Port:          0,
		InstallScript: "juicefs.sh",
	},
	// S3-backed competitors.
	{
		Name:          "rclone-s3",
		Protocol:      "nfs",
		Port:          2049,
		MountOpts:     "tcp,port=2049,mountport=2049,nfsvers=3",
		InstallScript: "rclone-s3.sh",
	},
	{
		Name:          "juicefs-s3",
		Protocol:      "nfs",
		Port:          2049,
		MountOpts:     "tcp",
		InstallScript: "juicefs-s3.sh",
	},
	{
		Name:          "s3ql",
		Protocol:      "nfs",
		Port:          2049,
		MountOpts:     "tcp",
		InstallScript: "s3ql.sh",
	},
}

// systemsByName provides O(1) lookup by system name.
var systemsByName = func() map[string]*System {
	m := make(map[string]*System, len(allSystems))
	for i := range allSystems {
		m[allSystems[i].Name] = &allSystems[i]
	}
	return m
}()

// AllSystems returns the full list of competitors to benchmark.
func AllSystems() []System {
	return allSystems
}

// FindSystem returns the system with the given name, or nil if not found.
func FindSystem(name string) *System {
	return systemsByName[name]
}
