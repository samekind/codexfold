//go:build !darwin && !linux

package testfs

func processResourceUsage() resourceUsage { return resourceUsage{} }
