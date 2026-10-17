//go:build windows

package mountfs

import (
	"syscall"
	"time"
)

var _ HostBackend = (*WindowsCoreHost)(nil)
var _ HostMetadataBackend = (*WindowsCoreHost)(nil)

func (host *WindowsCoreHost) executeLocked(request CoreRequest) CoreResponse {
	var reply CoreResponse
	if host.closed || host.process == nil || !host.healthy.Load() {
		reply.Errno = syscall.EIO
		return reply
	}
	request.Token = host.process.token
	if err := host.process.client.Call("Core.Execute", request, &reply); err != nil {
		// A lost response, including a write response, is ambiguous. Return an
		// error to the caller and never replay the operation on another engine.
		host.healthy.Store(false)
		reply = CoreResponse{Errno: syscall.EIO}
	}
	return reply
}

func (host *WindowsCoreHost) execute(request CoreRequest) CoreResponse {
	host.gate.RLock()
	defer host.gate.RUnlock()
	return host.executeLocked(request)
}

func (host *WindowsCoreHost) Getattr(name string) (Attr, syscall.Errno) {
	reply := host.execute(CoreRequest{Operation: "getattr", Name: name})
	return reply.Attr, reply.Errno
}
func (host *WindowsCoreHost) ReadDir(name string) ([]string, syscall.Errno) {
	reply := host.execute(CoreRequest{Operation: "readdir", Name: name})
	return reply.Entries, reply.Errno
}
func (host *WindowsCoreHost) Open(name string, flags int) (uint64, syscall.Errno) {
	host.gate.RLock()
	defer host.gate.RUnlock()
	reply := host.executeLocked(CoreRequest{Operation: "open", Name: name, Flags: flags})
	if reply.Errno == 0 {
		host.handlesMu.Lock()
		host.handles[reply.Handle] = true
		host.handlesMu.Unlock()
	}
	return reply.Handle, reply.Errno
}
func (host *WindowsCoreHost) Read(handle uint64, destination []byte, offset int64) (int, syscall.Errno) {
	if len(destination) > coreMaxIO {
		return 0, syscall.EINVAL
	}
	reply := host.execute(CoreRequest{Operation: "read", Handle: handle, Length: len(destination), Offset: offset})
	if reply.Errno == 0 {
		copy(destination, reply.Data)
	}
	return reply.Count, reply.Errno
}
func (host *WindowsCoreHost) Write(handle uint64, data []byte, offset int64) (int, syscall.Errno) {
	if len(data) > coreMaxIO {
		return 0, syscall.EINVAL
	}
	reply := host.execute(CoreRequest{Operation: "write", Handle: handle, Data: data, Offset: offset})
	return reply.Count, reply.Errno
}
func (host *WindowsCoreHost) TruncatePath(name string, size int64) syscall.Errno {
	return host.execute(CoreRequest{Operation: "truncate-path", Name: name, Size: size}).Errno
}
func (host *WindowsCoreHost) Truncate(handle uint64, size int64) syscall.Errno {
	return host.execute(CoreRequest{Operation: "truncate", Handle: handle, Size: size}).Errno
}
func (host *WindowsCoreHost) Flush(handle uint64) syscall.Errno {
	return host.execute(CoreRequest{Operation: "flush", Handle: handle}).Errno
}
func (host *WindowsCoreHost) Fsync(handle uint64) syscall.Errno {
	return host.execute(CoreRequest{Operation: "fsync", Handle: handle}).Errno
}
func (host *WindowsCoreHost) Release(handle uint64) syscall.Errno {
	host.gate.RLock()
	defer host.gate.RUnlock()
	errno := host.executeLocked(CoreRequest{Operation: "release", Handle: handle}).Errno
	// Release means the frontend caller has closed this handle even if the
	// engine has failed. A later cutover still requires a clean engine stop.
	host.handlesMu.Lock()
	delete(host.handles, handle)
	host.handlesMu.Unlock()
	return errno
}
func (host *WindowsCoreHost) Mkdir(name string, mode uint32) syscall.Errno {
	return host.execute(CoreRequest{Operation: "mkdir", Name: name, Mode: mode}).Errno
}
func (host *WindowsCoreHost) Rename(name, target string) syscall.Errno {
	return host.execute(CoreRequest{Operation: "rename", Name: name, Target: target}).Errno
}
func (host *WindowsCoreHost) Unlink(name string) syscall.Errno {
	return host.execute(CoreRequest{Operation: "unlink", Name: name}).Errno
}
func (host *WindowsCoreHost) Metadata(operation, name string, mode, uid uint32, access, modified time.Time) syscall.Errno {
	return host.execute(CoreRequest{Operation: operation, Name: name, Mode: mode, UID: uid, AccessTime: access, ModTime: modified}).Errno
}
