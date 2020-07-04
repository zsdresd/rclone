package accounting

import (
	"context"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/rc"
	"golang.org/x/time/rate"
)

// TokenBucket holds the global token bucket limiter
var TokenBucket tokenBucket

// TokenBucketSlot is the type to select which token bucket to use
type TokenBucketSlot int

// Slots for the token bucket
const (
	TokenBucketSlotAccounting TokenBucketSlot = iota
	TokenBucketSlotTransportRx
	TokenBucketSlotTransportTx
	TokenBucketSlots
)

type buckets [TokenBucketSlots]*rate.Limiter

// tokenBucket holds info about the rate limiters in use
type tokenBucket struct {
	mu          sync.RWMutex // protects the token bucket variables
	curr        buckets
	prev        buckets
	toggledOff  bool
	currLimitMu sync.Mutex // protects changes to the timeslot
	currLimit   fs.BwTimeSlot
}

// Return true if limit is disabled
//
// Call with lock held
func (bs *buckets) _isOff() bool {
	return bs[0] == nil
}

// Disable the limits
//
// Call with lock held
func (bs *buckets) _setOff() {
	for i := range bs {
		bs[i] = nil
	}
}

const maxBurstSize = 4 * 1024 * 1024 // must be bigger than the biggest request

// make a new empty token bucket with the bandwidth given
func newTokenBucket(bandwidth fs.SizeSuffix) (newTokenBucket buckets) {
	for i := range newTokenBucket {
		newTokenBucket[i] = rate.NewLimiter(rate.Limit(bandwidth), maxBurstSize)
		// empty the bucket
		err := newTokenBucket[i].WaitN(context.Background(), maxBurstSize)
		if err != nil {
			fs.Errorf(nil, "Failed to empty token bucket: %v", err)
		}
	}
	return newTokenBucket
}

// Start starts the token bucket if necessary
func (tb *tokenBucket) Start() {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	tb.currLimit = fs.Config.BwLimit.LimitAt(time.Now())
	if tb.currLimit.Bandwidth > 0 {
		tb.curr = newTokenBucket(tb.currLimit.Bandwidth)
		fs.Infof(nil, "Starting bandwidth limiter at %vBytes/s", &tb.currLimit.Bandwidth)

		// Start the SIGUSR2 signal handler to toggle bandwidth.
		// This function does nothing in windows systems.
		tb.startSignalHandler()
	}
}

// StartTokenTicker creates a ticker to update the bandwidth limiter every minute.
func (tb *tokenBucket) StartTokenTicker() {
	// If the timetable has a single entry or was not specified, we don't need
	// a ticker to update the bandwidth.
	if len(fs.Config.BwLimit) <= 1 {
		return
	}

	ticker := time.NewTicker(time.Minute)
	go func() {
		for range ticker.C {
			limitNow := fs.Config.BwLimit.LimitAt(time.Now())
			tb.currLimitMu.Lock()

			if tb.currLimit.Bandwidth != limitNow.Bandwidth {
				tb.mu.Lock()

				// If bwlimit is toggled off, the change should only
				// become active on the next toggle, which causes
				// an exchange of tb.curr <-> tb.prev
				var targetBucket *buckets
				if tb.toggledOff {
					targetBucket = &tb.prev
				} else {
					targetBucket = &tb.curr
				}

				// Set new bandwidth. If unlimited, set tokenbucket to nil.
				if limitNow.Bandwidth > 0 {
					*targetBucket = newTokenBucket(limitNow.Bandwidth)
					if tb.toggledOff {
						fs.Logf(nil, "Scheduled bandwidth change. "+
							"Limit will be set to %vBytes/s when toggled on again.", &limitNow.Bandwidth)
					} else {
						fs.Logf(nil, "Scheduled bandwidth change. Limit set to %vBytes/s", &limitNow.Bandwidth)
					}
				} else {
					targetBucket._setOff()
					fs.Logf(nil, "Scheduled bandwidth change. Bandwidth limits disabled")
				}

				tb.currLimit = limitNow
				tb.mu.Unlock()
			}
			tb.currLimitMu.Unlock()
		}
	}()
}

// LimitBandwith sleeps for the correct amount of time for the passage
// of n bytes according to the current bandwidth limit
func (tb *tokenBucket) LimitBandwidth(i TokenBucketSlot, n int) {
	tb.mu.RLock()

	// Limit the transfer speed if required
	if !tb.curr._isOff() {
		err := tb.curr[i].WaitN(context.Background(), n)
		if err != nil {
			fs.Errorf(nil, "Token bucket error: %v", err)
		}
	}

	tb.mu.RUnlock()
}

// SetBwLimit sets the current bandwidth limit
func (tb *tokenBucket) SetBwLimit(bandwidth fs.SizeSuffix) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if bandwidth > 0 {
		tb.curr = newTokenBucket(bandwidth)
		fs.Logf(nil, "Bandwidth limit set to %v", bandwidth)
	} else {
		tb.curr._setOff()
		fs.Logf(nil, "Bandwidth limit reset to unlimited")
	}
}

// read and set the bandwidth limits
func (tb *tokenBucket) rcBwlimit(ctx context.Context, in rc.Params) (out rc.Params, err error) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if in["rate"] != nil {
		bwlimit, err := in.GetString("rate")
		if err != nil {
			return out, err
		}
		var bws fs.BwTimetable
		err = bws.Set(bwlimit)
		if err != nil {
			return out, errors.Wrap(err, "bad bwlimit")
		}
		if len(bws) != 1 {
			return out, errors.New("need exactly 1 bandwidth setting")
		}
		bw := bws[0]
		tb.SetBwLimit(bw.Bandwidth)
	}
	bytesPerSecond := int64(-1)
	if !tb.curr._isOff() {
		bytesPerSecond = int64(tb.curr[0].Limit())
	}
	out = rc.Params{
		"rate":           fs.SizeSuffix(bytesPerSecond).String(),
		"bytesPerSecond": bytesPerSecond,
	}
	return out, nil
}

// Remote control for the token bucket
func init() {
	rc.Add(rc.Call{
		Path: "core/bwlimit",
		Fn: func(ctx context.Context, in rc.Params) (out rc.Params, err error) {
			return TokenBucket.rcBwlimit(ctx, in)
		},
		Title: "Set the bandwidth limit.",
		Help: `
This sets the bandwidth limit to that passed in.

Eg

    rclone rc core/bwlimit rate=off
    {
        "bytesPerSecond": -1,
        "rate": "off"
    }
    rclone rc core/bwlimit rate=1M
    {
        "bytesPerSecond": 1048576,
        "rate": "1M"
    }


If the rate parameter is not supplied then the bandwidth is queried

    rclone rc core/bwlimit
    {
        "bytesPerSecond": 1048576,
        "rate": "1M"
    }

The format of the parameter is exactly the same as passed to --bwlimit
except only one bandwidth may be specified.

In either case "rate" is returned as a human readable string, and
"bytesPerSecond" is returned as a number.
`,
	})
}
