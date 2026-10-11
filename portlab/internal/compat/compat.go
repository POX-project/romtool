// Package compat decides whether a donor ROM can boot on a target device.
// Rules distilled from the hyperdroid skill domains (android / fastboot /
// build / lineageos): arch, SDK/VNDK age, treble/SAR, A/B slots, dynamic
// partitions, AVB, kernel ownership.
package compat

import (
	"fmt"
	"strconv"
	"strings"

	"portlab/internal/project"
)

type Verdict string

const (
	Direct        Verdict = "DIRECT_PORT"
	DirectFix     Verdict = "DIRECT_PORT_WITH_FIXES"
	SGSI          Verdict = "SGSI_RECOMMENDED"
	GSIOnly       Verdict = "GSI_ONLY"
	Incompatible  Verdict = "INCOMPATIBLE"
)

type Report struct {
	Verdict       Verdict  `json:"verdict"`
	Score         int      `json:"score"`
	SuggestedMode string   `json:"suggested_mode"`
	CanBoot       bool     `json:"can_boot"`
	Reasons       []string `json:"reasons"`
	Fixes         []string `json:"fixes"`
}

func Check(donor, base *project.Fingerprint) *Report {
	rep := &Report{}
	score := 100
	add := func(d int, r string) {
		score += d
		if r != "" {
			rep.Reasons = append(rep.Reasons, r)
		}
	}
	fix := func(s string) { rep.Fixes = append(rep.Fixes, s) }

	if known(donor.Arch) && known(base.Arch) {
		if !archOK(donor.Arch, base.Arch) {
			add(-90, fmt.Sprintf("ARCH MISMATCH: donor %s vs target %s — cannot boot.", donor.Arch, base.Arch))
			rep.Verdict, rep.Score, rep.SuggestedMode, rep.CanBoot = Incompatible, max(0, score), "none", false
			fix("Pick a donor with matching CPU ABI (arm64 -> arm64). No GSI fallback fixes this.")
			return rep
		}
		add(0, fmt.Sprintf("Arch match: %s.", donor.Arch))
	} else {
		add(-10, fmt.Sprintf("Arch unknown (donor=%s target=%s) — verify with getprop ro.product.cpu.abi.", donor.Arch, base.Arch))
		fix("Unpack system/build.prop on both sides, then re-run check -refresh.")
	}

	dSDK, bSDK, bVendor := atoiFirst(donor.SDK), atoiFirst(base.SDK), atoiFirst(base.VendorSDK)
	if dSDK > 0 && bSDK > 0 {
		switch gap := dSDK - bSDK; {
		case gap <= 0:
			add(5, fmt.Sprintf("Donor SDK %d <= target SDK %d — vendor new enough.", dSDK, bSDK))
		case gap == 1:
			add(-5, fmt.Sprintf("Donor SDK %d is 1 above target %d — bootable with vendor shims.", dSDK, bSDK))
			fix("Keep target vendor/odm + kernel; take only donor system/product/system_ext.")
		case gap == 2:
			add(-20, fmt.Sprintf("Donor SDK %d is 2 above target %d — expect HAL/VNDK friction.", dSDK, bSDK))
			fix("Prefer SGSI mode (Phh Treble overlays + vendor shims).")
		default:
			add(-40, fmt.Sprintf("Donor SDK %d is %d above target %d — vendor too old for direct port.", dSDK, gap, bSDK))
			fix("Use GSI/SGSI fallback: generic system on target vendor.")
		}
	} else if dSDK > 0 && bVendor > 0 && dSDK-bVendor > 2 {
		add(-35, fmt.Sprintf("Donor SDK %d vs target vendor SDK %d — direct port unlikely.", dSDK, bVendor))
		fix("Fall back to SGSI/GSI.")
	} else {
		add(-5, "SDK unknown on one side — bootability uncertain until build.prop is available.")
		fix("Unpack system + vendor and re-run check -refresh.")
	}

	if donor.VNDK != "" && base.VNDK != "" && donor.VNDK != base.VNDK {
		add(-10, fmt.Sprintf("VNDK mismatch donor %s vs target %s — needs shims.", donor.VNDK, base.VNDK))
		fix("SGSI carries VNDK-lite shims; direct port needs manual vndk-stable libs.")
	} else if donor.VNDK != "" && base.VNDK != "" {
		add(5, fmt.Sprintf("VNDK match (%s).", donor.VNDK))
	}
	if isFalse(donor.Treble) && isTrue(base.Treble) {
		add(-15, "Donor non-Treble but target expects Treble — system/vendor split will fight.")
		fix("Use GSI layout (system-as-root), never copy donor vendor.")
	}
	if donor.SAR != "" && base.SAR != "" && donor.SAR != base.SAR {
		add(-10, fmt.Sprintf("System-as-root mismatch donor=%s target=%s — keep target boot/fstab.", donor.SAR, base.SAR))
		fix("Never overwrite target ramdisk with donor ramdisk.")
	}
	if donor.ABUpdate != "" && base.ABUpdate != "" && donor.ABUpdate != base.ABUpdate {
		add(-8, fmt.Sprintf("A/B mismatch donor=%s target=%s — updater-script must be rewritten.", donor.ABUpdate, base.ABUpdate))
		fix("Output reuses target updater-script/META-INF.")
	}
	if donor.Dynamic && !base.Dynamic {
		add(-15, "Donor dynamic (super) but target non-dynamic — partition tables differ.")
		fix("Map donor system into target dat/img slots, or convert donor system to GSI system.img.")
	} else if !donor.Dynamic && base.Dynamic {
		add(-5, "Donor non-dynamic, target dynamic — resizable, usually fine.")
	} else if donor.Dynamic && base.Dynamic {
		add(5, "Both dynamic — super layout compatible.")
	}
	if base.AVB {
		add(0, "Target uses AVB — output vbmeta is disabled (flags 3).")
	}
	if donor.KernelVersion != "" && base.KernelVersion != "" && donor.KernelVersion != base.KernelVersion {
		add(-5, fmt.Sprintf("Kernel differs donor %s vs target %s — port keeps TARGET kernel.", donor.KernelVersion, base.KernelVersion))
		fix("Never flash donor boot.img on target.")
	}
	if len(base.BootImages) == 0 {
		add(-15, "No boot.img in target — need stock kernel to boot anything.")
		fix("Provide stock boot.img/vendor_boot for the target.")
	}
	hasVendor := false
	for _, p := range base.Partitions {
		if p == "vendor" || p == "odm" {
			hasVendor = true
		}
	}
	if !hasVendor && base.RomType != "unknown" {
		add(-10, "No vendor/odm in target — HALs may be missing.")
		fix("Use full stock firmware as --base.")
	}

	score = minmax(score)
	rep.Score = score
	switch {
	case score >= 80:
		rep.Verdict, rep.SuggestedMode, rep.CanBoot = Direct, "direct", true
		fix("Direct hybrid: donor system/product/system_ext + target vendor/odm/boot.")
	case score >= 60:
		rep.Verdict, rep.SuggestedMode, rep.CanBoot = DirectFix, "direct", true
		fix("Direct port with fixes: fstab + build.prop + updater-script + vbmeta. Then refine.")
	case score >= 40:
		rep.Verdict, rep.SuggestedMode, rep.CanBoot = SGSI, "sgsi", true
		fix("SGSI fallback: donor system + Treble/Phh overlays on target vendor.")
	case score >= 20:
		rep.Verdict, rep.SuggestedMode, rep.CanBoot = GSIOnly, "gsi", true
		fix("GSI fallback: generic system.img + target vendor/boot.")
	default:
		rep.Verdict, rep.SuggestedMode, rep.CanBoot = Incompatible, "none", false
	}
	return rep
}

func known(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s != "" && s != "unknown"
}
func archOK(d, b string) bool {
	d, b = strings.ToLower(d), strings.ToLower(b)
	if d == b {
		return true
	}
	return d == "arm" && b == "arm64"
}
func atoiFirst(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	f := strings.Fields(s)[0]
	n, err := strconv.Atoi(f)
	if err == nil {
		return n
	}
	d := ""
	for _, r := range f {
		if r >= '0' && r <= '9' {
			d += string(r)
		} else {
			break
		}
	}
	n, _ = strconv.Atoi(d)
	return n
}
func isTrue(s string) bool  { return strings.EqualFold(strings.TrimSpace(s), "true") || s == "1" }
func isFalse(s string) bool { return strings.EqualFold(strings.TrimSpace(s), "false") || s == "0" }
func minmax(s int) int {
	if s < 0 {
		return 0
	}
	if s > 100 {
		return 100
	}
	return s
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
