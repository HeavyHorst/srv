package model

import "testing"

func TestKindNameTreatsLegacyRecordsAsVMs(t *testing.T) {
	if got := (Instance{}).KindName(); got != KindVM {
		t.Fatalf("legacy KindName() = %q, want %q", got, KindVM)
	}
	if got := (Instance{Kind: KindOrb}).KindName(); got != KindOrb {
		t.Fatalf("KindName() = %q, want %q", got, KindOrb)
	}
}

func TestImageNameDefaultsToArch(t *testing.T) {
	if got := (Instance{}).ImageName(); got != "arch" {
		t.Fatalf("ImageName() = %q, want arch", got)
	}
	if got := (Instance{Image: "ubuntu"}).ImageName(); got != "ubuntu" {
		t.Fatalf("ImageName() = %q, want ubuntu", got)
	}
}

func TestNormalizeMemoryMode(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"", MemoryModeFixed},
		{"fixed", MemoryModeFixed},
		{"pool", MemoryModePool},
		{" POOL ", MemoryModePool},
		{"poolish", MemoryModeFixed},
	} {
		if got := NormalizeMemoryMode(tc.in); got != tc.want {
			t.Errorf("NormalizeMemoryMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestUsesMemoryPoolRequiresModeAndID(t *testing.T) {
	for _, tc := range []struct {
		mode, poolID string
		want         bool
	}{
		{"", "", false},
		{"pool", "", false},
		{"pool", "  ", false},
		{"pool", "pool-1", true},
		{" POOL ", " pool-1 ", true},
		{"fixed", "pool-1", false},
	} {
		inst := Instance{MemoryMode: tc.mode, MemoryPoolID: tc.poolID}
		if got := inst.UsesMemoryPool(); got != tc.want {
			t.Errorf("UsesMemoryPool(mode=%q, pool=%q) = %v, want %v", tc.mode, tc.poolID, got, tc.want)
		}
	}
}
