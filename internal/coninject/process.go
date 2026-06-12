package coninject

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type procRow struct {
	ppid uint32
	name string
}

func snapshotProcs() (map[uint32]procRow, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("CreateToolhelp32Snapshot: %w", err)
	}
	defer windows.CloseHandle(snap)

	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	if err := windows.Process32First(snap, &e); err != nil {
		return nil, fmt.Errorf("Process32First: %w", err)
	}

	procs := make(map[uint32]procRow, 256)
	for {
		procs[e.ProcessID] = procRow{
			ppid: e.ParentProcessID,
			name: windows.UTF16ToString(e.ExeFile[:]),
		}
		if err := windows.Process32Next(snap, &e); err != nil {
			break
		}
	}
	return procs, nil
}

// FindClaudePID walks the parent chain from the current process up to the first
// claude.exe ancestor and returns its PID. The status line is a child of Claude
// Code, so this resolves the console-owning process to inject into.
func FindClaudePID() (uint32, error) {
	procs, err := snapshotProcs()
	if err != nil {
		return 0, err
	}
	pid := windows.GetCurrentProcessId()
	for i := 0; i < 64; i++ {
		row, ok := procs[pid]
		if !ok {
			break
		}
		if strings.EqualFold(row.name, "claude.exe") {
			return pid, nil
		}
		pid = row.ppid
	}
	return 0, fmt.Errorf("no claude.exe ancestor found")
}

// IsAlive reports whether a process with the given PID currently exists.
func IsAlive(pid uint32) bool {
	procs, err := snapshotProcs()
	if err != nil {
		return false
	}
	_, ok := procs[pid]
	return ok
}

// ListClaudePIDs returns the PIDs of every running claude.exe.
func ListClaudePIDs() ([]uint32, error) {
	procs, err := snapshotProcs()
	if err != nil {
		return nil, err
	}
	var pids []uint32
	for pid, row := range procs {
		if strings.EqualFold(row.name, "claude.exe") {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// FindBlockedClaude locates the claude.exe to act on when no PID is given. With
// a single instance it returns that one. With several it does not guess: it
// returns an error listing each PID and its detected state, so the user can
// re-run with an explicit -pid.
func FindBlockedClaude() (uint32, error) {
	pids, err := ListClaudePIDs()
	if err != nil {
		return 0, err
	}
	switch len(pids) {
	case 0:
		return 0, fmt.Errorf("no claude.exe process found")
	case 1:
		return pids[0], nil
	}

	sort.Slice(pids, func(i, j int) bool { return pids[i] < pids[j] })
	var b strings.Builder
	fmt.Fprintf(&b, "found %d claude.exe; choose one with -pid <PID>:", len(pids))
	for _, pid := range pids {
		state := "unreadable"
		if screen, err := ReadScreen(pid); err == nil {
			state = Classify(screen).String()
		}
		fmt.Fprintf(&b, "\n  -pid %d  (%s)", pid, state)
	}
	return 0, errors.New(b.String())
}
