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

/*
   Copyright 2019 The Go Authors. All rights reserved.
   Use of this source code is governed by a BSD-style
   license that can be found in the NOTICE.md file.
*/

package reader

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/awslabs/soci-snapshotter/cache"
	commonmetrics "github.com/awslabs/soci-snapshotter/fs/metrics/common"
	spanmanager "github.com/awslabs/soci-snapshotter/fs/span-manager"
	"github.com/awslabs/soci-snapshotter/metadata"
	"github.com/awslabs/soci-snapshotter/soci"
	"github.com/hashicorp/go-multierror"
	digest "github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

type Reader interface {
	OpenFile(id uint32) (io.ReaderAt, error)
	Metadata() metadata.Reader
	Close() error
	LastOnDemandReadTime() time.Time
}

// VerifiableReader produces a Reader with a given verifier.
type VerifiableReader struct {
	r *reader

	lastVerifyErr           atomic.Value
	prohibitVerifyFailure   bool
	prohibitVerifyFailureMu sync.RWMutex

	closed   bool
	closedMu sync.Mutex

	verifier func(uint32, string) (digest.Verifier, error)
}

func (vr *VerifiableReader) SkipVerify() Reader {
	return vr.r
}

func (vr *VerifiableReader) VerifyTOC(tocDigest digest.Digest) (Reader, error) {
	if vr.isClosed() {
		return nil, fmt.Errorf("reader is already closed")
	}
	vr.prohibitVerifyFailureMu.Lock()
	vr.prohibitVerifyFailure = true
	lastVerifyErr := vr.lastVerifyErr.Load()
	vr.prohibitVerifyFailureMu.Unlock()
	if err := lastVerifyErr; err != nil {
		return nil, errors.Wrapf(err.(error), "content error occures during caching contents")
	}
	vr.r.verify = true
	return vr.r, nil
}

// nolint:revive
func (vr *VerifiableReader) GetReader() *reader {
	return vr.r
}

func (vr *VerifiableReader) Metadata() metadata.Reader {
	// TODO: this shouldn't be called before verified
	return vr.r.r
}

func (vr *VerifiableReader) Close() error {
	vr.closedMu.Lock()
	defer vr.closedMu.Unlock()
	if vr.closed {
		return nil
	}
	vr.closed = true
	return vr.r.Close()
}

func (vr *VerifiableReader) isClosed() bool {
	vr.closedMu.Lock()
	closed := vr.closed
	vr.closedMu.Unlock()
	return closed
}

// NewReader creates a Reader based on the given soci blob and Span Manager.
// It returns VerifiableReader so the caller must provide a metadata.ChunkVerifier
// to use for verifying file or chunk contained in this stargz blob.
func NewReader(r metadata.Reader, layerSha digest.Digest, spanManager *spanmanager.SpanManager) (*VerifiableReader, error) {
	vr := &reader{
		spanManager: spanManager,
		r:           r,
		layerSha:    layerSha,
		verifier:    digestVerifier,
	}
	return &VerifiableReader{r: vr, verifier: digestVerifier}, nil
}

type reader struct {
	spanManager *spanmanager.SpanManager
	r           metadata.Reader
	layerSha    digest.Digest

	lastReadTime   time.Time
	lastReadTimeMu sync.Mutex

	closed   bool
	closedMu sync.Mutex

	verify   bool
	verifier func(uint32, string) (digest.Verifier, error)
}

func (gr *reader) Metadata() metadata.Reader {
	return gr.r
}

func (gr *reader) setLastReadTime(lastReadTime time.Time) {
	gr.lastReadTimeMu.Lock()
	gr.lastReadTime = lastReadTime
	gr.lastReadTimeMu.Unlock()
}

func (gr *reader) LastOnDemandReadTime() time.Time {
	gr.lastReadTimeMu.Lock()
	t := gr.lastReadTime
	gr.lastReadTimeMu.Unlock()
	return t
}

func (gr *reader) OpenFile(id uint32) (io.ReaderAt, error) {
	if gr.isClosed() {
		return nil, fmt.Errorf("reader is already closed")
	}
	var fr metadata.File
	fr, err := gr.r.OpenFile(id)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to open file %d", id)
	}
	return &file{
		id: id,
		fr: fr,
		gr: gr,
	}, nil
}

func (gr *reader) Close() (retErr error) {
	gr.closedMu.Lock()
	defer gr.closedMu.Unlock()
	if gr.closed {
		return nil
	}
	gr.closed = true
	if err := gr.r.Close(); err != nil {
		retErr = multierror.Append(retErr, err)
	}
	return
}

func (gr *reader) isClosed() bool {
	gr.closedMu.Lock()
	closed := gr.closed
	gr.closedMu.Unlock()
	return closed
}

type file struct {
	id uint32
	fr metadata.File
	gr *reader
}

// ReadAt reads the file when the file is requested by the container
func (sf *file) ReadAt(p []byte, offset int64) (int, error) {
	uncompFileSize := sf.fr.GetUncompressedFileSize()
	if soci.FileSize(offset) >= uncompFileSize {
		return 0, io.EOF
	}
	expectedSize := uncompFileSize - soci.FileSize(offset)
	if expectedSize > soci.FileSize(len(p)) {
		expectedSize = soci.FileSize(len(p))
	}
	fileOffsetStart := sf.fr.GetUncompressedOffset() + soci.FileSize(offset)
	fileOffsetEnd := fileOffsetStart + expectedSize
	r, err := sf.gr.spanManager.GetContents(fileOffsetStart, fileOffsetEnd)
	if err != nil {
		return 0, errors.Wrap(err, "failed to read the file")
	}

	commonmetrics.IncOperationCount(commonmetrics.OnDemandRemoteRegistryFetchCount, sf.gr.layerSha) // increment the number of on demand file fetches from remote registry
	sf.gr.setLastReadTime(time.Now())

	contents, err := io.ReadAll(r)
	if err != nil && err != io.EOF {
		return 0, err
	}
	n := copy(p, contents)
	if soci.FileSize(n) != expectedSize {
		return 0, fmt.Errorf("unexpected copied data size for on-demand fetch. read = %d, expected = %d", n, expectedSize)
	}
	commonmetrics.AddBytesCount(commonmetrics.OnDemandBytesServed, sf.gr.layerSha, int64(n)) // measure the number of on demand bytes served

	return n, nil
}

type CacheOption func(*cacheOptions)

type cacheOptions struct {
	cacheOpts []cache.Option
	filter    func(int64) bool
	reader    *io.SectionReader
}

func WithCacheOpts(cacheOpts ...cache.Option) CacheOption {
	return func(opts *cacheOptions) {
		opts.cacheOpts = cacheOpts
	}
}

func WithFilter(filter func(int64) bool) CacheOption {
	return func(opts *cacheOptions) {
		opts.filter = filter
	}
}

func WithReader(sr *io.SectionReader) CacheOption {
	return func(opts *cacheOptions) {
		opts.reader = sr
	}
}

func digestVerifier(id uint32, chunkDigestStr string) (digest.Verifier, error) {
	chunkDigest, err := digest.Parse(chunkDigestStr)
	if err != nil {
		return nil, errors.Wrap(err, "invalid chunk: no digset is recorded")
	}
	return chunkDigest.Verifier(), nil
}
