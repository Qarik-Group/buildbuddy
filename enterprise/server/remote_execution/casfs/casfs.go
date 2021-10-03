package casfs

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/container"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/dirtools"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/digest"
	"github.com/buildbuddy-io/buildbuddy/server/util/log"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/docker/go-units"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
	vfspb "github.com/buildbuddy-io/buildbuddy/proto/vfs"
	gstatus "google.golang.org/grpc/status"
)

// opStats tracks statistics for a single FUSE OP type.
type opStats struct {
	count     int
	timeSpent time.Duration
}

type CASFS struct {
	vfsClient  vfspb.FileSystemClient
	mountDir   string
	verbose    bool
	logFUSEOps bool

	server *fuse.Server // Not set until FS is mounted.

	root *Node

	mu             sync.Mutex
	internalTaskID string
	rpcCtx         context.Context
	opStats        map[string]*opStats
}

type Options struct {
	// Verbose enables logging of most per-file operations.
	Verbose bool
	// LogFUSEOps enabled logging of all operations received by the go fuse server from the kernel.
	LogFUSEOps bool
}

func New(vfsClient vfspb.FileSystemClient, mountDir string, options *Options) *CASFS {
	// This is so we always have a context set. The real context will be set in the PrepareForTask call.
	rpcCtx, cancel := context.WithCancel(context.Background())
	cancel()

	cfs := &CASFS{
		vfsClient:  vfsClient,
		mountDir:   mountDir,
		verbose:    options.Verbose,
		logFUSEOps: options.LogFUSEOps,
		rpcCtx:     rpcCtx,
		opStats:    make(map[string]*opStats),
	}
	root := &Node{cfs: cfs}
	cfs.root = root
	return cfs
}

func (cfs *CASFS) getRPCContext() context.Context {
	cfs.mu.Lock()
	defer cfs.mu.Unlock()
	return cfs.rpcCtx
}

func (cfs *CASFS) GetMountDir() string {
	return cfs.mountDir
}

type latencyRecorder struct {
	cfs *CASFS
}

func (r *latencyRecorder) Add(name string, dt time.Duration) {
	r.cfs.mu.Lock()
	defer r.cfs.mu.Unlock()
	stats, ok := r.cfs.opStats[name]
	if !ok {
		stats = &opStats{}
		r.cfs.opStats[name] = stats
	}
	stats.count++
	stats.timeSpent += dt
}

func (cfs *CASFS) Mount() error {
	if cfs.server != nil {
		return status.FailedPreconditionError("CASFS already mounted")
	}

	// The filesystem is mutated only through the FUSE filesystem so we can let the kernel cache node and attribute
	// information for an arbitrary amount of time.
	nodeAttrTimeout := 6 * time.Hour
	opts := &fs.Options{
		EntryTimeout: &nodeAttrTimeout,
		AttrTimeout:  &nodeAttrTimeout,
		MountOptions: fuse.MountOptions{
			AllowOther:    true,
			Debug:         cfs.logFUSEOps,
			DisableXAttrs: true,
			// Don't depend on the `fusermount` binary to make things simpler under Firecracker.
			DirectMount: true,
			FsName:      "casfs",
			MaxWrite:    fuse.MAX_KERNEL_WRITE,
		},
	}
	nodeFS := fs.NewNodeFS(cfs.root, opts)
	server, err := fuse.NewServer(nodeFS, cfs.mountDir, &opts.MountOptions)
	if err != nil {
		return status.UnavailableErrorf("could not mount CASFS at %q: %s", cfs.mountDir, err)
	}

	server.RecordLatencies(&latencyRecorder{cfs: cfs})

	go server.Serve()
	if err := server.WaitMount(); err != nil {
		return status.UnavailableErrorf("waiting for CASFS mount failed: %s", err)
	}

	cfs.server = server

	return nil
}

// PrepareForTask prepares the virtual filesystem layout according to the provided `fsLayout`.
// `ctx` is used for outgoing RPCs to the server and must stay alive as long as the filesystem is being used.
// The passed `taskID` os only used for logging purposes.
func (cfs *CASFS) PrepareForTask(ctx context.Context, taskID string, fsLayout *container.FileSystemLayout) error {
	cfs.mu.Lock()
	cfs.internalTaskID = taskID
	cfs.rpcCtx = ctx
	cfs.mu.Unlock()

	_, dirMap, err := dirtools.DirMapFromTree(fsLayout.Inputs)
	if err != nil {
		return err
	}

	var walkDir func(dir *repb.Directory, node *Node) error
	walkDir = func(dir *repb.Directory, parentNode *Node) error {
		for _, childDirNode := range dir.GetDirectories() {
			child := &Node{cfs: cfs, parent: parentNode}
			inode := cfs.root.NewPersistentInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFDIR})
			if !parentNode.AddChild(childDirNode.Name, inode, false) {
				return status.UnknownErrorf("could not add child %q to %q, already exists", childDirNode.Name, parentNode.relativePath())
			}
			if cfs.verbose {
				log.Debugf("[%s] Input directory: %s", cfs.taskID(), child.relativePath())
			}
			childDir, ok := dirMap[digest.NewKey(childDirNode.Digest)]
			if !ok {
				return status.NotFoundErrorf("could not find dir %q", childDirNode.Digest)
			}
			if err := walkDir(childDir, child); err != nil {
				return err
			}
		}
		for _, childFileNode := range dir.GetFiles() {
			child := &Node{cfs: cfs, parent: parentNode, fileNode: childFileNode, isInput: true}
			inode := cfs.root.NewPersistentInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFREG})
			if !parentNode.AddChild(childFileNode.Name, inode, false) {
				return status.UnknownErrorf("could not add child %q to %q, already exists", childFileNode.Name, parentNode.relativePath())
			}
			if cfs.verbose {
				log.Debugf("[%s] Input file: %s", cfs.taskID(), child.relativePath())
			}
		}
		for _, childSymlinkNode := range dir.GetSymlinks() {
			child := &Node{cfs: cfs, parent: parentNode, symlinkTarget: childSymlinkNode.GetTarget()}
			inode := cfs.root.NewPersistentInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFLNK})
			if !parentNode.AddChild(childSymlinkNode.Name, inode, false) {
				return status.UnknownErrorf("could not add child %q to %q, already exists", childSymlinkNode.Name, parentNode.relativePath())
			}
			if cfs.verbose {
				log.Debugf("[%s] Input symlink: %s", cfs.taskID(), child.relativePath())
			}
		}
		return nil
	}

	err = walkDir(fsLayout.Inputs.GetRoot(), cfs.root)
	if err != nil {
		return err
	}

	outputDirs := make(map[string]struct{})
	for _, dir := range fsLayout.OutputDirs {
		if cfs.verbose {
			log.Debugf("[%s] Output dir: %s", cfs.taskID(), dir)
		}
		outputDirs[dir] = struct{}{}
	}
	for _, file := range fsLayout.OutputFiles {
		if cfs.verbose {
			log.Debugf("[%s] Output file: %s", cfs.taskID(), file)
		}
		outputDirs[filepath.Dir(file)] = struct{}{}
	}
	for dir := range outputDirs {
		parts := strings.Split(dir, string(os.PathSeparator))
		node := cfs.root
		for _, p := range parts {
			childNode := node.GetChild(p)
			if childNode == nil {
				child := &Node{cfs: cfs, parent: node}
				inode := cfs.root.NewPersistentInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFDIR})
				if !node.AddChild(p, inode, false) {
					return status.UnknownErrorf("could not add child %q, already exists", p)
				}
				node = child
			} else {
				node = childNode.Operations().(*Node)
			}
		}
	}

	return nil
}

func (cfs *CASFS) taskID() string {
	cfs.mu.Lock()
	defer cfs.mu.Unlock()
	return cfs.internalTaskID
}

func (cfs *CASFS) FinishTask() error {
	cfs.mu.Lock()
	defer cfs.mu.Unlock()

	// TODO(vadim): propagate stats to ActionResult
	if cfs.verbose {
		var totalTime time.Duration
		log.Debugf("[%s] OP stats:", cfs.internalTaskID)
		for op, s := range cfs.opStats {
			log.Infof("[%s] %-20s num_calls=%-08d time=%.2fs", cfs.internalTaskID, op, s.count, s.timeSpent.Seconds())
			totalTime += s.timeSpent
		}
		log.Debugf("[%s] Total time spent in OPs: %s", cfs.internalTaskID, totalTime)
	}

	cfs.internalTaskID = "unset"

	return nil
}

func (cfs *CASFS) Unmount() error {
	if cfs.server == nil {
		return nil
	}

	return cfs.server.Unmount()
}

type Node struct {
	fs.Inode

	cfs    *CASFS
	parent *Node

	// Whether this node represents an input to the action.
	// Modifications to inputs are denied.
	isInput bool

	fileNode *repb.FileNode

	mu            sync.Mutex
	cachedAttrs   *vfspb.Attrs
	symlinkTarget string
}

func (n *Node) relativePath() string {
	return n.Path(nil)
}

func (n *Node) resetCachedAttrs() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cachedAttrs = nil
}

type remoteFile struct {
	ctx       context.Context
	vfsClient vfspb.FileSystemClient
	path      string
	id        uint64
	node      *Node
	// Optional file contents if the server returned the entire file contents inline as part of the Open RPC.
	data []byte

	mu         sync.Mutex
	readBytes  int
	readRPCs   int
	wroteBytes int
	writeRPCs  int
}

// rpcErrToSyscallErrno extracts the syscall errno passed via RPC error status.
// Returns a generic EIO errno if the error does not contain syscall err info.
func rpcErrToSyscallErrno(rpcErr error) syscall.Errno {
	if status.IsCanceledError(rpcErr) {
		return syscall.EINTR
	}

	s, ok := gstatus.FromError(rpcErr)
	if !ok {
		return syscall.EIO
	}
	for _, d := range s.Details() {
		if se, ok := d.(*vfspb.SyscallError); ok {
			return syscall.Errno(se.Errno)
		}
	}
	return syscall.EIO
}

func (f *remoteFile) Allocate(ctx context.Context, off uint64, size uint64, mode uint32) syscall.Errno {
	f.node.resetCachedAttrs()
	allocReq := &vfspb.AllocateRequest{
		HandleId: f.id,
		Mode:     mode,
		Offset:   int64(off),
		NumBytes: int64(size),
	}
	if _, err := f.vfsClient.Allocate(f.ctx, allocReq); err != nil {
		return rpcErrToSyscallErrno(err)
	}
	return fs.OK
}

func (f *remoteFile) Flush(ctx context.Context) syscall.Errno {
	if f.node.cfs.verbose {
		f.mu.Lock()
		log.Debugf("[%s] Flush %q, read %s (%d RPCs), wrote %s (%d RPCs)", f.node.cfs.taskID(), f.path, units.HumanSize(float64(f.readBytes)), f.readRPCs, units.HumanSize(float64(f.wroteBytes)), f.writeRPCs)
		f.mu.Unlock()
	}

	if _, err := f.vfsClient.Flush(f.ctx, &vfspb.FlushRequest{HandleId: f.id}); err != nil {
		return rpcErrToSyscallErrno(err)
	}
	return fs.OK
}

func (f *remoteFile) Fsync(ctx context.Context, flags uint32) syscall.Errno {
	if _, err := f.vfsClient.Fsync(f.ctx, &vfspb.FsyncRequest{HandleId: f.id}); err != nil {
		return rpcErrToSyscallErrno(err)
	}
	return fs.OK
}

func (f *remoteFile) Release(ctx context.Context) syscall.Errno {
	if f.node.cfs.verbose {
		f.mu.Lock()
		log.Debugf("[%s] Release %q, read %s (%d RPCs), wrote %s (%d RPCs)", f.node.cfs.taskID(), f.path, units.HumanSize(float64(f.readBytes)), f.readRPCs, units.HumanSize(float64(f.wroteBytes)), f.writeRPCs)
		f.mu.Unlock()
	}
	if _, err := f.vfsClient.Release(f.ctx, &vfspb.ReleaseRequest{HandleId: f.id}); err != nil {
		return rpcErrToSyscallErrno(err)
	}
	return fs.OK
}

type remoteFileReader struct {
	f        *remoteFile
	offset   int64
	numBytes int
}

func (r *remoteFileReader) Bytes(buf []byte) ([]byte, fuse.Status) {
	numBytes := r.numBytes
	if len(buf) < numBytes {
		numBytes = len(buf)
	}

	// If the file contents was returned inline as part of the Open RPC, read from it directly instead of making
	// additional RPCs.
	if r.f.data != nil {
		dataSize := int64(len(r.f.data))
		if r.offset >= dataSize {
			return nil, fuse.EIO
		}
		if int64(numBytes) > dataSize-r.offset {
			numBytes = int(dataSize - r.offset)
		}
		return r.f.data[r.offset : r.offset+int64(numBytes)], fuse.OK
	}

	readReq := &vfspb.ReadRequest{
		HandleId: r.f.id,
		Offset:   r.offset,
		NumBytes: int32(numBytes),
	}
	rsp, err := r.f.vfsClient.Read(r.f.ctx, readReq)
	if err != nil {
		return nil, fuse.ToStatus(rpcErrToSyscallErrno(err))
	}
	r.f.mu.Lock()
	r.f.readRPCs++
	r.f.readBytes += len(rsp.GetData())
	r.f.mu.Unlock()
	return rsp.GetData(), fuse.OK
}

func (r *remoteFileReader) Size() int {
	return r.numBytes
}

func (r *remoteFileReader) Done() {
}

func (f *remoteFile) Read(ctx context.Context, buf []byte, off int64) (res fuse.ReadResult, errno syscall.Errno) {
	res = &remoteFileReader{f: f, offset: off, numBytes: len(buf)}
	return
}

func (f *remoteFile) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	f.node.resetCachedAttrs()
	writeReq := &vfspb.WriteRequest{
		HandleId: f.id,
		Data:     data,
		Offset:   off,
	}
	rsp, err := f.vfsClient.Write(f.ctx, writeReq)
	if err != nil {
		return 0, rpcErrToSyscallErrno(err)
	}
	f.mu.Lock()
	f.writeRPCs++
	f.wroteBytes += int(rsp.GetNumBytes())
	f.mu.Unlock()
	return rsp.GetNumBytes(), 0
}

func openRemoteFile(ctx context.Context, vfsClient vfspb.FileSystemClient, path string, flags uint32, mode uint32, node *Node) (*remoteFile, error) {
	req := &vfspb.OpenRequest{
		Path:  path,
		Flags: flags,
		Mode:  mode,
	}
	rsp, err := vfsClient.Open(ctx, req)
	if err != nil {
		return nil, rpcErrToSyscallErrno(err)
	}
	return &remoteFile{
		ctx:       ctx,
		vfsClient: vfsClient,
		path:      path,
		id:        rsp.GetHandleId(),
		node:      node,
		data:      rsp.GetData(),
	}, nil
}

func (n *Node) Open(ctx context.Context, flags uint32) (fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	// Don't allow writes to input files.
	if n.isInput && (int(flags)&(os.O_WRONLY|os.O_RDWR)) != 0 {
		log.Warningf("[%s] Denied attempt to write to input file %q", n.cfs.taskID(), n.relativePath())
		return nil, 0, syscall.EPERM
	}

	if n.cfs.verbose {
		log.Debugf("[%s] Open %q", n.cfs.taskID(), n.relativePath())
	}

	rf, err := openRemoteFile(n.cfs.getRPCContext(), n.cfs.vfsClient, n.relativePath(), flags, 0, n)
	if err != nil {
		return nil, 0, rpcErrToSyscallErrno(err)
	}
	return rf, 0, 0
}

func (n *Node) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (node *fs.Inode, fh fs.FileHandle, fuseFlags uint32, errno syscall.Errno) {
	if n.cfs.verbose {
		log.Debugf("[%s] Create %q", n.cfs.taskID(), filepath.Join(n.relativePath(), name))
	}

	child := &Node{cfs: n.cfs, parent: n}
	inode := n.cfs.root.NewPersistentInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFREG})
	if !n.AddChild(name, inode, false) {
		log.Warningf("[%s] Could not add child %q to %q, already exists", n.cfs.taskID(), name, n.relativePath())
		return nil, nil, 0, syscall.EIO
	}

	rf, err := openRemoteFile(n.cfs.getRPCContext(), n.cfs.vfsClient, filepath.Join(n.relativePath(), name), flags, mode, child)
	if err != nil {
		return nil, nil, 0, rpcErrToSyscallErrno(err)
	}

	out.Mode = mode

	return inode, rf, 0, 0
}

func (n *Node) CopyFileRange(ctx context.Context, fhIn fs.FileHandle, offIn uint64, out *fs.Inode, fhOut fs.FileHandle, offOut uint64, len uint64, flags uint64) (uint32, syscall.Errno) {
	n.resetCachedAttrs()

	rf, ok := fhIn.(*remoteFile)
	if !ok {
		log.Warningf("file handle is not a *remoteFile")
		return 0, syscall.EBADF
	}
	wf, ok := fhOut.(*remoteFile)
	if !ok {
		log.Warningf("file handle is not a *remoteFile")
	}

	if n.cfs.verbose {
		log.Debugf("[%s] CopyFileRange %q => %q", n.cfs.taskID(), rf.path, wf.path)
	}

	rsp, err := n.cfs.vfsClient.CopyFileRange(n.cfs.getRPCContext(), &vfspb.CopyFileRangeRequest{
		ReadHandleId:      rf.id,
		ReadHandleOffset:  int64(offIn),
		WriteHandleId:     wf.id,
		WriteHandleOffset: int64(offOut),
		NumBytes:          uint32(len),
		Flags:             uint32(flags),
	})
	if err != nil {
		return 0, rpcErrToSyscallErrno(err)
	}
	return rsp.GetNumBytesCopied(), fs.OK
}

func (n *Node) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	newParentNode, ok := newParent.EmbeddedInode().Operations().(*Node)
	if !ok {
		log.Warningf("[%s] Parent is not a *Node", n.cfs.taskID())
		return syscall.EINVAL
	}
	if n.cfs.verbose {
		log.Debugf("[%s] Rename %q => %q", n.cfs.taskID(), filepath.Join(n.relativePath(), name), filepath.Join(newParentNode.relativePath(), newName))
	}

	existingSrcINode := n.GetChild(name)
	if existingSrcINode == nil {
		log.Warningf("[%s] Child %q not found in %q", n.cfs.taskID(), name, n.relativePath())
		return syscall.EINVAL
	}
	existingSrcNode, ok := existingSrcINode.Operations().(*Node)
	if !ok {
		log.Warningf("[%s] Source is not a *Node", n.cfs.taskID())
		return syscall.EINVAL
	}
	if existingSrcNode.isInput {
		log.Warningf("[%s] Denied attempt to rename input file %q", n.cfs.taskID(), filepath.Join(n.relativePath(), name))
		return syscall.EPERM
	}

	// TODO(vadim): don't allow directory rename to affect input files

	existingTargetNode := newParent.EmbeddedInode().GetChild(newName)
	if existingTargetNode != nil {
		existingNode, ok := existingTargetNode.Operations().(*Node)
		if !ok {
			log.Warningf("[%s] Existing target is not a *Node", n.cfs.taskID())
			return syscall.EINVAL
		}
		// Don't allow a rename to overwrite an input file.
		if existingNode.fileNode != nil {
			log.Warningf("[%s] Denied attempt to rename over input file %q", n.cfs.taskID(), existingNode.relativePath())
			return syscall.EPERM
		}
	}

	_, err := n.cfs.vfsClient.Rename(n.cfs.getRPCContext(), &vfspb.RenameRequest{
		OldPath: filepath.Join(n.relativePath(), name),
		NewPath: filepath.Join(newParent.EmbeddedInode().Path(nil), newName),
	})
	if err != nil {
		return rpcErrToSyscallErrno(err)
	}

	return fs.OK
}

func (n *Node) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	if n.cfs.verbose {
		log.Debugf("[%s] Getattr %q", n.cfs.taskID(), n.relativePath())
	}

	if n.fileNode != nil {
		out.Size = uint64(n.fileNode.GetDigest().SizeBytes)
		out.Mode = 0444
		if n.fileNode.GetIsExecutable() {
			out.Mode |= 0111
		}
		return fs.OK
	}

	n.mu.Lock()
	attrs := n.cachedAttrs
	n.mu.Unlock()

	if attrs == nil {
		rsp, err := n.cfs.vfsClient.GetAttr(n.cfs.getRPCContext(), &vfspb.GetAttrRequest{Path: n.relativePath()})
		if err != nil {
			return rpcErrToSyscallErrno(err)
		}
		attrs = rsp.GetAttrs()
		n.mu.Lock()
		n.cachedAttrs = rsp.GetAttrs()
		n.mu.Unlock()
	}
	out.Size = uint64(attrs.GetSize())
	out.Mode = attrs.GetPerm()
	return fs.OK
}

func (n *Node) Setattr(ctx context.Context, f fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if n.cfs.verbose {
		log.Debugf("[%s] Setattr %q", n.cfs.taskID(), n.relativePath())
	}

	// Do not allow modifying attributes of input files.
	if n.isInput {
		log.Warningf("[%s] Denied attempt to change input file attributes %q", n.cfs.taskID(), n.relativePath())
		return syscall.EPERM
	}

	req := &vfspb.SetAttrRequest{
		Path: n.relativePath(),
	}
	if m, ok := in.GetMode(); ok {
		req.SetPerms = &vfspb.SetAttrRequest_SetPerms{Perms: m}
	}
	if s, ok := in.GetSize(); ok {
		req.SetSize = &vfspb.SetAttrRequest_SetSize{Size: int64(s)}
	}

	rsp, err := n.cfs.vfsClient.SetAttr(n.cfs.getRPCContext(), req)
	if err != nil {
		return rpcErrToSyscallErrno(err)
	}

	n.mu.Lock()
	n.cachedAttrs = rsp.GetAttrs()
	n.mu.Unlock()

	out.Size = uint64(rsp.GetAttrs().GetSize())
	out.Mode = rsp.GetAttrs().GetPerm()

	return fs.OK
}

func (n *Node) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if n.cfs.verbose {
		log.Debugf("Mkdir %q", filepath.Join(n.relativePath(), name))
	}

	path := filepath.Join(n.relativePath(), name)
	_, err := n.cfs.vfsClient.Mkdir(n.cfs.getRPCContext(), &vfspb.MkdirRequest{Path: path, Perms: mode})
	if err != nil {
		return nil, rpcErrToSyscallErrno(err)
	}

	child := &Node{cfs: n.cfs, parent: n}
	inode := n.cfs.root.NewPersistentInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFDIR})
	if !n.AddChild(name, inode, false) {
		log.Warningf("[%s] Mkdir could not add child %q to %q, already exists", n.cfs.taskID(), name, n.relativePath())
		return nil, syscall.EIO
	}
	return inode, 0
}

func (n *Node) Rmdir(ctx context.Context, name string) syscall.Errno {
	if n.cfs.verbose {
		log.Debugf("[%s] Rmdir %q", n.cfs.taskID(), filepath.Join(n.relativePath(), name))
	}
	_, err := n.cfs.vfsClient.Rmdir(n.cfs.getRPCContext(), &vfspb.RmdirRequest{Path: filepath.Join(n.relativePath(), name)})
	if err != nil {
		return rpcErrToSyscallErrno(err)
	}
	return fs.OK
}

func (n *Node) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	if n.cfs.verbose {
		log.Debugf("[%s] Readdir %q", n.cfs.taskID(), n.relativePath())
	}
	// The default implementation in the fuse library has a bug that can return entries in a different order across
	// multiple readdir calls. This can cause filesystem users to get incorrect directory listings.
	// Sorting this list on every Readdir call is potentially inefficient, but it doesn't seem to be a problem in
	// practice.
	var names []string
	for k := range n.Children() {
		names = append(names, k)
	}
	sort.Strings(names)
	var r []fuse.DirEntry
	for _, k := range names {
		ch := n.Children()[k]
		entry := fuse.DirEntry{
			Mode: ch.Mode(),
			Name: k,
			Ino:  ch.StableAttr().Ino,
		}
		r = append(r, entry)
	}
	return fs.NewListDirStream(r), 0
}

func (n *Node) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (node *fs.Inode, errno syscall.Errno) {
	src := filepath.Join(n.relativePath(), name)
	if n.cfs.verbose {
		log.Debugf("[%s] Symlink %q -> %q", n.cfs.taskID(), src, target)
	}

	path := filepath.Join(n.relativePath(), name)

	reqTarget := target
	if strings.HasPrefix(target, n.cfs.mountDir) {
		reqTarget = strings.TrimPrefix(target, n.cfs.mountDir)
	}

	_, err := n.cfs.vfsClient.Symlink(n.cfs.getRPCContext(), &vfspb.SymlinkRequest{Path: path, Target: reqTarget})
	if err != nil {
		return nil, rpcErrToSyscallErrno(err)
	}

	child := &Node{cfs: n.cfs, parent: n, symlinkTarget: target}
	inode := n.cfs.root.NewPersistentInode(ctx, child, fs.StableAttr{Mode: fuse.S_IFLNK})
	if !n.AddChild(name, inode, false) {
		log.Warningf("[%s] Symlink could not add child %q to %q, already exists", n.cfs.taskID(), name, n.relativePath())
		return nil, syscall.EIO
	}

	return inode, 0
}

func (n *Node) Readlink(ctx context.Context) ([]byte, syscall.Errno) {
	if n.cfs.verbose {
		log.Debugf("[%s] Readlink %q", n.cfs.taskID(), n.relativePath())
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.symlinkTarget != "" {
		return []byte(n.symlinkTarget), 0
	}
	return nil, syscall.EINVAL
}

func (n *Node) Unlink(ctx context.Context, name string) syscall.Errno {
	relPath := filepath.Join(n.relativePath(), name)
	if n.cfs.verbose {
		log.Debugf("[%s] Unlink %q", n.cfs.taskID(), relPath)
	}
	existingTargetNode := n.GetChild(name)
	if existingTargetNode == nil {
		log.Warningf("[%s] Child %q does not exist in %q", n.cfs.taskID(), name, n.relativePath())
		return syscall.EINVAL
	}

	existingNode, ok := existingTargetNode.Operations().(*Node)
	if !ok {
		log.Warningf("[%s] Existing node is not a *Node", n.cfs.taskID())
		return syscall.EINVAL
	}

	if existingNode.fileNode != nil {
		log.Warningf("[%s] Denied attempt to unlink input file %q", n.cfs.taskID(), relPath)
		return syscall.EPERM
	}

	_, err := n.cfs.vfsClient.Unlink(n.cfs.getRPCContext(), &vfspb.UnlinkRequest{Path: relPath})
	if err != nil {
		return rpcErrToSyscallErrno(err)
	}

	n.mu.Lock()
	if existingNode.symlinkTarget != "" {
		existingNode.symlinkTarget = ""
	}
	n.mu.Unlock()

	return fs.OK
}
