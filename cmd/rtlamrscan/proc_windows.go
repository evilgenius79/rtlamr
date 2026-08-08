//go:build windows

package main

import (
	"log"
	"unsafe"

	"golang.org/x/sys/windows"
)

// killChildrenOnExit places this process in a Windows job object configured
// to kill every process in the job when the job handle closes. Child
// processes join the job automatically, so rtl_tcp and rtlamr are terminated
// even if the user closes the console window with the X button.
func killChildrenOnExit() {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		log.Printf("warning: could not create job object: %v", err)
		return
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		log.Printf("warning: could not configure job object: %v", err)
		return
	}

	if err = windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		log.Printf("warning: could not join job object: %v", err)
	}

	// The job handle is intentionally not closed: it must stay open for the
	// lifetime of this process so the kill-on-close semantics apply when the
	// process exits.
}
