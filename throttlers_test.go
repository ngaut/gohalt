package gohalt

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	ms0_0  time.Duration = 0
	ms0_9  time.Duration = time.Duration(0.9 * float64(1*time.Millisecond))
	ms1_0  time.Duration = 1 * time.Millisecond
	ms2_0  time.Duration = 2 * time.Millisecond
	ms3_0  time.Duration = 3 * time.Millisecond
	ms4_0  time.Duration = 4 * time.Millisecond
	ms5_0  time.Duration = 5 * time.Millisecond
	ms7_0  time.Duration = 7 * time.Millisecond
	ms8_0  time.Duration = 8 * time.Millisecond
	ms9_0  time.Duration = 9 * time.Millisecond
	ms10_0 time.Duration = 10 * time.Millisecond
	ms30_0 time.Duration = 30 * time.Millisecond
)

var trun Runner = NewRunnerSync(context.Background(), NewThrottlerBuffered(1))

type tcase struct {
	tms  uint64            // number of sub runs inside one case
	thr  Throttler         // throttler itself
	acts []Runnable        // actions that need to be throttled
	ins  []Runnable        // actions that need to be run inside throttle
	pres []Runnable        // actions that neeed to be run before throttle
	tss  []time.Duration   // timestamps that needs to be applied to contexts set
	ctxs []context.Context // contexts set for throttling
	errs []error           // expected throttler errors
	durs []time.Duration   // expected throttler durations
	idx  uint64            // carries seq number of sub run execution
	over bool              // if throttler needs to be over released
	pass bool              // if throttler doesn't need to be released
}

func (t *tcase) run(index int) (dur time.Duration, err error) {
	// get context with fallback
	ctx := context.Background()
	if index < len(t.ctxs) {
		ctx = t.ctxs[index]
	}
	// run additional pre action only if present
	if index < len(t.pres) {
		if pre := t.pres[index]; pre != nil {
			_ = pre(ctx)
		}
	}
	var ts time.Time
	// try catch panic into error
	func() {
		defer atomicIncr(&t.idx)
		defer func() {
			if msg := recover(); msg != nil {
				err = errors.New(msg.(string))
			}
		}()
		ts = time.Now()
		// force strict acquire order
		for index != int(atomicGet(&t.idx)) {
			sleep(ctx, time.Microsecond)
		}
		// set additional timestamp only if present
		if index < len(t.tss) {
			ctx = WithTimestamp(ctx, time.Now().Add(t.tss[index]))
		}
		err = t.thr.Acquire(ctx)
		// run additional in action only if present
		if index < len(t.ins) {
			if in := t.ins[index]; in != nil {
				_ = in(ctx)
			}
		}
	}()
	dur = time.Since(ts)
	// run additional action only if present
	if index < len(t.acts) {
		if act := t.acts[index]; act != nil {
			_ = act(ctx)
		}
	}
	limit := 1
	if t.over && uint64(index+1) == t.tms { // imitate over releasing on last call
		limit = index + 1
	}
	if t.pass {
		limit = 0
	}
	for i := 0; i < limit; i++ {
		if err := t.thr.Release(ctx); err != nil {
			return dur, err
		}
	}
	return
}

func (t *tcase) result(index int) (dur time.Duration, err error) {
	if index < len(t.errs) {
		err = t.errs[index]
	}
	if index < len(t.durs) {
		dur = t.durs[index]
	}
	return
}

func TestThrottlers(t *testing.T) {
	DefaultRetriedDuration = time.Millisecond
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	table := map[string]tcase{
		"Throttler echo should not throttle on nil input": {
			tms: 3,
			thr: NewThrottlerEcho(nil),
		},
		"Throttler echo should throttle on not nil input": {
			tms: 3,
			thr: NewThrottlerEcho(errors.New("test")),
			errs: []error{
				errors.New("test"),
				errors.New("test"),
				errors.New("test"),
			},
		},
		"Throttler wait should sleep for millisecond": {
			tms: 3,
			thr: NewThrottlerWait(ms1_0),
			durs: []time.Duration{
				ms0_9,
				ms0_9,
				ms0_9,
			},
		},
		"Throttler square should sleep for correct time periods": {
			tms: 3,
			thr: NewThrottlerSquare(ms1_0, 0, false),
			durs: []time.Duration{
				ms0_9,
				ms0_9,
				ms0_9,
			},
		},
		"Throttler square should sleep for correct time periods with reset": {
			tms: 5,
			thr: NewThrottlerSquare(ms1_0, 20*ms1_0, true),
			acts: []Runnable{
				delayed(ms30_0, nope),
				delayed(ms30_0, nope),
				delayed(ms30_0, nope),
				delayed(ms30_0, nope),
				delayed(ms30_0, nope),
			},
			durs: []time.Duration{
				ms0_9,
				ms0_9 * 4,
				ms0_9 * 9,
				ms0_9 * 16,
				ms0_9,
			},
		},
		"Throttler jitter should sleep for correct time periods": {
			tms: 3,
			thr: NewThrottlerJitter(ms1_0, 0, false, 0.0),
			durs: []time.Duration{
				ms0_9,
				ms0_9,
				ms0_9,
			},
		},
		"Throttler jitter should sleep for random time periods": {
			tms: 3,
			thr: NewThrottlerJitter(ms1_0, 0, false, 1.9),
			durs: []time.Duration{
				ms0_0,
				ms0_0,
				ms0_0,
			},
			pass: true,
		},
		"Throttler context should throttle on canceled context": {
			tms: 3,
			thr: NewThrottlerContext(),
			ctxs: []context.Context{
				cctx,
				context.Background(),
				cctx,
			},
			errs: []error{
				fmt.Errorf("throttler has received context error %w", cctx.Err()),
				nil,
				fmt.Errorf("throttler has received context error %w", cctx.Err()),
			},
		},
		"Throttler panic should panic": {
			tms: 3,
			thr: NewThrottlerPanic(),
			errs: []error{
				errors.New("throttler has reached panic"),
				errors.New("throttler has reached panic"),
				errors.New("throttler has reached panic"),
			},
		},
		"Throttler each should throttle on threshold": {
			tms: 6,
			thr: NewThrottlerEach(3),
			errs: []error{
				nil,
				nil,
				errors.New("throttler has reached periodic threshold"),
				nil,
				nil,
				errors.New("throttler has reached periodic threshold"),
			},
		},
		"Throttler before should throttle before threshold": {
			tms: 6,
			thr: NewThrottlerBefore(3),
			errs: []error{
				errors.New("throttler has not reached threshold yet"),
				errors.New("throttler has not reached threshold yet"),
				errors.New("throttler has not reached threshold yet"),
				nil,
				nil,
				nil,
			},
		},
		"Throttler after should throttle after threshold": {
			tms: 6,
			thr: NewThrottlerAfter(3),
			errs: []error{
				nil,
				nil,
				nil,
				errors.New("throttler has exceed threshold"),
				errors.New("throttler has exceed threshold"),
				errors.New("throttler has exceed threshold"),
			},
		},
		"Throttler chance should throttle on 1": {
			tms: 3,
			thr: NewThrottlerChance(1),
			errs: []error{
				errors.New("throttler has reached chance threshold"),
				errors.New("throttler has reached chance threshold"),
				errors.New("throttler has reached chance threshold"),
			},
		},
		"Throttler chance should throttle on >1": {
			tms: 3,
			thr: NewThrottlerChance(10.10),
			errs: []error{
				errors.New("throttler has reached chance threshold"),
				errors.New("throttler has reached chance threshold"),
				errors.New("throttler has reached chance threshold"),
			},
		},
		"Throttler chance should not throttle on 0": {
			tms: 3,
			thr: NewThrottlerChance(0),
		},
		"Throttler running should throttle on threshold": {
			tms: 3,
			thr: NewThrottlerRunning(1),
			acts: []Runnable{
				delayed(ms1_0, nope),
				delayed(ms1_0, nope),
				delayed(ms1_0, nope),
			},
			errs: []error{
				nil,
				errors.New("throttler has exceed running threshold"),
				errors.New("throttler has exceed running threshold"),
			},
			over: true,
		},
		"Throttler buffered should throttle on threshold": {
			tms: 3,
			thr: NewThrottlerBuffered(1),
			acts: []Runnable{
				delayed(ms1_0, nope),
				delayed(ms1_0, nope),
				delayed(ms1_0, nope),
			},
			durs: []time.Duration{
				0,
				ms0_9,
				ms0_9,
			},
			over: true,
		},
		"Throttler priority should throttle on threshold": {
			tms: 3,
			thr: NewThrottlerPriority(1, 0),
			acts: []Runnable{
				delayed(ms1_0, nope),
				delayed(ms1_0, nope),
				delayed(ms1_0, nope),
			},
			durs: []time.Duration{
				0,
				ms0_9,
				ms0_9,
			},
			over: true,
		},
		"Throttler priority should not throttle on priority": {
			tms: 7,
			thr: NewThrottlerPriority(5, 2),
			acts: []Runnable{
				delayed(ms3_0, nope),
				delayed(ms3_0, nope),
				delayed(ms3_0, nope),
				delayed(ms3_0, nope),
				delayed(ms3_0, nope),
				delayed(ms3_0, nope),
				delayed(ms3_0, nope),
			},
			ctxs: []context.Context{
				WithPriority(context.Background(), 1),
				WithPriority(context.Background(), 1),
				WithPriority(context.Background(), 1),
				WithPriority(context.Background(), 2),
				WithPriority(context.Background(), 2),
				WithPriority(context.Background(), 2),
				WithPriority(context.Background(), 2),
			},
			durs: []time.Duration{
				0,
				0,
				ms2_0,
				0,
				0,
				0,
				ms2_0,
			},
		},
		"Throttler timed should throttle after threshold": {
			tms: 6,
			thr: NewThrottlerTimed(
				2,
				ms2_0,
				ms0_0,
			),
			pres: []Runnable{
				nil,
				nil,
				nil,
				nil,
				delayed(ms3_0, nope),
				delayed(ms3_0, nope),
			},
			errs: []error{
				nil,
				nil,
				errors.New("throttler has exceed threshold"),
				errors.New("throttler has exceed threshold"),
				nil,
				nil,
			},
		},
		"Throttler timed should throttle after threshold with quantum": {
			tms: 6,
			thr: NewThrottlerTimed(
				2,
				ms8_0,
				ms4_0,
			),
			pres: []Runnable{
				nil,
				nil,
				nil,
				delayed(ms5_0, nope),
				nil,
				delayed(ms10_0, nope),
			},
			errs: []error{
				nil,
				nil,
				errors.New("throttler has exceed threshold"),
				nil,
				errors.New("throttler has exceed threshold"),
				nil,
			},
		},
		"Throttler latency should throttle on latency above threshold": {
			tms: 3,
			thr: NewThrottlerLatency(ms0_9, ms5_0),
			tss: []time.Duration{
				-ms5_0,
				ms0_0,
				ms0_0,
			},
			errs: []error{
				nil,
				errors.New("throttler has exceed latency threshold"),
				errors.New("throttler has exceed latency threshold"),
			},
		},
		"Throttler latency should not throttle on latency above threshold after retention": {
			tms: 3,
			thr: NewThrottlerLatency(ms0_9, ms3_0),
			tss: []time.Duration{
				-ms5_0,
				ms0_0,
				ms0_0,
			},
			pres: []Runnable{
				nil,
				nil,
				delayed(ms9_0, nope),
			},
			errs: []error{
				nil,
				errors.New("throttler has exceed latency threshold"),
				nil,
			},
		},
		"Throttler percentile should throttle on latency above threshold": {
			tms: 5,
			thr: NewThrottlerPercentile(ms3_0, 10, 0.5, ms7_0),
			tss: []time.Duration{
				ms0_0,
				-ms5_0,
				-ms5_0,
				-ms1_0,
			},
			errs: []error{
				nil,
				nil,
				errors.New("throttler has exceed latency threshold"),
				errors.New("throttler has exceed latency threshold"),
				errors.New("throttler has exceed latency threshold"),
			},
		},
		"Throttler percentile should throttle on latency above threshold after retention": {
			tms: 5,
			thr: NewThrottlerPercentile(ms3_0, 10, 1.5, ms5_0),
			tss: []time.Duration{
				ms0_0,
				-ms5_0,
				-ms5_0,
				-ms1_0,
			},
			pres: []Runnable{
				nil,
				nil,
				nil,
				nil,
				delayed(ms7_0, nope),
			},
			errs: []error{
				nil,
				nil,
				errors.New("throttler has exceed latency threshold"),
				errors.New("throttler has exceed latency threshold"),
				nil,
			},
		},
		"Throttler percentile should throttle on latency above threshold with capacity": {
			tms: 5,
			thr: NewThrottlerPercentile(ms3_0, 1, 0.5, ms7_0),
			tss: []time.Duration{
				ms0_0,
				-ms5_0,
				-ms1_0,
				-ms5_0,
			},
			errs: []error{
				nil,
				nil,
				errors.New("throttler has exceed latency threshold"),
				nil,
				errors.New("throttler has exceed latency threshold"),
			},
		},
		"Throttler monitor should throttle on internal stats error": {
			tms: 3,
			thr: NewThrottlerMonitor(
				mntmock{err: errors.New("test")},
				Stats{},
			),
			errs: []error{
				fmt.Errorf("throttler hasn't found any stats %w", errors.New("test")),
				fmt.Errorf("throttler hasn't found any stats %w", errors.New("test")),
				fmt.Errorf("throttler hasn't found any stats %w", errors.New("test")),
			},
		},
		"Throttler monitor should not throttle on stats below threshold": {
			tms: 3,
			thr: NewThrottlerMonitor(
				mntmock{
					stats: Stats{
						MEMAlloc:  100,
						MEMSystem: 1000,
						CPUPause:  100,
						CPUUsage:  0.1,
					},
				},
				Stats{
					MEMAlloc:  1000,
					MEMSystem: 2000,
					CPUPause:  500,
					CPUUsage:  0.3,
				},
			),
		},
		"Throttler monitor should not throttle on stats above threshold nil stats": {
			tms: 3,
			thr: NewThrottlerMonitor(
				mntmock{
					stats: Stats{
						MEMAlloc:  500,
						MEMSystem: 5000,
						CPUPause:  500,
						CPUUsage:  0.1,
					},
				},
				Stats{},
			),
		},
		"Throttler monitor should throttle on stats above threshold": {
			tms: 3,
			thr: NewThrottlerMonitor(
				mntmock{
					stats: Stats{
						MEMAlloc:  500,
						MEMSystem: 5000,
						CPUPause:  500,
						CPUUsage:  0.1,
					},
				},
				Stats{
					MEMAlloc:  1000,
					MEMSystem: 2000,
					CPUPause:  500,
					CPUUsage:  0.3,
				},
			),
			errs: []error{
				errors.New("throttler has exceed stats threshold"),
				errors.New("throttler has exceed stats threshold"),
				errors.New("throttler has exceed stats threshold"),
			},
		},
		"Throttler metric should throttle on internal metric error": {
			tms: 3,
			thr: NewThrottlerMetric(mtcmock{err: errors.New("test")}),
			errs: []error{
				fmt.Errorf("throttler hasn't found any metric %w", errors.New("test")),
				fmt.Errorf("throttler hasn't found any metric %w", errors.New("test")),
				fmt.Errorf("throttler hasn't found any metric %w", errors.New("test")),
			},
		},
		"Throttler metric should not throttle on metric below threshold": {
			tms: 3,
			thr: NewThrottlerMetric(mtcmock{metric: false}),
		},
		"Throttler metric should throttle on metric above threshold": {
			tms: 3,
			thr: NewThrottlerMetric(mtcmock{metric: true}),
			errs: []error{
				errors.New("throttler has reached metric threshold"),
				errors.New("throttler has reached metric threshold"),
				errors.New("throttler has reached metric threshold"),
			},
		},
		"Throttler enqueue should throttle on internal nil marshaler error": {
			tms: 3,
			thr: NewThrottlerEnqueue(enqmock{}),
			ctxs: []context.Context{
				WithMarshaler(context.Background(), nil),
				WithMarshaler(context.Background(), nil),
				WithMarshaler(context.Background(), nil),
			},
			errs: []error{
				errors.New("throttler hasn't found any marshaler"),
				errors.New("throttler hasn't found any marshaler"),
				errors.New("throttler hasn't found any marshaler"),
			},
		},
		"Throttler enqueue should throttle on internal message error": {
			tms: 3,
			thr: NewThrottlerEnqueue(enqmock{}),
			errs: []error{
				errors.New("throttler hasn't found any message"),
				errors.New("throttler hasn't found any message"),
				errors.New("throttler hasn't found any message"),
			},
		},
		"Throttler enqueue should throttle on internal marshaler error": {
			tms: 3,
			thr: NewThrottlerEnqueue(enqmock{}),
			ctxs: []context.Context{
				WithMarshaler(WithMessage(context.Background(), "test"), marshal(errors.New("test"))),
				WithMarshaler(WithMessage(context.Background(), "test"), marshal(errors.New("test"))),
				WithMarshaler(WithMessage(context.Background(), "test"), marshal(errors.New("test"))),
			},
			errs: []error{
				fmt.Errorf("throttler hasn't sent any message %w", errors.New("test")),
				fmt.Errorf("throttler hasn't sent any message %w", errors.New("test")),
				fmt.Errorf("throttler hasn't sent any message %w", errors.New("test")),
			},
		},
		"Throttler enqueue should throttle on internal enqueuer error": {
			tms: 3,
			thr: NewThrottlerEnqueue(enqmock{err: errors.New("test")}),
			ctxs: []context.Context{
				WithMessage(context.Background(), "test"),
				WithMessage(context.Background(), "test"),
				WithMessage(context.Background(), "test"),
			},
			errs: []error{
				fmt.Errorf("throttler hasn't sent any message %w", errors.New("test")),
				fmt.Errorf("throttler hasn't sent any message %w", errors.New("test")),
				fmt.Errorf("throttler hasn't sent any message %w", errors.New("test")),
			},
		},
		"Throttler enqueue should not throttle on enqueuer success": {
			tms: 3,
			thr: NewThrottlerEnqueue(enqmock{}),
			ctxs: []context.Context{
				WithMarshaler(WithMessage(context.Background(), "test"), marshal(nil)),
				WithMessage(context.Background(), "test"),
				WithMessage(context.Background(), "test"),
			},
		},
		"Throttler adaptive should throttle on throttling adoptee": {
			tms: 3,
			thr: NewThrottlerAdaptive(
				7,
				ms2_0,
				ms1_0,
				2,
				NewThrottlerEcho(errors.New("test")),
			),
			errs: []error{
				nil,
				errors.New("throttler has exceed threshold"),
				errors.New("throttler has exceed threshold"),
			},
		},
		"Throttler adaptive should not throttle on non throttling adoptee": {
			tms: 3,
			thr: NewThrottlerAdaptive(
				0,
				ms2_0,
				ms1_0,
				1,
				NewThrottlerEcho(nil),
			),
		},
		"Throttler pattern should throttle on internal key error": {
			tms: 3,
			thr: NewThrottlerPattern(),
			ctxs: []context.Context{
				context.Background(),
				WithKey(context.Background(), ""),
				WithKey(context.Background(), "test"),
			},
			errs: []error{
				errors.New("throttler hasn't found any key"),
				errors.New("throttler hasn't found any key"),
				errors.New("throttler hasn't found any key"),
			},
		},
		"Throttler pattern should throttle on matching throttler pattern": {
			tms: 5,
			thr: NewThrottlerPattern(
				Pattern{
					Pattern:   regexp.MustCompile("nontest"),
					Throttler: NewThrottlerEcho(nil),
				},
				Pattern{
					Pattern:   regexp.MustCompile("test"),
					Throttler: NewThrottlerEcho(errors.New("test")),
				},
			),
			ctxs: []context.Context{
				context.Background(),
				WithKey(context.Background(), "125"),
				WithKey(context.Background(), "test"),
				WithKey(context.Background(), "nontest"),
				WithKey(context.Background(), "non"),
			},
			errs: []error{
				errors.New("throttler hasn't found any key"),
				errors.New("throttler hasn't found any key"),
				errors.New("test"),
				nil,
				errors.New("throttler hasn't found any key"),
			},
		},
		"Throttler ring should throttle on internal index error": {
			tms: 3,
			thr: NewThrottlerRing(),
			errs: []error{
				errors.New("throttler hasn't found any index"),
				errors.New("throttler hasn't found any index"),
				errors.New("throttler hasn't found any index"),
			},
		},
		"Throttler ring should throttle on matching throttler index": {
			tms: 5,
			thr: NewThrottlerRing(
				NewThrottlerEcho(nil),
				NewThrottlerEcho(errors.New("test")),
			),
			errs: []error{
				nil,
				errors.New("test"),
				nil,
				errors.New("test"),
				nil,
			},
		},
		"Throttler all should not throttle on empty list": {
			tms: 3,
			thr: NewThrottlerAll(),
		},
		"Throttler all should not throttle on non internal errors": {
			tms: 3,
			thr: NewThrottlerAll(
				NewThrottlerEcho(nil),
				NewThrottlerEcho(nil),
				NewThrottlerEcho(nil),
			),
		},
		"Throttler all should not throttle on some internal errors": {
			tms: 3,
			thr: NewThrottlerAll(
				NewThrottlerEcho(errors.New("test")),
				NewThrottlerEcho(nil),
				NewThrottlerEcho(errors.New("test")),
			),
		},
		"Throttler all should throttle on all internal errors": {
			tms: 3,
			thr: NewThrottlerAll(
				NewThrottlerEcho(errors.New("test")),
				NewThrottlerEcho(errors.New("test")),
				NewThrottlerEcho(errors.New("test")),
			),
			errs: []error{
				errors.New("throttler has received internal errors"),
				errors.New("throttler has received internal errors"),
				errors.New("throttler has received internal errors"),
			},
		},
		"Throttler any should not throttle on empty list": {
			tms: 3,
			thr: NewThrottlerAny(),
		},
		"Throttler any should not throttle on non internal errors": {
			tms: 3,
			thr: NewThrottlerAny(
				NewThrottlerEcho(nil),
				NewThrottlerEcho(nil),
				NewThrottlerEcho(nil),
			),
		},
		"Throttler any should throttle on some internal errors": {
			tms: 3,
			thr: NewThrottlerAny(
				NewThrottlerEcho(errors.New("test")),
				NewThrottlerEcho(nil),
				NewThrottlerEcho(errors.New("test")),
			),
			errs: []error{
				errors.New("throttler has received internal errors"),
				errors.New("throttler has received internal errors"),
				errors.New("throttler has received internal errors"),
			},
		},
		"Throttler any should throttle on all internal errors": {
			tms: 3,
			thr: NewThrottlerAny(
				NewThrottlerEcho(errors.New("test")),
				NewThrottlerEcho(errors.New("test")),
				NewThrottlerEcho(errors.New("test")),
			),
			errs: []error{
				errors.New("throttler has received internal errors"),
				errors.New("throttler has received internal errors"),
				errors.New("throttler has received internal errors"),
			},
		},
		"Throttler not should not throttle on internal errors": {
			tms: 3,
			thr: NewThrottlerNot(NewThrottlerEcho(errors.New("test"))),
		},
		"Throttler not should throttle on non internal errors": {
			tms: 3,
			thr: NewThrottlerNot(NewThrottlerEcho(nil)),
			errs: []error{
				errors.New("throttler hasn't received any internal error"),
				errors.New("throttler hasn't received any internal error"),
				errors.New("throttler hasn't received any internal error"),
			},
		},
		"Throttler suppress should not throttle on internal error": {
			tms: 3,
			thr: NewThrottlerSuppress(NewThrottlerEcho(errors.New("test"))),
		},
		"Throttler suppress should throttle on non internal error": {
			tms: 3,
			thr: NewThrottlerSuppress(NewThrottlerEcho(nil)),
		},
		"Throttler retry should throttle on recurring internal error": {
			tms: 3,
			thr: NewThrottlerRetry(NewThrottlerEcho(errors.New("test")), 2),
			errs: []error{
				errors.New("test"),
				errors.New("test"),
				errors.New("test"),
			},
		},
		"Throttler retry should not throttle on retried recurring internal error": {
			tms: 3,
			thr: NewThrottlerRetry(NewThrottlerBefore(3), 2),
			errs: []error{
				errors.New("throttler has not reached threshold yet"),
				nil,
				nil,
			},
		},
		"Throttler cache should not throttle on cached throttler": {
			tms: 3,
			thr: NewThrottlerCache(NewThrottlerAfter(1), ms30_0),
			errs: []error{
				nil,
				nil,
				nil,
			},
			pass: true,
		},
		"Throttler cache should throttle on close to zero cached throttler": {
			tms: 3,
			thr: NewThrottlerCache(NewThrottlerAfter(2), ms1_0),
			ins: []Runnable{
				delayed(ms2_0, nope),
				delayed(ms2_0, nope),
				delayed(ms2_0, nope),
			},
			errs: []error{
				nil,
				nil,
				errors.New("throttler has exceed threshold"),
			},
			pass: true,
		},
		"Throttler cache should throttle on cached throttler": {
			tms: 3,
			thr: NewThrottlerCache(NewThrottlerAfter(1), ms30_0),
			errs: []error{
				nil,
				errors.New("throttler has exceed threshold"),
				errors.New("throttler has exceed threshold"),
			},
		},
	}
	for tname, ptrtcase := range table {
		t.Run(tname, func(t *testing.T) {
			var wg sync.WaitGroup
			wg.Add(int(ptrtcase.tms))
			for i := 0; i < int(ptrtcase.tms); i++ {
				t.Run(fmt.Sprintf("run %d", i+1), func(t *testing.T) {
					go func(index int, tcase *tcase) {
						defer wg.Done()
						rdur, rerr := tcase.result(index)
						dur, err := tcase.run(index)
						trun.Run(func(context.Context) error {
							log("expected error %v actual err %v", rerr, err)
							log("expected duration le %s actual duration %s", rdur/2, dur)
							require.Equal(t, rerr, err)
							require.LessOrEqual(t, int64(rdur/2), int64(dur))
							return nil
						})
					}(i, &ptrtcase)
				})
			}
			wg.Wait()
		})
	}
}

func BenchmarkComplexThrottlers(b *testing.B) {
	thr := NewThrottlerAll(
		NewThrottlerAny(
			NewThrottlerAfter(100),
			NewThrottlerRunning(600),
		),
		NewThrottlerAny(
			NewThrottlerLatency(50*time.Millisecond, 5*time.Second),
			NewThrottlerNot(NewThrottlerEach(50)),
		),
		NewThrottlerMonitor(
			NewMonitorSystem(time.Minute, time.Millisecond),
			Stats{MEMAlloc: 1000},
		),
	)
	ctx := context.Background()
	for i := 0; i < b.N; i++ {
		_ = thr.Acquire(ctx)
		_ = thr.Release(ctx)
	}
}
