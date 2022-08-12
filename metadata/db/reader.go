/*
   Copyright The Soci Snapshotter Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package db

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/awslabs/soci-snapshotter/metadata"
	"github.com/awslabs/soci-snapshotter/soci"
	"github.com/pkg/errors"
	"github.com/rs/xid"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/sync/errgroup"
)

// reader stores filesystem metadata parsed from ztoc to metadata DB
// and provides methods to read them.
type reader struct {
	db     *bolt.DB
	fsID   string
	rootID uint32
	sr     *io.SectionReader

	curID   uint32
	curIDMu sync.Mutex
	initG   *errgroup.Group
}

func (r *reader) nextID() (uint32, error) {
	r.curIDMu.Lock()
	defer r.curIDMu.Unlock()
	if r.curID == math.MaxUint32 {
		return 0, fmt.Errorf("sequence id too large")
	}
	r.curID++
	return r.curID, nil
}

// NewReader parses ztoc and stores filesystem metadata to the provided DB.
func NewReader(db *bolt.DB, sr *io.SectionReader, ztoc *soci.Ztoc, opts ...metadata.Option) (metadata.Reader, error) {
	var rOpts metadata.Options
	for _, o := range opts {
		if err := o(&rOpts); err != nil {
			return nil, errors.Wrapf(err, "failed to apply option")
		}
	}

	r := &reader{sr: sr, db: db, initG: new(errgroup.Group)}
	start := time.Now()
	if rOpts.Telemetry != nil && rOpts.Telemetry.InitMetadataStoreLatency != nil {
		rOpts.Telemetry.InitMetadataStoreLatency(start)
	}

	if err := r.init(ztoc, rOpts); err != nil {
		return nil, errors.Wrapf(err, "failed to initialize metadata")
	}
	return r, nil
}

// RootID returns ID of the root node.
func (r *reader) RootID() uint32 {
	return r.rootID
}

// Clone returns a new reader identical to the current reader
// but uses the provided section reader for retrieving file paylaods.
func (r *reader) Clone(sr *io.SectionReader) (metadata.Reader, error) {
	if err := r.waitInit(); err != nil {
		return nil, err
	}
	return &reader{
		db:     r.db,
		fsID:   r.fsID,
		rootID: r.rootID,
		sr:     sr,
		initG:  new(errgroup.Group),
	}, nil
}

func (r *reader) init(ztoc *soci.Ztoc, rOpts metadata.Options) (retErr error) {
	// Initialize root node
	var ok bool
	for i := 0; i < 100; i++ {
		fsID := xid.New().String()
		if err := r.initRootNode(fsID); err != nil {
			if errors.Is(err, bolt.ErrBucketExists) {
				continue // try with another id
			}
			return errors.Wrapf(err, "failed to initialize root node %q", fsID)
		}
		ok = true
		break
	}
	if !ok {
		return fmt.Errorf("failed to get a unique id for metadata reader")
	}

	if err := r.initNodes(ztoc); err != nil {
		return err
	}
	return nil
}

func (r *reader) initRootNode(fsID string) error {
	return r.db.Batch(func(tx *bolt.Tx) (err error) {
		filesystems, err := tx.CreateBucketIfNotExists(bucketKeyFilesystems)
		if err != nil {
			return err
		}
		lbkt, err := filesystems.CreateBucket([]byte(fsID))
		if err != nil {
			return err
		}
		r.fsID = fsID
		if _, err := lbkt.CreateBucket(bucketKeyZtocs); err != nil {
			return err
		}
		if _, err := lbkt.CreateBucket(bucketKeyMetadata); err != nil {
			return err
		}
		nodes, err := lbkt.CreateBucket(bucketKeyNodes)
		if err != nil {
			return err
		}
		rootID, err := r.nextID()
		if err != nil {
			return err
		}
		rootBucket, err := nodes.CreateBucket(encodeID(rootID))
		if err != nil {
			return err
		}
		if err := writeAttr(rootBucket, &metadata.Attr{
			Mode:    os.ModeDir | 0755,
			NumLink: 2, // The directory itself(.) and the parent link to this directory.
		}); err != nil {
			return err
		}
		r.rootID = rootID
		return err
	})
}

func (r *reader) initNodes(ztoc *soci.Ztoc) error {
	md := make(map[uint32]*metadataEntry)
	ztocID, err := r.nextID()
	if err != nil {
		return err
	}
	if err := r.db.Batch(func(tx *bolt.Tx) (err error) {
		ztocs, err := getZtocs(tx, r.fsID)
		if err != nil {
			return nil
		}
		ztocs.FillPercent = 1.0
		ze := &ztocEntry{
			id:                 ztocID,
			IndexByteData:      ztoc.IndexByteData,
			CompressedFileSize: ztoc.CompressedFileSize,
			MaxSpanID:          ztoc.MaxSpanId,
			Version:            ztoc.Version,
		}
		ztocBucket, err := ztocs.CreateBucket(encodeID(ze.id))
		if err != nil {
			return err
		}
		err = writeZtocEntry(ztocBucket, ze)
		if err != nil {
			return err
		}

		nodes, err := getNodes(tx, r.fsID)

		if err != nil {
			return err
		}
		nodes.FillPercent = 1.0 // we only do sequential write to this bucket
		var attr metadata.Attr
		for _, ent := range ztoc.Metadata {
			var id uint32
			var b *bolt.Bucket
			ent.Name = cleanEntryName(ent.Name)
			isLink := ent.Type == "hardlink"
			if isLink {
				id, err = getIDByName(md, ent.Linkname, r.rootID)
				if err != nil {
					return errors.Wrapf(err, "%q is a hardlink but cannot get link destination %q", ent.Name, ent.Linkname)
				}
				b, err = getNodeBucketByID(nodes, id)
				if err != nil {
					return errors.Wrapf(err, "cannot get hardlink destination %q ==> %q (%d)", ent.Name, ent.Linkname, id)
				}
				numLink, _ := binary.Varint(b.Get(bucketKeyNumLink))
				if err := putInt(b, bucketKeyNumLink, numLink+1); err != nil {
					return errors.Wrapf(err, "cannot put NumLink of %q ==> %q", ent.Name, ent.Linkname)
				}
			} else {
				// Write node bucket
				var found bool
				if ent.Type == "dir" {
					// Check if this directory is already created, if so overwrite it.
					id, err = getIDByName(md, ent.Name, r.rootID)
					if err == nil {
						b, err = getNodeBucketByID(nodes, id)
						if err != nil {
							return errors.Wrapf(err, "failed to get directory bucket %d", id)
						}
						found = true
						attr.NumLink = readNumLink(b)
					}
				}
				if !found {
					// No existing node. Create a new one.
					id, err = r.nextID()
					if err != nil {
						return err
					}
					b, err = nodes.CreateBucket(encodeID(id))
					if err != nil {
						return err
					}
					attr.NumLink = 1 // at least the parent dir references this directory.
					if ent.Type == "dir" {
						attr.NumLink++ // at least "." references this directory.
					}
				}
				if err := writeAttr(b, attrFromZtocEntry(&ent, &attr)); err != nil {
					return errors.Wrapf(err, "failed to set attr to %d(%q)", id, ent.Name)
				}
			}

			pdirName := parentDir(ent.Name)
			pid, pb, err := r.getOrCreateDir(nodes, md, pdirName, r.rootID)
			if err != nil {
				return errors.Wrapf(err, "failed to create parent directory %q of %q", pdirName, ent.Name)
			}
			if err := setChild(md, pb, pid, path.Base(ent.Name), id, ent.Type == "dir"); err != nil {
				return err
			}

			if !isLink {
				if md[id] == nil {
					md[id] = &metadataEntry{}
				}
				md[id].UncompressedOffset = ent.UncompressedOffset
				md[id].SpanStart = ent.SpanStart
				md[id].SpanEnd = ent.SpanEnd
				md[id].FirstSpanHasBits = strconv.FormatBool(ent.FirstSpanHasBits)
				md[id].ZtocID = ztocID
			}
		}
		return nil
	}); err != nil {
		return err
	}

	addendum := make([]struct {
		id []byte
		md *metadataEntry
	}, len(md))
	i := 0
	for id, d := range md {
		addendum[i].id, addendum[i].md = encodeID(id), d
		i++
	}
	sort.Slice(addendum, func(i, j int) bool {
		return bytes.Compare(addendum[i].id, addendum[j].id) < 0
	})

	if err := r.db.Batch(func(tx *bolt.Tx) (err error) {
		meta, err := getMetadata(tx, r.fsID)
		if err != nil {
			return err
		}
		meta.FillPercent = 1.0 // we only do sequential write to this bucket

		for _, m := range addendum {
			md, err := meta.CreateBucket(m.id)
			if err != nil {
				return err
			}
			if err := writeMetadataEntry(md, m.md); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}

	return nil
}

func (r *reader) getOrCreateDir(nodes *bolt.Bucket, md map[uint32]*metadataEntry, d string, rootID uint32) (id uint32, b *bolt.Bucket, err error) {
	id, err = getIDByName(md, d, rootID)
	if err != nil {
		id, err = r.nextID()
		if err != nil {
			return 0, nil, err
		}
		b, err = nodes.CreateBucket(encodeID(id))
		if err != nil {
			return 0, nil, err
		}
		attr := &metadata.Attr{
			Mode:    os.ModeDir | 0755,
			NumLink: 2, // The directory itself(.) and the parent link to this directory.
		}
		if err := writeAttr(b, attr); err != nil {
			return 0, nil, err
		}
		if d != "" {
			pid, pb, err := r.getOrCreateDir(nodes, md, parentDir(d), rootID)
			if err != nil {
				return 0, nil, err
			}
			if err := setChild(md, pb, pid, path.Base(d), id, true); err != nil {
				return 0, nil, err
			}
		}
	} else {
		b, err = getNodeBucketByID(nodes, id)
		if err != nil {
			return 0, nil, errors.Wrapf(err, "failed to get dir bucket %d", id)
		}
	}
	return id, b, nil
}

func (r *reader) waitInit() error {
	// TODO: add timeout
	return errors.Wrapf(r.initG.Wait(), "initialization failed")
}

func (r *reader) view(fn func(tx *bolt.Tx) error) error {
	if err := r.waitInit(); err != nil {
		return err
	}
	return r.db.View(func(tx *bolt.Tx) error {
		return fn(tx)
	})
}

func (r *reader) update(fn func(tx *bolt.Tx) error) error {
	if err := r.waitInit(); err != nil {
		return err
	}
	return r.db.Batch(func(tx *bolt.Tx) error {
		return fn(tx)
	})
}

// Close closes this reader. This removes underlying filesystem metadata as well.
func (r *reader) Close() error {
	return r.update(func(tx *bolt.Tx) (err error) {
		filesystems := tx.Bucket(bucketKeyFilesystems)
		if filesystems == nil {
			return nil
		}
		return filesystems.DeleteBucket([]byte(r.fsID))
	})
}

// GetAttr returns file attribute of specified node.
func (r *reader) GetAttr(id uint32) (attr metadata.Attr, _ error) {
	if r.rootID == id { // no need to wait for root dir
		if err := r.db.View(func(tx *bolt.Tx) error {
			nodes, err := getNodes(tx, r.fsID)
			if err != nil {
				return errors.Wrapf(err, "nodes bucket of %q not found for sarching attr %d", r.fsID, id)
			}
			b, err := getNodeBucketByID(nodes, id)
			if err != nil {
				return errors.Wrapf(err, "failed to get attr bucket %d", id)
			}
			return readAttr(b, &attr)
		}); err != nil {
			return metadata.Attr{}, err
		}
		return attr, nil
	}
	if err := r.view(func(tx *bolt.Tx) error {
		nodes, err := getNodes(tx, r.fsID)
		if err != nil {
			return errors.Wrapf(err, "nodes bucket of %q not found for sarching attr %d", r.fsID, id)
		}
		b, err := getNodeBucketByID(nodes, id)
		if err != nil {
			return errors.Wrapf(err, "failed to get attr bucket %d", id)
		}
		return readAttr(b, &attr)
	}); err != nil {
		return metadata.Attr{}, err
	}
	return
}

// GetChild returns a child node that has the specified base name.
func (r *reader) GetChild(pid uint32, base string) (id uint32, attr metadata.Attr, _ error) {
	if err := r.view(func(tx *bolt.Tx) error {
		metadataEntries, err := getMetadata(tx, r.fsID)
		if err != nil {
			return errors.Wrapf(err, "metadata bucket of %q not found for getting child of %d", r.fsID, pid)
		}
		md, err := getMetadataBucketByID(metadataEntries, pid)
		if err != nil {
			return errors.Wrapf(err, "failed to get parent metadata %d", pid)
		}
		id, err = readChild(md, base)
		if err != nil {
			return errors.Wrapf(err, "failed to read child %q of %d", base, pid)
		}
		nodes, err := getNodes(tx, r.fsID)
		if err != nil {
			return errors.Wrapf(err, "nodes bucket of %q not found for getting child of %d", r.fsID, pid)
		}
		child, err := getNodeBucketByID(nodes, id)
		if err != nil {
			return errors.Wrapf(err, "failed to get child bucket %d", id)
		}
		return readAttr(child, &attr)
	}); err != nil {
		return 0, metadata.Attr{}, err
	}
	return
}

// ForeachChild calls the specified callback function for each child node.
// When the callback returns non-nil error, this stops the iteration.
func (r *reader) ForeachChild(id uint32, f func(name string, id uint32, mode os.FileMode) bool) error {
	type childInfo struct {
		id   uint32
		mode os.FileMode
	}
	children := make(map[string]childInfo)
	if err := r.view(func(tx *bolt.Tx) error {
		metadataEntries, err := getMetadata(tx, r.fsID)
		if err != nil {
			return errors.Wrapf(err, "nodes bucket of %q not found for getting child of %d", r.fsID, id)
		}
		md, err := getMetadataBucketByID(metadataEntries, id)
		if err != nil {
			return nil // no child
		}

		var nodes *bolt.Bucket
		firstName := md.Get(bucketKeyChildName)
		if len(firstName) != 0 {
			firstID := decodeID(md.Get(bucketKeyChildID))
			if nodes == nil {
				nodes, err = getNodes(tx, r.fsID)
				if err != nil {
					return errors.Wrapf(err, "nodes bucket of %q not found for getting children of %d", r.fsID, id)
				}
			}
			firstChild, err := getNodeBucketByID(nodes, firstID)
			if err != nil {
				return errors.Wrapf(err, "failed to get first child bucket %d", firstID)
			}
			mode, _ := binary.Uvarint(firstChild.Get(bucketKeyMode))
			children[string(firstName)] = childInfo{firstID, os.FileMode(uint32(mode))}
		}

		cbkt := md.Bucket(bucketKeyChildrenExtra)
		if cbkt == nil {
			return nil // no child
		}
		if nodes == nil {
			nodes, err = getNodes(tx, r.fsID)
			if err != nil {
				return errors.Wrapf(err, "nodes bucket of %q not found for getting children of %d", r.fsID, id)
			}
		}
		return cbkt.ForEach(func(k, v []byte) error {
			id := decodeID(v)
			child, err := getNodeBucketByID(nodes, id)
			if err != nil {
				return errors.Wrapf(err, "failed to get child bucket %d", id)
			}
			mode, _ := binary.Uvarint(child.Get(bucketKeyMode))
			children[string(k)] = childInfo{id, os.FileMode(uint32(mode))}
			return nil
		})
	}); err != nil {
		return err
	}
	for k, e := range children {
		if !f(k, e.id, e.mode) {
			break
		}
	}
	return nil
}

// OpenFile returns a section reader of the specified node.
func (r *reader) OpenFile(id uint32) (metadata.File, error) {
	var size int64
	var config *soci.FileExtractConfig

	if err := r.view(func(tx *bolt.Tx) error {
		nodes, err := getNodes(tx, r.fsID)
		if err != nil {
			return errors.Wrapf(err, "nodes bucket of %q not found for opening %d", r.fsID, id)
		}
		b, err := getNodeBucketByID(nodes, id)
		if err != nil {
			return errors.Wrapf(err, "failed to get file bucket %d", id)
		}
		size, _ = binary.Varint(b.Get(bucketKeySize))
		m, _ := binary.Uvarint(b.Get(bucketKeyMode))
		if !os.FileMode(uint32(m)).IsRegular() {
			return fmt.Errorf("%q is not a regular file", id)
		}

		ztocs, err := getZtocs(tx, r.fsID)
		if err != nil {
			return errors.Wrapf(err, "ztocs bucket of %q not found for opening %d", r.fsID, id)
		}

		metadataEntries, err := getMetadata(tx, r.fsID)
		if err != nil {
			return errors.Wrapf(err, "metadata bucket of %q not found for opening %d", r.fsID, id)
		}
		if md, err := getMetadataBucketByID(metadataEntries, id); err == nil {
			config, err = getFileExtractConfig(md, ztocs, soci.FileSize(size))
			if err != nil {
				return errors.Wrapf(err, "can't get the file extract config for %d", id)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	fr := &fileReader{
		r:             r,
		extractConfig: config,
	}
	return &file{io.NewSectionReader(fr, 0, size), config.UncompressedOffset, soci.FileSize(size)}, nil
}

func getFileExtractConfig(md *bolt.Bucket, ztocs *bolt.Bucket, uncompressedSize soci.FileSize) (*soci.FileExtractConfig, error) {
	var config soci.FileExtractConfig
	config.UncompressedSize = uncompressedSize
	ucompOffset, _ := binary.Varint(md.Get(bucketKeyUncompressedOffset))
	config.UncompressedOffset = soci.FileSize(ucompOffset)
	spanStart, _ := binary.Varint(md.Get(bucketKeySpanStart))
	config.SpanStart = soci.SpanId(spanStart)
	spanEnd, _ := binary.Varint(md.Get(bucketKeySpanEnd))
	config.SpanEnd = soci.SpanId(spanEnd)
	config.FirstSpanHasBits = string(md.Get(bucketKeyFirstSpanHasBits))
	ztoc, err := getZtocBucketByID(ztocs, decodeID(md.Get(bucketKeyZtocID)))
	if err != nil {
		return nil, err
	}
	config.IndexByteData = ztoc.Get(bucketKeyIndexByteData)
	compFileSize, _ := binary.Uvarint(ztoc.Get(bucketKeyCompressedFileSize))
	config.CompressedFileSize = soci.FileSize(compFileSize)
	maxSpanID, _ := binary.Uvarint(ztoc.Get(bucketKeyMaxSpanID))
	config.MaxSpanId = soci.SpanId(maxSpanID)
	return &config, nil
}

type file struct {
	io.ReaderAt
	uncompressedOffset soci.FileSize
	uncompressedSize   soci.FileSize
}

func (fr *file) GetUncompressedFileSize() soci.FileSize {
	return fr.uncompressedSize
}

func (fr *file) GetUncompressedOffset() soci.FileSize {
	return fr.uncompressedOffset
}

type fileReader struct {
	r             *reader
	extractConfig *soci.FileExtractConfig
}

// ReadAt reads file payload of this file.
func (fr *fileReader) ReadAt(p []byte, off int64) (n int, err error) {
	if soci.FileSize(off) >= fr.extractConfig.UncompressedSize {
		return 0, io.EOF
	}
	if off < 0 {
		return 0, errors.New("invalid offset")
	}
	decompressedFile, err := soci.ExtractFile(fr.r.sr, fr.extractConfig)
	if err != nil {
		return 0, err
	}
	return copy(p, decompressedFile[off:]), nil
}

// TODO: share it with memory pkg
func attrFromZtocEntry(src *soci.FileMetadata, dst *metadata.Attr) *metadata.Attr {
	dst.Size = int64(src.UncompressedSize)
	dst.ModTime = src.ModTime
	dst.LinkName = src.Linkname
	dst.Mode = soci.GetFileMode(src)
	dst.UID = src.UID
	dst.GID = src.GID
	dst.DevMajor = int(src.Devmajor)
	dst.DevMinor = int(src.Devminor)
	xattrs := make(map[string][]byte)
	for k, v := range src.Xattrs {
		xattrs[k] = []byte(v)
	}
	dst.Xattrs = xattrs
	return dst
}

func getIDByName(md map[uint32]*metadataEntry, name string, rootID uint32) (uint32, error) {
	name = cleanEntryName(name)
	if name == "" {
		return rootID, nil
	}
	dir, base := filepath.Split(name)
	pid, err := getIDByName(md, dir, rootID)
	if err != nil {
		return 0, err
	}
	if md[pid] == nil {
		return 0, fmt.Errorf("not found metadata of %d", pid)
	}
	if md[pid].children == nil {
		return 0, fmt.Errorf("not found children of %q", pid)
	}
	c, ok := md[pid].children[base]
	if !ok {
		return 0, fmt.Errorf("not found child %q in %d", base, pid)
	}
	return c.id, nil
}

func setChild(md map[uint32]*metadataEntry, pb *bolt.Bucket, pid uint32, base string, id uint32, isDir bool) error {
	if md[pid] == nil {
		md[pid] = &metadataEntry{}
	}
	if md[pid].children == nil {
		md[pid].children = make(map[string]childEntry)
	}
	md[pid].children[base] = childEntry{base, id}
	if isDir {
		numLink, _ := binary.Varint(pb.Get(bucketKeyNumLink))
		if err := putInt(pb, bucketKeyNumLink, numLink+1); err != nil {
			return errors.Wrapf(err, "cannot add numlink for children")
		}
	}
	return nil
}

func parentDir(p string) string {
	dir, _ := path.Split(p)
	return strings.TrimSuffix(dir, "/")
}

func cleanEntryName(name string) string {
	// Use path.Clean to consistently deal with path separators across platforms.
	return strings.TrimPrefix(path.Clean("/"+name), "/")
}

func (r *reader) NumOfNodes() (i int, _ error) {
	if err := r.view(func(tx *bolt.Tx) error {
		nodes, err := getNodes(tx, r.fsID)
		if err != nil {
			return err
		}
		return nodes.ForEach(func(k, v []byte) error {
			b := nodes.Bucket(k)
			if b == nil {
				return fmt.Errorf("entry bucket for %q not found", string(k))
			}
			var attr metadata.Attr
			if err := readAttr(b, &attr); err != nil {
				return err
			}
			i++
			return nil
		})
	}); err != nil {
		return 0, err
	}
	return
}