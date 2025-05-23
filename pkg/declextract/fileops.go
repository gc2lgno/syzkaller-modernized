// Copyright 2024 syzkaller project authors. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.

package declextract

import (
	"fmt"
	"slices"
	"strings"

	"github.com/google/syzkaller/pkg/ast"
)

const (
	ioctlCmdArg = 1
	ioctlArgArg = 2
)

func (ctx *context) serializeFileOps() {
	for _, ioctl := range ctx.Ioctls {
		ctx.ioctls[ioctl.Name] = ioctl.Type
	}
	uniqueFuncs := ctx.resolveFopsCallbacks()
	fopsToFiles := ctx.mapFopsToFiles(uniqueFuncs)
	for _, fops := range ctx.FileOps {
		files := fopsToFiles[fops]
		canGenerate := Tristate(len(files) != 0)
		for _, op := range []*Function{fops.open, fops.read, fops.write, fops.mmap} {
			if op == nil {
				continue
			}
			if op == fops.open && (uniqueFuncs[fops.read] == 1 || uniqueFuncs[fops.write] == 1 ||
				uniqueFuncs[fops.mmap] == 1 || uniqueFuncs[fops.ioctl] == 1) {
				continue
			}
			ctx.noteInterface(&Interface{
				Type:             IfaceFileop,
				Name:             op.Name,
				Func:             op.Name,
				Files:            []string{op.File},
				AutoDescriptions: canGenerate,
			})
		}
		var ioctlCmds []string
		if fops.ioctl != nil {
			ioctlCmds = ctx.inferCommandVariants(fops.Ioctl, fops.SourceFile, ioctlCmdArg)
			for _, cmd := range ioctlCmds {
				ctx.noteInterface(&Interface{
					Type:             IfaceIoctl,
					Name:             cmd,
					IdentifyingConst: cmd,
					Files:            []string{fops.ioctl.File},
					Func:             fops.Ioctl,
					AutoDescriptions: canGenerate,
					scopeArg:         ioctlCmdArg,
					scopeVal:         cmd,
				})
			}
			if len(ioctlCmds) == 0 {
				ctx.noteInterface(&Interface{
					Type:             IfaceIoctl,
					Name:             fops.Ioctl,
					Files:            []string{fops.ioctl.File},
					Func:             fops.Ioctl,
					AutoDescriptions: canGenerate,
				})
			}
		}
		if len(files) == 0 {
			continue // each unmapped entry means some code we don't know how to cover yet
		}
		ctx.createFops(fops, files, ioctlCmds)
	}
}

func (ctx *context) createFops(fops *FileOps, files, ioctlCmds []string) {
	name := ctx.uniqualize("fops name", fops.Name)
	// If it has only open, then emit only openat that returns generic fd.
	fdt := "fd"
	if len(fops.ops) > 1 || fops.Open == "" {
		fdt = fmt.Sprintf("fd_%v", name)
		ctx.fmt("resource %v[fd]\n", fdt)
	}
	suffix := autoSuffix + "_" + name
	fileFlags := fmt.Sprintf("\"%s\"", files[0])
	if len(files) > 1 {
		fileFlags = fmt.Sprintf("%v_files", name)
		ctx.fmt("%v = ", fileFlags)
		for i, file := range files {
			ctx.fmt("%v \"%v\"", comma(i), file)
		}
		ctx.fmt("\n")
	}
	ctx.fmt("openat%v(fd const[AT_FDCWD], file ptr[in, string[%v]], flags flags[open_flags], mode const[0]) %v\n",
		suffix, fileFlags, fdt)
	if fops.Read != "" {
		ctx.fmt("read%v(fd %v, buf ptr[out, array[int8]], len bytesize[buf])\n", suffix, fdt)
	}
	if fops.Write != "" {
		ctx.fmt("write%v(fd %v, buf ptr[in, array[int8]], len bytesize[buf])\n", suffix, fdt)
	}
	if fops.Mmap != "" {
		ctx.fmt("mmap%v(addr vma, len len[addr], prot flags[mmap_prot],"+
			" flags flags[mmap_flags], fd %v, offset fileoff)\n", suffix, fdt)
	}
	if fops.Ioctl != "" {
		ctx.createIoctls(fops, ioctlCmds, suffix, fdt)
	}
	ctx.fmt("\n")
}

func (ctx *context) createIoctls(fops *FileOps, ioctlCmds []string, suffix, fdt string) {
	const defaultArgType = "ptr[in, array[int8]]"
	cmds := ctx.inferCommandVariants(fops.Ioctl, fops.SourceFile, ioctlCmdArg)
	if len(cmds) == 0 {
		retType := ctx.inferReturnType(fops.Ioctl, fops.SourceFile, -1, "")
		argType := ctx.inferArgType(fops.Ioctl, fops.SourceFile, ioctlArgArg, -1, "")
		if argType == "" {
			argType = defaultArgType
		}
		ctx.fmt("ioctl%v(fd %v, cmd intptr, arg %v) %v\n", suffix, fdt, argType, retType)
		return
	}
	for _, cmd := range cmds {
		argType := defaultArgType
		if typ := ctx.ioctls[cmd]; typ != nil {
			f := &Field{
				Name: strings.ToLower(cmd),
				Type: typ,
			}
			argType = ctx.fieldType(f, nil, "", false)
		} else {
			argType = ctx.inferArgType(fops.Ioctl, fops.SourceFile, ioctlArgArg, ioctlCmdArg, cmd)
			if argType == "" {
				argType = defaultArgType
			}
		}
		retType := ctx.inferReturnType(fops.Ioctl, fops.SourceFile, ioctlCmdArg, cmd)
		name := ctx.uniqualize("ioctl cmd", cmd)
		ctx.fmt("ioctl%v_%v(fd %v, cmd const[%v], arg %v) %v\n",
			autoSuffix, name, fdt, cmd, argType, retType)
	}
}

// mapFopsToFiles maps file_operations to actual file names.
func (ctx *context) mapFopsToFiles(uniqueFuncs map[*Function]int) map[*FileOps][]string {
	// Mapping turns out to be more of an art than science because
	// (1) there are lots of common callback functions that present in lots of file_operations
	// in different combinations, (2) some file operations are updated at runtime,
	// (3) some file operations are chained at runtime and we see callbacks from several
	// of them at the same time, (4) some callbacks are not reached (e.g. debugfs files
	// always have write callback, but can be installed without write permission).
	// If a callback that is present in only 1 file_operations is matched,
	// it's a stronger prioritization signal for that file_operations.

	funcToFops := make(map[*Function][]*FileOps)
	for _, fops := range ctx.FileOps {
		for _, fn := range fops.ops {
			funcToFops[fn] = append(funcToFops[fn], fops)
		}
	}
	// Maps file names to set of all callbacks that operations on the file has reached.
	fileToFuncs := make(map[string]map[*Function]bool)
	for _, file := range ctx.probe.Files {
		funcs := make(map[*Function]bool)
		fileToFuncs[file.Name] = funcs
		for _, pc := range file.Cover {
			fn := ctx.findFunc(ctx.probe.PCs[pc].Func, ctx.probe.PCs[pc].File)
			if len(funcToFops[fn]) != 0 {
				funcs[fn] = true
			}
		}
	}
	// This is a special entry for files that has only open callback
	// (it does not make sense to differentiate them further).
	generic := &FileOps{
		Name:    "generic",
		Open:    "only_open",
		fileOps: &fileOps{},
	}
	ctx.FileOps = append(ctx.FileOps, generic)
	fopsToFiles := make(map[*FileOps][]string)
	for _, file := range ctx.probe.Files {
		// There is a single non US-ASCII file in sysfs: "/sys/bus/pci/drivers/CAFÉ NAND".
		// Ignore it for now as descriptions shouldn't contain non US-ASCII chars.
		if ast.IsValidStringLit(file.Name) >= 0 {
			continue
		}
		// For each file figure out the potential file_operations that match this file best.
		best := ctx.mapFileToFops(fileToFuncs[file.Name], funcToFops, uniqueFuncs, generic)
		for _, fops := range best {
			fopsToFiles[fops] = append(fopsToFiles[fops], file.Name)
		}
	}
	for fops, files := range fopsToFiles {
		slices.Sort(files)
		fopsToFiles[fops] = files
	}
	return fopsToFiles
}

func (ctx *context) mapFileToFops(funcs map[*Function]bool, funcToFops map[*Function][]*FileOps,
	uniqueFuncs map[*Function]int, generic *FileOps) []*FileOps {
	// First collect all candidates (all file_operations for which at least 1 callback was triggered).
	candidates := ctx.fileCandidates(funcs, funcToFops, uniqueFuncs)
	if len(candidates) == 0 {
		candidates[generic] = 0
	}
	// Now find the best set of candidates.
	// There are lots of false positives due to common callback functions.
	maxScore := 0
	for fops := range candidates {
		ops := fops.ops
		// All else being equal prefer file_operations with more callbacks defined.
		score := len(ops)
		for _, fn := range ops {
			if !funcs[fn] {
				continue
			}
			// Matched callbacks increase the score.
			score += 10
			// If we matched ioctl, bump score by a lot.
			// We do want to emit ioctl's b/c they the only non-trivial
			// operations we emit at the moment.
			if fn == fops.ioctl {
				score += 100
			}
			// Unique callbacks are the strongest prioritization signal.
			// Besides some corner cases there is no way we can reach a unique callback
			// from a wrong file (a corner case would be in one callback calls another
			// callback directly).
			if uniqueFuncs[fn] == 1 {
				score += 1000
			}
		}
		candidates[fops] = score
		maxScore = max(maxScore, score)
	}
	// Now, take the candidates with the highest score (there still may be several of them).
	var best []*FileOps
	for fops, score := range candidates {
		if score == maxScore {
			best = append(best, fops)
		}
	}
	best = sortAndDedupSlice(best)
	// Now, filter out some excessive file_operations.
	// An example of an excessive case is if we have 2 file_operations with just read+write,
	// currently we emit generic read/write operations, so we would emit completly equal
	// descriptions for both. Ioctl commands is the only non-generic descriptions we emit now,
	// so if a file_operations has an ioctl handler, it won't be considered excessive.
	// Note that if we generate specialized descriptions for read/write/mmap in future,
	// then these won't be considered excessive as well.
	excessive := make(map[*FileOps]bool)
	for i := 0; i < len(best); i++ {
		for j := i + 1; j < len(best); j++ {
			a, b := best[i], best[j]
			if (a.Ioctl == b.Ioctl) &&
				(a.Read == "") == (b.Read == "") &&
				(a.Write == "") == (b.Write == "") &&
				(a.Mmap == "") == (b.Mmap == "") &&
				(a.Ioctl == "") == (b.Ioctl == "") {
				excessive[b] = true
			}
		}
	}
	// Finally record the file for the best non-excessive file_operations
	// (there are still can be several of them).
	best = slices.DeleteFunc(best, func(fops *FileOps) bool {
		return excessive[fops]
	})
	return best
}

func (ctx *context) fileCandidates(funcs map[*Function]bool, funcToFops map[*Function][]*FileOps,
	uniqueFuncs map[*Function]int) map[*FileOps]int {
	candidates := make(map[*FileOps]int)
	for fn := range funcs {
		for _, fops := range funcToFops[fn] {
			if fops.Open != "" && len(fops.ops) == 1 {
				// If it has only open, it's not very interesting
				// (we will use generic for it below).
				continue
			}
			hasUnique := false
			for _, fn := range fops.ops {
				if uniqueFuncs[fn] == 1 {
					hasUnique = true
				}
			}
			// If we've triggered at least one unique callback, we take this
			// file_operations in any case. Otherwise check if file_operations
			// has open/ioctl that we haven't triggered.
			// Note that it may have open/ioctl, and this is the right file_operations
			// for the file, yet we haven't triggered them for reasons described
			// in the beginning of the function.
			if !hasUnique {
				if fops.open != nil && !funcs[fops.open] {
					continue
				}
				if fops.ioctl != nil && !funcs[fops.ioctl] {
					continue
				}
			}
			candidates[fops] = 0
		}
	}
	return candidates
}

func (ctx *context) resolveFopsCallbacks() map[*Function]int {
	uniqueFuncs := make(map[*Function]int)
	for _, fops := range ctx.FileOps {
		fops.fileOps = &fileOps{
			open:  ctx.mustFindFunc(fops.Open, fops.SourceFile),
			read:  ctx.mustFindFunc(fops.Read, fops.SourceFile),
			write: ctx.mustFindFunc(fops.Write, fops.SourceFile),
			mmap:  ctx.mustFindFunc(fops.Mmap, fops.SourceFile),
			ioctl: ctx.mustFindFunc(fops.Ioctl, fops.SourceFile),
		}
		for _, op := range []*Function{fops.open, fops.read, fops.write, fops.mmap, fops.ioctl} {
			if op == nil {
				continue
			}
			fops.ops = append(fops.ops, op)
			uniqueFuncs[op]++
		}
	}
	return uniqueFuncs
}
