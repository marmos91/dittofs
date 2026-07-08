package backend

// local-disk is the no-storage-backend control: a plain directory re-exported
// over NFS (knfsd) and SMB (Samba) and mounted back over loopback. It carries
// zero S3 cost, so its numbers are the protocol-overhead ceiling every
// S3-backed cell is read against. Cold pass = OS page-cache drop only (no S3).
func init() {
	register(newSrcBackend(srcBackend{
		name:   "local-disk",
		protos: []Protocol{ProtoNFS3, ProtoNFS4, ProtoSMB3},
		srcDir: "/srv/bench-local-disk",
	}))
}
