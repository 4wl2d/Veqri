package api

import (
	"sort"
	"sync"
	"testing"
	"time"
)

func TestDesktopRevisionStrictlyIncreasesWhenWallTimeStallsOrRegresses(t *testing.T) {
	server := &Server{}
	base := time.Date(2026, time.July, 12, 20, 0, 0, 123_000_000, time.UTC)
	first := server.nextDesktopRevision(base)
	second := server.nextDesktopRevision(base)
	third := server.nextDesktopRevision(base.Add(-time.Hour))
	fourth := server.nextDesktopRevision(base.Add(time.Second))

	if first != base.UnixMilli() {
		t.Fatalf("first revision = %d, want wall time %d", first, base.UnixMilli())
	}
	if second != first+1 || third != second+1 {
		t.Fatalf("stalled/regressed revisions = [%d %d %d], want consecutive values", first, second, third)
	}
	if fourth != base.Add(time.Second).UnixMilli() || fourth <= third {
		t.Fatalf("advanced-wall revision = %d, previous = %d", fourth, third)
	}
}

func TestDesktopRevisionIsUniqueUnderConcurrentEqualWallTime(t *testing.T) {
	server := &Server{}
	base := time.Date(2026, time.July, 12, 20, 0, 0, 0, time.UTC)
	const count = 128
	revisions := make([]int64, count)
	var group sync.WaitGroup
	group.Add(count)
	for index := range revisions {
		go func() {
			defer group.Done()
			revisions[index] = server.nextDesktopRevision(base)
		}()
	}
	group.Wait()

	sort.Slice(revisions, func(left, right int) bool { return revisions[left] < revisions[right] })
	for index, revision := range revisions {
		want := base.UnixMilli() + int64(index)
		if revision != want {
			t.Fatalf("revision[%d] = %d, want %d", index, revision, want)
		}
	}
}
