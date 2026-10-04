package testenv

import "testing"

func TestPlanLocalBinds_AllFields(t *testing.T) {
	cfg := ServerConfig{
		DataDir:   "/local/data",
		ModsDir:   "/local/mods",
		ConfigDir: "/local/config",
		OutputDir: "/local/output",
	}
	plan := planLocalBinds("/tmp/stage-root", cfg)

	want := []bindPlanEntry{
		{localDir: "/local/data", remoteDir: "/tmp/stage-root/data", containerDest: "/data"},
		{localDir: "/local/mods", remoteDir: "/tmp/stage-root/mods", containerDest: "/data/mods"},
		{localDir: "/local/config", remoteDir: "/tmp/stage-root/config", containerDest: "/data/config"},
		{localDir: "/local/output", remoteDir: "/tmp/stage-root/output", containerDest: "/data/output"},
	}
	if len(plan) != len(want) {
		t.Fatalf("len(plan) = %d, want %d: %+v", len(plan), len(want), plan)
	}
	for i, got := range plan {
		if got != want[i] {
			t.Errorf("plan[%d] = %+v, want %+v", i, got, want[i])
		}
	}
}

func TestPlanLocalBinds_EmptyFieldsSkipped(t *testing.T) {
	plan := planLocalBinds("/tmp/stage-root", ServerConfig{ModsDir: "/local/mods"})
	if len(plan) != 1 {
		t.Fatalf("len(plan) = %d, want 1 (only ModsDir was set): %+v", len(plan), plan)
	}
	if plan[0].containerDest != "/data/mods" {
		t.Errorf("containerDest = %q, want /data/mods", plan[0].containerDest)
	}
}

func TestPlanLocalBinds_NothingRequested(t *testing.T) {
	if plan := planLocalBinds("/tmp/stage-root", ServerConfig{}); len(plan) != 0 {
		t.Fatalf("plan = %+v, want empty", plan)
	}
}

func TestPlanLocalBinds_MountDirsUsesBaseNameForDestination(t *testing.T) {
	cfg := ServerConfig{MountDirs: []string{"/home/user/testdata/resourcepacks", "saves"}}
	plan := planLocalBinds("/tmp/stage-root", cfg)

	want := []bindPlanEntry{
		{
			localDir:      "/home/user/testdata/resourcepacks",
			remoteDir:     "/tmp/stage-root/mount-resourcepacks",
			containerDest: "/data/resourcepacks",
		},
		{
			localDir:      "saves",
			remoteDir:     "/tmp/stage-root/mount-saves",
			containerDest: "/data/saves",
		},
	}
	if len(plan) != len(want) {
		t.Fatalf("len(plan) = %d, want %d: %+v", len(plan), len(want), plan)
	}
	for i, got := range plan {
		if got != want[i] {
			t.Errorf("plan[%d] = %+v, want %+v", i, got, want[i])
		}
	}
}

func TestPlanLocalBinds_RemoteDirsAreDistinctAcrossAllFields(t *testing.T) {
	// A real bug this guards against: two fields accidentally resolving to
	// the same remote subdirectory, silently merging their contents.
	cfg := ServerConfig{
		DataDir:   "/local/a",
		ModsDir:   "/local/b",
		ConfigDir: "/local/c",
		OutputDir: "/local/d",
		MountDirs: []string{"/local/e", "/local/f"},
	}
	plan := planLocalBinds("/tmp/stage-root", cfg)

	seen := map[string]bool{}
	for _, entry := range plan {
		if seen[entry.remoteDir] {
			t.Errorf("remoteDir %q used by more than one bind entry", entry.remoteDir)
		}
		seen[entry.remoteDir] = true
	}
	if len(seen) != 6 {
		t.Fatalf("got %d distinct remote dirs, want 6: %+v", len(seen), plan)
	}
}
