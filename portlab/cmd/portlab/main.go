package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"portlab/internal/audit"
	"portlab/internal/compat"
	"portlab/internal/engine"
	"portlab/internal/project"
	"portlab/internal/util"
)

const version = "0.1.0"

func usage() {
	fmt.Printf(`PortLab v%s — donor ROM -> any device (standalone, romtool untouched)

USAGE:
  portlab <command> [options]

COMMANDS:
  init      Create port project from donor + target
  info      Show fingerprints + metadata
  check     Bootability verdict + boot-readiness (defines if donor can boot)
  audit     Mismatch audit: graded HARD_BRICK/NO_START/NO_BOOT risks (-deep for HAL/fstab/modules)
  make      Build hybrid work/ until bootable (keeps target vendor/boot)
  refine    Auto-refine: unpack work, patch props, repack, sync back
  to-gsi    Fallback: convert donor to GSI/SGSI zip when direct port cannot boot
  metadata  Edit output metadata (name/device/version/maintainer/...)
  repack    Pack work/ to flashable zip (via romtool) + zipalign
  menu      Interactive donor/target picker + refine loop
  doctor    Check toolkit + romtool wiring

EXAMPLES:
  portlab init -donor donors/cmf_phone1/NothingOS_2.6_tetris_fullota.zip -base projects/lineage-m20 -dir ./ports/nothing-on-m20
  portlab check -project ./ports/nothing-on-m20
  portlab make -project ./ports/nothing-on-m20 -mode auto
  portlab to-gsi -project ./ports/nothing-on-m20 -sgsi
  portlab metadata -project ./ports/nothing-on-m20 -set maintainer=you -set version=v1
  portlab repack -project ./ports/nothing-on-m20 -output ./output/port.zip
`, version)
}

func main() {
	lowerSelfPriority()
	if len(os.Args) < 2 {
		usage()
		return
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "-h", "--help", "help":
		usage()
	case "doctor":
		doctor()
	case "init":
		fs := flag.NewFlagSet("init", flag.ExitOnError)
		donor, base, dir, name := fs.String("donor", "", "donor zip/dir"), fs.String("base", "", "target zip/dir"), fs.String("dir", "", "out dir"), fs.String("name", "", "port name")
		_ = fs.Parse(args)
		if *donor == "" || *base == "" || *dir == "" {
			fmt.Println("Usage: portlab init -donor <zip/dir> -base <zip/dir> -dir <out> [-name <n>]")
			os.Exit(1)
		}
		p, err := project.Init(*donor, *base, *dir, *name)
		if err != nil {
			fatal(err)
		}
		rep := compat.Check(&p.Donor, &p.Base)
		p.Score, p.Verdict, p.Mode = rep.Score, string(rep.Verdict), rep.SuggestedMode
		_ = p.Save()
		fmt.Printf("Created %s\n", p.Dir)
		printCheck(p)
	case "info":
		p := mustLoad(args)
		printInfo(p)
	case "check":
		fs := flag.NewFlagSet("check", flag.ExitOnError)
		dir, refresh := fs.String("project", ".", "port dir"), fs.Bool("refresh", false, "re-fingerprint")
		_ = fs.Parse(args)
		p, err := project.Load(*dir)
		if err != nil {
			fatal(err)
		}
		if *refresh {
			_ = p.Refresh()
			rep := compat.Check(&p.Donor, &p.Base)
			p.Score, p.Verdict = rep.Score, string(rep.Verdict)
			if p.Mode == "undecided" {
				p.Mode = rep.SuggestedMode
			}
			_ = p.Save()
		}
		printCheck(p)
	case "make":
		memNotice()
		fs := flag.NewFlagSet("make", flag.ExitOnError)
		dir, mode, perm := fs.String("project", ".", "port dir"), fs.String("mode", "auto", "direct|sgsi|gsi|auto"), fs.Bool("permissive", false, "selinux permissive")
		force := fs.Bool("force", false, "bypass CanBoot refusal (arch mismatch and missing boot still refuse)")
		_ = fs.Parse(args)
		p, err := project.Load(*dir)
		if err != nil {
			fatal(err)
		}
		if err := engine.Make(p, engine.Options{Mode: *mode, Permissive: *perm, AvbDisable: true, Force: *force}); err != nil {
			fatal(err)
		}
		fmt.Printf("Built %s/work (%s). Report: %s\n", p.Dir, p.Mode, p.LastReport)
		printCheck(p)
	case "to-gsi":
		memNotice()
		fs := flag.NewFlagSet("to-gsi", flag.ExitOnError)
		dir, out, sgsi := fs.String("project", ".", "port dir"), fs.String("output", "", "out zip"), fs.Bool("sgsi", false, "SGSI")
		_ = fs.Parse(args)
		for _, a := range args {
			if a == "-sgsi" {
				*sgsi = true
			}
		}
		p, err := project.Load(*dir)
		if err != nil {
			fatal(err)
		}
		if err := engine.ToGSI(p, *out, *sgsi); err != nil {
			fatal(err)
		}
		kind := "GSI"
		if *sgsi {
			kind = "SGSI"
		}
		fmt.Printf("%s ready: %s\n", kind, p.LastReport)
	case "metadata":
		fs := flag.NewFlagSet("metadata", flag.ExitOnError)
		dir := fs.String("project", ".", "port dir")
		_ = fs.String("set", "", "key=value (repeatable)")
		_ = fs.Parse(args)
		p, err := project.Load(*dir)
		if err != nil {
			fatal(err)
		}
		kv := parseSetArgs(args)
		if len(kv) > 0 {
			if err := p.SetMetadata(kv); err != nil {
				fatal(err)
			}
			fmt.Println("Metadata updated.")
		}
		printInfo(p)
	case "repack":
		memNotice()
		fs := flag.NewFlagSet("repack", flag.ExitOnError)
		dir, out := fs.String("project", ".", "port dir"), fs.String("output", "", "out zip")
		_ = fs.Parse(args)
		p, err := project.Load(*dir)
		if err != nil {
			fatal(err)
		}
		z, err := engine.Repack(p, *out)
		if err != nil {
			fatal(err)
		}
		fmt.Printf("Flashable zip: %s\n", z)
	case "refine":
		memNotice()
		fs := flag.NewFlagSet("refine", flag.ExitOnError)
		dir, perm := fs.String("project", ".", "port dir"), fs.Bool("permissive", false, "selinux permissive")
		_ = fs.Parse(args)
		p, err := project.Load(*dir)
		if err != nil {
			fatal(err)
		}
		notes, err := engine.Refine(p, *perm)
		for _, n := range notes {
			fmt.Printf("  * %s\n", n)
		}
		if err != nil {
			fatal(err)
		}
		fmt.Println("Refine complete — re-run check, then repack.")
	case "audit":
		fs := flag.NewFlagSet("audit", flag.ExitOnError)
		dir, deep := fs.String("project", ".", "port dir"), fs.Bool("deep", false, "unpack + HAL/fstab/modules audit (slow)")
		_ = fs.Parse(args)
		p, err := project.Load(*dir)
		if err != nil {
			fatal(err)
		}
		var rep *audit.Report
		if *deep {
			var derr error
			rep, derr = audit.DeepAudit(p)
			if derr != nil {
				fmt.Printf("deep audit unavailable: %v (file-level results below)\n", derr)
			}
			if rep == nil {
				rep = audit.Audit(p)
			}
		} else {
			rep = audit.Audit(p)
		}
		printAudit(p, rep)
	case "menu":
		fs := flag.NewFlagSet("menu", flag.ExitOnError)
		dir := fs.String("project", "", "port dir")
		_ = fs.Parse(args)
		runMenu(*dir)
	default:
		fmt.Printf("Unknown command: %s\n\n", cmd)
		usage()
		os.Exit(1)
	}
}

func printAudit(p *project.Project, rep *audit.Report) {
	counts := rep.Counts()
	fmt.Printf("Mismatch audit: %s | HARD_BRICK=%d NO_START=%d NO_BOOT=%d DEGRADED=%d INFO=%d\n",
		p.Dir, counts[audit.HardBrick], counts[audit.NoStart], counts[audit.NoBoot], counts[audit.Degraded], counts[audit.Info])
	bySev := rep.BySeverity()
	blockers := 0
	for _, sev := range audit.SortedSev() {
		for _, f := range bySev[sev] {
			mark := " "
			if f.AutoFixed {
				mark = "✓"
			}
			fmt.Printf("[%s][%s] %s: %s\n  next: %s\n", sev, mark, f.ID, f.Detail, f.Next)
			if !f.AutoFixed && (sev == audit.HardBrick || sev == audit.NoStart || sev == audit.NoBoot) {
				blockers++
			}
		}
	}
	data, _ := json.MarshalIndent(rep, "", "  ")
	_ = os.WriteFile(filepath.Join(p.Dir, "port_audit.json"), data, 0644)
	fmt.Printf("Saved %s\n", filepath.Join(p.Dir, "port_audit.json"))
	if blockers > 0 {
		fmt.Printf("RESULT: %d blocker(s) — do not flash until cleared.\n", blockers)
		os.Exit(1)
	}
	fmt.Println("RESULT: no blockers — remaining items are warnings/guidance.")
}

func fatal(err error) {
	fmt.Printf("Error: %v\n", err)
	os.Exit(1)
}

// lowerSelfPriority nices portlab itself (plus idle IO) so its in-process
// work — copies, zips, hashing GB images — never freezes the desktop.
// Children already spawn via nice/ionice wrappers. Best effort: failures
// are ignored, the tool just runs at normal priority.
func lowerSelfPriority() {
	util.SetSelfNice(10)
	util.SetSelfIdleIO()
}

// memNotice warns once on low-memory boxes (no-swap OOM freezes the PC).
func memNotice() {
	if w := util.WarnMem(2 << 30); w != "" {
		fmt.Printf("NOTE: %s\n", w)
	}
}

func mustLoad(args []string) *project.Project {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	dir := fs.String("project", ".", "port dir")
	_ = fs.Parse(args)
	p, err := project.Load(*dir)
	if err != nil {
		fatal(err)
	}
	return p
}

func parseSetArgs(args []string) map[string]string {
	kv := map[string]string{}
	for i := 0; i < len(args); i++ {
		if args[i] == "-set" && i+1 < len(args) {
			if parts := strings.SplitN(args[i+1], "=", 2); len(parts) == 2 {
				kv[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
		if strings.HasPrefix(args[i], "-set=") {
			if parts := strings.SplitN(strings.TrimPrefix(args[i], "-set="), "=", 2); len(parts) == 2 {
				kv[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
			}
		}
	}
	return kv
}

func printInfo(p *project.Project) {
	fmt.Printf("Project: %s\nName: %s  Device: %s  Mode: %s  Score: %d  Verdict: %s\n", p.Dir, p.Meta.Name, p.Meta.Device, p.Mode, p.Score, p.Verdict)
	fmt.Printf("Maintainer: %s  Version: %s\n", p.Meta.Maintainer, p.Meta.Version)
	if p.Meta.Changelog != "" {
		fmt.Printf("Changelog: %s\n", p.Meta.Changelog)
	}
	for _, k := range engine.SortedKeys(p.Meta.Extra) {
		fmt.Printf("  extra %s=%s\n", k, p.Meta.Extra[k])
	}
	fmt.Println()
	printFP("DONOR", &p.Donor)
	printFP("TARGET", &p.Base)
}

func printFP(label string, f *project.Fingerprint) {
	fmt.Printf("[%s] %s\n", label, f.SourcePath)
	fmt.Printf("  device=%s brand=%s model=%s platform=%s\n", f.Device, f.Brand, f.Model, f.Platform)
	fmt.Printf("  arch=%s sdk=%s vendor_sdk=%s vndk=%s android=%s\n", f.Arch, f.SDK, f.VendorSDK, f.VNDK, f.AndroidVersion)
	fmt.Printf("  treble=%s sar=%s ab=%s dynamic=%v avb=%v type=%s kernel=%s\n", f.Treble, f.SAR, f.ABUpdate, f.Dynamic, f.AVB, f.RomType, f.KernelVersion)
	fmt.Printf("  partitions=%s boot=%s\n", strings.Join(f.Partitions, ","), strings.Join(f.BootImages, ","))
	if len(f.UpdaterDevices) > 0 {
		fmt.Printf("  asserts=%s\n", strings.Join(f.UpdaterDevices, ","))
	}
}

func printCheck(p *project.Project) {
	rep := compat.Check(&p.Donor, &p.Base)
	fmt.Printf("Compatibility: %s (score %d) -> %s can_boot=%v\n", rep.Verdict, rep.Score, rep.SuggestedMode, rep.CanBoot)
	for _, r := range rep.Reasons {
		fmt.Printf("  - %s\n", r)
	}
	fmt.Println("Fixes:")
	for _, f := range rep.Fixes {
		fmt.Printf("  * %s\n", f)
	}
	fmt.Println("Boot readiness:")
	for _, c := range engine.BootChecks(p) {
		fmt.Printf("  [%s] %s: %s\n", c.Status, c.Name, c.Detail)
	}
}

func doctor() {
	fmt.Printf("Workspace: %s\n", util.WorkspaceRoot())
	fmt.Printf("romtool: %s\n", util.RomtoolBin())
	if _, err := os.Stat(util.RomtoolBin()); err != nil {
		fmt.Println("  MISSING — build romtool first (./build.sh). Portlab repack needs it.")
	} else {
		fmt.Println("  OK")
	}
	for _, t := range []string{"payload-dumper-go", "magiskboot", "simg2img", "zipalign", "7z"} {
		if _, err := os.Stat(filepath.Join(util.ToolkitBin(), t)); err == nil {
			fmt.Printf("  OK %s (toolkit)\n", t)
		} else if _, err := exec.LookPath(t); err == nil {
			fmt.Printf("  OK %s (PATH)\n", t)
		} else {
			fmt.Printf("  MISSING %s\n", t)
		}
	}
}

func runMenu(loaded string) {
	rd := bufio.NewReader(os.Stdin)
	read := func(pr string) string {
		fmt.Print(pr)
		t, _ := rd.ReadString('\n')
		return strings.TrimSpace(t)
	}
	cwd, _ := os.Getwd()
	var p *project.Project
	if loaded != "" {
		if l, err := project.Load(loaded); err == nil {
			p = l
		}
	}
	for {
		fmt.Println("\n=== PORTLAB ===")
		if p != nil {
			fmt.Printf("Project: %s | %s -> %s | %s/%d\n", p.Dir, p.Donor.Device, p.Base.Device, p.Verdict, p.Score)
		} else {
			fmt.Println("No project loaded.")
		}
		fmt.Println(" [1] Init (pick donor + target)")
		fmt.Println(" [2] Info")
		fmt.Println(" [3] Check (can it boot? GSI needed?)")
		fmt.Println(" [4] Make hybrid port")
		fmt.Println(" [5] Refine (unpack+patch+repack work)")
		fmt.Println(" [6] To GSI/SGSI")
		fmt.Println(" [7] Metadata")
		fmt.Println(" [8] Repack flashable zip")
		fmt.Println(" [9] Mismatch audit")
		fmt.Println(" [0] Exit")
		switch strings.ToLower(read("Select: ")) {
		case "1":
			donor := pickROM(read, cwd, "donor")
			base := pickROM(read, cwd, "target")
			out := read("Out dir [./ports/port-<time>]: ")
			if out == "" {
				out = "./ports/portlab-port"
			}
			name := read("Name (empty=auto): ")
			np, err := project.Init(donor, base, out, name)
			if err != nil {
				fmt.Println("init:", err)
				continue
			}
			rep := compat.Check(&np.Donor, &np.Base)
			np.Score, np.Verdict, np.Mode = rep.Score, string(rep.Verdict), rep.SuggestedMode
			_ = np.Save()
			p = np
			printCheck(p)
		case "2":
			if ensureP(read, &p) {
				printInfo(p)
			}
		case "3":
			if ensureP(read, &p) {
				printCheck(p)
			}
		case "4":
			if ensureP(read, &p) {
				mode := read("Mode [auto/direct/sgsi/gsi]: ")
				if mode == "" {
					mode = "auto"
				}
				perm := strings.ToLower(read("Permissive? [y/N]: ")) == "y"
				if err := engine.Make(p, engine.Options{Mode: mode, Permissive: perm, AvbDisable: true}); err != nil {
					fmt.Println("make:", err)
				} else {
					fmt.Println("Built work/. See port_report.json")
				}
			}
		case "5":
			if ensureP(read, &p) {
				perm := strings.ToLower(read("Permissive? [y/N]: ")) == "y"
				notes, err := engine.Refine(p, perm)
				for _, n := range notes {
					fmt.Println("  *", n)
				}
				if err != nil {
					fmt.Println("refine:", err)
				} else {
					fmt.Println("Refine complete.")
				}
			}
		case "6":
			if ensureP(read, &p) {
				sgsi := strings.ToLower(read("SGSI? [y/N]: ")) == "y"
				out := read("Out zip (empty=auto): ")
				if err := engine.ToGSI(p, out, sgsi); err != nil {
					fmt.Println("to-gsi:", err)
				} else {
					fmt.Println("Ready:", p.LastReport)
				}
			}
		case "7":
			if ensureP(read, &p) {
				m := map[string]string{}
				for {
					kv := read("key=value (empty=done): ")
					if kv == "" {
						break
					}
					if parts := strings.SplitN(kv, "=", 2); len(parts) == 2 {
						m[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
					}
				}
				if len(m) > 0 {
					_ = p.SetMetadata(m)
				}
				printInfo(p)
			}
		case "8":
			if ensureP(read, &p) {
				out := read("Out zip (empty=auto): ")
				z, err := engine.Repack(p, out)
				if err != nil {
					fmt.Println("repack:", err)
				} else {
					fmt.Println("Zip:", z)
				}
			}
		case "9":
			if ensureP(read, &p) {
				deep := strings.ToLower(read("Deep (unpack+HAL, slow)? [y/N]: ")) == "y"
				var rep *audit.Report
				if deep {
					var derr error
					rep, derr = audit.DeepAudit(p)
					if derr != nil {
						fmt.Println("deep:", derr)
					}
					if rep == nil {
						rep = audit.Audit(p)
					}
				} else {
					rep = audit.Audit(p)
				}
				printAudit(p, rep)
			}
		case "0", "q", "exit":
			return
		}
	}
}

func ensureP(read func(string) string, p **project.Project) bool {
	if *p != nil {
		return true
	}
	d := read("Project dir: ")
	if d == "" {
		return false
	}
	l, err := project.Load(d)
	if err != nil {
		fmt.Println(err)
		return false
	}
	*p = l
	return true
}

func pickROM(read func(string) string, workspace, role string) string {
	var cands []string
	for _, d := range []string{filepath.Join(workspace, "donors"), filepath.Join(workspace, "original images"), filepath.Join(workspace, "projects"), workspace} {
		ents, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range ents {
			full := filepath.Join(d, e.Name())
			if e.IsDir() {
				cands = append(cands, full)
			} else if strings.HasSuffix(strings.ToLower(e.Name()), ".zip") || strings.HasSuffix(strings.ToLower(e.Name()), ".bin") {
				cands = append(cands, full)
			}
		}
	}
	for i, c := range cands {
		fmt.Printf("  [%d] %s\n", i+1, c)
	}
	sel := read(fmt.Sprintf("%s (number or path): ", role))
	var n int
	if _, err := fmt.Sscanf(sel, "%d", &n); err == nil && n >= 1 && n <= len(cands) {
		return cands[n-1]
	}
	return sel
}
