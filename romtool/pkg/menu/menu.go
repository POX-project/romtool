package menu

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"romtool/pkg/project"
	"romtool/pkg/toolkit"
)

const (
	ColorReset  = "\033[0m"
	ColorBold   = "\033[1m"
	ColorRed    = "\033[31m"
	ColorGreen  = "\033[32m"
	ColorYellow = "\033[33m"
	ColorBlue   = "\033[34m"
	ColorPurple = "\033[35m"
	ColorCyan   = "\033[36m"
	ColorWhite  = "\033[37m"
)

type Menu struct {
	Proj   *project.Project
	Tk     *toolkit.Toolkit
	Reader *bufio.Reader
}

func NewMenu(proj *project.Project, tk *toolkit.Toolkit) *Menu {
	return &Menu{
		Proj:   proj,
		Tk:     tk,
		Reader: bufio.NewReader(os.Stdin),
	}
}

func (m *Menu) readLine(prompt string) string {
	fmt.Print(prompt)
	text, _ := m.Reader.ReadString('\n')
	return strings.TrimSpace(text)
}

func (m *Menu) pause() {
	fmt.Print(ColorYellow + "\nPress Enter to continue..." + ColorReset)
	_, _ = m.Reader.ReadString('\n')
}

// Run launches the interactive loop.
func (m *Menu) Run() {
	for {
		m.PrintDashboard()
		m.PrintOptions()

		choice := strings.ToUpper(m.readLine(ColorBold + "Select an option: " + ColorReset))

		switch choice {
		case "1":
			m.ShowDetailedStatus()
			m.pause()

		case "2":
			fmt.Println(ColorCyan + "\n--- Unpacking All Partitions & Boot Images ---" + ColorReset)
			if err := m.Proj.UnpackAll(); err != nil {
				fmt.Printf(ColorRed+"Error: %v\n"+ColorReset, err)
			} else {
				fmt.Println(ColorGreen + "All components unpacked successfully!" + ColorReset)
			}
			m.pause()

		case "3":
			m.UnpackPartitionInteractive()
			m.pause()

		case "4":
			m.UnpackBootInteractive()
			m.pause()

		case "5":
			m.CheckModifications()
			m.pause()

		case "6":
			fmt.Println(ColorCyan + "\n--- Repacking Modified Components ---" + ColorReset)
			repacked, err := m.Proj.RepackModified(false)
			if err != nil {
				fmt.Printf(ColorRed+"Error repacking: %v\n"+ColorReset, err)
			} else if len(repacked) == 0 {
				fmt.Println(ColorYellow + "No modifications detected. Everything is up to date." + ColorReset)
			} else {
				fmt.Printf(ColorGreen+"Successfully repacked: %s\n"+ColorReset, strings.Join(repacked, ", "))
			}
			m.pause()

		case "7":
			m.RepackPartitionInteractive()
			m.pause()

		case "8":
			m.RepackBootInteractive()
			m.pause()

		case "9":
			fmt.Println(ColorCyan + "\n--- Building Flashable ROM Zip ---" + ColorReset)
			outName := m.readLine("Enter output zip path [press Enter for default in output/]: ")
			if err := m.Proj.RepackRomZip(outName); err != nil {
				fmt.Printf(ColorRed+"Error building ROM zip: %v\n"+ColorReset, err)
			}
			m.pause()

		case "D":
			m.RunDoctor()
			m.pause()

		case "0", "Q", "EXIT":
			fmt.Println(ColorGreen + "Exiting ROM Tool. Goodbye!" + ColorReset)
			return

		default:
			fmt.Println(ColorRed + "Invalid option. Please try again." + ColorReset)
		}
	}
}

func (m *Menu) PrintDashboard() {
	fmt.Print("\033[H\033[2J") // Clear screen
	fmt.Println(ColorBold + ColorCyan + "==================================================================" + ColorReset)
	fmt.Println(ColorBold + ColorCyan + "              ANDROID ROM TOOLKIT & REPACKER                      " + ColorReset)
	fmt.Println(ColorBold + ColorCyan + "==================================================================" + ColorReset)

	man := m.Proj.Manifest
	dynTag := ColorGreen + "[DYNAMIC ROM]" + ColorReset
	if !man.IsDynamic {
		dynTag = ColorYellow + "[NON-DYNAMIC ROM]" + ColorReset
	}

	fmt.Printf(" Project:  %s%s%s\n", ColorBold, m.Proj.Dir, ColorReset)
	fmt.Printf(" ROM Name: %s%s%s %s\n", ColorBold+ColorWhite, man.RomName, ColorReset, dynTag)
	fmt.Printf(" Format:   %s%s%s | JSON Tracker: %s%s%s\n", ColorPurple, man.RomType, ColorReset, ColorGreen, project.ProjectConfigFile, ColorReset)

	if man.IsDynamic && man.DynamicConfig != nil {
		for _, g := range man.DynamicConfig.Groups {
			var groupTotal int64
			for _, p := range g.Partitions {
				groupTotal += man.DynamicConfig.Partitions[p]
			}
			maxMB := float64(g.MaxSize) / (1024 * 1024)
			curMB := float64(groupTotal) / (1024 * 1024)
			fmt.Printf(" Dynamic Group: %s%s%s (%.1f MB / %.1f MB max)\n", ColorCyan, g.Name, ColorReset, curMB, maxMB)
		}
	}

	fmt.Println(ColorCyan + "------------------------------------------------------------------" + ColorReset)
	fmt.Printf("%-14s %-8s %-8s %-10s %-10s %-12s\n", "PARTITION", "FORMAT", "FS", "SIZE", "UNPACKED", "STATUS")
	fmt.Println(ColorCyan + "------------------------------------------------------------------" + ColorReset)

	var partNames []string
	for name := range man.Partitions {
		partNames = append(partNames, name)
	}
	sort.Strings(partNames)

	for _, name := range partNames {
		p := man.Partitions[name]
		sizeMB := float64(p.CurrentSize) / (1024 * 1024)
		if sizeMB == 0 && p.OriginalSize > 0 {
			sizeMB = float64(p.OriginalSize) / (1024 * 1024)
		}

		unpackedStr := ColorRed + "No" + ColorReset
		statusStr := "Clean"

		if p.Unpacked {
			unpackedStr = ColorGreen + "Yes" + ColorReset
			modified, _ := m.Proj.IsPartitionModified(name)
			if modified {
				statusStr = ColorYellow + ColorBold + "MODIFIED" + ColorReset
			} else if p.Repacked {
				statusStr = ColorGreen + "Repacked" + ColorReset
			} else {
				statusStr = ColorCyan + "Unpacked" + ColorReset
			}
		}

		fsStr := p.FsType
		if fsStr == "" {
			fsStr = "-"
		}

		fmt.Printf("%-14s %-8s %-8s %-10.1f %-19s %s\n",
			name, p.Format, fsStr, sizeMB, unpackedStr, statusStr)
	}

	if len(man.BootImages) > 0 {
		fmt.Println(ColorCyan + "------------------------------------------------------------------" + ColorReset)
		fmt.Printf("%-14s %-8s %-8s %-10s %-10s %-12s\n", "BOOT IMAGE", "SOURCE", "RAMDISK", "KERNEL", "UNPACKED", "STATUS")
		fmt.Println(ColorCyan + "------------------------------------------------------------------" + ColorReset)
		for name, b := range man.BootImages {
			unpackedStr := ColorRed + "No" + ColorReset
			statusStr := "Original"
			if b.Unpacked {
				unpackedStr = ColorGreen + "Yes" + ColorReset
				modified, _ := m.Proj.IsBootModified(name)
				if modified {
					statusStr = ColorYellow + ColorBold + "MODIFIED" + ColorReset
				} else if b.Repacked {
					statusStr = ColorGreen + "Repacked" + ColorReset
				} else {
					statusStr = ColorCyan + "Unpacked" + ColorReset
				}
			}

			ramdiskStr := "-"
			if b.HasRamdisk {
				ramdiskStr = "Yes"
			}
			kernelStr := "-"
			if b.HasKernel {
				kernelStr = "Yes"
			}

			fmt.Printf("%-14s %-8s %-8s %-10s %-19s %s\n",
				name, b.SourceFile, ramdiskStr, kernelStr, unpackedStr, statusStr)
		}
	}
	fmt.Println(ColorCyan + "==================================================================" + ColorReset)
}

func (m *Menu) PrintOptions() {
	fmt.Println(ColorBold + "Actions:" + ColorReset)
	fmt.Println(" [1] Detailed Project & File Overview")
	fmt.Println(" [2] Unpack All (Partitions + Boot Images)")
	fmt.Println(" [3] Unpack Specific Partition (e.g. system, vendor, product, odm)")
	fmt.Println(" [4] Unpack Boot Image (kernel & editable ramdisk)")
	fmt.Println(" [5] Check for Modifications in partitions/ and boot/")
	fmt.Println(" [6] " + ColorGreen + "Repack Modified Partitions & Boot (Dynamic recalculation)" + ColorReset)
	fmt.Println(" [7] Repack Specific Partition")
	fmt.Println(" [8] Repack Boot Image")
	fmt.Println(" [9] " + ColorBold + ColorCyan + "Build Flashable ROM Zip (Zipalign included)" + ColorReset)
	fmt.Println(" [D] Doctor / Check Toolkit & Tools")
	fmt.Println(" [0] Exit")
	fmt.Println()
}

func (m *Menu) ShowDetailedStatus() {
	fmt.Println(ColorCyan + "\n--- Detailed Project Status ---" + ColorReset)
	fmt.Printf("Workspace:     %s\n", m.Proj.Dir)
	fmt.Printf("ROM Root Dir:  %s\n", m.Proj.RomRootDir())
	fmt.Printf("Partitions:    %s\n", m.Proj.PartitionsDir())
	fmt.Printf("Boot Dir:      %s\n", m.Proj.BootDir())
	fmt.Printf("Raw Imgs:      %s\n", m.Proj.RawImagesDir())
	fmt.Printf("Output Dir:    %s\n", m.Proj.OutputDir())
	fmt.Println("\nNote: You can edit files in the 'partitions/' and 'boot/' folders directly.")
	fmt.Println("When done, choose option [6] or [9] to automatically detect changes and repack!")
}

func (m *Menu) UnpackPartitionInteractive() {
	fmt.Println("\nAvailable partitions:")
	var names []string
	for n := range m.Proj.Manifest.Partitions {
		names = append(names, n)
	}
	sort.Strings(names)
	for i, n := range names {
		fmt.Printf(" [%d] %s\n", i+1, n)
	}

	choice := m.readLine("Enter partition name or number: ")
	var target string
	for i, n := range names {
		if choice == fmt.Sprintf("%d", i+1) || strings.EqualFold(choice, n) {
			target = n
			break
		}
	}

	if target == "" {
		fmt.Println(ColorRed + "Invalid partition selection." + ColorReset)
		return
	}

	fmt.Printf(ColorCyan+"Unpacking partition %s...\n"+ColorReset, target)
	if err := m.Proj.UnpackPartition(target); err != nil {
		fmt.Printf(ColorRed+"Error: %v\n"+ColorReset, err)
	} else {
		fmt.Printf(ColorGreen+"Partition %s unpacked to %s\n"+ColorReset, target, filepath.Join(m.Proj.PartitionsDir(), target))
	}
}

func (m *Menu) RepackPartitionInteractive() {
	fmt.Println("\nUnpacked partitions:")
	var names []string
	for n, p := range m.Proj.Manifest.Partitions {
		if p.Unpacked {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		fmt.Println(ColorYellow + "No partitions have been unpacked yet." + ColorReset)
		return
	}

	sort.Strings(names)
	for i, n := range names {
		fmt.Printf(" [%d] %s\n", i+1, n)
	}

	choice := m.readLine("Enter partition name or number to repack: ")
	var target string
	for i, n := range names {
		if choice == fmt.Sprintf("%d", i+1) || strings.EqualFold(choice, n) {
			target = n
			break
		}
	}

	if target == "" {
		fmt.Println(ColorRed + "Invalid partition selection." + ColorReset)
		return
	}

	fmt.Printf(ColorCyan+"Repacking partition %s...\n"+ColorReset, target)
	if err := m.Proj.RepackPartition(target); err != nil {
		fmt.Printf(ColorRed+"Error: %v\n"+ColorReset, err)
	} else {
		fmt.Printf(ColorGreen+"Partition %s successfully repacked!\n"+ColorReset, target)
	}
}

func (m *Menu) UnpackBootInteractive() {
	for name := range m.Proj.Manifest.BootImages {
		fmt.Printf(ColorCyan+"Unpacking %s...\n"+ColorReset, name)
		if err := m.Proj.UnpackBootImage(name); err != nil {
			fmt.Printf(ColorRed+"Error unpacking %s: %v\n"+ColorReset, name, err)
		} else {
			fmt.Printf(ColorGreen+"%s unpacked to %s\n"+ColorReset, name, filepath.Join(m.Proj.BootDir(), name))
		}
	}
}

func (m *Menu) RepackBootInteractive() {
	for name := range m.Proj.Manifest.BootImages {
		fmt.Printf(ColorCyan+"Repacking %s...\n"+ColorReset, name)
		if err := m.Proj.RepackBootImage(name); err != nil {
			fmt.Printf(ColorRed+"Error repacking %s: %v\n"+ColorReset, name, err)
		} else {
			fmt.Printf(ColorGreen+"%s repacked successfully!\n"+ColorReset, name)
		}
	}
}

func (m *Menu) CheckModifications() {
	fmt.Println(ColorCyan + "\n--- Checking for Modifications ---" + ColorReset)
	var modifiedCount int

	for name, p := range m.Proj.Manifest.Partitions {
		if !p.Unpacked {
			continue
		}
		mod, err := m.Proj.IsPartitionModified(name)
		if err != nil {
			fmt.Printf(" [%s] Error checking: %v\n", name, err)
		} else if mod {
			fmt.Printf(" [%s] %sMODIFIED%s (Files changed in %s)\n", name, ColorYellow+ColorBold, ColorReset, p.UnpackedDir)
			modifiedCount++
		} else {
			fmt.Printf(" [%s] %sClean%s\n", name, ColorGreen, ColorReset)
		}
	}

	for name, b := range m.Proj.Manifest.BootImages {
		if !b.Unpacked {
			continue
		}
		mod, err := m.Proj.IsBootModified(name)
		if err != nil {
			fmt.Printf(" [%s] Error checking: %v\n", name, err)
		} else if mod {
			fmt.Printf(" [%s] %sMODIFIED%s (Files changed in %s)\n", name, ColorYellow+ColorBold, ColorReset, b.RamdiskDir)
			modifiedCount++
		} else {
			fmt.Printf(" [%s] %sClean%s\n", name, ColorGreen, ColorReset)
		}
	}

	if modifiedCount == 0 {
		fmt.Println(ColorGreen + "\nNo modified files detected." + ColorReset)
	} else {
		fmt.Printf(ColorYellow+"\nDetected %d modified component(s). Run option [6] or [9] to repack them.\n"+ColorReset, modifiedCount)
	}
}

func (m *Menu) RunDoctor() {
	fmt.Println(ColorCyan + "\n--- Toolkit & Environment Diagnostics ---" + ColorReset)
	fmt.Printf("Toolkit Dir: %s\n\n", m.Tk.BinDir)

	results := m.Tk.CheckTools()
	var names []string
	for n := range results {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, name := range names {
		s := results[name]
		if s.Available {
			fmt.Printf("  [%s✓%s] %-14s %s\n", ColorGreen, ColorReset, name, s.Path)
		} else {
			fmt.Printf("  [%s✗%s] %-14s %sNOT FOUND%s\n", ColorRed, ColorReset, name, ColorRed, ColorReset)
		}
	}
}
