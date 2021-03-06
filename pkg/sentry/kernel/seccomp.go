// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kernel

import (
	"syscall"

	"gvisor.googlesource.com/gvisor/pkg/abi/linux"
	"gvisor.googlesource.com/gvisor/pkg/binary"
	"gvisor.googlesource.com/gvisor/pkg/bpf"
	"gvisor.googlesource.com/gvisor/pkg/sentry/arch"
	"gvisor.googlesource.com/gvisor/pkg/sentry/usermem"
	"gvisor.googlesource.com/gvisor/pkg/syserror"
)

const maxSyscallFilterInstructions = 1 << 15

type seccompResult int

const (
	// seccompResultDeny indicates that a syscall should not be executed.
	seccompResultDeny seccompResult = iota

	// seccompResultAllow indicates that a syscall should be executed.
	seccompResultAllow

	// seccompResultKill indicates that the task should be killed immediately,
	// with the exit status indicating that the task was killed by SIGSYS.
	seccompResultKill

	// seccompResultTrace indicates that a ptracer was successfully notified as
	// a result of a SECCOMP_RET_TRACE.
	seccompResultTrace
)

// seccompData is equivalent to struct seccomp_data, which contains the data
// passed to seccomp-bpf filters.
type seccompData struct {
	// nr is the system call number.
	nr int32

	// arch is an AUDIT_ARCH_* value indicating the system call convention.
	arch uint32

	// instructionPointer is the value of the instruction pointer at the time
	// of the system call.
	instructionPointer uint64

	// args contains the first 6 system call arguments.
	args [6]uint64
}

func (d *seccompData) asBPFInput() bpf.Input {
	return bpf.InputBytes{binary.Marshal(nil, usermem.ByteOrder, d), usermem.ByteOrder}
}

func seccompSiginfo(t *Task, errno, sysno int32, ip usermem.Addr) *arch.SignalInfo {
	si := &arch.SignalInfo{
		Signo: int32(linux.SIGSYS),
		Errno: errno,
		Code:  arch.SYS_SECCOMP,
	}
	si.SetCallAddr(uint64(ip))
	si.SetSyscall(sysno)
	si.SetArch(t.SyscallTable().AuditNumber)
	return si
}

// checkSeccompSyscall applies the task's seccomp filters before the execution
// of syscall sysno at instruction pointer ip. (These parameters must be passed
// in because vsyscalls do not use the values in t.Arch().)
//
// Preconditions: The caller must be running on the task goroutine.
func (t *Task) checkSeccompSyscall(sysno int32, args arch.SyscallArguments, ip usermem.Addr) seccompResult {
	result := t.evaluateSyscallFilters(sysno, args, ip)
	switch result & linux.SECCOMP_RET_ACTION {
	case linux.SECCOMP_RET_TRAP:
		// "Results in the kernel sending a SIGSYS signal to the triggering
		// task without executing the system call. ... The SECCOMP_RET_DATA
		// portion of the return value will be passed as si_errno." -
		// Documentation/prctl/seccomp_filter.txt
		t.SendSignal(seccompSiginfo(t, int32(result&linux.SECCOMP_RET_DATA), sysno, ip))
		return seccompResultDeny

	case linux.SECCOMP_RET_ERRNO:
		// "Results in the lower 16-bits of the return value being passed to
		// userland as the errno without executing the system call."
		t.Arch().SetReturn(-uintptr(result & linux.SECCOMP_RET_DATA))
		return seccompResultDeny

	case linux.SECCOMP_RET_TRACE:
		// "When returned, this value will cause the kernel to attempt to
		// notify a ptrace()-based tracer prior to executing the system call.
		// If there is no tracer present, -ENOSYS is returned to userland and
		// the system call is not executed."
		if t.ptraceSeccomp(uint16(result & linux.SECCOMP_RET_DATA)) {
			return seccompResultTrace
		}
		// This useless-looking temporary is needed because Go.
		tmp := uintptr(syscall.ENOSYS)
		t.Arch().SetReturn(-tmp)
		return seccompResultDeny

	case linux.SECCOMP_RET_ALLOW:
		// "Results in the system call being executed."
		return seccompResultAllow

	case linux.SECCOMP_RET_KILL:
		// "Results in the task exiting immediately without executing the
		// system call. The exit status of the task will be SIGSYS, not
		// SIGKILL."
		fallthrough
	default: // consistent with Linux
		return seccompResultKill
	}
}

func (t *Task) evaluateSyscallFilters(sysno int32, args arch.SyscallArguments, ip usermem.Addr) uint32 {
	data := seccompData{
		nr:                 sysno,
		arch:               t.tc.st.AuditNumber,
		instructionPointer: uint64(ip),
	}
	// data.args is []uint64 and args is []arch.SyscallArgument (uintptr), so
	// we can't do any slicing tricks or even use copy/append here.
	for i, arg := range args {
		if i >= len(data.args) {
			break
		}
		data.args[i] = arg.Uint64()
	}
	input := data.asBPFInput()

	ret := uint32(linux.SECCOMP_RET_ALLOW)
	f := t.syscallFilters.Load()
	if f == nil {
		return ret
	}

	// "Every filter successfully installed will be evaluated (in reverse
	// order) for each system call the task makes." - kernel/seccomp.c
	for i := len(f.([]bpf.Program)) - 1; i >= 0; i-- {
		thisRet, err := bpf.Exec(f.([]bpf.Program)[i], input)
		if err != nil {
			t.Debugf("seccomp-bpf filter %d returned error: %v", i, err)
			thisRet = linux.SECCOMP_RET_KILL
		}
		// "If multiple filters exist, the return value for the evaluation of a
		// given system call will always use the highest precedent value." -
		// Documentation/prctl/seccomp_filter.txt
		//
		// (Note that this contradicts prctl(2): "If the filters permit prctl()
		// calls, then additional filters can be added; they are run in order
		// until the first non-allow result is seen." prctl(2) is incorrect.)
		//
		// "The ordering ensures that a min_t() over composed return values
		// always selects the least permissive choice." -
		// include/uapi/linux/seccomp.h
		if (thisRet & linux.SECCOMP_RET_ACTION) < (ret & linux.SECCOMP_RET_ACTION) {
			ret = thisRet
		}
	}

	return ret
}

// AppendSyscallFilter adds BPF program p as a system call filter.
//
// Preconditions: The caller must be running on the task goroutine.
func (t *Task) AppendSyscallFilter(p bpf.Program) error {
	// Cap the combined length of all syscall filters (plus a penalty of 4
	// instructions per filter beyond the first) to
	// maxSyscallFilterInstructions. (This restriction is inherited from
	// Linux.)
	totalLength := p.Length()
	var newFilters []bpf.Program

	// While syscallFilters are an atomic.Value we must take the mutex to
	// prevent our read-copy-update from happening while another task
	// is syncing syscall filters to us, this keeps the filters in a
	// consistent state.
	t.mu.Lock()
	defer t.mu.Unlock()
	if sf := t.syscallFilters.Load(); sf != nil {
		oldFilters := sf.([]bpf.Program)
		for _, f := range oldFilters {
			totalLength += f.Length() + 4
		}
		newFilters = append(newFilters, oldFilters...)
	}

	if totalLength > maxSyscallFilterInstructions {
		return syserror.ENOMEM
	}

	newFilters = append(newFilters, p)
	t.syscallFilters.Store(newFilters)
	return nil
}

// SyncSyscallFiltersToThreadGroup will copy this task's filters to all other
// threads in our thread group.
func (t *Task) SyncSyscallFiltersToThreadGroup() error {
	f := t.syscallFilters.Load()

	t.tg.pidns.owner.mu.RLock()
	defer t.tg.pidns.owner.mu.RUnlock()

	// Note: No new privs is always assumed to be set.
	for ot := t.tg.tasks.Front(); ot != nil; ot = ot.Next() {
		if ot.ThreadID() != t.ThreadID() {
			// We must take the other task's mutex to prevent it from
			// appending to its own syscall filters while we're syncing.
			ot.mu.Lock()
			var copiedFilters []bpf.Program
			if f != nil {
				copiedFilters = append(copiedFilters, f.([]bpf.Program)...)
			}
			ot.syscallFilters.Store(copiedFilters)
			ot.mu.Unlock()
		}
	}
	return nil
}

// SeccompMode returns a SECCOMP_MODE_* constant indicating the task's current
// seccomp syscall filtering mode, appropriate for both prctl(PR_GET_SECCOMP)
// and /proc/[pid]/status.
func (t *Task) SeccompMode() int {
	f := t.syscallFilters.Load()
	if f != nil && len(f.([]bpf.Program)) > 0 {
		return linux.SECCOMP_MODE_FILTER
	}
	return linux.SECCOMP_MODE_NONE
}
