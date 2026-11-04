package platform

import "golang.org/x/sys/windows"

// calls is the table of system calls the storage makes on a file's handle or
// on the directory's. Each entry is the golang.org/x/sys/windows function
// itself, never a wrapper, and an argument the call takes as a command is
// passed as data, so a test that swaps the table sees each call with its
// arguments as Windows would.
type calls struct {
	createFile func(
		name *uint16,
		access uint32,
		mode uint32,
		sa *windows.SecurityAttributes,
		createmode uint32,
		attrs uint32,
		templatefile windows.Handle,
	) (windows.Handle, error)
	flushFileBuffers           func(handle windows.Handle) error
	closeHandle                func(handle windows.Handle) error
	getFileInformationByHandle func(
		handle windows.Handle,
		data *windows.ByHandleFileInformation,
	) error
	setFileInformationByHandle func(
		handle windows.Handle,
		class uint32,
		inBuffer *byte,
		inBufferLen uint32,
	) error
	writeFile func(
		handle windows.Handle,
		p []byte,
		done *uint32,
		overlapped *windows.Overlapped,
	) error
}

// realTable is the table NewOS gives every OS.
var realTable = calls{
	createFile:                 windows.CreateFile,
	flushFileBuffers:           windows.FlushFileBuffers,
	closeHandle:                windows.CloseHandle,
	getFileInformationByHandle: windows.GetFileInformationByHandle,
	setFileInformationByHandle: windows.SetFileInformationByHandle,
	writeFile:                  windows.WriteFile,
}
