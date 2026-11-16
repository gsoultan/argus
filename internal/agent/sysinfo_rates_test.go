package agent

import (
	"testing"
	"time"
)

func TestNetRates(t *testing.T) {
	var u UsageSampler
	t0 := time.Unix(1_700_000_000, 0)
	first := u.netRates([]netCounters{{name: "eth0", rx: 1000, tx: 500, speedMbps: 1000}}, t0)
	if len(first) != 1 || first[0].RxBps != nil || first[0].TxBps != nil {
		t.Fatalf("first sample = %+v, want no rate with nothing to compare", first)
	}
	if first[0].SpeedMbps != 1000 {
		t.Errorf("speed = %d", first[0].SpeedMbps)
	}

	// 60 s later: 6000 more bytes in (100 B/s), and the out counter went
	// backwards (interface reset), which has no honest rate.
	second := u.netRates([]netCounters{{name: "eth0", rx: 7000, tx: 100}, {name: "eth1", rx: 5, tx: 5}}, t0.Add(time.Minute))
	if len(second) != 2 {
		t.Fatalf("second = %+v", second)
	}
	if second[0].RxBps == nil || *second[0].RxBps != 100 {
		t.Errorf("rx = %v, want 100", second[0].RxBps)
	}
	if second[0].TxBps != nil {
		t.Errorf("tx = %v after a counter reset, want none", *second[0].TxBps)
	}
	if second[1].RxBps != nil {
		t.Errorf("a new interface has a rate on its first sighting: %v", *second[1].RxBps)
	}

	// Bounded however many interfaces the platform lists.
	many := make([]netCounters, maxNet+10)
	for i := range many {
		many[i] = netCounters{name: string(rune('a'+i%26)) + string(rune('a'+i/26))}
	}
	if got := u.netRates(many, t0.Add(2*time.Minute)); len(got) != maxNet {
		t.Errorf("rates = %d, want bounded at %d", len(got), maxNet)
	}
}

func TestDiskRates(t *testing.T) {
	var u UsageSampler
	t0 := time.Unix(1_700_000_000, 0)
	if got := u.diskRates(nil, t0); got != nil {
		t.Fatalf("no disks read = %+v, want nil (not reported)", got)
	}
	first := u.diskRates([]diskCounters{{name: "nvme0n1", reads: 100, writes: 50, readBytes: 4096, wroteBytes: 8192}}, t0)
	if len(first) != 1 || first[0].ReadOps != nil {
		t.Fatalf("first = %+v, want no rate yet", first)
	}
	second := u.diskRates([]diskCounters{{name: "nvme0n1", reads: 700, writes: 40, readBytes: 4096 + 60*1024, wroteBytes: 8192}}, t0.Add(time.Minute))
	d := second[0]
	if d.ReadOps == nil || *d.ReadOps != 10 || d.ReadBps == nil || *d.ReadBps != 1024 {
		t.Errorf("read = %v ops %v B/s, want 10 and 1024", d.ReadOps, d.ReadBps)
	}
	if d.WriteOps != nil {
		t.Errorf("writes went backwards (counter wrapped), rate = %v, want none", *d.WriteOps)
	}
	if d.WriteBps == nil || *d.WriteBps != 0 {
		t.Errorf("write bytes = %v, want 0", d.WriteBps)
	}
}

func TestStealSince(t *testing.T) {
	if stealSince(cpuTimes{}, cpuTimes{total: 10, steal: 5}) != nil {
		t.Error("a first sample has steal")
	}
	if v := stealSince(cpuTimes{total: 100, steal: 0}, cpuTimes{total: 200, steal: 25}); v == nil || *v != 25 {
		t.Errorf("steal = %v, want 25", v)
	}
}
