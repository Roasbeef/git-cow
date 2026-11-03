package cow

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// indexStat holds the stat fields stored in a Git index cache entry.
type indexStat struct {
	ctimeSec  uint32
	ctimeNsec uint32
	mtimeSec  uint32
	mtimeNsec uint32
	dev       uint32
	ino       uint32
	uid       uint32
	gid       uint32
	size      uint32
}

// Git index constants.
const (
	indexHeaderSize   = 12
	indexEntryMinSize = 62
	ceStagemask       = 0x3000
	ceExtended        = 0x4000
	ceSkipWorktree    = 0x4000
	ceIntentToAdd     = 0x2000
	modeTypeMask      = 0o170000
	modeGitlink       = 0o160000
)

// indexEntryRef locates one stage-0 cache entry inside a parsed index buffer.
type indexEntryRef struct {
	offset int
	path   string
}

// refreshIndexStats updates the cached stat fields (ctime, dev, ino, uid,
// gid) of stage-0 entries in the Git index at indexPath to match the cloned
// files under worktree, so the next Git command does not rehash every file
// because cloning gave it a new inode number.
//
// An entry is updated only when its recorded mtime and size match the file in
// worktree. When srcWorktree is non-empty (as in Copy, where the template was
// not checked with "git status"), each entry is also required to be non-racy
// relative to the template index's mtime and to match the template file's own
// stat fields, so a dirty file in the template is never marked clean.
//
// Any unfamiliar index version, mandatory extension (such as "link" or
// "sdir") or parse error leaves the index file untouched so Git refreshes it
// the ordinary way.
func refreshIndexStats(worktree, indexPath, srcWorktree string) {
	indexInfo, err := os.Lstat(indexPath)
	if err != nil {
		return
	}

	indexMtime := indexInfo.ModTime()
	indexMtimeSec := uint32(indexMtime.Unix())

	data, err := os.ReadFile(indexPath)
	if err != nil || len(data) < indexHeaderSize+sha1.Size {
		return
	}

	if string(data[:4]) != "DIRC" {
		return
	}

	ver := binary.BigEndian.Uint32(data[4:8])
	if ver != 2 && ver != 3 && ver != 4 {
		return
	}

	bodyEnd := len(data) - sha1.Size
	wantSum := sha1.Sum(data[:bodyEnd])
	if !bytes.Equal(wantSum[:], data[bodyEnd:]) {
		return
	}

	numEntries := int(binary.BigEndian.Uint32(data[8:12]))
	entries := make([]indexEntryRef, 0, numEntries)
	pos := indexHeaderSize

	var prevPath []byte

	for range numEntries {
		entryStart := pos
		if entryStart+indexEntryMinSize > bodyEnd {
			return
		}

		mode := binary.BigEndian.Uint32(data[entryStart+24 : entryStart+28])
		flags := binary.BigEndian.Uint16(data[entryStart+60 : entryStart+62])
		pos = entryStart + indexEntryMinSize

		var extFlags uint16
		if flags&ceExtended != 0 {
			if pos+2 > bodyEnd {
				return
			}

			extFlags = binary.BigEndian.Uint16(data[pos : pos+2])
			pos += 2
		}

		var pathBytes []byte
		if ver == 4 {
			strip, nextPos, ok := readIndexVarint(data, pos, bodyEnd)
			if !ok || strip > len(prevPath) {
				return
			}

			pos = nextPos
			nul := bytes.IndexByte(data[pos:bodyEnd], 0)
			if nul < 0 {
				return
			}

			suffix := data[pos : pos+nul]
			pos += nul + 1

			prefixLen := len(prevPath) - strip
			pathBytes = make([]byte, prefixLen+len(suffix))
			copy(pathBytes, prevPath[:prefixLen])
			copy(pathBytes[prefixLen:], suffix)
			prevPath = pathBytes
		} else {
			nul := bytes.IndexByte(data[pos:bodyEnd], 0)
			if nul < 0 {
				return
			}

			pathBytes = data[pos : pos+nul]
			pos += nul + 1

			if rem := (pos - entryStart) % 8; rem != 0 {
				pos += 8 - rem
			}

			if pos > bodyEnd {
				return
			}
		}

		if len(pathBytes) == 0 || flags&ceStagemask != 0 ||
			extFlags&(ceSkipWorktree|ceIntentToAdd) != 0 ||
			mode&modeTypeMask == modeGitlink {
			continue
		}

		entries = append(entries, indexEntryRef{
			offset: entryStart,
			path:   filepath.FromSlash(string(pathBytes)),
		})
	}

	// Verify extensions reach bodyEnd and bail out on any mandatory extension
	// (Per gitformat-index(5), signatures whose first byte is not 'A'..'Z'
	// such as "link" or "sdir" change how entries are interpreted).
	for pos < bodyEnd {
		if pos+8 > bodyEnd {
			return
		}

		if b := data[pos]; b < 'A' || b > 'Z' {
			return
		}

		extSize := int(binary.BigEndian.Uint32(data[pos+4 : pos+8]))
		pos += 8
		if extSize < 0 || pos+extSize > bodyEnd {
			return
		}

		pos += extSize
	}

	if pos != bodyEnd || len(entries) == 0 {
		return
	}

	var (
		changed     atomic.Bool
		skippedRacy atomic.Bool
	)

	updateOne := func(e indexEntryRef) {
		off := e.offset
		idxCtimeSec := binary.BigEndian.Uint32(data[off+0 : off+4])
		idxCtimeNsec := binary.BigEndian.Uint32(data[off+4 : off+8])
		idxMtimeSec := binary.BigEndian.Uint32(data[off+8 : off+12])
		idxMtimeNsec := binary.BigEndian.Uint32(data[off+12 : off+16])
		idxDev := binary.BigEndian.Uint32(data[off+16 : off+20])
		idxIno := binary.BigEndian.Uint32(data[off+20 : off+24])
		idxUid := binary.BigEndian.Uint32(data[off+28 : off+32])
		idxGid := binary.BigEndian.Uint32(data[off+32 : off+36])
		idxSize := binary.BigEndian.Uint32(data[off+36 : off+40])

		if srcWorktree != "" {
			// Per Git's is_racy_stat, an entry whose mtime is at or after the
			// index file's own mtime is racily clean and must be rehashed by Git.
			if idxMtimeSec >= indexMtimeSec {
				skippedRacy.Store(true)

				return
			}

			srcSt, ok := statIndexFields(filepath.Join(srcWorktree, e.path))
			if !ok || srcSt.ctimeSec != idxCtimeSec || srcSt.ctimeNsec != idxCtimeNsec ||
				srcSt.mtimeSec != idxMtimeSec || srcSt.mtimeNsec != idxMtimeNsec ||
				srcSt.dev != idxDev || srcSt.ino != idxIno ||
				srcSt.uid != idxUid || srcSt.gid != idxGid || srcSt.size != idxSize {
				return
			}
		}

		dstSt, ok := statIndexFields(filepath.Join(worktree, e.path))
		if !ok || dstSt.mtimeSec != idxMtimeSec || dstSt.mtimeNsec != idxMtimeNsec || dstSt.size != idxSize {
			return
		}

		binary.BigEndian.PutUint32(data[off+0:off+4], dstSt.ctimeSec)
		binary.BigEndian.PutUint32(data[off+4:off+8], dstSt.ctimeNsec)
		binary.BigEndian.PutUint32(data[off+16:off+20], dstSt.dev)
		binary.BigEndian.PutUint32(data[off+20:off+24], dstSt.ino)
		binary.BigEndian.PutUint32(data[off+28:off+32], dstSt.uid)
		binary.BigEndian.PutUint32(data[off+32:off+36], dstSt.gid)
		changed.Store(true)
	}

	workers := min(runtime.GOMAXPROCS(0), 8)
	if len(entries) < 256 || workers <= 1 {
		for _, e := range entries {
			updateOne(e)
		}
	} else {
		var next atomic.Int64
		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					i := int(next.Add(1) - 1)
					if i >= len(entries) {
						return
					}
					updateOne(entries[i])
				}
			}()
		}
		wg.Wait()
	}

	if !changed.Load() {
		return
	}

	sum := sha1.Sum(data[:bodyEnd])
	copy(data[bodyEnd:], sum[:])
	if err := os.WriteFile(indexPath, data, 0o644); err != nil {
		return
	}

	// Preserve the template index's mtime when racy entries were skipped so
	// Git still sees entry.mtime >= index.mtime for those entries.
	if skippedRacy.Load() {
		_ = os.Chtimes(indexPath, time.Now(), indexMtime)
	}
}

// readIndexVarint decodes a Git offset-encoded varint used in index v4.
func readIndexVarint(data []byte, pos, end int) (int, int, bool) {
	if pos >= end {
		return 0, pos, false
	}

	b := data[pos]
	pos++
	val := int(b & 0x7f)

	for b&0x80 != 0 {
		if pos >= end {
			return 0, pos, false
		}

		val++
		b = data[pos]
		pos++
		val = (val << 7) | int(b&0x7f)
	}

	return val, pos, true
}
