package rawblock

import (
	"errors"
	"log"
	"sync"
	"time"

	"github.com/apple/foundationdb/bindings/go/src/fdb"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
)

const (
	// Page size.
	defaultBatchSize       = 4096
	defaultMaxPendingBytes = 10000000
	defaultFlushEvery      = time.Millisecond

	commitLogKey = "commitlog-"
)

type clStatus int

const (
	clStatusUnopened clStatus = iota
	clStatusOpen
	clStatusClosed
)

// truncationToken is a token that can be passed to the commitlog to truncate the commitlogs up to
// a specific point. It should be treated as opaque by external callers.
type truncationToken struct {
	upTo tuple.Tuple
}

type Commitlog interface {
	Write([]byte) error
	Open() error
	Close() error
}

type CommitlogOptions struct {
	IdealBatchSize  int
	MaxPendingBytes int
	FlushEvery      time.Duration
}

func NewCommitlogOptions() CommitlogOptions {
	return CommitlogOptions{
		IdealBatchSize:  defaultBatchSize,
		MaxPendingBytes: defaultMaxPendingBytes,
		FlushEvery:      defaultFlushEvery,
	}
}

type flushOutcome struct {
	lastID tuple.Tuple
	err    error
	doneCh chan struct{}
}

func newFlushOutcome() *flushOutcome {
	return &flushOutcome{
		doneCh: make(chan struct{}, 0),
	}
}

func (f *flushOutcome) waitForFlush() error {
	<-f.doneCh
	return f.err
}

func (f *flushOutcome) notify(lastID tuple.Tuple, err error) {
	f.lastID = lastID
	f.err = err
	close(f.doneCh)
}

type commitlog struct {
	sync.Mutex
	status        clStatus
	db            fdb.Database
	prevBatch     []byte
	currBatch     []byte
	lastFlushTime time.Time
	flushOutcome  *flushOutcome
	closeCh       chan struct{}
	closeDoneCh   chan error
	opts          CommitlogOptions
}

func NewCommitlog(db fdb.Database, opts CommitlogOptions) Commitlog {
	return &commitlog{
		status:       clStatusUnopened,
		db:           db,
		flushOutcome: newFlushOutcome(),
		closeCh:      make(chan struct{}, 1),
		closeDoneCh:  make(chan error, 1),
		opts:         opts,
	}
}

func (c *commitlog) Open() error {
	c.Lock()
	if c.status != clStatusUnopened {
		c.Unlock()
		return errors.New("commitlog cannot be opened more than once")
	}
	c.status = clStatusOpen
	c.Unlock()

	go func() {
		for {
			i := 0
			select {
			case <-c.closeCh:
				c.closeDoneCh <- c.flush()
				return
			default:
			}
			time.Sleep(time.Millisecond)
			if i%10 == 0 {
				// TODO(rartoul): Remove this.
				// Truncate regularly to measure performance impact.
				if err := c.Truncate(); err != nil {
					log.Printf("error truncating commitlog: %v", err)
				}
			}
			if err := c.flush(); err != nil {
				log.Printf("error flushing commitlog: %v", err)
			}
			i++
		}
	}()

	return nil
}

func (c *commitlog) Close() error {
	c.Lock()
	if c.status != clStatusOpen {
		c.Unlock()
		return errors.New("cannot close commit log that is not open")
	}
	c.status = clStatusClosed
	c.Unlock()

	c.closeCh <- struct{}{}
	return <-c.closeDoneCh
}

// TODO(rartoul): Kind of gross that this just takes a []byte but more
// flexible for now.
func (c *commitlog) Write(b []byte) error {
	if len(b) == 0 {
		return errors.New("commit log can not write empty chunk")
	}

	c.Lock()
	if c.status != clStatusOpen {
		c.Unlock()
		return errors.New("cannot write into commit log that is not open")
	}

	if len(c.currBatch)+len(b) > c.opts.MaxPendingBytes {
		c.Unlock()
		return errors.New("commit log queue is full")
	}

	c.currBatch = append(c.currBatch, b...)
	currFlushOutcome := c.flushOutcome
	c.Unlock()
	return currFlushOutcome.waitForFlush()
}

func (c *commitlog) Truncate(token truncationToken) error {
	_, err := c.db.Transact(func(tr fdb.Transaction) (interface{}, error) {
		tr.ClearRange(fdb.KeyRange{Begin: tuple.Tuple{commitLogKey}, End: token.upTo})
		return nil, nil
	})

	return err
}

func (c *commitlog) flush() error {
	c.Lock()
	if !(time.Since(c.lastFlushTime) >= c.opts.FlushEvery && len(c.currBatch) > 0) {
		c.Unlock()
		return nil
	}

	toWrite := c.currBatch
	c.currBatch, c.prevBatch = c.prevBatch, c.currBatch
	c.currBatch = c.currBatch[:0]
	currFlushOutcome := c.flushOutcome
	c.flushOutcome = newFlushOutcome()
	c.Unlock()

	key, err := c.db.Transact(func(tr fdb.Transaction) (interface{}, error) {
		// TODO(rartoul): Need to be smarter about this because don't want to actually
		// break chunks across writes I.E every call to WriteBatch() should end up
		// in one key so that each key is a complete unit.
		var (
			startIdx = 0
			key      tuple.Tuple
		)
		for startIdx < len(toWrite) {
			key := c.nextKey()
			endIdx := startIdx + c.opts.IdealBatchSize
			if endIdx > len(toWrite) {
				endIdx = len(toWrite)
			}
			tr.Set(key, toWrite[startIdx:endIdx])
			startIdx = endIdx
		}

		return key, nil
	})
	currFlushOutcome.notify(key.(tuple.Tuple), err)
	return err
}

func (c *commitlog) nextKey() tuple.Tuple {
	// TODO(rartoul): This should have some kind of host identifier in it.
	return tuple.Tuple{commitLogKey, time.Now().UnixNano()}
}
