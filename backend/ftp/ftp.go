// Package ftp interfaces with FTP servers
package ftp

import (
	"io"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/jlaffaye/ftp"
	"github.com/ncw/rclone/fs"
	"github.com/ncw/rclone/fs/config"
	"github.com/ncw/rclone/fs/config/obscure"
	"github.com/ncw/rclone/fs/hash"
	"github.com/ncw/rclone/lib/readers"
	"github.com/pkg/errors"
)

// Register with Fs
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "ftp",
		Description: "FTP Connection",
		NewFs:       NewFs,
		Options: []fs.Option{
			{
				Name:     "host",
				Help:     "FTP host to connect to",
				Optional: false,
				Examples: []fs.OptionExample{{
					Value: "ftp.example.com",
					Help:  "Connect to ftp.example.com",
				}},
			}, {
				Name:     "user",
				Help:     "FTP username, leave blank for current username, " + os.Getenv("USER"),
				Optional: true,
			}, {
				Name:     "port",
				Help:     "FTP port, leave blank to use default (21) ",
				Optional: true,
			}, {
				Name:       "pass",
				Help:       "FTP password",
				IsPassword: true,
				Optional:   false,
			},
		},
	})
}

// Fs represents a remote FTP server
type Fs struct {
	name     string       // name of this remote
	root     string       // the path we are working on if any
	features *fs.Features // optional features
	url      string
	user     string
	pass     string
	dialAddr string
	poolMu   sync.Mutex
	pool     []*ftp.ServerConn
}

// Object describes an FTP file
type Object struct {
	fs     *Fs
	remote string
	info   *FileInfo
}

// FileInfo is the metadata known about an FTP file
type FileInfo struct {
	Name    string
	Size    uint64
	ModTime time.Time
	IsDir   bool
}

// ------------------------------------------------------------

// Name of this fs
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String returns a description of the FS
func (f *Fs) String() string {
	return f.url
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// Open a new connection to the FTP server.
func (f *Fs) ftpConnection() (*ftp.ServerConn, error) {
	fs.Debugf(f, "Connecting to FTP server")
	c, err := ftp.DialTimeout(f.dialAddr, fs.Config.ConnectTimeout)
	if err != nil {
		fs.Errorf(f, "Error while Dialing %s: %s", f.dialAddr, err)
		return nil, errors.Wrap(err, "ftpConnection Dial")
	}
	err = c.Login(f.user, f.pass)
	if err != nil {
		_ = c.Quit()
		fs.Errorf(f, "Error while Logging in into %s: %s", f.dialAddr, err)
		return nil, errors.Wrap(err, "ftpConnection Login")
	}
	return c, nil
}

// Get an FTP connection from the pool, or open a new one
func (f *Fs) getFtpConnection() (c *ftp.ServerConn, err error) {
	f.poolMu.Lock()
	if len(f.pool) > 0 {
		c = f.pool[0]
		f.pool = f.pool[1:]
	}
	f.poolMu.Unlock()
	if c != nil {
		return c, nil
	}
	return f.ftpConnection()
}

// Return an FTP connection to the pool
//
// It nils the pointed to connection out so it can't be reused
//
// if err is not nil then it checks the connection is alive using a
// NOOP request
func (f *Fs) putFtpConnection(pc **ftp.ServerConn, err error) {
	c := *pc
	*pc = nil
	if err != nil {
		// If not a regular FTP error code then check the connection
		_, isRegularError := errors.Cause(err).(*textproto.Error)
		if !isRegularError {
			nopErr := c.NoOp()
			if nopErr != nil {
				fs.Debugf(f, "Connection failed, closing: %v", nopErr)
				_ = c.Quit()
				return
			}
		}
	}
	f.poolMu.Lock()
	f.pool = append(f.pool, c)
	f.poolMu.Unlock()
}

// NewFs contstructs an Fs from the path, container:path
func NewFs(name, root string) (ff fs.Fs, err error) {
	// defer fs.Trace(nil, "name=%q, root=%q", name, root)("fs=%v, err=%v", &ff, &err)
	// FIXME Convert the old scheme used for the first beta - remove after release
	if ftpURL := config.FileGet(name, "url"); ftpURL != "" {
		fs.Infof(name, "Converting old configuration")
		u, err := url.Parse(ftpURL)
		if err != nil {
			return nil, errors.Wrapf(err, "Failed to parse old url %q", ftpURL)
		}
		parts := strings.Split(u.Host, ":")
		config.FileSet(name, "host", parts[0])
		if len(parts) > 1 {
			config.FileSet(name, "port", parts[1])
		}
		config.FileSet(name, "host", u.Host)
		config.FileSet(name, "user", config.FileGet(name, "username"))
		config.FileSet(name, "pass", config.FileGet(name, "password"))
		config.FileDeleteKey(name, "username")
		config.FileDeleteKey(name, "password")
		config.FileDeleteKey(name, "url")
		config.SaveConfig()
		if u.Path != "" && u.Path != "/" {
			fs.Errorf(name, "Path %q in FTP URL no longer supported - put it on the end of the remote %s:%s", u.Path, name, u.Path)
		}
	}
	host := config.FileGet(name, "host")
	user := config.FileGet(name, "user")
	pass := config.FileGet(name, "pass")
	port := config.FileGet(name, "port")
	pass, err = obscure.Reveal(pass)
	if err != nil {
		return nil, errors.Wrap(err, "NewFS decrypt password")
	}
	if user == "" {
		user = os.Getenv("USER")
	}
	if port == "" {
		port = "21"
	}

	dialAddr := host + ":" + port
	u := "ftp://" + path.Join(dialAddr+"/", root)
	f := &Fs{
		name:     name,
		root:     root,
		url:      u,
		user:     user,
		pass:     pass,
		dialAddr: dialAddr,
	}
	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
	}).Fill(f)
	// Make a connection and pool it to return errors early
	c, err := f.getFtpConnection()
	if err != nil {
		return nil, errors.Wrap(err, "NewFs")
	}
	f.putFtpConnection(&c, nil)
	if root != "" {
		// Check to see if the root actually an existing file
		remote := path.Base(root)
		f.root = path.Dir(root)
		if f.root == "." {
			f.root = ""
		}
		_, err := f.NewObject(remote)
		if err != nil {
			if err == fs.ErrorObjectNotFound || errors.Cause(err) == fs.ErrorNotAFile {
				// File doesn't exist so return old f
				f.root = root
				return f, nil
			}
			return nil, err
		}
		// return an error with an fs which points to the parent
		return f, fs.ErrorIsFile
	}
	return f, err
}

// translateErrorFile turns FTP errors into rclone errors if possible for a file
func translateErrorFile(err error) error {
	switch errX := err.(type) {
	case *textproto.Error:
		switch errX.Code {
		case ftp.StatusFileUnavailable:
			err = fs.ErrorObjectNotFound
		}
	}
	return err
}

// translateErrorDir turns FTP errors into rclone errors if possible for a directory
func translateErrorDir(err error) error {
	switch errX := err.(type) {
	case *textproto.Error:
		switch errX.Code {
		case ftp.StatusFileUnavailable:
			err = fs.ErrorDirNotFound
		}
	}
	return err
}

// NewObject finds the Object at remote.  If it can't be found
// it returns the error fs.ErrorObjectNotFound.
func (f *Fs) NewObject(remote string) (o fs.Object, err error) {
	// defer fs.Trace(remote, "")("o=%v, err=%v", &o, &err)
	fullPath := path.Join(f.root, remote)
	dir := path.Dir(fullPath)
	base := path.Base(fullPath)

	c, err := f.getFtpConnection()
	if err != nil {
		return nil, errors.Wrap(err, "NewObject")
	}
	files, err := c.List(dir)
	f.putFtpConnection(&c, err)
	if err != nil {
		return nil, translateErrorFile(err)
	}
	for i, file := range files {
		if file.Type != ftp.EntryTypeFolder && file.Name == base {
			o := &Object{
				fs:     f,
				remote: remote,
			}
			info := &FileInfo{
				Name:    remote,
				Size:    files[i].Size,
				ModTime: files[i].Time,
			}
			o.info = info

			return o, nil
		}
	}
	return nil, fs.ErrorObjectNotFound
}

// List the objects and directories in dir into entries.  The
// entries can be returned in any order but should be for a
// complete directory.
//
// dir should be "" to list the root, and should not have
// trailing slashes.
//
// This should return ErrDirNotFound if the directory isn't
// found.
func (f *Fs) List(dir string) (entries fs.DirEntries, err error) {
	// defer fs.Trace(dir, "curlevel=%d", curlevel)("")
	c, err := f.getFtpConnection()
	if err != nil {
		return nil, errors.Wrap(err, "list")
	}
	files, err := c.List(path.Join(f.root, dir))
	f.putFtpConnection(&c, err)
	if err != nil {
		return nil, translateErrorDir(err)
	}
	for i := range files {
		object := files[i]
		newremote := path.Join(dir, object.Name)
		switch object.Type {
		case ftp.EntryTypeFolder:
			if object.Name == "." || object.Name == ".." {
				continue
			}
			d := fs.NewDir(newremote, object.Time)
			entries = append(entries, d)
		default:
			o := &Object{
				fs:     f,
				remote: newremote,
			}
			info := &FileInfo{
				Name:    newremote,
				Size:    object.Size,
				ModTime: object.Time,
			}
			o.info = info
			entries = append(entries, o)
		}
	}
	return entries, nil
}

// Hashes are not supported
func (f *Fs) Hashes() hash.Set {
	return 0
}

// Precision shows Modified Time not supported
func (f *Fs) Precision() time.Duration {
	return fs.ModTimeNotSupported
}

// Put in to the remote path with the modTime given of the given size
//
// May create the object even if it returns an error - if so
// will return the object and the error, otherwise will return
// nil and the error
func (f *Fs) Put(in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	// fs.Debugf(f, "Trying to put file %s", src.Remote())
	err := f.mkParentDir(src.Remote())
	if err != nil {
		return nil, errors.Wrap(err, "Put mkParentDir failed")
	}
	o := &Object{
		fs:     f,
		remote: src.Remote(),
	}
	err = o.Update(in, src, options...)
	return o, err
}

// PutStream uploads to the remote path with the modTime given of indeterminate size
func (f *Fs) PutStream(in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.Put(in, src, options...)
}

// getInfo reads the FileInfo for a path
func (f *Fs) getInfo(remote string) (fi *FileInfo, err error) {
	// defer fs.Trace(remote, "")("fi=%v, err=%v", &fi, &err)
	dir := path.Dir(remote)
	base := path.Base(remote)

	c, err := f.getFtpConnection()
	if err != nil {
		return nil, errors.Wrap(err, "getInfo")
	}
	files, err := c.List(dir)
	f.putFtpConnection(&c, err)
	if err != nil {
		return nil, translateErrorFile(err)
	}

	for i := range files {
		if files[i].Name == base {
			info := &FileInfo{
				Name:    remote,
				Size:    files[i].Size,
				ModTime: files[i].Time,
				IsDir:   files[i].Type == ftp.EntryTypeFolder,
			}
			return info, nil
		}
	}
	return nil, fs.ErrorObjectNotFound
}

// mkdir makes the directory and parents using unrooted paths
func (f *Fs) mkdir(abspath string) error {
	if abspath == "." || abspath == "/" {
		return nil
	}
	fi, err := f.getInfo(abspath)
	if err == nil {
		if fi.IsDir {
			return nil
		}
		return fs.ErrorIsFile
	} else if err != fs.ErrorObjectNotFound {
		return errors.Wrapf(err, "mkdir %q failed", abspath)
	}
	parent := path.Dir(abspath)
	err = f.mkdir(parent)
	if err != nil {
		return err
	}
	c, connErr := f.getFtpConnection()
	if connErr != nil {
		return errors.Wrap(connErr, "mkdir")
	}
	err = c.MakeDir(abspath)
	f.putFtpConnection(&c, err)
	return err
}

// mkParentDir makes the parent of remote if necessary and any
// directories above that
func (f *Fs) mkParentDir(remote string) error {
	parent := path.Dir(remote)
	return f.mkdir(path.Join(f.root, parent))
}

// Mkdir creates the directory if it doesn't exist
func (f *Fs) Mkdir(dir string) (err error) {
	// defer fs.Trace(dir, "")("err=%v", &err)
	root := path.Join(f.root, dir)
	return f.mkdir(root)
}

// Rmdir removes the directory (container, bucket) if empty
//
// Return an error if it doesn't exist or isn't empty
func (f *Fs) Rmdir(dir string) error {
	c, err := f.getFtpConnection()
	if err != nil {
		return errors.Wrap(translateErrorFile(err), "Rmdir")
	}
	err = c.RemoveDir(path.Join(f.root, dir))
	f.putFtpConnection(&c, err)
	return translateErrorDir(err)
}

// Move renames a remote file object
func (f *Fs) Move(src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't move - not same remote type")
		return nil, fs.ErrorCantMove
	}
	err := f.mkParentDir(remote)
	if err != nil {
		return nil, errors.Wrap(err, "Move mkParentDir failed")
	}
	c, err := f.getFtpConnection()
	if err != nil {
		return nil, errors.Wrap(err, "Move")
	}
	err = c.Rename(
		path.Join(srcObj.fs.root, srcObj.remote),
		path.Join(f.root, remote),
	)
	f.putFtpConnection(&c, err)
	if err != nil {
		return nil, errors.Wrap(err, "Move Rename failed")
	}
	dstObj, err := f.NewObject(remote)
	if err != nil {
		return nil, errors.Wrap(err, "Move NewObject failed")
	}
	return dstObj, nil
}

// DirMove moves src, srcRemote to this remote at dstRemote
// using server side move operations.
//
// Will only be called if src.Fs().Name() == f.Name()
//
// If it isn't possible then return fs.ErrorCantDirMove
//
// If destination exists then return fs.ErrorDirExists
func (f *Fs) DirMove(src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok {
		fs.Debugf(srcFs, "Can't move directory - not same remote type")
		return fs.ErrorCantDirMove
	}
	srcPath := path.Join(srcFs.root, srcRemote)
	dstPath := path.Join(f.root, dstRemote)

	// Check if destination exists
	fi, err := f.getInfo(dstPath)
	if err == nil {
		if fi.IsDir {
			return fs.ErrorDirExists
		}
		return fs.ErrorIsFile
	} else if err != fs.ErrorObjectNotFound {
		return errors.Wrapf(err, "DirMove getInfo failed")
	}

	// Make sure the parent directory exists
	err = f.mkdir(path.Dir(dstPath))
	if err != nil {
		return errors.Wrap(err, "DirMove mkParentDir dst failed")
	}

	// Do the move
	c, err := f.getFtpConnection()
	if err != nil {
		return errors.Wrap(err, "DirMove")
	}
	err = c.Rename(
		srcPath,
		dstPath,
	)
	f.putFtpConnection(&c, err)
	if err != nil {
		return errors.Wrapf(err, "DirMove Rename(%q,%q) failed", srcPath, dstPath)
	}
	return nil
}

// ------------------------------------------------------------

// Fs returns the parent Fs
func (o *Object) Fs() fs.Info {
	return o.fs
}

// String version of o
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.remote
}

// Hash returns the hash of an object returning a lowercase hex string
func (o *Object) Hash(t hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}

// Size returns the size of an object in bytes
func (o *Object) Size() int64 {
	return int64(o.info.Size)
}

// ModTime returns the modification time of the object
func (o *Object) ModTime() time.Time {
	return o.info.ModTime
}

// SetModTime sets the modification time of the object
func (o *Object) SetModTime(modTime time.Time) error {
	return nil
}

// Storable returns a boolean as to whether this object is storable
func (o *Object) Storable() bool {
	return true
}

// ftpReadCloser implements io.ReadCloser for FTP objects.
type ftpReadCloser struct {
	rc  io.ReadCloser
	c   *ftp.ServerConn
	f   *Fs
	err error // errors found during read
}

// Read bytes into p
func (f *ftpReadCloser) Read(p []byte) (n int, err error) {
	n, err = f.rc.Read(p)
	if err != nil && err != io.EOF {
		f.err = err // store any errors for Close to examine
	}
	return
}

// Close the FTP reader and return the connection to the pool
func (f *ftpReadCloser) Close() error {
	err := f.rc.Close()
	// if errors while reading or closing, dump the connection
	if err != nil || f.err != nil {
		_ = f.c.Quit()
	} else {
		f.f.putFtpConnection(&f.c, nil)
	}
	// mask the error if it was caused by a premature close
	switch errX := err.(type) {
	case *textproto.Error:
		switch errX.Code {
		case ftp.StatusTransfertAborted, ftp.StatusFileUnavailable:
			err = nil
		}
	}
	return err
}

// Open an object for read
func (o *Object) Open(options ...fs.OpenOption) (rc io.ReadCloser, err error) {
	// defer fs.Trace(o, "")("rc=%v, err=%v", &rc, &err)
	path := path.Join(o.fs.root, o.remote)
	var offset, limit int64
	for _, option := range options {
		switch x := option.(type) {
		case *fs.SeekOption:
			offset, limit = x.Offset, 0
		case *fs.RangeOption:
			offset, limit = x.Decode(o.Size())
		default:
			if option.Mandatory() {
				fs.Logf(o, "Unsupported mandatory option: %v", option)
			}
		}
	}
	c, err := o.fs.getFtpConnection()
	if err != nil {
		return nil, errors.Wrap(err, "open")
	}
	fd, err := c.RetrFrom(path, uint64(offset))
	if err != nil {
		o.fs.putFtpConnection(&c, err)
		return nil, errors.Wrap(err, "open")
	}
	rc = &ftpReadCloser{rc: readers.NewLimitedReadCloser(fd, limit), c: c, f: o.fs}
	return rc, nil
}

// Update the already existing object
//
// Copy the reader into the object updating modTime and size
//
// The new object may have been created if an error is returned
func (o *Object) Update(in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (err error) {
	// defer fs.Trace(o, "src=%v", src)("err=%v", &err)
	path := path.Join(o.fs.root, o.remote)
	// remove the file if upload failed
	remove := func() {
		removeErr := o.Remove()
		if removeErr != nil {
			fs.Debugf(o, "Failed to remove: %v", removeErr)
		} else {
			fs.Debugf(o, "Removed after failed upload: %v", err)
		}
	}
	c, err := o.fs.getFtpConnection()
	if err != nil {
		return errors.Wrap(err, "Update")
	}
	err = c.Stor(path, in)
	if err != nil {
		_ = c.Quit()
		remove()
		return errors.Wrap(err, "update stor")
	}
	o.fs.putFtpConnection(&c, nil)
	o.info, err = o.fs.getInfo(path)
	if err != nil {
		return errors.Wrap(err, "update getinfo")
	}
	return nil
}

// Remove an object
func (o *Object) Remove() (err error) {
	// defer fs.Trace(o, "")("err=%v", &err)
	path := path.Join(o.fs.root, o.remote)
	// Check if it's a directory or a file
	info, err := o.fs.getInfo(path)
	if err != nil {
		return err
	}
	if info.IsDir {
		err = o.fs.Rmdir(o.remote)
	} else {
		c, err := o.fs.getFtpConnection()
		if err != nil {
			return errors.Wrap(err, "Remove")
		}
		err = c.Delete(path)
		o.fs.putFtpConnection(&c, err)
	}
	return err
}

// Check the interfaces are satisfied
var (
	_ fs.Fs          = &Fs{}
	_ fs.Mover       = &Fs{}
	_ fs.DirMover    = &Fs{}
	_ fs.PutStreamer = &Fs{}
	_ fs.Object      = &Object{}
)
