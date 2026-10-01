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
	"sync"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestTimeWindowHistogram(window time.Duration) (*timeWindowHistogram, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1700000000, 0)}
	return newTimeWindowHistogram(window, clock.now), clock
}

func assertWithinBucket(t *testing.T, name string, have, want float64) {
	t.Helper()
	if math.Abs(have-want) > want/subBuckets {
		t.Errorf("%s: have %v, want %v (±1/%d)", name, have, want, subBuckets)
	}
}

func TestTimeWindowHistogramBucketsContiguous(t *testing.T) {
	var next int64
	for b := range numBuckets {
		lower, width := bucketBounds(b)
		if lower != next {
			t.Fatalf("bucket %d starts at %d, want %d", b, lower, next)
		}
		next = lower + width
	}
	if next != math.MinInt64 {
		t.Fatalf("buckets end at %d, want to cover up to MaxInt64", next)
	}
}

func TestTimeWindowHistogramBucketOf(t *testing.T) {
	values := []int64{math.MaxInt64}
	for e := range 63 {
		values = append(values, 1<<e-1, 1<<e, 1<<e+1)
	}
	for _, v := range values {
		lower, width := bucketBounds(bucketOf(v))
		if v < lower || v-lower >= width {
			t.Fatalf("value %d outside bucket [%d, %d+%d)", v, lower, lower, width)
		}
		if lower >= subBuckets && width*subBuckets > lower {
			t.Fatalf("bucket [%d, %d+%d) wider than 1/%d of its lower bound", lower, lower, width, subBuckets)
		}
	}
}

func TestTimeWindowHistogram10000(t *testing.T) {
	h, _ := newTestTimeWindowHistogram(time.Minute)
	for i := 1; i <= 10000; i++ {
		h.Update(int64(i))
	}
	s := h.Snapshot()
	if s.Count() != 10000 || s.Size() != 10000 {
		t.Errorf("count/size: have %d/%d, want 10000/10000", s.Count(), s.Size())
	}
	if s.Min() != 1 || s.Max() != 10000 {
		t.Errorf("min/max: have %d/%d, want 1/10000", s.Min(), s.Max())
	}
	if s.Sum() != 50005000 || s.Mean() != 5000.5 {
		t.Errorf("sum/mean: have %d/%v, want 50005000/5000.5", s.Sum(), s.Mean())
	}
	if want := 8333333.25; math.Abs(s.Variance()-want) > 1e-3 {
		t.Errorf("variance: have %v, want %v", s.Variance(), want)
	}
	ps := []float64{0, 0.5, 0.75, 0.99, 1}
	want := []float64{1, 5000, 7500, 9900, 10000}
	for i, have := range s.Percentiles(ps) {
		assertWithinBucket(t, "percentile", have, want[i])
	}
}

func TestTimeWindowHistogramSmallValuesExact(t *testing.T) {
	h, _ := newTestTimeWindowHistogram(time.Minute)
	for range 99 {
		h.Update(10)
	}
	h.Update(1000)
	h.Update(1)
	s := h.Snapshot()
	if p := s.Percentile(0.5); p != 10 {
		t.Errorf("p50: have %v, want 10", p)
	}
	if p := s.Percentile(0.98); p != 10 {
		t.Errorf("p98: have %v, want 10", p)
	}
	if p := s.Percentile(0); p != 1 {
		t.Errorf("p0: have %v, want 1", p)
	}
}

func TestTimeWindowHistogramNegativeValues(t *testing.T) {
	h, _ := newTestTimeWindowHistogram(time.Minute)
	h.Update(-100)
	h.Update(5)
	s := h.Snapshot()
	if s.Min() != 0 || s.Sum() != 5 || s.Percentile(0) != 0 {
		t.Errorf("negative value not recorded as 0: min %d, sum %d, p0 %v", s.Min(), s.Sum(), s.Percentile(0))
	}
}

func TestTimeWindowHistogramDisabled(t *testing.T) {
	metricsEnabled = false
	defer func() { metricsEnabled = true }()
	h, _ := newTestTimeWindowHistogram(time.Minute)
	h.Update(10)
	if s := h.Snapshot(); s.Count() != 0 {
		t.Errorf("count: have %d, want 0", s.Count())
	}
}

func TestTimeWindowHistogramSnapshotDoesNotReset(t *testing.T) {
	h, _ := newTestTimeWindowHistogram(time.Minute)
	h.Update(10)
	h.Update(20)
	first, second := h.Snapshot(), h.Snapshot()
	if first.Size() != 2 || second.Size() != 2 || first.Percentile(0.5) != second.Percentile(0.5) {
		t.Errorf("snapshots differ: %+v vs %+v", first, second)
	}
}

func TestTimeWindowHistogramMergesAllLiveSlots(t *testing.T) {
	const window = time.Minute
	h, clock := newTestTimeWindowHistogram(window)
	for i := range windowSlots {
		h.Update(1000 * int64(i+1))
		clock.advance(window / windowSlots)
	}
	clock.advance(-time.Nanosecond)
	s := h.Snapshot()
	if s.Size() != windowSlots || s.Min() != 1000 || s.Max() != 6000 {
		t.Fatalf("size/min/max: have %d/%d/%d, want %d/1000/6000", s.Size(), s.Min(), s.Max(), windowSlots)
	}
	assertWithinBucket(t, "p50", s.Percentile(0.5), 3000)
	if p0, p100 := s.Percentile(0), s.Percentile(1); p0 != 1000 || p100 != 6000 {
		t.Fatalf("p0/p100: have %v/%v, want min/max 1000/6000", p0, p100)
	}

	clock.advance(time.Nanosecond)
	s = h.Snapshot()
	if s.Size() != windowSlots-1 || s.Min() != 2000 {
		t.Fatalf("after oldest slot expired: size %d, min %d, want %d, 2000", s.Size(), s.Min(), windowSlots-1)
	}
}

func TestTimeWindowHistogramExpiredSlotExcluded(t *testing.T) {
	const window = time.Minute
	for _, tc := range []struct {
		name    string
		advance time.Duration
	}{
		{"slot reused", window},
		{"slot not reused", window + window/windowSlots},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, clock := newTestTimeWindowHistogram(window)
			for range 100 {
				h.Update(1)
			}
			clock.advance(tc.advance)
			h.Update(1000)
			h.Update(2000)
			s := h.Snapshot()
			if s.Count() != 102 || s.Size() != 2 {
				t.Fatalf("count/size: have %d/%d, want 102/2", s.Count(), s.Size())
			}
			assertWithinBucket(t, "p50", s.Percentile(0.5), 1000)
			assertWithinBucket(t, "p100", s.Percentile(1), 2000)
		})
	}
}

func TestTimeWindowHistogramFullExpiry(t *testing.T) {
	const window = time.Minute
	h, clock := newTestTimeWindowHistogram(window)
	h.Update(1000)
	clock.advance(window)
	s := h.Snapshot()
	if s.Count() != 1 || s.Size() != 0 {
		t.Fatalf("count/size: have %d/%d, want 1/0", s.Count(), s.Size())
	}
	if s.Min() != 0 || s.Max() != 0 || s.Mean() != 0 || s.StdDev() != 0 || s.Percentile(0.5) != 0 {
		t.Fatalf("non-zero statistics for empty window: %+v", s)
	}
}

func TestTimeWindowHistogramVarianceExcludesExpired(t *testing.T) {
	const window = time.Minute
	h, clock := newTestTimeWindowHistogram(window)
	h.Update(1_000_000)
	clock.advance(window / 2)
	h.Update(10)
	h.Update(20)
	clock.advance(window / 2)
	s := h.Snapshot()
	if s.Mean() != 15 || s.Variance() != 25 {
		t.Fatalf("mean/variance: have %v/%v, want 15/25", s.Mean(), s.Variance())
	}
}

func TestTimeWindowHistogramConstantLargeValues(t *testing.T) {
	h, _ := newTestTimeWindowHistogram(time.Minute)
	for range 100 {
		h.Update(987654321)
	}
	if s := h.Snapshot(); s.Variance() != 0 || s.StdDev() != 0 {
		t.Fatalf("variance/stddev of constant values: have %v/%v, want 0/0", s.Variance(), s.StdDev())
	}
}

func TestTimeWindowHistogramClear(t *testing.T) {
	h, _ := newTestTimeWindowHistogram(time.Minute)
	h.Update(42)
	h.Clear()
	if s := h.Snapshot(); s.Count() != 0 || s.Size() != 0 {
		t.Fatalf("clear left count %d, size %d", s.Count(), s.Size())
	}
	h.Update(7)
	if s := h.Snapshot(); s.Size() != 1 || s.Min() != 7 || s.Max() != 7 {
		t.Fatalf("after clear: size %d, min %d, max %d", s.Size(), s.Min(), s.Max())
	}
}

func TestTimeWindowHistogramConcurrent(t *testing.T) {
	h := NewTimeWindowHistogram(time.Minute)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := range 1000 {
				h.Update(int64(i))
			}
		}()
		go func() {
			defer wg.Done()
			for range 100 {
				h.Snapshot().Percentiles([]float64{0.5, 0.99})
			}
		}()
	}
	wg.Wait()
	if c := h.Snapshot().Count(); c != 4000 {
		t.Fatalf("count: have %d, want 4000", c)
	}
}

func TestGetOrRegisterTimeWindowHistogram(t *testing.T) {
	r := NewRegistry()
	GetOrRegisterTimeWindowHistogram("foo", r, time.Minute).Update(47)
	if c := GetOrRegisterTimeWindowHistogram("foo", r, time.Minute).Snapshot().Count(); c != 1 {
		t.Fatalf("count: have %d, want 1", c)
	}
}

func BenchmarkTimeWindowHistogramUpdate(b *testing.B) {
	h := NewTimeWindowHistogram(time.Minute)
	for i := 0; b.Loop(); i++ {
		h.Update(int64(i))
	}
}

func BenchmarkTimeWindowHistogramSnapshot(b *testing.B) {
	h := NewTimeWindowHistogram(time.Minute)
	for i := range 100000 {
		h.Update(int64(i) * 1000)
	}
	ps := []float64{0.5, 0.75, 0.95, 0.99, 0.999, 0.9999}
	for b.Loop() {
		h.Snapshot().Percentiles(ps)
	}
}
