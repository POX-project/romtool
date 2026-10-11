package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"romtool/pkg/menu"
	"romtool/pkg/project"
	"romtool/pkg/sdat"
	"romtool/pkg/toolkit"
)

const version = "1.0.0"

func printUsage() {
	fmt.Printf(`Android ROM Repacker Tool v%s

USAGE:
  romtool [command] [options]

COMMANDS:
  menu                  Launch interactive TUI dashboard (default if no command)
  init                  Initialize a new project from a ROM zip or folder
  status                Show current project status and dynamic/non-dynamic info
  check                 Check for user modifications in partitions/ and boot/
  unpack-all            Unpack all partitions and boot images
  unpack-partition      Unpack a specific partition (e.g. system, vendor, odm)
  unpack-boot           Unpack boot/recovery image (kernel + editable ramdisk)
  repack                Repack modified partitions and boot images
  repack-partition      Repack a specific partition
  repack-boot           Repack boot image
  repack-rom            Repack modified components and build flashable ROM zip
  doctor                Check portable dev toolkit status and environment
  sdat2img              Convert Android .transfer.list + .new.dat to raw .img
  img2sdat              Convert raw .img to Android .transfer.list + .new.dat

OPTIONS:
  -project <dir>        Path to project directory (default: current directory)
  -rom <path>           Path to ROM zip file (required for init)
  -name <name>          Partition name (e.g. system, vendor, product, odm)
  -output <path>        Output zip path for repack-rom
  -toolkit <dir>        Custom toolkit bin directory
  -all                  Force repack all partitions, not just modified ones

EXAMPLES:
  # Initialize project from ROM zip:
  romtool init -rom /path/to/rom.zip -dir ./my_rom_project

  # Open interactive menu:
  romtool menu -project ./my_rom_project

  # Unpack odm partition:
  romtool unpack-partition -project ./my_rom_project -name odm

  # After modifying files in ./my_rom_project/partitions/odm/:
  romtool check -project ./my_rom_project
  romtool repack -project ./my_rom_project

  # Build final flashable ROM zip:
  romtool repack-rom -project ./my_rom_project -output ./output/custom_rom.zip
`, version)
}

func main() {
	if len(os.Args) < 2 {
		runDefaultMenu()
		return
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "help", "-h", "--help":
		printUsage()
		return

	case "doctor":
		tk := toolkit.FindToolkit("")
		m := menu.NewMenu(nil, tk)
		m.RunDoctor()
		return

	case "sdat2img":
		if len(args) < 3 {
			fmt.Println("Usage: romtool sdat2img <transfer.list> <new.dat> <output.img>")
			os.Exit(1)
		}
		if err := sdat.Sdat2Img(args[0], args[1], args[2]); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Successfully converted to %s\n", args[2])
		return

	case "img2sdat":
		fs := flag.NewFlagSet("img2sdat", flag.ExitOnError)
		nameFlag := fs.String("name", "system", "Partition name")
		imgFlag := fs.String("img", "", "Path to input .img file")
		outFlag := fs.String("out", "./output", "Output directory")
		_ = fs.Parse(args)

		if *imgFlag == "" {
			fmt.Println("Error: -img is required")
			os.Exit(1)
		}
		res, err := sdat.Img2Sdat(*imgFlag, *outFlag, *nameFlag, 1024)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Successfully created %s.transfer.list and %s.new.dat (%d blocks, %d bytes)\n",
			*nameFlag, *nameFlag, res.TotalBlocks, res.ImageSize)
		return

	case "init":
		fs := flag.NewFlagSet("init", flag.ExitOnError)
		romFlag := fs.String("rom", "", "Path to ROM zip or directory")
		dirFlag := fs.String("dir", "", "Project directory")
		tkFlag := fs.String("toolkit", "", "Toolkit directory")
		_ = fs.Parse(args)

		if *romFlag == "" {
			fmt.Println("Error: -rom is required for init")
			os.Exit(1)
		}
		if *dirFlag == "" {
			*dirFlag = strings.TrimSuffix(filepath.Base(*romFlag), filepath.Ext(*romFlag)) + "_proj"
		}

		tk := toolkit.FindToolkit(*tkFlag)
		proj, err := project.Init(tk, *romFlag, *dirFlag)
		if err != nil {
			fmt.Printf("Error initializing project: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("\nProject initialized successfully at %s!\n", proj.Dir)
		fmt.Printf("Dynamic ROM: %v (%s)\n", proj.Manifest.IsDynamic, proj.Manifest.RomType)
		fmt.Printf("Run 'romtool menu -project %s' to start editing!\n", proj.Dir)
		return

	case "menu":
		fs := flag.NewFlagSet("menu", flag.ExitOnError)
		dirFlag := fs.String("project", ".", "Project directory")
		tkFlag := fs.String("toolkit", "", "Toolkit directory")
		_ = fs.Parse(args)

		tk := toolkit.FindToolkit(*tkFlag)
		proj, err := project.Load(tk, *dirFlag)
		if err != nil {
			fmt.Printf("Error loading project: %v\n", err)
			os.Exit(1)
		}
		m := menu.NewMenu(proj, tk)
		m.Run()
		return

	case "status":
		fs := flag.NewFlagSet("status", flag.ExitOnError)
		dirFlag := fs.String("project", ".", "Project directory")
		tkFlag := fs.String("toolkit", "", "Toolkit directory")
		_ = fs.Parse(args)

		tk := toolkit.FindToolkit(*tkFlag)
		proj, err := project.Load(tk, *dirFlag)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		m := menu.NewMenu(proj, tk)
		m.PrintDashboard()
		return

	case "check":
		fs := flag.NewFlagSet("check", flag.ExitOnError)
		dirFlag := fs.String("project", ".", "Project directory")
		tkFlag := fs.String("toolkit", "", "Toolkit directory")
		_ = fs.Parse(args)

		tk := toolkit.FindToolkit(*tkFlag)
		proj, err := project.Load(tk, *dirFlag)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		m := menu.NewMenu(proj, tk)
		m.CheckModifications()
		return

	case "unpack-all":
		fs := flag.NewFlagSet("unpack-all", flag.ExitOnError)
		dirFlag := fs.String("project", ".", "Project directory")
		tkFlag := fs.String("toolkit", "", "Toolkit directory")
		_ = fs.Parse(args)

		tk := toolkit.FindToolkit(*tkFlag)
		proj, err := project.Load(tk, *dirFlag)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		if err := proj.UnpackAll(); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("All components unpacked successfully!")
		return

	case "unpack-partition":
		fs := flag.NewFlagSet("unpack-partition", flag.ExitOnError)
		dirFlag := fs.String("project", ".", "Project directory")
		nameFlag := fs.String("name", "", "Partition name")
		tkFlag := fs.String("toolkit", "", "Toolkit directory")
		_ = fs.Parse(args)

		if *nameFlag == "" {
			fmt.Println("Error: -name is required")
			os.Exit(1)
		}
		tk := toolkit.FindToolkit(*tkFlag)
		proj, err := project.Load(tk, *dirFlag)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		if err := proj.UnpackPartition(*nameFlag); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Partition %s unpacked successfully to %s!\n", *nameFlag, filepath.Join(proj.PartitionsDir(), *nameFlag))
		return

	case "unpack-boot":
		fs := flag.NewFlagSet("unpack-boot", flag.ExitOnError)
		dirFlag := fs.String("project", ".", "Project directory")
		tkFlag := fs.String("toolkit", "", "Toolkit directory")
		_ = fs.Parse(args)

		tk := toolkit.FindToolkit(*tkFlag)
		proj, err := project.Load(tk, *dirFlag)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		for name := range proj.Manifest.BootImages {
			if err := proj.UnpackBootImage(name); err != nil {
				fmt.Printf("Error unpacking %s: %v\n", name, err)
			}
		}
		fmt.Println("Boot images unpacked successfully!")
		return

	case "repack":
		fs := flag.NewFlagSet("repack", flag.ExitOnError)
		dirFlag := fs.String("project", ".", "Project directory")
		allFlag := fs.Bool("all", false, "Force repack all partitions")
		tkFlag := fs.String("toolkit", "", "Toolkit directory")
		_ = fs.Parse(args)

		tk := toolkit.FindToolkit(*tkFlag)
		proj, err := project.Load(tk, *dirFlag)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		repacked, err := proj.RepackModified(*allFlag)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		if len(repacked) == 0 {
			fmt.Println("No modifications detected. Everything is up to date.")
		} else {
			fmt.Printf("Repacked: %s\n", strings.Join(repacked, ", "))
		}
		return

	case "repack-partition":
		fs := flag.NewFlagSet("repack-partition", flag.ExitOnError)
		dirFlag := fs.String("project", ".", "Project directory")
		nameFlag := fs.String("name", "", "Partition name")
		tkFlag := fs.String("toolkit", "", "Toolkit directory")
		_ = fs.Parse(args)

		if *nameFlag == "" {
			fmt.Println("Error: -name is required")
			os.Exit(1)
		}
		tk := toolkit.FindToolkit(*tkFlag)
		proj, err := project.Load(tk, *dirFlag)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		if err := proj.RepackPartition(*nameFlag); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Partition %s repacked successfully!\n", *nameFlag)
		return

	case "repack-boot":
		fs := flag.NewFlagSet("repack-boot", flag.ExitOnError)
		dirFlag := fs.String("project", ".", "Project directory")
		tkFlag := fs.String("toolkit", "", "Toolkit directory")
		_ = fs.Parse(args)

		tk := toolkit.FindToolkit(*tkFlag)
		proj, err := project.Load(tk, *dirFlag)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		for name := range proj.Manifest.BootImages {
			if err := proj.RepackBootImage(name); err != nil {
				fmt.Printf("Error repacking %s: %v\n", name, err)
			}
		}
		fmt.Println("Boot images repacked successfully!")
		return

	case "repack-rom":
		fs := flag.NewFlagSet("repack-rom", flag.ExitOnError)
		dirFlag := fs.String("project", ".", "Project directory")
		outFlag := fs.String("output", "", "Output zip path")
		tkFlag := fs.String("toolkit", "", "Toolkit directory")
		_ = fs.Parse(args)

		tk := toolkit.FindToolkit(*tkFlag)
		proj, err := project.Load(tk, *dirFlag)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		if err := proj.RepackRomZip(*outFlag); err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}
		return

	default:
		fmt.Printf("Unknown command: %s\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func runDefaultMenu() {
	// If current directory contains rom_project.json, load it
	if _, err := os.Stat(project.ProjectConfigFile); err == nil {
		tk := toolkit.FindToolkit("")
		proj, err := project.Load(tk, ".")
		if err == nil {
			m := menu.NewMenu(proj, tk)
			m.Run()
			return
		}
	}

	printUsage()
}
