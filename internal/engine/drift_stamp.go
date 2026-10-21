package engine

// stamp is what a file's metadata says about when and where it last changed, beyond
// the size and modification time a tool can set back.
type stamp struct {
	ctimeNs int64
	ino     uint64
	dev     uint64
}
