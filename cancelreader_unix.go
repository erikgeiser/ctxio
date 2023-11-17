//go:build solaris

package ctxio

// NewCancelReader returns a reader and a cancel function. If the input reader
// is an *os.File, the cancel function can be used to interrupt a blocking call
// read call. In this case, the cancel function returns true if the call was
// cancelled successfully. If the input reader is not a *os.File or the file
// descriptor is 1024 or larger, the cancel function does nothing and always
// returns false. The generic unix implementation is based on the posix select
// syscall.
func NewCancelReader(file File) (cancelReader, error) {
	return NewSelectCancelReader(file)
}
