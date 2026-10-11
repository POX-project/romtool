package compat

import (
	"testing"

	"portlab/internal/project"
)

func fp(arch, sdk, vndk string) *project.Fingerprint {
	return &project.Fingerprint{
		Arch: arch, SDK: sdk, VNDK: vndk,
		Treble: "true", SAR: "true", ABUpdate: "true",
		RomType: "dynamic_block_ota",
		Partitions: []string{"system", "vendor"},
		BootImages: []string{"boot"},
	}
}

func TestArchMismatchIncompatible(t *testing.T) {
	rep := Check(fp("x86_64", "34", "34"), fp("arm64", "34", "34"))
	if rep.Verdict != Incompatible || rep.CanBoot || rep.SuggestedMode != "none" {
		t.Fatalf("want INCOMPATIBLE, got %+v", rep)
	}
}

func TestSameGenerationDirect(t *testing.T) {
	rep := Check(fp("arm64", "34", "34"), fp("arm64", "34", "34"))
	if rep.Verdict != Direct || !rep.CanBoot {
		t.Fatalf("want DIRECT_PORT, got %+v", rep)
	}
}

func TestSdkGapTiers(t *testing.T) {
	// gap 1 -> direct with fixes
	if rep := Check(fp("arm64", "34", "34"), fp("arm64", "33", "33")); rep.SuggestedMode != "direct" {
		t.Fatalf("gap1: %+v", rep)
	}
	// gap 4 -> sgsi or gsi, still bootable
	if rep := Check(fp("arm64", "34", "34"), fp("arm64", "30", "30")); !rep.CanBoot {
		t.Fatalf("gap4 should still be bootable via fallback: %+v", rep)
	}
}
