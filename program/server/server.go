// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package server provides RPC access to a local program being debugged.
// It is the remote end of the client implementation of the Program interface.
package server

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"code.google.com/p/ogle/gosym"

	"code.google.com/p/ogle/debug/dwarf"
	"code.google.com/p/ogle/debug/elf"
	"code.google.com/p/ogle/debug/macho"

	"code.google.com/p/ogle/arch"
	"code.google.com/p/ogle/program"
	"code.google.com/p/ogle/program/proxyrpc"
)

type breakpoint struct {
	pc        uint64
	origInstr [arch.MaxBreakpointSize]byte
}

type Server struct {
	arch       arch.Architecture
	executable string // Name of executable.
	dwarfData  *dwarf.Data
	table      *gosym.Table

	mu sync.Mutex

	fc chan func() error
	ec chan error

	proc        *os.Process
	procIsUp    bool
	stoppedPid  int
	stoppedRegs syscall.PtraceRegs
	breakpoints map[uint64]breakpoint
	files       []*file // Index == file descriptor.
}

// New parses the executable and builds local data structures for answering requests.
// It returns a Server ready to serve requests about the executable.
func New(executable string) (*Server, error) {
	fd, err := os.Open(executable)
	if err != nil {
		return nil, err
	}
	defer fd.Close()
	architecture, dwarfData, table, err := loadExecutable(fd)
	if err != nil {
		return nil, err
	}
	srv := &Server{
		arch:        *architecture,
		executable:  executable,
		dwarfData:   dwarfData,
		table:       table,
		fc:          make(chan func() error),
		ec:          make(chan error),
		breakpoints: make(map[uint64]breakpoint),
	}
	go ptraceRun(srv.fc, srv.ec)
	return srv, nil
}

func loadExecutable(f *os.File) (*arch.Architecture, *dwarf.Data, *gosym.Table, error) {
	// TODO: How do we detect NaCl?
	if obj, err := elf.NewFile(f); err == nil {
		dwarfData, err := obj.DWARF()
		if err != nil {
			return nil, nil, nil, err
		}

		table, err := parseElf(obj)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("parsing go symbol table: %v", err)
		}

		switch obj.Machine {
		case elf.EM_ARM:
			return &arch.ARM, dwarfData, table, nil
		case elf.EM_386:
			switch obj.Class {
			case elf.ELFCLASS32:
				return &arch.X86, dwarfData, table, nil
			case elf.ELFCLASS64:
				return &arch.AMD64, dwarfData, table, nil
			}
		case elf.EM_X86_64:
			return &arch.AMD64, dwarfData, table, nil
		}
		return nil, nil, nil, fmt.Errorf("unrecognized ELF architecture")
	}
	if obj, err := macho.NewFile(f); err == nil {
		dwarfData, err := obj.DWARF()
		if err != nil {
			return nil, nil, nil, err
		}

		/* TODO
		table, err := parseMachO(obj)
		if err != nil {
			return nil, nil, nil, err
		}
		*/
		switch obj.Cpu {
		case macho.Cpu386:
			return &arch.X86, dwarfData, nil, nil
		case macho.CpuAmd64:
			return &arch.AMD64, dwarfData, nil, nil
		}
		return nil, nil, nil, fmt.Errorf("unrecognized Mach-O architecture")
	}
	return nil, nil, nil, fmt.Errorf("unrecognized binary format")
}

// parseElf returns the gosym.Table representation of the old symbol tables.
// TODO: Delete this once we know how to get PC/line data out of DWARF.
func parseElf(f *elf.File) (*gosym.Table, error) {
	symdat, err := f.Section(".gosymtab").Data() // TODO unused.
	if err != nil {
		return nil, err
	}
	pclndat, err := f.Section(".gopclntab").Data()
	if err != nil {
		return nil, err
	}

	pcln := gosym.NewLineTable(pclndat, f.Section(".text").Addr)
	tab, err := gosym.NewTable(symdat, pcln)
	if err != nil {
		return nil, err
	}

	return tab, nil
}

type file struct {
	mode  string
	index int
	f     program.File
}

func (s *Server) Open(req *proxyrpc.OpenRequest, resp *proxyrpc.OpenResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// TODO: Better simulation. For now we just open the named OS file.
	var flag int
	switch req.Mode {
	case "r":
		flag = os.O_RDONLY
	case "w":
		flag = os.O_WRONLY
	case "rw":
		flag = os.O_RDWR
	default:
		return fmt.Errorf("Open: bad open mode %q", req.Mode)
	}
	osFile, err := os.OpenFile(req.Name, flag, 0)
	if err != nil {
		return err
	}
	// Find a file descriptor (index) slot.
	index := 0
	for ; index < len(s.files) && s.files[index] != nil; index++ {
	}
	f := &file{
		mode:  req.Mode,
		index: index,
		f:     osFile,
	}
	if index == len(s.files) {
		s.files = append(s.files, f)
	} else {
		s.files[index] = f
	}
	return nil
}

func (s *Server) ReadAt(req *proxyrpc.ReadAtRequest, resp *proxyrpc.ReadAtResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fd := req.FD
	if fd < 0 || len(s.files) <= fd || s.files[fd] == nil {
		return fmt.Errorf("ReadAt: bad file descriptor %d", fd)
	}
	f := s.files[fd]
	buf := make([]byte, req.Len) // TODO: Don't allocate every time
	n, err := f.f.ReadAt(buf, req.Offset)
	resp.Data = buf[:n]
	return err
}

func (s *Server) Close(req *proxyrpc.CloseRequest, resp *proxyrpc.CloseResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	fd := req.FD
	if fd < 0 || fd >= len(s.files) || s.files[fd] == nil {
		return fmt.Errorf("Close: bad file descriptor %d", fd)
	}
	err := s.files[fd].f.Close()
	// Remove it regardless
	s.files[fd] = nil
	return err
}

func (s *Server) Run(req *proxyrpc.RunRequest, resp *proxyrpc.RunResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.proc != nil {
		s.proc.Kill()
		s.proc = nil
		s.procIsUp = false
		s.stoppedPid = 0
		s.stoppedRegs = syscall.PtraceRegs{}
	}
	p, err := s.startProcess(s.executable, nil, &os.ProcAttr{
		Files: []*os.File{
			nil,       // TODO: be able to feed the target's stdin.
			os.Stderr, // TODO: be able to capture the target's stdout.
			os.Stderr,
		},
		Sys: &syscall.SysProcAttr{
			Pdeathsig: syscall.SIGKILL,
			Ptrace:    true,
		},
	})
	if err != nil {
		return err
	}
	s.proc = p
	s.stoppedPid = p.Pid
	return nil
}

func (s *Server) Resume(req *proxyrpc.ResumeRequest, resp *proxyrpc.ResumeResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.proc == nil {
		return fmt.Errorf("Resume: Run did not successfully start a process")
	}

	if !s.procIsUp {
		s.procIsUp = true
		_, err := s.waitForTrap(s.stoppedPid)
		if err != nil {
			return err
		}
		err = s.ptraceSetOptions(s.stoppedPid, syscall.PTRACE_O_TRACECLONE)
		if err != nil {
			return fmt.Errorf("ptraceSetOptions: %v", err)
		}
	} else if _, ok := s.breakpoints[s.stoppedRegs.Rip]; ok {
		err := s.ptraceSingleStep(s.stoppedPid)
		if err != nil {
			return fmt.Errorf("ptraceSingleStep: %v", err)
		}
		_, err = s.waitForTrap(s.stoppedPid)
		if err != nil {
			return err
		}
	}

	err := s.setBreakpoints()
	if err != nil {
		return err
	}
	err = s.ptraceCont(s.stoppedPid, 0)
	if err != nil {
		return fmt.Errorf("ptraceCont: %v", err)
	}

	s.stoppedPid, err = s.waitForTrap(-1)
	if err != nil {
		return err
	}

	err = s.liftBreakpoints()
	if err != nil {
		return err
	}

	err = s.ptraceGetRegs(s.stoppedPid, &s.stoppedRegs)
	if err != nil {
		return fmt.Errorf("ptraceGetRegs: %v", err)
	}

	s.stoppedRegs.Rip -= uint64(s.arch.BreakpointSize)

	err = s.ptraceSetRegs(s.stoppedPid, &s.stoppedRegs)
	if err != nil {
		return fmt.Errorf("ptraceSetRegs: %v", err)
	}

	resp.Status.PC = s.stoppedRegs.Rip
	resp.Status.SP = s.stoppedRegs.Rsp
	return nil
}

func (s *Server) waitForTrap(pid int) (wpid int, err error) {
	for {
		wpid, status, err := s.wait(pid)
		if err != nil {
			return 0, fmt.Errorf("wait: %v", err)
		}
		if status.StopSignal() == syscall.SIGTRAP && status.TrapCause() != syscall.PTRACE_EVENT_CLONE {
			return wpid, nil
		}
		err = s.ptraceCont(wpid, 0) // TODO: non-zero when wait catches other signals?
		if err != nil {
			return 0, fmt.Errorf("ptraceCont: %v", err)
		}
	}
}

func (s *Server) Breakpoint(req *proxyrpc.BreakpointRequest, resp *proxyrpc.BreakpointResponse) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	addrs, err := s.eval(req.Address)
	if err != nil {
		return err
	}
	var bp breakpoint
	for _, addr := range addrs {
		pc, err := s.evalAddress(addr)
		if err != nil {
			return err
		}
		if _, alreadySet := s.breakpoints[pc]; alreadySet {
			return fmt.Errorf("breakpoint already set at %#x (TODO)", pc)
		}

		err = s.ptracePeek(s.stoppedPid, uintptr(pc), bp.origInstr[:s.arch.BreakpointSize])
		if err != nil {
			return fmt.Errorf("ptracePeek: %v", err)
		}
		bp.pc = pc
		s.breakpoints[pc] = bp
	}

	return nil
}

func (s *Server) setBreakpoints() error {
	for pc := range s.breakpoints {
		err := s.ptracePoke(s.stoppedPid, uintptr(pc), s.arch.BreakpointInstr[:s.arch.BreakpointSize])
		if err != nil {
			return fmt.Errorf("setBreakpoints: %v", err)
		}
	}
	return nil
}

func (s *Server) liftBreakpoints() error {
	for pc, breakpoint := range s.breakpoints {
		err := s.ptracePoke(s.stoppedPid, uintptr(pc), breakpoint.origInstr[:s.arch.BreakpointSize])
		if err != nil {
			return fmt.Errorf("liftBreakpoints: %v", err)
		}
	}
	return nil
}

func (s *Server) Eval(req *proxyrpc.EvalRequest, resp *proxyrpc.EvalResponse) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	resp.Result, err = s.eval(req.Expr)
	return err
}

// eval evaluates an expression.
// TODO: very weak.
func (s *Server) eval(expr string) ([]string, error) {
	switch {
	case strings.HasPrefix(expr, "re:"):
		// Regular expression. Return list of symbols.
		re, err := regexp.Compile(expr[3:])
		if err != nil {
			return nil, err
		}
		return s.lookupRE(re)

	case strings.HasPrefix(expr, "sym:"):
		// Symbol lookup. Return address.
		addr, err := s.lookupSym(expr[4:])
		if err != nil {
			return nil, err
		}
		return []string{fmt.Sprintf("%#x", addr)}, nil

	case strings.HasPrefix(expr, "src:"):
		// Numerical address. Return file.go:123.
		addr, err := strconv.ParseUint(expr[4:], 0, 0)
		if err != nil {
			return nil, err
		}
		file, line, ok := s.lookupSource(addr)
		if !ok {
			return nil, fmt.Errorf("no PC/line data for: %q", expr)
		}
		return []string{fmt.Sprintf("%s:%d", file, line)}, nil

	case len(expr) > 0 && '0' <= expr[0] && expr[0] <= '9':
		// Numerical address. Return symbol.
		addr, err := strconv.ParseUint(expr, 0, 0)
		if err != nil {
			return nil, err
		}
		funcName, err := s.lookupPC(addr)
		if err != nil {
			return nil, err
		}
		return []string{funcName}, nil
	}

	return nil, fmt.Errorf("bad expression syntax: %q", expr)
}

func (s *Server) lookupSource(pc uint64) (file string, line int, ok bool) {
	if s.table == nil {
		return
	}
	file, line, fn := s.table.PCToLine(pc)
	return file, line, fn != nil
}

// evalAddress takes a simple expression, either a symbol or hex value,
// and evaluates it as an address.
func (s *Server) evalAddress(expr string) (uint64, error) {
	// Might be a symbol.
	addr, err := s.lookupSym(expr)
	if err == nil {
		return addr, nil
	}

	// Must be a number.
	addr, err = strconv.ParseUint(expr, 0, 0)
	if err != nil {
		return 0, fmt.Errorf("eval: %q is neither symbol nor number", expr)
	}

	return addr, nil
}

func (s *Server) Frames(req *proxyrpc.FramesRequest, resp *proxyrpc.FramesResponse) error {
	// TODO: verify that we're stopped.
	s.mu.Lock()
	defer s.mu.Unlock()

	if req.Count != 1 {
		// TODO: implement.
		return fmt.Errorf("Frames.Count != 1 is not implemented")
	}

	// TODO: we're assuming we're at a function's entry point (LowPC).

	regs := syscall.PtraceRegs{}
	err := s.ptraceGetRegs(s.stoppedPid, &regs)
	if err != nil {
		return err
	}
	fp := regs.Rsp + uint64(s.arch.PointerSize)

	entry, err := s.entryForPC(regs.Rip)
	if err != nil {
		return err
	}

	var buf [8]byte
	frame := program.Frame{}
	r := s.dwarfData.Reader()
	r.Seek(entry.Offset)
	for {
		entry, err := r.Next()
		if err != nil {
			return err
		}
		if entry.Tag == 0 {
			break
		}
		if entry.Tag != dwarf.TagFormalParameter {
			continue
		}
		if entry.Children {
			// TODO: handle this??
			return fmt.Errorf("FormalParameter has children, expected none")
		}
		// TODO: the returned frame should be structured instead of a hacked up string.
		location := uintptr(0)
		for _, f := range entry.Field {
			switch f.Attr {
			case dwarf.AttrLocation:
				offset := evalLocation(f.Val.([]uint8))
				location = uintptr(int64(fp) + offset)
				frame.S += fmt.Sprintf("(%d(FP))", offset)
			case dwarf.AttrName:
				frame.S += " " + f.Val.(string)
			case dwarf.AttrType:
				t, err := s.dwarfData.Type(f.Val.(dwarf.Offset))
				if err == nil {
					frame.S += fmt.Sprintf("[%v]", t)
				}
				if t.String() != "int" || t.Size() != int64(s.arch.IntSize) {
					break
				}
				if location == 0 {
					return fmt.Errorf("no location for FormalParameter")
				}
				err = s.ptracePeek(s.stoppedPid, location, buf[:s.arch.IntSize])
				if err != nil {
					return err
				}
				frame.S += fmt.Sprintf("==%#x", s.arch.Int(buf[:s.arch.IntSize]))
			}
		}
	}
	resp.Frames = append(resp.Frames, frame)
	return nil
}