package bloomshipper

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dolthub/swiss"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/utils/keymutex"

	v1 "github.com/grafana/loki/pkg/storage/bloom/v1"
	"github.com/grafana/loki/pkg/storage/chunk/cache"
	"github.com/grafana/loki/pkg/util/constants"
)

var downloadQueueCapacity = 10000

type options struct {
	ignoreNotFound bool // ignore 404s from object storage; default=true
	fetchAsync     bool // dispatch downloading of block and return immediately; default=false
}

func (o *options) apply(opts ...FetchOption) {
	for _, opt := range opts {
		opt(o)
	}
}

type FetchOption func(opts *options)

func WithIgnoreNotFound(v bool) FetchOption {
	return func(opts *options) {
		opts.ignoreNotFound = v
	}
}

func WithFetchAsync(v bool) FetchOption {
	return func(opts *options) {
		opts.fetchAsync = v
	}
}

type fetcher interface {
	FetchMetas(ctx context.Context, refs []MetaRef) ([]Meta, error)
	FetchBlocks(ctx context.Context, refs []BlockRef, opts ...FetchOption) ([]*CloseableBlockQuerier, error)
	Close()
}

// Compiler check to ensure Fetcher implements the fetcher interface
var _ fetcher = &Fetcher{}

type Fetcher struct {
	client Client

	metasCache      cache.Cache
	blocksCache     cache.TypedCache[string, BlockDirectory]
	localFSResolver KeyResolver

	q *downloadQueue[BlockRef, BlockDirectory]

	cfg     bloomStoreConfig
	metrics *fetcherMetrics
	logger  log.Logger
}

func NewFetcher(cfg bloomStoreConfig, client Client, metasCache cache.Cache, blocksCache cache.TypedCache[string, BlockDirectory], reg prometheus.Registerer, logger log.Logger) (*Fetcher, error) {
	fetcher := &Fetcher{
		cfg:             cfg,
		client:          client,
		metasCache:      metasCache,
		blocksCache:     blocksCache,
		localFSResolver: NewPrefixedResolver(cfg.workingDir, defaultKeyResolver{}),
		metrics:         newFetcherMetrics(reg, constants.Loki, "bloom_store"),
		logger:          logger,
	}
	q, err := newDownloadQueue[BlockRef, BlockDirectory](downloadQueueCapacity, cfg.numWorkers, fetcher.processTask, logger)
	if err != nil {
		return nil, errors.Wrap(err, "creating download queue for fetcher")
	}
	fetcher.q = q
	return fetcher, nil
}

func (f *Fetcher) Close() {
	f.q.close()
}

// FetchMetas implements fetcher
func (f *Fetcher) FetchMetas(ctx context.Context, refs []MetaRef) ([]Meta, error) {
	if ctx.Err() != nil {
		return nil, errors.Wrap(ctx.Err(), "fetch Metas")
	}

	keys := make([]string, 0, len(refs))
	for _, ref := range refs {
		keys = append(keys, f.client.Meta(ref).Addr())
	}
	cacheHits, cacheBufs, _, err := f.metasCache.Fetch(ctx, keys)
	if err != nil {
		return nil, err
	}

	fromCache, missing, err := f.processMetasCacheResponse(ctx, refs, cacheHits, cacheBufs)
	if err != nil {
		return nil, err
	}

	fromStorage, err := f.client.GetMetas(ctx, missing)
	if err != nil {
		return nil, err
	}

	err = f.writeBackMetas(ctx, fromStorage)
	if err != nil {
		return nil, err
	}

	results := append(fromCache, fromStorage...)
	f.metrics.metasFetched.Observe(float64(len(results)))
	// TODO(chaudum): get metas size from storage
	// getting the size from the metas would require quite a bit of refactoring
	// so leaving this for a separate PR if it's really needed
	// f.metrics.metasFetchedSize.WithLabelValues(sourceCache).Observe(float64(fromStorage.Size()))
	return results, nil
}

func (f *Fetcher) processMetasCacheResponse(_ context.Context, refs []MetaRef, keys []string, bufs [][]byte) ([]Meta, []MetaRef, error) {
	found := make(map[string][]byte, len(refs))
	for i, k := range keys {
		found[k] = bufs[i]
	}

	metas := make([]Meta, 0, len(found))
	missing := make([]MetaRef, 0, len(refs)-len(keys))

	var lastErr error
	var size int64
	for i, ref := range refs {
		if raw, ok := found[f.client.Meta(ref).Addr()]; ok {
			meta := Meta{
				MetaRef: ref,
			}
			lastErr = json.Unmarshal(raw, &meta)
			metas = append(metas, meta)
		} else {
			missing = append(missing, refs[i])
		}
	}

	f.metrics.metasFetchedSize.WithLabelValues(sourceCache).Observe(float64(size))
	return metas, missing, lastErr
}

func (f *Fetcher) writeBackMetas(ctx context.Context, metas []Meta) error {
	var err error
	keys := make([]string, len(metas))
	data := make([][]byte, len(metas))
	for i := range metas {
		keys[i] = f.client.Meta(metas[i].MetaRef).Addr()
		data[i], err = json.Marshal(metas[i])
	}
	if err != nil {
		return err
	}
	return f.metasCache.Store(ctx, keys, data)
}

// FetchBlocks implements fetcher
func (f *Fetcher) FetchBlocks(ctx context.Context, refs []BlockRef, opts ...FetchOption) ([]*CloseableBlockQuerier, error) {
	// apply fetch options
	cfg := &options{ignoreNotFound: true, fetchAsync: false}
	cfg.apply(opts...)

	// first, resolve blocks from cache and enqueue missing blocks to download queue
	n := len(refs)
	results := make([]*CloseableBlockQuerier, n)
	responses := make(chan downloadResponse[BlockDirectory], n)
	errors := make(chan error, n)

	found, missing := 0, 0

	// TODO(chaudum): Can fetching items from cache be made more efficient
	// by fetching all keys at once.
	// The problem is keeping the order of the responses.

	var enqueueTime time.Duration
	for i := 0; i < n; i++ {
		key := f.client.Block(refs[i]).Addr()
		dir, isFound, err := f.getBlockDir(ctx, key)
		if err != nil {
			return results, err
		}
		if !isFound {
			f.metrics.downloadQueueSize.Observe(float64(len(f.q.queue)))
			start := time.Now()
			f.q.enqueue(downloadRequest[BlockRef, BlockDirectory]{
				ctx:     ctx,
				item:    refs[i],
				key:     key,
				idx:     i,
				async:   cfg.fetchAsync,
				results: responses,
				errors:  errors,
			})
			missing++
			f.metrics.blocksMissing.Inc()
			enqueueTime += time.Since(start)
			f.metrics.downloadQueueEnqueueTime.Observe(time.Since(start).Seconds())
			continue
		}
		found++
		f.metrics.blocksFound.Inc()
		results[i] = dir.BlockQuerier()
	}

	// fetchAsync defines whether the function may return early or whether it
	// should wait for responses from the download queue
	if cfg.fetchAsync {
		f.metrics.blocksFetched.Observe(float64(found))
		level.Debug(f.logger).Log("msg", "request unavailable blocks in the background", "missing", missing, "found", found, "enqueue_time", enqueueTime)
		return results, nil
	}

	level.Debug(f.logger).Log("msg", "wait for unavailable blocks", "missing", missing, "found", found, "enqueue_time", enqueueTime)
	// second, wait for missing blocks to be fetched and append them to the
	// results
	for i := 0; i < missing; i++ {
		select {
		case err := <-errors:
			// TODO(owen-d): add metrics for missing blocks
			if f.client.IsObjectNotFoundErr(err) && cfg.ignoreNotFound {
				level.Warn(f.logger).Log("msg", "ignore not found block", "err", err)
				continue
			}

			f.metrics.blocksFetched.Observe(float64(found))
			return results, err
		case res := <-responses:
			found++
			results[res.idx] = res.item.BlockQuerier()
		}
	}

	level.Debug(f.logger).Log("msg", "return found blocks", "found", found)
	f.metrics.blocksFetched.Observe(float64(found))
	return results, nil
}

func (f *Fetcher) processTask(ctx context.Context, task downloadRequest[BlockRef, BlockDirectory]) {
	if ctx.Err() != nil {
		task.errors <- ctx.Err()
		return
	}

	// check if block was fetched while task was waiting in queue
	result, exists, err := f.getBlockDir(ctx, task.key)
	if err != nil {
		task.errors <- err
		return
	}

	// return item from cache
	if exists {
		level.Debug(f.logger).Log("msg", "send download response", "reason", "cache")
		task.results <- downloadResponse[BlockDirectory]{
			item: result,
			key:  task.key,
			idx:  task.idx,
		}
		return
	}

	// fetch from storage
	result, err = f.fetchBlock(ctx, task.item)
	if err != nil {
		task.errors <- err
		return
	}

	// return item from storage
	level.Debug(f.logger).Log("msg", "send download response", "reason", "storage")
	task.results <- downloadResponse[BlockDirectory]{
		item: result,
		key:  task.key,
		idx:  task.idx,
	}
}

// getBlockDir resolves a block directory without fetching the block from
// remote object storage. Returns three arguments: the block directory, a
// boolean whether the block was found in cache, and optionally an error.
func (f *Fetcher) getBlockDir(ctx context.Context, key string) (BlockDirectory, bool, error) {
	var zero BlockDirectory

	if ctx.Err() != nil {
		return zero, false, errors.Wrap(ctx.Err(), "get block")
	}

	_, fromCache, _, err := f.blocksCache.Fetch(ctx, []string{key})
	if err != nil {
		return zero, false, err
	}

	// item found in cache
	if len(fromCache) == 1 {
		f.metrics.blocksFetchedSize.WithLabelValues(sourceCache).Observe(float64(fromCache[0].Size()))
		return fromCache[0], true, nil
	}

	// item wasn't found
	return zero, false, nil
}

func (f *Fetcher) fetchBlock(ctx context.Context, ref BlockRef) (BlockDirectory, error) {
	var zero BlockDirectory

	if ctx.Err() != nil {
		return zero, errors.Wrap(ctx.Err(), "fetch block")
	}

	fromStorage, err := f.client.GetBlock(ctx, ref)
	if err != nil {
		return zero, err
	}

	// item found in storage
	err = f.writeBackBlocks(ctx, []BlockDirectory{fromStorage})
	f.metrics.blocksFetchedSize.WithLabelValues(sourceStorage).Observe(float64(fromStorage.Size()))
	return fromStorage, err
}

func (f *Fetcher) loadBlocksFromFS(_ context.Context, refs []BlockRef) ([]BlockDirectory, []BlockRef, error) {
	blockDirs := make([]BlockDirectory, 0, len(refs))
	missing := make([]BlockRef, 0, len(refs))

	for _, ref := range refs {
		path := f.localFSResolver.Block(ref).LocalPath()
		// the block directory does not contain the .tar.gz extension
		// since it is stripped when the archive is extracted into a folder
		path = strings.TrimSuffix(path, ".tar.gz")
		if ok, clean := f.isBlockDir(path); ok {
			blockDirs = append(blockDirs, NewBlockDirectory(ref, path, f.logger))
		} else {
			_ = clean(path)
			missing = append(missing, ref)
		}
	}

	return blockDirs, missing, nil
}

var noopClean = func(string) error { return nil }

func (f *Fetcher) isBlockDir(path string) (bool, func(string) error) {
	return isBlockDir(path, f.logger)
}

func isBlockDir(path string, logger log.Logger) (bool, func(string) error) {
	info, err := os.Stat(path)
	if err != nil && os.IsNotExist(err) {
		level.Warn(logger).Log("msg", "path does not exist", "path", path)
		return false, noopClean
	}
	if !info.IsDir() {
		return false, os.Remove
	}
	for _, file := range []string{
		filepath.Join(path, v1.BloomFileName),
		filepath.Join(path, v1.SeriesFileName),
	} {
		if _, err := os.Stat(file); err != nil && os.IsNotExist(err) {
			level.Warn(logger).Log("msg", "path does not contain required file", "path", path, "file", file)
			return false, os.RemoveAll
		}
	}
	return true, nil
}

func (f *Fetcher) writeBackBlocks(ctx context.Context, blocks []BlockDirectory) error {
	keys := make([]string, len(blocks))
	for i := range blocks {
		keys[i] = f.client.Block(blocks[i].BlockRef).Addr()
	}
	return f.blocksCache.Store(ctx, keys, blocks)
}

type processFunc[T any, R any] func(context.Context, downloadRequest[T, R])

type downloadRequest[T any, R any] struct {
	ctx     context.Context
	item    T
	key     string
	idx     int
	async   bool
	results chan<- downloadResponse[R]
	errors  chan<- error
}

type downloadResponse[R any] struct {
	item R
	key  string
	idx  int
}

type downloadQueue[T any, R any] struct {
	queue         chan downloadRequest[T, R]
	enqueued      *swiss.Map[string, struct{}]
	enqueuedMutex sync.Mutex
	mu            keymutex.KeyMutex
	wg            sync.WaitGroup
	done          chan struct{}
	process       processFunc[T, R]
	logger        log.Logger
}

func newDownloadQueue[T any, R any](size, workers int, process processFunc[T, R], logger log.Logger) (*downloadQueue[T, R], error) {
	if size < 1 {
		return nil, errors.New("queue size needs to be greater than 0")
	}
	if workers < 1 {
		return nil, errors.New("queue requires at least 1 worker")
	}
	q := &downloadQueue[T, R]{
		queue:    make(chan downloadRequest[T, R], size),
		enqueued: swiss.NewMap[string, struct{}](uint32(size)),
		mu:       keymutex.NewHashed(workers),
		done:     make(chan struct{}),
		process:  process,
		logger:   logger,
	}
	for i := 0; i < workers; i++ {
		q.wg.Add(1)
		go q.runWorker()
	}
	return q, nil
}

func (q *downloadQueue[T, R]) enqueue(t downloadRequest[T, R]) {
	if !t.async {
		q.queue <- t
	}
	// for async task we attempt to dedupe task already in progress.
	q.enqueuedMutex.Lock()
	defer q.enqueuedMutex.Unlock()
	if q.enqueued.Has(t.key) {
		return
	}
	select {
	case q.queue <- t:
		q.enqueued.Put(t.key, struct{}{})
	default:
		// todo we probably want a metric on dropped items
		level.Warn(q.logger).Log("msg", "download queue is full, dropping item", "key", t.key)
		return
	}
}

func (q *downloadQueue[T, R]) runWorker() {
	defer q.wg.Done()
	for {
		select {
		case <-q.done:
			return
		case task := <-q.queue:
			q.do(task.ctx, task)
		}
	}
}

func (q *downloadQueue[T, R]) do(ctx context.Context, task downloadRequest[T, R]) {
	if ctx.Err() != nil {
		task.errors <- ctx.Err()
		return
	}
	q.mu.LockKey(task.key)
	defer func() {
		err := q.mu.UnlockKey(task.key)
		if err != nil {
			level.Error(q.logger).Log("msg", "failed to unlock key in block lock", "key", task.key, "err", err)
		}
		if task.async {
			q.enqueuedMutex.Lock()
			_ = q.enqueued.Delete(task.key)
			q.enqueuedMutex.Unlock()
		}
	}()

	q.process(ctx, task)
}

func (q *downloadQueue[T, R]) close() {
	close(q.done)
	q.wg.Wait()
}
