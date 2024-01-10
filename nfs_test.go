package nfs_test

import (
	"bytes"
	"fmt"
	"net"
	"reflect"
	"sort"
	"testing"

	nfs "github.com/ikmak/go-nfs"
	"github.com/ikmak/go-nfs/helpers"

	"github.com/go-git/go-billy/v5/memfs"
	nfsc "github.com/willscott/go-nfs-client/nfs"
	rpc "github.com/willscott/go-nfs-client/nfs/rpc"
	"github.com/willscott/go-nfs-client/nfs/xdr"
)

func TestNFS(t *testing.T) {
	// make an empty in-memory server.
	listener, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatal(err)
	}

	mem := memfs.New()
	// File needs to exist in the root for memfs to acknowledge the root exists.
	_, _ = mem.Create("/test")

	handler := helpers.NewNullAuthHandler(mem)
	cacheHelper := helpers.NewCachingHandler(handler, 1024)
	go func() {
		_ = nfs.Serve(listener, cacheHelper)
	}()

	c, err := rpc.DialTCP(listener.Addr().Network(), nil, listener.Addr().(*net.TCPAddr).String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var mounter nfsc.Mount
	mounter.Client = c
	target, err := mounter.Mount("/", rpc.AuthNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = mounter.Unmount()
	}()

	_, err = target.FSInfo()
	if err != nil {
		t.Fatal(err)
	}

	// Validate sample file creation
	_, err = target.Create("/helloworld.txt", 0666)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := mem.Stat("/helloworld.txt"); err != nil {
		t.Fatal(err)
	} else {
		if info.Size() != 0 || info.Mode().Perm() != 0666 {
			t.Fatal("incorrect creation.")
		}
	}

	// Validate writing to a file.
	f, err := target.OpenFile("/helloworld.txt", 0666)
	if err != nil {
		t.Fatal(err)
	}
	b := []byte("hello world")
	_, err = f.Write(b)
	if err != nil {
		t.Fatal(err)
	}
	mf, _ := mem.Open("/helloworld.txt")
	buf := make([]byte, len(b))
	if _, err = mf.Read(buf[:]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf, b) {
		t.Fatal("written does not match expected")
	}

	// for test nfs.ReadDirPlus in case of many files
	dirF1, err := mem.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	shouldBeNames := []string{".", ".."}
	for _, f := range dirF1 {
		shouldBeNames = append(shouldBeNames, f.Name())
	}
	for i := 0; i < 100; i++ {
		fName := fmt.Sprintf("f-%03d.txt", i)
		shouldBeNames = append(shouldBeNames, fName)
		f, err := mem.Create(fName)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	manyEntitiesPlus, err := target.ReadDirPlus("/")
	if err != nil {
		t.Fatal(err)
	}
	actualBeNamesPlus := []string{}
	for _, e := range manyEntitiesPlus {
		actualBeNamesPlus = append(actualBeNamesPlus, e.Name())
	}

	as := sort.StringSlice(shouldBeNames)
	bs := sort.StringSlice(actualBeNamesPlus)
	as.Sort()
	bs.Sort()
	if !reflect.DeepEqual(as, bs) {
		t.Fatal("nfs.ReadDirPlus error")
	}

	// for test nfs.ReadDir in case of many files
	manyEntities, err := readDir(target, "/")
	if err != nil {
		t.Fatal(err)
	}
	actualBeNames := []string{}
	for _, e := range manyEntities {
		actualBeNames = append(actualBeNames, e.FileName)
	}

	as2 := sort.StringSlice(shouldBeNames)
	bs2 := sort.StringSlice(actualBeNames)
	as2.Sort()
	bs2.Sort()
	if !reflect.DeepEqual(as2, bs2) {
		t.Fatal("nfs.ReadDir error")
	}

	// for test nfs.ReadDirPlus in case of empty directory
	_, err = target.Mkdir("/empty", 0755)
	if err != nil {
		t.Fatal(err)
	}

	emptyEntitiesPlus, err := target.ReadDirPlus("/empty")
	if err != nil {
		t.Fatal(err)
	}
	if len(emptyEntitiesPlus) != 2 || emptyEntitiesPlus[0].Name() != "." || emptyEntitiesPlus[1].Name() != ".." {
		t.Fatal("nfs.ReadDirPlus error reading empty dir")
	}

	// for test nfs.ReadDir in case of empty directory
	emptyEntities, err := readDir(target, "/empty")
	if err != nil {
		t.Fatal(err)
	}
	if len(emptyEntities) != 2 || emptyEntities[0].FileName != "." || emptyEntities[1].FileName != ".." {
		t.Fatal("nfs.ReadDir error reading empty dir")
	}
}

type readDirEntry struct {
	FileId   uint64
	FileName string
	Cookie   uint64
}

// readDir implementation "appropriated" from go-nfs-client implementation of READDIRPLUS
func readDir(target *nfsc.Target, dir string) ([]*readDirEntry, error) {
	_, fh, err := target.Lookup(dir)
	if err != nil {
		return nil, err
	}

	type readDirArgs struct {
		rpc.Header
		Handle      []byte
		Cookie      uint64
		CookieVerif uint64
		Count       uint32
	}

	type readDirList struct {
		IsSet bool         `xdr:"union"`
		Entry readDirEntry `xdr:"unioncase=1"`
	}

	type readDirListOK struct {
		DirAttrs   nfsc.PostOpAttr
		CookieVerf uint64
	}

	cookie := uint64(0)
	cookieVerf := uint64(0)
	eof := false

	var entries []*readDirEntry
	for !eof {
		res, err := target.Call(&readDirArgs{
			Header: rpc.Header{
				Rpcvers: 2,
				Vers:    nfsc.Nfs3Vers,
				Prog:    nfsc.Nfs3Prog,
				Proc:    uint32(nfs.NFSProcedureReadDir),
				Cred:    rpc.AuthNull,
				Verf:    rpc.AuthNull,
			},
			Handle:      fh,
			Cookie:      cookie,
			CookieVerif: cookieVerf,
			Count:       4096,
		})
		if err != nil {
			return nil, err
		}

		status, err := xdr.ReadUint32(res)
		if err != nil {
			return nil, err
		}

		if err = nfsc.NFS3Error(status); err != nil {
			return nil, err
		}

		dirListOK := new(readDirListOK)
		if err = xdr.Read(res, dirListOK); err != nil {
			return nil, err
		}

		for {
			var item readDirList
			if err = xdr.Read(res, &item); err != nil {
				return nil, err
			}

			if !item.IsSet {
				break
			}

			cookie = item.Entry.Cookie
			entries = append(entries, &item.Entry)
		}

		if err = xdr.Read(res, &eof); err != nil {
			return nil, err
		}

		cookieVerf = dirListOK.CookieVerf
	}

	return entries, nil
}
