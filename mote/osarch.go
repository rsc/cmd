// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"debug/buildinfo"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"fmt"
)

// binaryOSArch reports the GOOS and GOARCH that the binary file
// was built for. For Go binaries it reads the embedded build info;
// for other binaries it makes a best effort based on the executable
// file format.
func binaryOSArch(file string) (goos, goarch string, err error) {
	if bi, err := buildinfo.ReadFile(file); err == nil {
		for _, s := range bi.Settings {
			switch s.Key {
			case "GOOS":
				goos = s.Value
			case "GOARCH":
				goarch = s.Value
			}
		}
		if goos != "" && goarch != "" {
			return goos, goarch, nil
		}
	}
	if f, err := elf.Open(file); err == nil {
		defer f.Close()
		goos = "linux"
		switch f.OSABI {
		case elf.ELFOSABI_FREEBSD:
			goos = "freebsd"
		case elf.ELFOSABI_NETBSD:
			goos = "netbsd"
		case elf.ELFOSABI_OPENBSD:
			goos = "openbsd"
		case elf.ELFOSABI_SOLARIS:
			goos = "solaris"
		}
		switch f.Machine {
		case elf.EM_X86_64:
			goarch = "amd64"
		case elf.EM_386:
			goarch = "386"
		case elf.EM_AARCH64:
			goarch = "arm64"
		case elf.EM_ARM:
			goarch = "arm"
		case elf.EM_RISCV:
			goarch = "riscv64"
		case elf.EM_S390:
			goarch = "s390x"
		case elf.EM_LOONGARCH:
			goarch = "loong64"
		case elf.EM_PPC64:
			goarch = "ppc64"
			if f.ByteOrder == binary.LittleEndian {
				goarch = "ppc64le"
			}
		default:
			return "", "", fmt.Errorf("%s: unrecognized ELF machine %v", file, f.Machine)
		}
		return goos, goarch, nil
	}
	if f, err := macho.Open(file); err == nil {
		defer f.Close()
		goos = "darwin"
		switch f.Cpu {
		case macho.CpuAmd64:
			goarch = "amd64"
		case macho.CpuArm64:
			goarch = "arm64"
		default:
			return "", "", fmt.Errorf("%s: unrecognized Mach-O cpu %v", file, f.Cpu)
		}
		return goos, goarch, nil
	}
	if f, err := pe.Open(file); err == nil {
		defer f.Close()
		goos = "windows"
		switch f.Machine {
		case pe.IMAGE_FILE_MACHINE_AMD64:
			goarch = "amd64"
		case pe.IMAGE_FILE_MACHINE_I386:
			goarch = "386"
		case pe.IMAGE_FILE_MACHINE_ARM64:
			goarch = "arm64"
		default:
			return "", "", fmt.Errorf("%s: unrecognized PE machine %#x", file, f.Machine)
		}
		return goos, goarch, nil
	}
	return "", "", fmt.Errorf("%s: cannot determine GOOS/GOARCH", file)
}
