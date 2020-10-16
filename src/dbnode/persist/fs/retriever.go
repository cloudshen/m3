// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

// The block retriever is used to stream blocks of data from disk. It controls
// the fetch concurrency on a per-Namespace basis I.E if the server is using
// spinning-disks the concurrency can be set to 1 to serialize all disk fetches
// for a given namespace, and the concurrency be set higher in the case of SSDs.
// This fetch concurrency is primarily implemented via the number of concurrent
// fetchLoops that the retriever creates.
//
// The block retriever also handles batching of requests for data, as well as
// re-arranging the order of requests to increase data locality when seeking
// through and across files.

package fs

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/m3db/m3/src/dbnode/namespace"
	"github.com/m3db/m3/src/dbnode/persist/fs/wide"
	"github.com/m3db/m3/src/dbnode/sharding"
	"github.com/m3db/m3/src/dbnode/storage/block"
	"github.com/m3db/m3/src/dbnode/storage/limits"
	"github.com/m3db/m3/src/dbnode/ts"
	"github.com/m3db/m3/src/dbnode/x/xio"
	"github.com/m3db/m3/src/x/checked"
	"github.com/m3db/m3/src/x/context"
	"github.com/m3db/m3/src/x/ident"
	"github.com/m3db/m3/src/x/pool"

	"go.uber.org/zap"
)

var (
	errBlockRetrieverNotOpen             = errors.New("block retriever is not open")
	errBlockRetrieverAlreadyOpenOrClosed = errors.New("block retriever already open or is closed")
	errBlockRetrieverAlreadyClosed       = errors.New("block retriever already closed")
	errNoSeekerMgr                       = errors.New("there is no open seeker manager")
	errUnsetRequestType                  = errors.New("request type unset")
)

type streamReqType uint8

const (
	defaultRetrieveRequestQueueCapacity = 4096

	streamInvalidReq streamReqType = iota
	streamDataReq
	streamIdxChecksumReq
	streamReadMismatchReq
)

type blockRetrieverStatus int

type newSeekerMgrFn func(
	bytesPool pool.CheckedBytesPool,
	opts Options,
	blockRetrieverOpts BlockRetrieverOptions,
) DataFileSetSeekerManager

const (
	blockRetrieverNotOpen blockRetrieverStatus = iota
	blockRetrieverOpen
	blockRetrieverClosed
)

type blockRetriever struct {
	sync.RWMutex

	opts           BlockRetrieverOptions
	fsOpts         Options
	logger         *zap.Logger
	queryLimits    limits.QueryLimits
	bytesReadLimit limits.LookbackLimit

	newSeekerMgrFn newSeekerMgrFn

	reqPool    RetrieveRequestPool
	bytesPool  pool.CheckedBytesPool
	idPool     ident.Pool
	nsMetadata namespace.Metadata

	blockSize               time.Duration
	nsCacheBlocksOnRetrieve bool

	status                     blockRetrieverStatus
	reqsByShardIdx             []*shardRetrieveRequests
	seekerMgr                  DataFileSetSeekerManager
	notifyFetch                chan struct{}
	fetchLoopsShouldShutdownCh chan struct{}
	fetchLoopsHaveShutdownCh   chan struct{}
}

// NewBlockRetriever returns a new block retriever for TSDB file sets.
func NewBlockRetriever(
	opts BlockRetrieverOptions,
	fsOpts Options,
) (DataBlockRetriever, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	return &blockRetriever{
		opts:           opts,
		fsOpts:         fsOpts,
		logger:         fsOpts.InstrumentOptions().Logger(),
		queryLimits:    opts.QueryLimits(),
		bytesReadLimit: opts.QueryLimits().BytesReadLimit(),
		newSeekerMgrFn: NewSeekerManager,
		reqPool:        opts.RetrieveRequestPool(),
		bytesPool:      opts.BytesPool(),
		idPool:         opts.IdentifierPool(),
		status:         blockRetrieverNotOpen,
		notifyFetch:    make(chan struct{}, 1),
		// We just close this channel when the fetchLoops should shutdown, so no
		// buffering is required
		fetchLoopsShouldShutdownCh: make(chan struct{}),
		fetchLoopsHaveShutdownCh:   make(chan struct{}, opts.FetchConcurrency()),
	}, nil
}

func (r *blockRetriever) Open(
	ns namespace.Metadata,
	shardSet sharding.ShardSet,
) error {
	r.Lock()
	defer r.Unlock()

	if r.status != blockRetrieverNotOpen {
		return errBlockRetrieverAlreadyOpenOrClosed
	}

	seekerMgr := r.newSeekerMgrFn(r.bytesPool, r.fsOpts, r.opts)
	if err := seekerMgr.Open(ns, shardSet); err != nil {
		return err
	}

	r.nsMetadata = ns
	r.status = blockRetrieverOpen
	r.seekerMgr = seekerMgr

	// Cache blockSize result and namespace specific block caching option
	r.blockSize = ns.Options().RetentionOptions().BlockSize()
	r.nsCacheBlocksOnRetrieve = ns.Options().CacheBlocksOnRetrieve()

	for i := 0; i < r.opts.FetchConcurrency(); i++ {
		go r.fetchLoop(seekerMgr)
	}
	return nil
}

func (r *blockRetriever) CacheShardIndices(shards []uint32) error {
	r.RLock()
	if r.status != blockRetrieverOpen {
		r.RUnlock()
		return errBlockRetrieverNotOpen
	}
	seekerMgr := r.seekerMgr
	r.RUnlock()

	// Don't hold the RLock() for duration of CacheShardIndices because
	// it can take a very long time and it could block the regular read
	// path (which sometimes needs to acquire an exclusive lock). In practice
	// this is fine, it just means that the Retriever could be closed while a
	// call to CacheShardIndices is still outstanding.
	return seekerMgr.CacheShardIndices(shards)
}

func (r *blockRetriever) AssignShardSet(shardSet sharding.ShardSet) {
	// NB(bodu): Block retriever will always be open before calling this method.
	// But have this check anyways to be safe.
	r.RLock()
	defer r.RUnlock()
	if r.status != blockRetrieverOpen {
		return
	}
	r.seekerMgr.AssignShardSet(shardSet)
}

func (r *blockRetriever) fetchLoop(seekerMgr DataFileSetSeekerManager) {
	var (
		seekerResources = NewReusableSeekerResources(r.fsOpts)
		inFlight        []*retrieveRequest
		currBatchReqs   []*retrieveRequest
	)
	for {
		// Free references to the inflight requests
		for i := range inFlight {
			inFlight[i] = nil
		}
		inFlight = inFlight[:0]

		// Select in flight requests
		r.RLock()
		// Move requests from shard retriever reqs into in flight slice
		for _, reqs := range r.reqsByShardIdx {
			reqs.Lock()
			if len(reqs.queued) > 0 {
				inFlight = append(inFlight, reqs.queued...)
				reqs.resetQueued()
			}
			reqs.Unlock()
		}

		status := r.status
		n := len(inFlight)
		r.RUnlock()

		// Exit if not open and fulfilled all open requests
		if n == 0 && status != blockRetrieverOpen {
			break
		}

		// If no fetches then no work to do, yield
		if n == 0 {
			select {
			case <-r.notifyFetch:
				continue
			case <-r.fetchLoopsShouldShutdownCh:
				break
			}
		}

		// Files are all by shard and block time, the locality of
		// files is therefore foremost by block time as that is when they are
		// all written. Note that this sort does NOT mean that we're going to stripe
		// through different files at once as you might expect at first, but simply
		// that since all the fileset files are written at the end of a block period
		// those files are more likely to be physically located close to each other
		// on disk. In other words, instead of accessing files like this:
		// 		shard1T1 --> shard1T2 --> shard1T3 --> shard2T1 --> shard2T2 --> shard2T3
		// its probably faster to access them like this:
		// 		shard1T1 --> shard2T1 --> shard1T2 --> shard2T2 --> shard1T3 --> shard2T3
		// so we re-arrange the order of the requests to achieve that
		sort.Sort(retrieveRequestByStartAscShardAsc(inFlight))

		// Iterate through all in flight requests and send them to the seeker in
		// batches of block time + shard.
		currBatchShard := uint32(0)
		currBatchStart := time.Time{}
		currBatchReqs = currBatchReqs[:0]
		for _, req := range inFlight {
			if !req.start.Equal(currBatchStart) ||
				req.shard != currBatchShard {
				// Fetch any outstanding in the current batch
				if len(currBatchReqs) > 0 {
					r.fetchBatch(
						seekerMgr, currBatchShard, currBatchStart, currBatchReqs, seekerResources)
					for i := range currBatchReqs {
						currBatchReqs[i] = nil
					}
					currBatchReqs = currBatchReqs[:0]
				}

				// Set the new batch attributes
				currBatchShard = req.shard
				currBatchStart = req.start
			}

			// Enqueue into the current batch
			currBatchReqs = append(currBatchReqs, req)
		}

		// Fetch any finally outstanding in the current batch
		if len(currBatchReqs) > 0 {
			r.fetchBatch(
				seekerMgr, currBatchShard, currBatchStart, currBatchReqs, seekerResources)
			for i := range currBatchReqs {
				currBatchReqs[i] = nil
			}
			currBatchReqs = currBatchReqs[:0]
		}
	}

	r.fetchLoopsHaveShutdownCh <- struct{}{}
}

func (r *blockRetriever) processIndexChecksumRequest(
	req *retrieveRequest,
	seeker ConcurrentDataFileSetSeeker,
	seekerResources ReusableSeekerResources,
) {
	entry, err := seeker.SeekIndexEntryToIndexChecksum(req.id, seekerResources)
	if err != nil {
		req.onError(err)
		return
	}

	req.onIndexChecksumCompleted(entry)
}

func (r *blockRetriever) fetchBatch(
	seekerMgr DataFileSetSeekerManager,
	shard uint32,
	blockStart time.Time,
	reqs []*retrieveRequest,
	seekerResources ReusableSeekerResources,
) {
	// Resolve the seeker from the seeker mgr
	seeker, err := seekerMgr.Borrow(shard, blockStart)
	if err != nil {
		for _, req := range reqs {
			req.onError(err)
		}
		return
	}

	defer func() {
		err = seekerMgr.Return(shard, blockStart, seeker)
		if err != nil {
			r.logger.Error("err returning seeker for shard",
				zap.Uint32("shard", shard),
				zap.Int64("blockStart", blockStart.Unix()),
				zap.Error(err),
			)
		}
	}()

	// NB: filter out stream checksum requests, as these are handled seperately.
	// Rebuild reqs so the wrong reference is less likely to be used later.
	reqsPreProcessing := reqs
	reqs = reqs[:0]

	// Sort the requests by offset into the file before seeking
	// to ensure all seeks are in ascending order
	var limitErr error
	for _, req := range reqsPreProcessing {
		if req.streamReqType == streamInvalidReq {
			req.onError(errUnsetRequestType)
			continue
		}

		// NB: index checksum queries should ignore limits.
		if req.streamReqType == streamIdxChecksumReq {
			r.processIndexChecksumRequest(req, seeker, seekerResources)
			continue
		}

		reqs = append(reqs, req)
	}

	// NB: remaining requests must be within query limits.
	if err := r.queryLimits.AnyExceeded(); err != nil {
		for _, req := range reqs {
			req.onError(err)
		}
		return
	}

	for _, req := range reqs {
		if limitErr != nil {
			req.onError(limitErr)
			continue
		}

		entry, err := seeker.SeekIndexEntry(req.id, seekerResources)
		if err != nil && err != errSeekIDNotFound {
			req.onError(err)
			continue
		}

		if err := r.bytesReadLimit.Inc(int(entry.Size)); err != nil {
			req.onError(err)
			limitErr = err
			continue
		}

		if err == errSeekIDNotFound {
			req.notFound = true
		}

		req.indexEntry = entry
	}

	sort.Sort(retrieveRequestByOffsetAsc(reqs))
	tagDecoderPool := r.fsOpts.TagDecoderPool()

	blockCachingEnabled := r.opts.CacheBlocksOnRetrieve() && r.nsCacheBlocksOnRetrieve

	// Seek and execute all requests
	for _, req := range reqs {
		var (
			data checked.Bytes
			err  error
		)

		// Only try to seek the ID if it exists and there haven't been any errors so
		// far, otherwise we'll get a checksum mismatch error because the default
		// offset value for indexEntry is zero.
		if req.foundAndHasNoError() {
			data, err = seeker.SeekByIndexEntry(req.indexEntry, seekerResources)
			if err != nil && err != errSeekIDNotFound {
				req.onError(err)
				continue
			}
		}

		var (
			seg, onRetrieveSeg ts.Segment
			checksum           = req.indexEntry.DataChecksum
		)
		if data != nil {
			seg = ts.NewSegment(data, nil, checksum, ts.FinalizeHead)
		}

		// We don't need to call onRetrieve.OnRetrieveBlock if the ID was not found.
		callOnRetrieve := blockCachingEnabled && req.onRetrieve != nil && req.foundAndHasNoError()
		if callOnRetrieve {
			// NB(r): Need to also trigger callback with a copy of the data.
			// This is used by the database to cache the in memory data for
			// consequent fetches.
			if data != nil {
				dataCopy := r.bytesPool.Get(data.Len())
				onRetrieveSeg = ts.NewSegment(dataCopy, nil, checksum, ts.FinalizeHead)
				dataCopy.AppendAll(data.Bytes())
			}
			if tags := req.indexEntry.EncodedTags; tags != nil && tags.Len() > 0 {
				decoder := tagDecoderPool.Get()
				// DecRef because we're transferring ownership from the index entry to
				// the tagDecoder which will IncRef().
				tags.DecRef()
				decoder.Reset(tags)
				req.tags = decoder
			}
		} else {
			// If we didn't transfer ownership of the tags to the decoder above, then we
			// no longer need them and we can can finalize them.
			if tags := req.indexEntry.EncodedTags; tags != nil {
				tags.DecRef()
				tags.Finalize()
			}
		}

		// Complete request.
		req.onRetrieved(seg, req.nsCtx)

		if !callOnRetrieve {
			// No need to call the onRetrieve callback.
			req.onCallerOrRetrieverDone()
			continue
		}

		go func(r *retrieveRequest) {
			// Call the onRetrieve callback and finalize.
			r.onRetrieve.OnRetrieveBlock(r.id, r.tags, r.start, onRetrieveSeg, r.nsCtx)
			r.onCallerOrRetrieverDone()
		}(req)
	}
}

/*









 */

func (r *blockRetriever) wideFetchBatch(
	reqs []*retrieveRequest,
	seeker ConcurrentDataFileSetSeeker,
	seekerResources ReusableSeekerResources,
) []*retrieveRequest {
	streamRequests := reqs[:0]
	for _, req := range reqs {
		if req.streamReqType == streamDataReq {
			streamRequests = append(streamRequests, req)
		}

		mismatches, err := seeker.SeekIndexEntryToReadMismatches(
			req.id, req.batchReader, seekerResources)

		if err != nil && err != errSeekIDNotFound {
			req.onError(err)
			continue
		}

		if err == errSeekIDNotFound {
			req.onIndexMismatchBatchCompleted(wide.ReadMismatchBatch{})
			continue
		}

		seriesAdded := false
		// Add data to any mismatches that originated on this node.
		for _, mismatch := range mismatches {
			if mismatch.EncodedTags == nil {
				continue
			}

			if seriesAdded {
				err = fmt.Errorf("cannot add data to more than one series per request")
				req.onError(err)
				continue
			}

		}

		req.onIndexMismatchBatchCompleted(nil)
	}

	// Seek and execute all requests
	for _, req := range reqs {
		var (
			data checked.Bytes
			err  error
		)

		// Only try to seek the ID if it exists and there haven't been any errors so
		// far, otherwise we'll get a checksum mismatch error because the default
		// offset value for indexEntry is zero.
		if req.foundAndHasNoError() {
			data, err = seeker.SeekByIndexEntry(req.indexEntry, seekerResources)
			if err != nil && err != errSeekIDNotFound {
				req.onError(err)
				continue
			}
		}

		var (
			seg, onRetrieveSeg ts.Segment
			checksum           = req.indexEntry.DataChecksum
		)
		if data != nil {
			seg = ts.NewSegment(data, nil, checksum, ts.FinalizeHead)
		}

		// If we didn't transfer ownership of the tags to the decoder above, then we
		// no longer need them and we can can finalize them.
		if tags := req.indexEntry.EncodedTags; tags != nil {
			tags.DecRef()
			tags.Finalize()
		}

		// Complete request.
		req.onRetrieved(seg, req.nsCtx)

		// No need to call the onRetrieve callback.
		req.onCallerOrRetrieverDone()
		continue
	}

	return streamRequests
}

/*









 */

func (r *blockRetriever) streamRequest(
	ctx context.Context,
	req *retrieveRequest,
	shard uint32,
	id ident.ID,
	startTime time.Time,
	onNoID func(),
	nsCtx namespace.Context,
) error {
	req.shard = shard
	// NB(r): Clone the ID as we're not positive it will stay valid throughout
	// the lifecycle of the async request.
	req.id = r.idPool.Clone(id)
	req.start = startTime
	req.blockSize = r.blockSize

	req.resultWg.Add(1)

	// Ensure to finalize at the end of request
	ctx.RegisterFinalizer(req)

	// Capture variable and RLock() because this slice can be modified in the
	// Open() method
	r.RLock()
	// This should never happen unless caller tries to use Stream() before Open()
	if r.seekerMgr == nil {
		r.RUnlock()
		return errNoSeekerMgr
	}
	r.RUnlock()

	idExists, err := r.seekerMgr.Test(id, shard, startTime)
	if err != nil {
		return err
	}

	// If the ID is not in the seeker's bloom filter, then it's definitely not on
	// disk and we can return immediately.
	if !idExists {
		if onNoID != nil {
			onNoID()
		}

		return nil
	}
	reqs, err := r.shardRequests(shard)
	if err != nil {
		return err
	}

	reqs.Lock()
	reqs.queued = append(reqs.queued, req)
	reqs.Unlock()

	// Notify fetch loop
	select {
	case r.notifyFetch <- struct{}{}:
	default:
		// Loop busy, already ready to consume notification
	}

	// The request may not have completed yet, but it has an internal
	// waitgroup which the caller will have to wait for before retrieving
	// the data. This means that even though we're returning nil for error
	// here, the caller may still encounter an error when they attempt to
	// read the data.
	return nil
}

func (req *retrieveRequest) toBlock() xio.BlockReader {
	return xio.BlockReader{
		SegmentReader: req,
		Start:         req.start,
		BlockSize:     req.blockSize,
	}
}

func (r *blockRetriever) Stream(
	ctx context.Context,
	shard uint32,
	id ident.ID,
	startTime time.Time,
	onRetrieve block.OnRetrieveBlock,
	nsCtx namespace.Context,
) (xio.BlockReader, error) {
	req := r.reqPool.Get()
	req.onRetrieve = onRetrieve
	req.streamReqType = streamDataReq

	onNoIDFn := func() {
		// No need to call req.onRetrieve.OnRetrieveBlock if there is no data.
		req.onRetrieved(ts.Segment{}, namespace.Context{})
	}

	err := r.streamRequest(ctx, req, shard, id, startTime, onNoIDFn, nsCtx)
	if err != nil {
		return xio.EmptyBlockReader, err
	}

	// The request may not have completed yet, but it has an internal
	// waitgroup which the caller will have to wait for before retrieving
	// the data. This means that even though we're returning nil for error
	// here, the caller may still encounter an error when they attempt to
	// read the data.
	return req.toBlock(), nil
}

func (r *blockRetriever) StreamIndexChecksum(
	ctx context.Context,
	shard uint32,
	id ident.ID,
	startTime time.Time,
	nsCtx namespace.Context,
) (block.StreamedChecksum, error) {
	req := r.reqPool.Get()
	req.streamReqType = streamIdxChecksumReq

	err := r.streamRequest(ctx, req, shard, id, startTime, nil, nsCtx)
	if err != nil {
		return block.EmptyStreamedChecksum, err
	}

	// The request may not have completed yet, but it has an internal
	// waitgroup which the caller will have to wait for before retrieving
	// the data. This means that even though we're returning nil for error
	// here, the caller may still encounter an error when they attempt to
	// read the data.
	return req, nil
}

func (r *blockRetriever) StreamReadMismatches(
	ctx context.Context,
	shard uint32,
	batchReader wide.IndexChecksumBlockBatchReader,
	id ident.ID,
	startTime time.Time,
	nsCtx namespace.Context,
) (wide.StreamedMismatchBatch, error) {
	req := r.reqPool.Get()
	req.batchReader = batchReader
	req.streamReqType = streamReadMismatchReq

	err := r.streamRequest(ctx, req, shard, id, startTime, nil, nsCtx)
	if err != nil {
		return wide.EmptyStreamedMismatchBatch, err
	}

	// The request may not have completed yet, but it has an internal
	// waitgroup which the caller will have to wait for before retrieving
	// the data. This means that even though we're returning nil for error
	// here, the caller may still encounter an error when they attempt to
	// read the data.
	return req, nil
}

func (r *blockRetriever) shardRequests(
	shard uint32,
) (*shardRetrieveRequests, error) {
	r.RLock()
	if r.status != blockRetrieverOpen {
		r.RUnlock()
		return nil, errBlockRetrieverNotOpen
	}
	if int(shard) < len(r.reqsByShardIdx) {
		reqs := r.reqsByShardIdx[shard]
		r.RUnlock()
		return reqs, nil
	}
	r.RUnlock()

	r.Lock()
	defer r.Unlock()

	// Check if raced with another call to this method
	if r.status != blockRetrieverOpen {
		return nil, errBlockRetrieverNotOpen
	}
	if int(shard) < len(r.reqsByShardIdx) {
		reqs := r.reqsByShardIdx[shard]
		return reqs, nil
	}

	reqsByShardIdx := make([]*shardRetrieveRequests, shard+1)

	for i := range reqsByShardIdx {
		if i < len(r.reqsByShardIdx) {
			reqsByShardIdx[i] = r.reqsByShardIdx[i]
			continue
		}
		capacity := defaultRetrieveRequestQueueCapacity
		reqsByShardIdx[i] = &shardRetrieveRequests{
			shard:  uint32(i),
			queued: make([]*retrieveRequest, 0, capacity),
		}
	}

	r.reqsByShardIdx = reqsByShardIdx
	reqs := r.reqsByShardIdx[shard]

	return reqs, nil
}

func (r *blockRetriever) Close() error {
	r.Lock()
	if r.status == blockRetrieverClosed {
		r.Unlock()
		return errBlockRetrieverAlreadyClosed
	}
	r.nsMetadata = nil
	r.status = blockRetrieverClosed

	r.blockSize = 0
	r.Unlock()

	close(r.fetchLoopsShouldShutdownCh)
	for i := 0; i < r.opts.FetchConcurrency(); i++ {
		<-r.fetchLoopsHaveShutdownCh
	}

	return r.seekerMgr.Close()
}

type shardRetrieveRequests struct {
	sync.Mutex
	shard  uint32
	queued []*retrieveRequest
}

func (reqs *shardRetrieveRequests) resetQueued() {
	// Free references to the queued requests
	for i := range reqs.queued {
		reqs.queued[i] = nil
	}
	reqs.queued = reqs.queued[:0]
}

// Don't forget to update the resetForReuse method when adding a new field
type retrieveRequest struct {
	resultWg sync.WaitGroup

	pool *reqPool

	id         ident.ID
	tags       ident.TagIterator
	start      time.Time
	blockSize  time.Duration
	onRetrieve block.OnRetrieveBlock
	nsCtx      namespace.Context

	streamReqType streamReqType
	indexEntry    IndexEntry
	indexChecksum ident.IndexChecksum
	mismatchBatch wide.ReadMismatchBatch
	reader        xio.SegmentReader

	batchReader wide.IndexChecksumBlockBatchReader

	err error

	// Finalize requires two calls to finalize (once both the user of the
	// request and the retriever fetch loop is done, and only then, can
	// we free this request) so we track this with an atomic here.
	finalizes uint32
	shard     uint32

	notFound bool
}

func (req *retrieveRequest) onIndexChecksumCompleted(
	indexChecksum ident.IndexChecksum) {
	if req.err == nil {
		req.indexChecksum = indexChecksum
		// If there was an error, we've already called done.
		req.resultWg.Done()
	}
}

func (req *retrieveRequest) RetrieveIndexChecksum() (ident.IndexChecksum, error) {
	req.resultWg.Wait()
	if req.err != nil {
		return ident.IndexChecksum{}, req.err
	}
	return req.indexChecksum, nil
}

func (req *retrieveRequest) onIndexMismatchBatchCompleted(
	batch wide.ReadMismatchBatch) {
	if req.err == nil {
		req.mismatchBatch = batch
		// If there was an error, we've already called done.
		req.resultWg.Done()
	}
}

func (req *retrieveRequest) RetrieveMismatchBatch() (wide.ReadMismatchBatch, error) {
	req.resultWg.Wait()
	if req.err != nil {
		return wide.ReadMismatchBatch{}, req.err
	}
	return req.mismatchBatch, nil
}

func (req *retrieveRequest) onError(err error) {
	if req.err == nil {
		req.err = err
		req.resultWg.Done()
	}
}

func (req *retrieveRequest) onRetrieved(segment ts.Segment, nsCtx namespace.Context) {
	req.Reset(segment)
	req.nsCtx = nsCtx
}

func (req *retrieveRequest) onCallerOrRetrieverDone() {
	if atomic.AddUint32(&req.finalizes, 1) != 2 {
		return
	}
	// NB: streamIdxChecksumReq ids are used to sort the resultant list,
	// so they should not be finalized here.
	if req.streamReqType == streamDataReq {
		req.id.Finalize()
	}
	req.id = nil
	if req.tags != nil {
		req.tags.Close()
		req.tags = ident.EmptyTagIterator
	}
	req.reader.Finalize()
	req.reader = nil
	req.pool.Put(req)
}

func (req *retrieveRequest) Reset(segment ts.Segment) {
	req.reader.Reset(segment)
	if req.err == nil {
		// If there was an error, we've already called done.
		req.resultWg.Done()
	}
}

func (req *retrieveRequest) ResetWindowed(segment ts.Segment, start time.Time, blockSize time.Duration) {
	req.Reset(segment)
	req.start = start
	req.blockSize = blockSize
}

func (req *retrieveRequest) SegmentReader() (xio.SegmentReader, error) {
	return req.reader, nil
}

// NB: be aware to avoid calling Clone() in a hot path, since it copies the
// underlying bytes.
func (req *retrieveRequest) Clone(
	pool pool.CheckedBytesPool,
) (xio.SegmentReader, error) {
	req.resultWg.Wait() // wait until result is ready
	if req.err != nil {
		return nil, req.err
	}
	return req.reader.Clone(pool)
}

func (req *retrieveRequest) Start() time.Time {
	return req.start
}

func (req *retrieveRequest) BlockSize() time.Duration {
	return req.blockSize
}

func (req *retrieveRequest) Read(b []byte) (int, error) {
	req.resultWg.Wait()
	if req.err != nil {
		return 0, req.err
	}
	return req.reader.Read(b)
}

func (req *retrieveRequest) Segment() (ts.Segment, error) {
	req.resultWg.Wait()
	if req.err != nil {
		return ts.Segment{}, req.err
	}
	return req.reader.Segment()
}

func (req *retrieveRequest) Finalize() {
	// May not actually finalize the request, depending on if
	// retriever is done too
	req.onCallerOrRetrieverDone()
}

func (req *retrieveRequest) resetForReuse() {
	req.resultWg = sync.WaitGroup{}
	req.finalizes = 0
	req.shard = 0
	req.id = nil
	req.tags = ident.EmptyTagIterator
	req.start = time.Time{}
	req.blockSize = 0
	req.onRetrieve = nil
	req.streamReqType = streamInvalidReq
	req.indexEntry = IndexEntry{}
	req.indexChecksum = ident.IndexChecksum{}
	req.mismatchBatch = wide.ReadMismatchBatch{}
	req.batchReader = nil
	req.reader = nil
	req.err = nil
	req.notFound = false
}

func (req *retrieveRequest) foundAndHasNoError() bool {
	return !req.notFound && req.err == nil
}

type retrieveRequestByStartAscShardAsc []*retrieveRequest

func (r retrieveRequestByStartAscShardAsc) Len() int      { return len(r) }
func (r retrieveRequestByStartAscShardAsc) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r retrieveRequestByStartAscShardAsc) Less(i, j int) bool {
	if !r[i].start.Equal(r[j].start) {
		return r[i].start.Before(r[j].start)
	}
	return r[i].shard < r[j].shard
}

type retrieveRequestByOffsetAsc []*retrieveRequest

func (r retrieveRequestByOffsetAsc) Len() int      { return len(r) }
func (r retrieveRequestByOffsetAsc) Swap(i, j int) { r[i], r[j] = r[j], r[i] }
func (r retrieveRequestByOffsetAsc) Less(i, j int) bool {
	return r[i].indexEntry.Offset < r[j].indexEntry.Offset
}

// RetrieveRequestPool is the retrieve request pool.
type RetrieveRequestPool interface {
	Init()
	Get() *retrieveRequest
	Put(req *retrieveRequest)
}

type reqPool struct {
	segmentReaderPool xio.SegmentReaderPool
	pool              pool.ObjectPool
}

// NewRetrieveRequestPool returns a new retrieve request pool.
func NewRetrieveRequestPool(
	segmentReaderPool xio.SegmentReaderPool,
	opts pool.ObjectPoolOptions,
) RetrieveRequestPool {
	return &reqPool{
		segmentReaderPool: segmentReaderPool,
		pool:              pool.NewObjectPool(opts),
	}
}

func (p *reqPool) Init() {
	p.pool.Init(func() interface{} {
		return &retrieveRequest{pool: p}
	})
}

func (p *reqPool) Get() *retrieveRequest {
	req := p.pool.Get().(*retrieveRequest)
	req.resetForReuse()
	req.reader = p.segmentReaderPool.Get()
	return req
}

func (p *reqPool) Put(req *retrieveRequest) {
	// Also call reset for reuse to nil any references before
	// putting back in pool to avoid holding strong refs to any
	// shortly lived objects while still in the pool
	req.resetForReuse()
	p.pool.Put(req)
}
