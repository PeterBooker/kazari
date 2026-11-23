package site

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/sync/singleflight"

	"github.com/PeterBooker/kazari/internal/blob"
	"github.com/PeterBooker/kazari/internal/instrument"
	"github.com/PeterBooker/kazari/internal/lru"
)

// DefaultCompressionCache is the default Options.CompressionCache.
const DefaultCompressionCache = 64 << 20

// A coding is a content coding the site sends. codings lists them in the
// order the site prefers them among those a client weighs the same.
type coding struct {
	name   string
	encode func(src []byte) ([]byte, error)
}

var codings = []*coding{
	{name: "gzip", encode: encodeGzip},
}

// gzipLevel is the default ratio: a file is compressed once while it stays
// cached, not on every request.
const gzipLevel = gzip.DefaultCompression

var gzipWriters sync.Pool

func encodeGzip(src []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(len(src) / 3)
	gz, _ := gzipWriters.Get().(*gzip.Writer)
	if gz == nil {
		// The level is a valid constant, so this cannot fail.
		gz, _ = gzip.NewWriterLevel(&buf, gzipLevel)
	} else {
		gz.Reset(&buf)
	}
	defer func() {
		gz.Reset(io.Discard)
		gzipWriters.Put(gz)
	}()
	if _, err := gz.Write(src); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// errBusy is a compression refused because every slot is taken.
var errBusy = errors.New("site: every compression slot is taken")

type encodedKey struct {
	hash   string
	coding string
}

// entryOverhead estimates what an entry holds beside its bytes: the key and
// the cache's bookkeeping.
const entryOverhead = 192

// compressor keeps files' encoded representations in memory. A miss is
// compressed once however many requests ask for it together, by one of a
// fixed number of slots; when all are taken a request does not wait.
type compressor struct {
	blobs  blob.Store
	cache  *lru.Cache[encodedKey, []byte]
	flight singleflight.Group
	slots  chan struct{}

	lookups      metric.Int64Counter
	hit, miss    metric.AddOption
	evictions    metric.Int64Counter
	compressions metric.Int64Counter
	byCoding     map[string]metric.AddOption
	busy         metric.Int64Counter
}

// Attribute keys of the compression metrics.
const (
	cacheResultKey = attribute.Key("kazari.compression.cache.result")
	codingKey      = attribute.Key("kazari.compression.coding")
)

func newCompressor(blobs blob.Store, budget int64, slots int, tel instrument.Providers) *compressor {
	if slots <= 0 {
		slots = runtime.GOMAXPROCS(0)
	}
	m := tel.Meter(scope)
	c := &compressor{
		blobs: blobs,
		cache: lru.New[encodedKey, []byte](0, budget),
		slots: make(chan struct{}, slots),
		lookups: instrument.Handle(m.Int64Counter("kazari.site.compression.cache.lookups",
			metric.WithDescription("Lookups of a file's encoded representation in memory, by whether it was there."),
			metric.WithUnit("{lookup}"))),
		hit:  metric.WithAttributes(cacheResultKey.String("hit")),
		miss: metric.WithAttributes(cacheResultKey.String("miss")),
		evictions: instrument.Handle(m.Int64Counter("kazari.site.compression.cache.evictions",
			metric.WithDescription("Encoded representations pushed out of memory to keep within its budget."),
			metric.WithUnit("{entry}"))),
		compressions: instrument.Handle(m.Int64Counter("kazari.site.compressions",
			metric.WithDescription("Files compressed, by content coding."),
			metric.WithUnit("{compression}"))),
		byCoding: map[string]metric.AddOption{},
		busy: instrument.Handle(m.Int64Counter("kazari.site.compression.busy",
			metric.WithDescription("Responses sent as stored because every compression slot was taken."),
			metric.WithUnit("{response}"))),
	}
	for _, cd := range codings {
		c.byCoding[cd.name] = metric.WithAttributes(codingKey.String(cd.name))
	}
	usage := instrument.Handle(m.Int64ObservableUpDownCounter("kazari.site.compression.cache.usage",
		metric.WithDescription("Bytes of encoded representations held in memory."), metric.WithUnit("By")))
	_ = instrument.Handle(m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		_, n := c.cache.Size()
		o.ObserveInt64(usage, n)
		return nil
	}, usage))
	return c
}

func (c *compressor) cached(hash string) func(*coding) bool {
	return func(cd *coding) bool { return c.cache.Contains(encodedKey{hash, cd.name}) }
}

// encoded returns the blob in cd, compressing it on a miss. A compression
// runs to the end even if the request that started it goes away, since
// the requests that joined it, and the next ones, want it too. errBusy
// means no slot was free.
func (c *compressor) encoded(ctx context.Context, hash string, cd *coding) ([]byte, error) {
	key := encodedKey{hash, cd.name}
	if b, ok := c.cache.Get(key); ok {
		c.lookups.Add(ctx, 1, c.hit)
		return b, nil
	}
	c.lookups.Add(ctx, 1, c.miss)
	v, err, _ := c.flight.Do(hash+" "+cd.name, func() (any, error) {
		// A compression that ended just before this one began cached it.
		if b, ok := c.cache.Get(key); ok {
			return b, nil
		}
		select {
		case c.slots <- struct{}{}:
		default:
			return nil, errBusy
		}
		defer func() { <-c.slots }()
		b, err := c.compress(context.WithoutCancel(ctx), hash, cd)
		if err != nil {
			return nil, err
		}
		c.compressions.Add(ctx, 1, c.byCoding[cd.name])
		if n := c.cache.Add(key, b, int64(len(b))+entryOverhead); n > 0 {
			c.evictions.Add(ctx, int64(n))
		}
		return b, nil
	})
	if err != nil {
		return nil, err
	}
	b, _ := v.([]byte)
	return b, nil
}

// anyCached returns the first coding acc accepts that is cached for the
// blob, with its bytes; nil when there is none.
func (c *compressor) anyCached(ctx context.Context, acc acceptance, hash string) (*coding, []byte) {
	for _, cd := range codings {
		if acc.of(cd.name) <= 0 {
			continue
		}
		if b, ok := c.cache.Get(encodedKey{hash, cd.name}); ok {
			c.lookups.Add(ctx, 1, c.hit)
			return cd, b
		}
	}
	return nil, nil
}

func (c *compressor) compress(ctx context.Context, hash string, cd *coding) ([]byte, error) {
	rc, err := c.blobs.Open(ctx, hash)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	src, err := io.ReadAll(io.LimitReader(rc, maxCompressSize+1))
	if err != nil {
		return nil, err
	}
	if len(src) > maxCompressSize {
		return nil, fmt.Errorf("site: blob %s is over %d bytes", hash, maxCompressSize)
	}
	return cd.encode(src)
}
