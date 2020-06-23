package gohalt

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

type Throttler interface {
	Acquire(context.Context) error
	Release(context.Context) error
}

type NewThrottler func() Throttler

type techo struct {
	err error
}

func NewThrottlerEcho(err error) techo {
	return techo{err: err}
}

func (thr techo) Acquire(context.Context) error {
	return thr.err
}

func (thr techo) Release(context.Context) error {
	return thr.err
}

type twait struct {
	dur time.Duration
}

func NewThrottlerWait(duration time.Duration) twait {
	return twait{dur: duration}
}

func (thr twait) Acquire(context.Context) error {
	time.Sleep(thr.dur)
	return nil
}

func (thr twait) Release(context.Context) error {
	return nil
}

type tpanic struct{}

func NewThrottlerPanic() tpanic {
	return tpanic{}
}

func (thr tpanic) Acquire(context.Context) error {
	log.Fatal("throttler panic has happened")
	return nil
}

func (thr tpanic) Release(context.Context) error {
	return nil
}

type teach struct {
	cur uint64
	num uint64
}

func NewThrottlerEach(num uint64) *teach {
	return &teach{num: num}
}

func (thr *teach) Acquire(context.Context) error {
	atomic.AddUint64(&thr.cur, 1)
	if cur := atomic.LoadUint64(&thr.cur); cur%thr.num == 0 {
		return fmt.Errorf("throttler has reached periodic skip %d", cur)
	}
	return nil
}

func (thr *teach) Release(context.Context) error {
	return nil
}

type tafter struct {
	cur uint64
	num uint64
}

func NewThrottlerAfter(num uint64) *tafter {
	return &tafter{num: num}
}

func (thr *tafter) Acquire(context.Context) error {
	atomic.AddUint64(&thr.cur, 1)
	if cur := atomic.LoadUint64(&thr.cur); cur < thr.num {
		return fmt.Errorf("throttler has not reached pass yet %d", cur)
	}
	return nil
}

func (thr *tafter) Release(context.Context) error {
	return nil
}

type tfixed struct {
	cur uint64
	max uint64
}

func NewThrottlerFixed(max uint64) *tfixed {
	return &tfixed{max: max}
}

func (thr *tfixed) Acquire(context.Context) error {
	if cur := atomic.LoadUint64(&thr.cur); cur > thr.max {
		return fmt.Errorf("throttler has exceed fixed limit %d", cur)
	}
	atomic.AddUint64(&thr.cur, 1)
	return nil
}

func (thr *tfixed) Release(context.Context) error {
	return nil
}

type tatomic struct {
	run uint64
	max uint64
}

func NewThrottlerAtomic(max uint64) *tatomic {
	return &tatomic{max: max}
}

func (thr *tatomic) Acquire(context.Context) error {
	if run := atomic.LoadUint64(&thr.run); run > thr.max {
		return fmt.Errorf("throttler has exceed running limit %d", run)
	}
	atomic.AddUint64(&thr.run, 1)
	return nil
}

func (thr *tatomic) Release(context.Context) error {
	if run := atomic.LoadUint64(&thr.run); run <= 0 {
		return errors.New("throttler has nothing to release")
	}
	atomic.AddUint64(&thr.run, ^uint64(0))
	return nil
}

type tbuffered struct {
	run chan struct{}
}

func NewThrottlerBlocking(size uint64) *tbuffered {
	return &tbuffered{run: make(chan struct{}, size)}
}

func (thr *tbuffered) Acquire(context.Context) error {
	thr.run <- struct{}{}
	return nil
}

func (thr *tbuffered) Release(ctx context.Context) error {
	select {
	case <-thr.run:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("throttler context error has occured %w", ctx.Err())
	default:
		return errors.New("throttler has nothing to release")
	}
}

type tpriority struct {
	run map[uint8]chan struct{}
	lim uint8
	mut sync.Mutex
}

func NewThrottlerBlockingPriority(size uint64, priority uint8) *tpriority {
	if priority == 0 {
		priority = 1
	}
	thr := tpriority{lim: priority}
	sum := float64(priority) / 2 * float64((2 + (priority - 1)))
	koef := uint64(math.Ceil(float64(size) / sum))
	for i := uint8(1); i <= priority; i++ {
		thr.run[i] = make(chan struct{}, uint64(i)*koef)
	}
	return &thr
}

func (thr *tpriority) Acquire(ctx context.Context) error {
	thr.mut.Lock()
	run := thr.run[ctxPriority(ctx, thr.lim)]
	thr.mut.Unlock()
	run <- struct{}{}
	return nil
}

func (thr *tpriority) Release(ctx context.Context) error {
	thr.mut.Lock()
	run := thr.run[ctxPriority(ctx, thr.lim)]
	thr.mut.Unlock()
	select {
	case <-run:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("throttler context error has occured %w", ctx.Err())
	default:
		return errors.New("throttler has nothing to release")
	}
}

type ttimed struct {
	*tfixed
	wnd time.Duration
	sld uint64
}

func NewThrottlerTimed(ctx context.Context, max uint64, window time.Duration, slide uint64) ttimed {
	thr := NewThrottlerFixed(max)
	delta, interval := max, window
	if slide > 0 && window > time.Duration(slide) {
		delta = uint64(math.Ceil(float64(delta) / float64(slide)))
		interval /= time.Duration(slide)
	}
	loop(ctx, interval, func(ctx context.Context) error {
		atomic.AddUint64(&thr.cur, ^uint64(delta-1))
		if cur := atomic.LoadUint64(&thr.cur); cur < 0 {
			atomic.StoreUint64(&thr.cur, 0)
		}
		return ctx.Err()
	})
	return ttimed{tfixed: thr, wnd: window, sld: slide}
}

func (thr ttimed) Acquire(ctx context.Context) error {
	return thr.tfixed.Acquire(ctx)
}

func (thr ttimed) Release(ctx context.Context) error {
	return thr.tfixed.Release(ctx)
}

type tstats struct {
	stats    Stats
	alloc    uint64
	system   uint64
	avgpause uint64
	avgusage float64
}

func NewThrottlerStats(stats Stats, alloc uint64, system uint64, avgpause uint64, avgusage float64) tstats {
	return tstats{
		stats:    stats,
		alloc:    alloc,
		system:   system,
		avgpause: avgpause,
		avgusage: avgusage,
	}
}

func (thr tstats) Acquire(context.Context) error {
	alloc, system, avgpause, usage := thr.stats.Stats()
	if alloc >= thr.alloc || system >= thr.system ||
		avgpause >= thr.avgpause || usage >= thr.avgusage {
		return fmt.Errorf(
			`throttler has exceed stats limits
alloc %d mb, system %d mb, avg gc cpu pause %s, avg cpu usage %.2f%%`,
			alloc/1024,
			system/1024,
			time.Duration(avgpause),
			usage,
		)
	}
	return nil
}

func (thr tstats) Release(context.Context) error {
	return nil
}

type tmetric struct {
	metric Metric
}

func NewThrottlerMetric(metric Metric) tmetric {
	return tmetric{metric: metric}
}

func (thr tmetric) Acquire(ctx context.Context) error {
	val, err := thr.metric.QueryBinary(ctx)
	if err != nil {
		return fmt.Errorf("throttler error has occured %w", err)
	}
	if val {
		return errors.New("throttler metric has been reached")
	}
	return nil
}

func (thr tmetric) Release(context.Context) error {
	return nil
}

type tchance struct {
	pos float64
}

func NewThrottlerChance(possibillity float64) tchance {
	possibillity = math.Abs(possibillity)
	if possibillity > 1.0 {
		possibillity = 1.0
	}
	return tchance{pos: possibillity}
}

func (thr tchance) Acquire(context.Context) error {
	if thr.pos > 1.0-rand.Float64() {
		return errors.New("throttler has missed a chance")
	}
	return nil
}

func (thr tchance) Release(context.Context) error {
	return nil
}

type tlatency struct {
	lat uint64
	max uint64
	ret time.Duration
}

func NewThrottlerLatency(max time.Duration, retention time.Duration) *tlatency {
	return &tlatency{max: uint64(max), ret: retention}
}

func (thr tlatency) Acquire(context.Context) error {
	if lat := atomic.LoadUint64(&thr.lat); lat > thr.max {
		return fmt.Errorf("throttler has exceed latency limit %s", time.Duration(lat))
	}
	return nil
}

func (thr *tlatency) Release(ctx context.Context) error {
	if lat := atomic.LoadUint64(&thr.lat); lat < thr.max {
		lat := uint64(ctxTimestamp(ctx) - time.Now().UTC().UnixNano())
		if lat > thr.lat {
			atomic.StoreUint64(&thr.lat, lat)
		}
		once(ctx, thr.ret, func(context.Context) error {
			atomic.StoreUint64(&thr.lat, 0)
			return nil
		})
	}
	return nil
}

type tpercentile struct {
	lat *latheap
	mut sync.Mutex
	max uint64
	pnt float64
	ret time.Duration
}

func NewThrottlerPercentile(max time.Duration, percentile float64, retention time.Duration) *tpercentile {
	percentile = math.Abs(percentile)
	if percentile > 1.0 {
		percentile = 1.0
	}
	return &tpercentile{
		lat: &latheap{},
		max: uint64(max),
		pnt: percentile,
		ret: retention,
	}
}

func (thr *tpercentile) Acquire(ctx context.Context) error {
	thr.mut.Lock()
	size := float64(thr.lat.Len())
	pos := int(math.Round(size * thr.pnt))
	lat := thr.lat.At(pos)
	thr.mut.Unlock()
	if lat > thr.max {
		once(ctx, thr.ret, func(context.Context) error {
			thr.mut.Lock()
			thr.lat = &latheap{}
			thr.mut.Unlock()
			return nil
		})
		return fmt.Errorf("throttler has exceed latency limit %s", time.Duration(lat))
	}
	return nil
}

func (thr *tpercentile) Release(ctx context.Context) error {
	lat := uint64(ctxTimestamp(ctx) - time.Now().UTC().UnixNano())
	thr.mut.Lock()
	heap.Push(thr.lat, lat)
	thr.mut.Unlock()
	return nil
}

type tadaptive struct {
	ttimed
	step uint64
	thr  Throttler
}

func NewThrottlerAdaptive(
	ctx context.Context,
	max uint64,
	window time.Duration,
	slide uint64,
	step uint64,
	thr Throttler,
) tadaptive {
	return tadaptive{
		ttimed: NewThrottlerTimed(ctx, max, window, slide),
		step:   step,
		thr:    thr,
	}
}

func (thr tadaptive) Acquire(ctx context.Context) error {
	err := thr.thr.Acquire(ctx)
	if err != nil {
		atomic.AddUint64(&thr.ttimed.max, ^uint64(thr.step*thr.step))
	} else {
		atomic.AddUint64(&thr.ttimed.max, thr.step)
	}

	return thr.ttimed.Acquire(ctx)
}

func (thr tadaptive) Release(ctx context.Context) error {
	return thr.ttimed.Release(ctx)
}

type tcontext struct{}

func NewThrottlerContext() tcontext {
	return tcontext{}
}

func (thr tcontext) Acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("throttler context error has occured %w", ctx.Err())
	default:
		return nil
	}
}

func (thr tcontext) Release(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("throttler context error has occured %w", ctx.Err())
	default:
		return nil
	}
}

type tkeyed struct {
	store  sync.Map
	newthr NewThrottler
}

func NewThrottlerKeyed(newthr NewThrottler) *tkeyed {
	return &tkeyed{newthr: newthr}
}

func (thr *tkeyed) Acquire(ctx context.Context) error {
	if key := ctxKey(ctx); key != nil {
		r, _ := thr.store.LoadOrStore(key, thr.newthr())
		return r.(Throttler).Acquire(ctx)
	}
	return errors.New("throttler can't find any key")
}

func (thr *tkeyed) Release(ctx context.Context) error {
	if key := ctxKey(ctx); key != nil {
		if r, ok := thr.store.Load(key); ok {
			return r.(Throttler).Release(ctx)
		}
		return errors.New("throttler has nothing to release")
	}
	return errors.New("throttler can't find any key")
}

type tenqueue struct {
	enq Enqueuer
}

func NewThrottlerEnqueue(enqueuer Enqueuer) tenqueue {
	return tenqueue{enq: enqueuer}
}

func (thr tenqueue) Acquire(ctx context.Context) error {
	if marshaler, data := ctxMarshaler(ctx), ctxData(ctx); marshaler != nil && data != nil {
		message, err := marshaler(data)
		if err != nil {
			return fmt.Errorf("throttler can't enqueue %w", err)
		}
		if err := thr.enq.Publish(ctx, message); err != nil {
			return fmt.Errorf("throttler can't enqueue %w", err)
		}
		return nil
	}
	return errors.New("throttler can't find any data")
}

func (thr tenqueue) Release(ctx context.Context) error {
	return nil
}

type tall []Throttler

func NewThrottlerAll(thrs []Throttler) tall {
	return tall(thrs)
}

func (thrs tall) Acquire(ctx context.Context) error {
	err := errors.New("throttler error has happened")
	for _, thr := range thrs {
		if threrr := thr.Acquire(ctx); threrr != nil {
			err = fmt.Errorf("%w %w", err, threrr)
			continue
		}
		return nil
	}
	return err
}

func (thrs tall) Release(ctx context.Context) error {
	err := errors.New("throttler error has happened")
	for _, thr := range thrs {
		if threrr := thr.Release(ctx); threrr != nil {
			err = fmt.Errorf("%w %w", err, threrr)
			continue
		}
		return nil
	}
	return err
}

type tany []Throttler

func NewThrottlerAny(thrs []Throttler) tany {
	return tany(thrs)
}

func (thrs tany) Acquire(ctx context.Context) error {
	var wg sync.WaitGroup
	errs := make(chan error)
	for _, thr := range thrs {
		wg.Add(1)
		go func(thr Throttler) {
			if err := thr.Acquire(ctx); err != nil {
				errs <- err
			}
			wg.Done()
		}(thr)
	}
	err := errors.New("throttler error has happened")
	go func() {
		for threrr := range errs {
			err = fmt.Errorf("%w\n%w", err, threrr)
		}
	}()
	wg.Wait()
	close(errs)
	return err
}

func (thrs tany) Release(ctx context.Context) error {
	var wg sync.WaitGroup
	errs := make(chan error)
	for _, thr := range thrs {
		wg.Add(1)
		go func(thr Throttler) {
			if err := thr.Release(ctx); err != nil {
				errs <- err
			}
			wg.Done()
		}(thr)
	}
	err := errors.New("throttler error has happened")
	go func() {
		for threrr := range errs {
			err = fmt.Errorf("%w\n%w", err, threrr)
		}
	}()
	wg.Wait()
	close(errs)
	return err
}

type trevert struct {
	thr Throttler
}

func NewThrottlerRevert(thr Throttler) trevert {
	return trevert{thr: thr}
}

func (thr trevert) Acquire(ctx context.Context) error {
	if err := thr.thr.Acquire(ctx); err != nil {
		return nil
	}
	return errors.New("throttler revert error has happened")
}

func (thr trevert) Release(ctx context.Context) error {
	if err := thr.thr.Release(ctx); err != nil {
		return nil
	}
	return errors.New("throttler revert error has happened")
}