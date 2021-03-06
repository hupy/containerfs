package main

import (
	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"fmt"
	cfs "github.com/ipdcode/containerfs/fs"
	"github.com/ipdcode/containerfs/logger"
	//mp "github.com/ipdcode/containerfs/proto/mp"
	"github.com/lxmgo/config"
	"golang.org/x/net/context"
	"log"
	"math"
	"os"
	"runtime/debug"
	"sync"
	"syscall"
	"time"
)

var uuid string
var mountPoint string

// FS struct
type FS struct {
	cfs *cfs.CFS
}

type dir struct {
	inode  uint64
	parent *dir
	fs     *FS

	// mu protects the fields below.
	//
	// If multiple dir.mu instances need to be locked at the same
	// time, the locks must be taken in topologically sorted
	// order, parent first.
	//
	// As there can be only one db.Update at a time, those calls
	// must be considered as lock operations too. To avoid lock
	// ordering related deadlocks, never hold mu while calling
	// db.Update.
	mu sync.Mutex

	name string

	// each in-memory child, so we can return the same node on
	// multiple Lookups and know what to do on .save()
	//
	// each child also stores its own name; if the value in the child
	// is an empty string, that means the child has been unlinked
	active map[string]*refcount
}

var _ = fs.FS(&FS{})

// Root ...
func (fs *FS) Root() (fs.Node, error) {
	n := newDir(fs, 0, nil, "")
	return n, nil
}

/*
   Blocks  uint64 // Total data blocks in file system.
   Bfree   uint64 // Free blocks in file system.
   Bavail  uint64 // Free blocks in file system if you're not root.
   Files   uint64 // Total files in file system.
   Ffree   uint64 // Free files in file system.
   Bsize   uint32 // Block size
   Namelen uint32 // Maximum file name length?
   Frsize  uint32 // Fragment size, smallest addressable data size in the file system.
*/

// Statfs ...
func (fs *FS) Statfs(ctx context.Context, req *fuse.StatfsRequest, resp *fuse.StatfsResponse) error {
	err, ret := cfs.GetFSInfo(fs.cfs.VolID)
	if err != 0 {
		return fuse.Errno(syscall.EIO)
	}
	resp.Bsize = 4 * 1024
	resp.Frsize = resp.Bsize
	resp.Blocks = ret.TotalSpace / uint64(resp.Bsize)
	resp.Bfree = ret.FreeSpace / uint64(resp.Bsize)
	resp.Bavail = ret.FreeSpace / uint64(resp.Bsize)
	return nil
}

type refcount struct {
	node   node
	kernel bool
	refs   uint32
}

func newDir(filesys *FS, inode uint64, parent *dir, name string) *dir {
	d := &dir{
		inode:  inode,
		name:   name,
		parent: parent,
		fs:     filesys,
		active: make(map[string]*refcount),
	}
	return d
}

var _ node = (*dir)(nil)
var _ fs.Node = (*dir)(nil)
var _ fs.NodeCreater = (*dir)(nil)
var _ fs.NodeForgetter = (*dir)(nil)
var _ fs.NodeMkdirer = (*dir)(nil)
var _ fs.NodeRemover = (*dir)(nil)
var _ fs.NodeRenamer = (*dir)(nil)
var _ fs.NodeStringLookuper = (*dir)(nil)
var _ fs.HandleReadDirAller = (*dir)(nil)

func (d *dir) setName(name string) {

	d.mu.Lock()
	d.name = name
	d.mu.Unlock()

}

func (d *dir) setParentInode(pdir *dir) {

	d.mu.Lock()
	defer d.mu.Unlock()
	d.parent = pdir
}

// Attr ...
func (d *dir) Attr(ctx context.Context, a *fuse.Attr) error {

	a.Mode = os.ModeDir | 0755
	//a.Valid = time.Second
	a.Inode = d.inode
	return nil
}

func (d *dir) Lookup(ctx context.Context, name string) (fs.Node, error) {

	d.mu.Lock()
	defer d.mu.Unlock()

	if a, ok := d.active[name]; ok {
		return a.node, nil
	}

	ret, inodeType, inode := d.fs.cfs.StatDirect(d.inode, name)

	if ret == 2 {
		return nil, fuse.ENOENT
	}
	if ret != 0 {
		return nil, fuse.ENOENT
	}
	n, _ := d.reviveNode(inodeType, inode, name)

	a := &refcount{node: n}
	d.active[name] = a

	a.kernel = true

	return a.node, nil
}

func (d *dir) reviveDir(inode uint64, name string) (*dir, error) {
	child := newDir(d.fs, inode, d, name)
	return child, nil
}

func (d *dir) reviveNode(inodeType bool, inode uint64, name string) (node, error) {
	if inodeType {
		child := &File{
			inode:  inode,
			name:   name,
			parent: d,
		}
		return child, nil
	}
	child, _ := d.reviveDir(inode, name)
	return child, nil

}

// ReadDirAll ...
func (d *dir) ReadDirAll(ctx context.Context) ([]fuse.Dirent, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	var res []fuse.Dirent
	ret, dirents := d.fs.cfs.ListDirect(d.inode)

	if ret == 2 {
		return nil, fuse.Errno(syscall.ENOENT)
	}
	if ret != 0 {
		return nil, fuse.Errno(syscall.EIO)
	}
	for _, v := range dirents {
		de := fuse.Dirent{
			Name: v.Name,
		}
		if v.InodeType {
			de.Type = fuse.DT_File
		} else {
			de.Type = fuse.DT_Dir
		}
		res = append(res, de)
	}

	return res, nil
}

// Create ...
func (d *dir) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (fs.Node, fs.Handle, error) {

	logger.Debug("Create path %v name %v Flags %v", d.name, req.Name, req.Flags)

	d.mu.Lock()
	defer d.mu.Unlock()
	ret, cfile := d.fs.cfs.CreateFileDirect(d.inode, req.Name, int(req.Flags))
	if ret != 0 {
		if ret == 17 {
			return nil, nil, fuse.Errno(syscall.EEXIST)

		}
		return nil, nil, fuse.Errno(syscall.EIO)

	}

	child := &File{
		inode:   cfile.Inode,
		name:    req.Name,
		parent:  d,
		handles: 1,
		writers: 1,
		cfile:   cfile,
	}

	d.active[req.Name] = &refcount{node: child}

	return child, child, nil
}

func (d *dir) forgetChild(name string, child node) {
	if name == "" {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	a, ok := d.active[name]
	if !ok {
		return
	}

	a.kernel = false
	if a.refs == 0 {
		delete(d.active, name)
	}
}

func (d *dir) Forget() {

	if d.parent == nil {
		return
	}

	d.mu.Lock()
	name := d.name
	d.mu.Unlock()

	d.parent.forgetChild(name, d)
}

// Mkdir ...
func (d *dir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (fs.Node, error) {

	ret, inode := d.fs.cfs.CreateDirDirect(d.inode, req.Name)
	if ret == -1 {
		return nil, fuse.Errno(syscall.EIO)
	}
	if ret == 1 {
		return nil, fuse.Errno(syscall.EPERM)
	}
	if ret == 2 {
		return nil, fuse.Errno(syscall.ENOENT)
	}
	if ret == 17 {
		return nil, fuse.Errno(syscall.EEXIST)
	}

	child := newDir(d.fs, inode, d, req.Name)

	d.mu.Lock()
	defer d.mu.Unlock()
	d.active[req.Name] = &refcount{node: child, kernel: true}

	return child, nil
}

// Remove ...
func (d *dir) Remove(ctx context.Context, req *fuse.RemoveRequest) error {

	if req.Dir {
		ret := d.fs.cfs.DeleteDirDirect(d.inode, req.Name)
		if ret != 0 {
			if ret == 2 {
				return fuse.Errno(syscall.EPERM)
			}
			return fuse.Errno(syscall.EIO)

		}
	} else {
		ret := d.fs.cfs.DeleteFileDirect(d.inode, req.Name)
		if ret != 0 {
			if ret == 2 {
				return fuse.Errno(syscall.EPERM)
			}
			return fuse.Errno(syscall.EIO)
		}
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if a, ok := d.active[req.Name]; ok {
		delete(d.active, req.Name)
		a.node.setName("")
	}

	return nil
}

// Rename ...
func (d *dir) Rename(ctx context.Context, req *fuse.RenameRequest, newDir fs.Node) error {

	ret, _, _ := d.fs.cfs.StatDirect(newDir.(*dir).inode, req.NewName)
	if ret == 0 {
		logger.Error("Rename Failed , newName in newDir is already exsit")
		return fuse.Errno(syscall.EPERM)
	}

	if newDir != d {

		d.mu.Lock()
		defer d.mu.Unlock()

		logger.Debug("Rename d.inode %v, req.OldName %v, newDir.(*dir).inode %v , req.NewName %v", d.inode, req.OldName, newDir.(*dir).inode, req.NewName)

		ret := d.fs.cfs.RenameDirect(d.inode, req.OldName, newDir.(*dir).inode, req.NewName)
		if ret != 0 {
			if ret == 2 {
				return fuse.Errno(syscall.ENOENT)
			} else if ret == 1 || ret == 17 {
				return fuse.Errno(syscall.EPERM)
			} else {
				return fuse.Errno(syscall.EIO)
			}
		}

		if aOld, ok := d.active[req.OldName]; ok {
			delete(d.active, req.OldName)
			aOld.node.setName(req.NewName)
			aOld.node.setParentInode(newDir.(*dir))
			//d.active[req.NewName] = aOld

		}

	} else {

		d.mu.Lock()
		defer d.mu.Unlock()

		logger.Debug("Rename d.inode %v, req.OldName %v, newDir.(*dir).inode %v , req.NewName %v", d.inode, req.OldName, newDir.(*dir).inode, req.NewName)

		ret := d.fs.cfs.RenameDirect(d.inode, req.OldName, d.inode, req.NewName)
		if ret != 0 {
			if ret == 2 {
				return fuse.Errno(syscall.ENOENT)
			} else if ret == 1 || ret == 17 {
				return fuse.Errno(syscall.EPERM)
			} else {
				return fuse.Errno(syscall.EIO)
			}
		}

		if a, ok := d.active[req.NewName]; ok {
			a.node.setName("")
		}

		if aOld, ok := d.active[req.OldName]; ok {
			aOld.node.setName(req.NewName)
			delete(d.active, req.OldName)
			d.active[req.NewName] = aOld
		}
	}

	return nil
}

type node interface {
	fs.Node
	setName(name string)
	setParentInode(pdir *dir)
}

// File struct
type File struct {
	mu    sync.Mutex
	inode uint64

	parent  *dir
	name    string
	writers uint
	handles uint32
	cfile   *cfs.CFile
}

var _ node = (*File)(nil)
var _ = fs.Node(&File{})
var _ = fs.Handle(&File{})

func (f *File) setName(name string) {

	f.mu.Lock()
	f.name = name
	f.mu.Unlock()

}

func (f *File) setParentInode(pdir *dir) {

	f.mu.Lock()
	f.parent = pdir
	f.mu.Unlock()
}

// Attr ...
func (f *File) Attr(ctx context.Context, a *fuse.Attr) error {

	f.mu.Lock()
	defer f.mu.Unlock()
	ret, inode, inodeInfo := f.parent.fs.cfs.GetInodeInfoDirect(f.parent.inode, f.name)
	if ret != 0 {
		return nil
	}

	a.Ctime = time.Unix(inodeInfo.ModifiTime, 0)
	a.Mtime = time.Unix(inodeInfo.ModifiTime, 0)
	a.Atime = time.Unix(inodeInfo.AccessTime, 0)
	a.Size = uint64(inodeInfo.FileSize)
	a.Inode = uint64(inode)

	a.BlockSize = 4 * 1024 // this is for fuse attr quick update
	a.Blocks = uint64(math.Ceil(float64(a.Size) / float64(a.BlockSize)))
	a.Mode = 0666
	//a.Valid = 0

	return nil
}

var _ = fs.NodeOpener(&File{})

// Open ...
func (f *File) Open(ctx context.Context, req *fuse.OpenRequest, resp *fuse.OpenResponse) (fs.Handle, error) {
	var ret int32

	logger.Debug("Open path %v name %v Flags %v", f.parent.name, f.name, req.Flags)

	if int(req.Flags)&os.O_TRUNC != 0 {
		return nil, fuse.Errno(syscall.EPERM)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.writers > 0 {
		if int(req.Flags)&os.O_WRONLY != 0 || int(req.Flags)&os.O_RDWR != 0 {
			return nil, fuse.Errno(syscall.EPERM)
		}
	}

	if f.cfile == nil && f.handles == 0 {
		ret, f.cfile = f.parent.fs.cfs.OpenFileDirect(f.parent.inode, f.name, int(req.Flags))
		if ret != 0 {
			return nil, fuse.Errno(syscall.EIO)
		}
	} else {
		f.parent.fs.cfs.UpdateOpenFileDirect(f.parent.inode, f.name, f.cfile, int(req.Flags))
	}

	tmp := f.handles + 1
	f.handles = tmp

	if int(req.Flags)&os.O_WRONLY != 0 || int(req.Flags)&os.O_RDWR != 0 {
		tmp := f.writers + 1
		f.writers = tmp
	}

	resp.Flags = fuse.OpenDirectIO
	return f, nil
}

var _ = fs.HandleReleaser(&File{})

// Release ...
func (f *File) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	logger.Debug("Release...")

	f.mu.Lock()
	defer f.mu.Unlock()

	f.handles--

	if int(req.Flags)&os.O_WRONLY != 0 || int(req.Flags)&os.O_RDWR != 0 {
		//f.cfile.Flush()
		f.cfile.CloseConns()
		f.writers--
	}

	if f.handles == 0 {
		f.cfile = nil
	}

	logger.Debug("Release end...")

	return nil
}

var _ = fs.HandleReader(&File{})

// Read ...
func (f *File) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {

	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.cfile.ReaderMap[req.Handle]; !ok {
		rdinfo := cfs.ReaderInfo{}
		rdinfo.LastOffset = int64(0)
		f.cfile.ReaderMap[req.Handle] = &rdinfo
	}
	if req.Offset == f.cfile.FileSize {

		logger.Debug("Request Read file offset equal filesize")
		return nil
	}

	length := f.cfile.Read(req.Handle, &resp.Data, req.Offset, int64(req.Size))
	if length != int64(req.Size) {
		logger.Debug("== Read reqsize:%v, but return datasize:%v ==\n", req.Size, length)
	}
	if length < 0 {
		logger.Error("Request Read file I/O Error(return data from cfs less than zero)")
		return fuse.Errno(syscall.EIO)
	}
	return nil
}

var _ = fs.HandleWriter(&File{})

// Write ...
func (f *File) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {

	f.mu.Lock()
	defer f.mu.Unlock()

	w := f.cfile.Write(req.Data, int32(len(req.Data)))
	if w != int32(len(req.Data)) {
		if w == -1 {
			return fuse.Errno(syscall.ENOSPC)
		}
		return fuse.Errno(syscall.EIO)

	}
	resp.Size = int(w)
	return nil
}

var _ = fs.HandleFlusher(&File{})

// Flush ...
func (f *File) Flush(ctx context.Context, req *fuse.FlushRequest) error {
	logger.Debug("Flush...")
	f.mu.Lock()
	defer f.mu.Unlock()

	f.cfile.Flush()
	return nil
}

var _ fs.NodeFsyncer = (*File)(nil)

// Fsync ...
func (f *File) Fsync(ctx context.Context, req *fuse.FsyncRequest) error {
	logger.Debug("Fsync...")
	f.mu.Lock()
	defer f.mu.Unlock()

	f.cfile.Flush()
	return nil
}

var _ = fs.NodeSetattrer(&File{})

// Setattr ...
func (f *File) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) error {
	return nil
}

func main() {

	c, err := config.NewConfig(os.Args[1])
	if err != nil {
		fmt.Println("NewConfig err")
		os.Exit(1)
	}
	uuid = c.String("uuid")
	mountPoint = c.String("mountpoint")
	cfs.VolMgrAddr = c.String("volmgr")
	bufferType, err := c.Int("buffertype")
	if err != nil {
		fmt.Println("wrong buffertype")
		os.Exit(1)
	}
	cfs.MetaNodePeers = c.Strings("metanode")

	switch bufferType {
	case 0:
		cfs.BufferSize = 512 * 1024
	case 1:
		cfs.BufferSize = 256 * 1024
	case 2:
		cfs.BufferSize = 128 * 1024
	default:
		cfs.BufferSize = 512 * 1024
	}

	logger.SetConsole(true)
	logger.SetRollingFile(c.String("log"), "fuse.log", 10, 100, logger.MB) //each 100M rolling
	switch level := c.String("loglevel"); level {
	case "error":
		logger.SetLevel(logger.ERROR)
	case "debug":
		logger.SetLevel(logger.DEBUG)
	case "info":
		logger.SetLevel(logger.INFO)
	default:
		logger.SetLevel(logger.ERROR)
	}

	defer func() {
		if err := recover(); err != nil {
			logger.Error("panic !!! :%v", err)
			logger.Error("stacks:%v", string(debug.Stack()))
		}
	}()

	cfs.MetaNodeAddr, _ = cfs.GetLeader(uuid)
	fmt.Printf("Leader:%v\n", cfs.MetaNodeAddr)
	ticker := time.NewTicker(time.Second * 60)
	go func() {
		for range ticker.C {
			cfs.MetaNodeAddr, _ = cfs.GetLeader(uuid)
			fmt.Printf("Leader:%v\n", cfs.MetaNodeAddr)
		}
	}()

	err = mount(uuid, mountPoint)
	if err != nil {
		log.Fatal(err)
	}
}

func mount(uuid, mountPoint string) error {
	cfs := cfs.OpenFileSystem(uuid)
	c, err := fuse.Mount(
		mountPoint,
		fuse.MaxReadahead(128*1024),
		fuse.AsyncRead(),
		fuse.WritebackCache(),
		fuse.FSName("ContainerFS-"+uuid),
		fuse.LocalVolume(),
		fuse.VolumeName("ContainerFS-"+uuid))
	if err != nil {
		return err
	}
	defer c.Close()

	filesys := &FS{
		cfs: cfs,
	}
	if err := fs.Serve(c, filesys); err != nil {
		return err
	}
	// check if the mount process has an error to report
	<-c.Ready
	if err := c.MountError; err != nil {
		return err
	}

	return nil
}
