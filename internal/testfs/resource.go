package testfs

import "time"

type resourceUsage struct {
	MaxRSSBytes uint64
	UserCPU     time.Duration
	SystemCPU   time.Duration
}

func subtractUsage(after resourceUsage, before resourceUsage) resourceUsage {
	result := after
	if after.UserCPU >= before.UserCPU {
		result.UserCPU = after.UserCPU - before.UserCPU
	}
	if after.SystemCPU >= before.SystemCPU {
		result.SystemCPU = after.SystemCPU - before.SystemCPU
	}
	return result
}
