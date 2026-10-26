package main

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// residentMin is how much VRAM a side must hold to count as having its model
// loaded. Below it is a CUDA context left after an unload, which an unload
// cannot free anyway.
const residentMin = 1024 // MiB

// vramProbe measures who really holds the GPU's memory, however they got it:
// nvidia-smi lists the processes using VRAM, and each one is attributed to the
// side whose upstream port it, or one of its ancestors, listens on (llama's
// model runs in a child of the router, simple-enhance's in a child of its
// server).
type vramProbe struct {
	smi    string                                             // nvidia-smi's path
	ports  map[int]string                                     // upstream port -> side
	models map[string]func(context.Context) ([]string, error) // optional: what a side has loaded
}

// vramUse is one process's VRAM; side is "" when it belongs to none of them.
type vramUse struct {
	side string
	pid  int
	comm string
	mib  int
}

type vramSnap struct {
	usedMiB, totalMiB int
	procs             []vramUse
	models            map[string][]string // per side, from vramProbe.models
}

func newVRAMProbe(upstreams map[string]string) *vramProbe {
	p := &vramProbe{smi: "nvidia-smi", ports: map[int]string{}, models: map[string]func(context.Context) ([]string, error){}}
	if path, err := exec.LookPath("nvidia-smi"); err == nil {
		p.smi = path
	} else if _, err := os.Stat("/usr/bin/nvidia-smi"); err == nil {
		p.smi = "/usr/bin/nvidia-smi"
	}
	for side, addr := range upstreams {
		if _, port, err := net.SplitHostPort(addr); err == nil {
			if n, err := strconv.Atoi(port); err == nil {
				p.ports[n] = side
			}
		}
	}
	return p
}

func (p *vramProbe) snapshot(ctx context.Context) (vramSnap, error) {
	var s vramSnap
	rows, err := p.query(ctx, "--query-gpu=memory.used,memory.total")
	if err != nil {
		return s, err
	}
	if len(rows) == 0 || len(rows[0]) < 2 {
		return s, fmt.Errorf("nvidia-smi: no GPU listed")
	}
	s.usedMiB, _ = strconv.Atoi(rows[0][0])
	s.totalMiB, _ = strconv.Atoi(rows[0][1])

	rows, err = p.query(ctx, "--query-compute-apps=pid,used_memory")
	if err != nil {
		return s, err
	}
	sockets := listeningSockets(p.ports)
	for _, r := range rows {
		if len(r) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(r[0])
		mib, err2 := strconv.Atoi(r[1])
		if err1 != nil || err2 != nil {
			continue
		}
		s.procs = append(s.procs, vramUse{side: ownerOf(pid, sockets), pid: pid, comm: comm(pid), mib: mib})
	}

	for side, models := range p.models {
		if s.byside()[side] == 0 {
			continue
		}
		if ids, err := models(ctx); err == nil && len(ids) > 0 {
			if s.models == nil {
				s.models = map[string][]string{}
			}
			s.models[side] = ids
		}
	}
	return s, nil
}

func (p *vramProbe) query(ctx context.Context, q string) ([][]string, error) {
	out, err := exec.CommandContext(ctx, p.smi, q, "--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %v", err)
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, ",")
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		rows = append(rows, f)
	}
	return rows, nil
}

// byside sums the VRAM of each side's processes.
func (s vramSnap) byside() map[string]int {
	m := map[string]int{}
	for _, u := range s.procs {
		m[u.side] += u.mib
	}
	return m
}

// holders lists the sides that have a model loaded.
func (s vramSnap) holders() []string {
	var hs []string
	for side, mib := range s.byside() {
		if side != "" && mib >= residentMin {
			hs = append(hs, side)
		}
	}
	return hs
}

// listeningSockets maps the inodes of the sockets listening on the given ports
// (from /proc/net/tcp and tcp6) to their side.
func listeningSockets(ports map[int]string) map[string]string {
	inodes := map[string]string{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(fh)
		sc.Scan() // header
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 10 || fields[3] != "0A" { // 0A is LISTEN
				continue
			}
			i := strings.LastIndexByte(fields[1], ':')
			port, err := strconv.ParseInt(fields[1][i+1:], 16, 32)
			if err != nil {
				continue
			}
			if side, ok := ports[int(port)]; ok {
				inodes["socket:["+fields[9]+"]"] = side
			}
		}
		fh.Close()
	}
	return inodes
}

// ownerOf walks from pid up through its parents and returns the side of the
// first one holding a listening socket of a side, or "".
func ownerOf(pid int, sockets map[string]string) string {
	for seen := 0; pid > 1 && seen < 64; seen++ {
		fds, _ := filepath.Glob(fmt.Sprintf("/proc/%d/fd/*", pid))
		for _, fd := range fds {
			if link, err := os.Readlink(fd); err == nil {
				if side, ok := sockets[link]; ok {
					return side
				}
			}
		}
		pid = ppid(pid)
	}
	return ""
}

func ppid(pid int) int {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// The command name can hold spaces and parentheses; the fields after
	// its closing paren are "state ppid ...".
	f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.Atoi(f[1])
	return n
}

func comm(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(b))
}
