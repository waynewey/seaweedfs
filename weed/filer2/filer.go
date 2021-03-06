package filer2

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chrislusf/seaweedfs/weed/glog"
	"github.com/chrislusf/seaweedfs/weed/operation"
	"github.com/chrislusf/seaweedfs/weed/pb/filer_pb"
	"github.com/chrislusf/seaweedfs/weed/wdclient"
	"github.com/karlseguin/ccache"
)

type Filer struct {
	store          FilerStore
	directoryCache *ccache.Cache
	MasterClient   *wdclient.MasterClient
}

func NewFiler(masters []string) *Filer {
	return &Filer{
		directoryCache: ccache.New(ccache.Configure().MaxSize(1000).ItemsToPrune(100)),
		MasterClient:   wdclient.NewMasterClient(context.Background(), "filer", masters),
	}
}

func (f *Filer) SetStore(store FilerStore) {
	f.store = store
}

func (f *Filer) DisableDirectoryCache() {
	f.directoryCache = nil
}

func (fs *Filer) GetMaster() string {
	return fs.MasterClient.GetMaster()
}

func (fs *Filer) KeepConnectedToMaster() {
	fs.MasterClient.KeepConnectedToMaster()
}

func (f *Filer) CreateEntry(entry *Entry) error {

	dirParts := strings.Split(string(entry.FullPath), "/")

	// fmt.Printf("directory parts: %+v\n", dirParts)

	var lastDirectoryEntry *Entry

	for i := 1; i < len(dirParts); i++ {
		dirPath := "/" + filepath.Join(dirParts[:i]...)
		// fmt.Printf("%d directory: %+v\n", i, dirPath)

		// first check local cache
		dirEntry := f.cacheGetDirectory(dirPath)

		// not found, check the store directly
		if dirEntry == nil {
			glog.V(4).Infof("find uncached directory: %s", dirPath)
			dirEntry, _ = f.FindEntry(FullPath(dirPath))
		} else {
			glog.V(4).Infof("found cached directory: %s", dirPath)
		}

		// no such existing directory
		if dirEntry == nil {

			// create the directory
			now := time.Now()

			dirEntry = &Entry{
				FullPath: FullPath(dirPath),
				Attr: Attr{
					Mtime:  now,
					Crtime: now,
					Mode:   os.ModeDir | 0770,
					Uid:    entry.Uid,
					Gid:    entry.Gid,
				},
			}

			glog.V(2).Infof("create directory: %s %v", dirPath, dirEntry.Mode)
			mkdirErr := f.store.InsertEntry(dirEntry)
			if mkdirErr != nil {
				return fmt.Errorf("mkdir %s: %v", dirPath, mkdirErr)
			}
		} else if !dirEntry.IsDirectory() {
			return fmt.Errorf("%s is a file", dirPath)
		}

		// cache the directory entry
		f.cacheSetDirectory(dirPath, dirEntry, i)

		// remember the direct parent directory entry
		if i == len(dirParts)-1 {
			lastDirectoryEntry = dirEntry
		}

	}

	if lastDirectoryEntry == nil {
		return fmt.Errorf("parent folder not found: %v", entry.FullPath)
	}

	/*
		if !hasWritePermission(lastDirectoryEntry, entry) {
			glog.V(0).Infof("directory %s: %v, entry: uid=%d gid=%d",
				lastDirectoryEntry.FullPath, lastDirectoryEntry.Attr, entry.Uid, entry.Gid)
			return fmt.Errorf("no write permission in folder %v", lastDirectoryEntry.FullPath)
		}
	*/

	oldEntry, _ := f.FindEntry(entry.FullPath)

	if err := f.store.InsertEntry(entry); err != nil {
		return fmt.Errorf("insert entry %s: %v", entry.FullPath, err)
	}

	f.deleteChunksIfNotNew(oldEntry, entry)

	return nil
}

func (f *Filer) UpdateEntry(entry *Entry) (err error) {
	return f.store.UpdateEntry(entry)
}

func (f *Filer) FindEntry(p FullPath) (entry *Entry, err error) {
	return f.store.FindEntry(p)
}

func (f *Filer) DeleteEntryMetaAndData(p FullPath, isRecursive bool, shouldDeleteChunks bool) (err error) {
	entry, err := f.FindEntry(p)
	if err != nil {
		return err
	}

	if entry.IsDirectory() {
		entries, err := f.ListDirectoryEntries(p, "", false, 1)
		if err != nil {
			return fmt.Errorf("list folder %s: %v", p, err)
		}
		if isRecursive {
			for _, sub := range entries {
				f.DeleteEntryMetaAndData(sub.FullPath, isRecursive, shouldDeleteChunks)
			}
		} else {
			if len(entries) > 0 {
				return fmt.Errorf("folder %s is not empty", p)
			}
		}
		f.cacheDelDirectory(string(p))
	}

	if shouldDeleteChunks {
		f.deleteChunks(entry.Chunks)
	}

	return f.store.DeleteEntry(p)
}

func (f *Filer) ListDirectoryEntries(p FullPath, startFileName string, inclusive bool, limit int) ([]*Entry, error) {
	if strings.HasSuffix(string(p), "/") && len(p) > 1 {
		p = p[0 : len(p)-1]
	}
	return f.store.ListDirectoryEntries(p, startFileName, inclusive, limit)
}

func (f *Filer) cacheDelDirectory(dirpath string) {
	if f.directoryCache == nil {
		return
	}
	f.directoryCache.Delete(dirpath)
	return
}

func (f *Filer) cacheGetDirectory(dirpath string) *Entry {
	if f.directoryCache == nil {
		return nil
	}
	item := f.directoryCache.Get(dirpath)
	if item == nil {
		return nil
	}
	return item.Value().(*Entry)
}

func (f *Filer) cacheSetDirectory(dirpath string, dirEntry *Entry, level int) {

	if f.directoryCache == nil {
		return
	}

	minutes := 60
	if level < 10 {
		minutes -= level * 6
	}

	f.directoryCache.Set(dirpath, dirEntry, time.Duration(minutes)*time.Minute)
}

func (f *Filer) deleteChunks(chunks []*filer_pb.FileChunk) {
	for _, chunk := range chunks {
		f.DeleteFileByFileId(chunk.FileId)
	}
}

func (f *Filer) DeleteFileByFileId(fileId string) {
	fileUrlOnVolume, err := f.MasterClient.LookupFileId(fileId)
	if err != nil {
		glog.V(0).Infof("can not find file %s: %v", fileId, err)
	}
	if err := operation.DeleteFromVolumeServer(fileUrlOnVolume, ""); err != nil {
		glog.V(0).Infof("deleting file %s: %v", fileId, err)
	}
}

func (f *Filer) deleteChunksIfNotNew(oldEntry, newEntry *Entry) {

	if oldEntry == nil {
		return
	}
	if newEntry == nil {
		f.deleteChunks(oldEntry.Chunks)
	}

	var toDelete []*filer_pb.FileChunk

	for _, oldChunk := range oldEntry.Chunks {
		found := false
		for _, newChunk := range newEntry.Chunks {
			if oldChunk.FileId == newChunk.FileId {
				found = true
				break
			}
		}
		if !found {
			toDelete = append(toDelete, oldChunk)
		}
	}
	f.deleteChunks(toDelete)
}
