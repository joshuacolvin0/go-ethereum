// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package metrics

import (
	"math"
	"math/bits"
	"sync"
	"time"
)

const (
	windowSlots   = 6
	subBucketBits = 4
	subBuckets    = 1 << subBucketBits
	numBuckets    = (64 - subBucketBits) * subBuckets
)

type timeWindowHistogram struct {
	mu        sync.Mutex
	now       func() time.Time
	start     time.Time
	slotWidth time.Duration
	total     int64
	slots     [windowSlots]windowSlot
}

type windowSlot struct {
	epoch    int64
	count    int64
	sum      int64
	sumSq    float64
	min, max int64
	buckets  [numBuckets]uint32
}

func GetOrRegisterTimeWindowHistogram(name string, r Registry, window time.Duration) Histogram {
	if r == nil {
		r = DefaultRegistry
	}
	return r.GetOrRegister(name, func() any { return NewTimeWindowHistogram(window) }).(Histogram)
}

// NewTimeWindowHistogram returns a histogram whose Count is cumulative and
// whose other statistics cover the last window, expiring in steps of window/6.
// Percentiles are nearest-rank: exact below 16, within 1/16 above. Negative
// values are recorded as 0.
func NewTimeWindowHistogram(window time.Duration) Histogram {
	return newTimeWindowHistogram(window, time.Now)
}

func newTimeWindowHistogram(window time.Duration, now func() time.Time) *timeWindowHistogram {
	return &timeWindowHistogram{
		now:       now,
		start:     now(),
		slotWidth: max(window/windowSlots, 1),
	}
}

func (h *timeWindowHistogram) Clear() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.total = 0
	h.slots = [windowSlots]windowSlot{}
}

func (h *timeWindowHistogram) Update(v int64) {
	if !metricsEnabled {
		return
	}
	v = max(v, 0)

	h.mu.Lock()
	defer h.mu.Unlock()

	epoch := h.epoch()
	slot := &h.slots[epoch%windowSlots]
	if slot.count == 0 || slot.epoch != epoch {
		*slot = windowSlot{epoch: epoch, min: math.MaxInt64, max: math.MinInt64}
	}
	slot.buckets[bucketOf(v)]++

	h.total++
	slot.count++
	slot.sum += v
	slot.sumSq += float64(v) * float64(v)
	slot.min = min(slot.min, v)
	slot.max = max(slot.max, v)
}

func (h *timeWindowHistogram) Snapshot() HistogramSnapshot {
	h.mu.Lock()
	defer h.mu.Unlock()

	snap := &timeWindowSnapshot{count: h.total, min: math.MaxInt64, max: math.MinInt64}
	current := h.epoch()
	var merged [numBuckets]uint64
	for i := range h.slots {
		slot := &h.slots[i]
		if slot.count == 0 || current-slot.epoch >= windowSlots {
			continue
		}
		for b, n := range slot.buckets {
			merged[b] += uint64(n)
		}
		snap.size += slot.count
		snap.sum += slot.sum
		snap.sumSq += slot.sumSq
		snap.min = min(snap.min, slot.min)
		snap.max = max(snap.max, slot.max)
	}
	if snap.size == 0 {
		snap.min, snap.max = 0, 0
		return snap
	}
	for b, n := range merged {
		if n > 0 {
			lower, width := bucketBounds(b)
			snap.buckets = append(snap.buckets, bucketCount{lower: lower, width: width, n: n})
		}
	}
	return snap
}

func (h *timeWindowHistogram) epoch() int64 {
	return int64(h.now().Sub(h.start) / h.slotWidth)
}

func bucketOf(v int64) int {
	if v < subBuckets {
		return int(v)
	}
	shift := bits.Len64(uint64(v)) - 1 - subBucketBits
	return (shift+1)<<subBucketBits | int(v>>shift)&(subBuckets-1)
}

func bucketBounds(bucket int) (lower, width int64) {
	group, sub := bucket>>subBucketBits, int64(bucket&(subBuckets-1))
	if group == 0 {
		return sub, 1
	}
	shift := group - 1
	return (subBuckets + sub) << shift, 1 << shift
}

type bucketCount struct {
	lower, width int64
	n            uint64
}

type timeWindowSnapshot struct {
	count    int64
	size     int64
	sum      int64
	sumSq    float64
	min, max int64
	buckets  []bucketCount
}

func (s *timeWindowSnapshot) Count() int64 { return s.count }
func (s *timeWindowSnapshot) Size() int    { return int(s.size) }
func (s *timeWindowSnapshot) Sum() int64   { return s.sum }
func (s *timeWindowSnapshot) Min() int64   { return s.min }
func (s *timeWindowSnapshot) Max() int64   { return s.max }

func (s *timeWindowSnapshot) Mean() float64 {
	if s.size == 0 {
		return 0
	}
	return float64(s.sum) / float64(s.size)
}

func (s *timeWindowSnapshot) Variance() float64 {
	if s.size == 0 {
		return 0
	}
	mean := s.Mean()
	return max(s.sumSq/float64(s.size)-mean*mean, 0)
}

func (s *timeWindowSnapshot) StdDev() float64 { return math.Sqrt(s.Variance()) }

func (s *timeWindowSnapshot) Percentile(p float64) float64 {
	if s.size == 0 {
		return 0
	}
	rank := p * float64(s.size)
	var seen float64
	for _, b := range s.buckets {
		n := float64(b.n)
		if seen+n >= rank {
			v := float64(b.lower) + float64(b.width-1)*(rank-seen)/n
			return max(float64(s.min), min(float64(s.max), v))
		}
		seen += n
	}
	return float64(s.max)
}

func (s *timeWindowSnapshot) Percentiles(ps []float64) []float64 {
	scores := make([]float64, len(ps))
	for i, p := range ps {
		scores[i] = s.Percentile(p)
	}
	return scores
}
